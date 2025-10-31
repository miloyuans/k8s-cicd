// 修改后的 telegram/bot.go：修复 safe 函数定义（单参数转义）；使用简单 Markdown 转义函数。

package telegram

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"k8s-cicd/agent/config"

	"github.com/fatih/color"
	"github.com/sirupsen/logrus"
)

// NotificationBuffer 通知缓冲 (5s 窗口，相同状态合并)
type NotificationBuffer struct {
	mu          sync.Mutex
	buffers     map[string][]*NotificationItem // key: "success" or "failure", value: list of items
	mergeTimer  *time.Timer
	mergeChan   chan struct{}
}

// NotificationItem 通知项
type NotificationItem struct {
	Service    string
	Env        string
	User       string
	OldVersion string
	NewVersion string
	Success    bool
	SendTime   time.Time
}

// BotManager 简化版：仅发送通知（添加缓冲）
type BotManager struct {
	Token   string // Bot Token
	GroupID string // 群组ID
	Enabled bool   // 是否启用
	buffer  *NotificationBuffer
}

// NewBotManager 创建 BotManager（从配置读取，初始化缓冲）
func NewBotManager(cfg *config.TelegramConfig) *BotManager {
	if cfg == nil || !cfg.Enabled || cfg.Token == "" || cfg.GroupID == "" {
		logrus.Warn(color.YellowString("Telegram 配置无效，通知功能禁用"))
		return &BotManager{Enabled: false}
	}

	bm := &BotManager{
		Token:   cfg.Token,
		GroupID: cfg.GroupID,
		Enabled: true,
		buffer:  &NotificationBuffer{
			buffers:   make(map[string][]*NotificationItem),
			mergeChan: make(chan struct{}, 1),
		},
	}
	bm.buffer.mergeTimer = time.AfterFunc(5*time.Second, bm.flushBuffer)
	logrus.Info(color.GreenString("Telegram BotManager 创建成功（通知专用，5s合并缓冲）"))
	return bm
}

// escapeForMarkdown 简单 Markdown 转义函数 (单参数)
func escapeForMarkdown(text string) string {
	// 基本转义 Markdown 特殊字符
	escapeChars := []string{"_", "*", "[", "]", "(", ")", "~", "`", ">", "#", "+", "-", "=", "|", "{", "}", ".", "!"}
	for _, char := range escapeChars {
		text = strings.ReplaceAll(text, char, "\\"+char)
	}
	return text
}

// AddNotification 添加通知到缓冲
func (bm *BotManager) AddNotification(service, env, user, oldVersion, newVersion string, success bool) {
	if !bm.Enabled {
		return
	}
	item := &NotificationItem{
		Service:    service,
		Env:        env,
		User:       user,
		OldVersion: oldVersion,
		NewVersion: newVersion,
		Success:    success,
		SendTime:   time.Now(),
	}
	statusKey := map[bool]string{true: "success", false: "failure"}[success]

	bm.buffer.mu.Lock()
	if _, ok := bm.buffer.buffers[statusKey]; !ok {
		bm.buffer.buffers[statusKey] = []*NotificationItem{}
	}
	bm.buffer.buffers[statusKey] = append(bm.buffer.buffers[statusKey], item)
	bm.buffer.mu.Unlock()

	// 重置定时器
	if bm.buffer.mergeTimer != nil {
		bm.buffer.mergeTimer.Reset(5 * time.Second)
	}
}

// flushBuffer 刷新缓冲，合并发送
func (bm *BotManager) flushBuffer() {
	bm.buffer.mu.Lock()
	defer bm.buffer.mu.Unlock()

	for statusKey, items := range bm.buffer.buffers {
		if len(items) == 0 {
			continue
		}
		success := statusKey == "success"
		mergedMsg := bm.generateMergedMessage(items, success)
		bm.sendMessage(mergedMsg, "MarkdownV2")
		bm.buffer.buffers[statusKey] = []*NotificationItem{}
	}
	bm.buffer.mergeChan <- struct{}{}
}

// generateMergedMessage 生成合并消息（5s内相同状态）
func (bm *BotManager) generateMergedMessage(items []*NotificationItem, success bool) string {
	// 使用 escapeForMarkdown 转义
	safe := escapeForMarkdown
	header := fmt.Sprintf("*Deployment %s (%d 服务)*\n\n", map[bool]string{true: "成功", false: "失败"}[success], len(items))
	status := map[bool]string{true: "Success *部署成功*", false: "Failure *部署失败-已回滚*"}[success]

	body := "**详情**:\n"
	for _, item := range items {
		body += fmt.Sprintf("• 服务: `%s` | 环境: `%s` | 操作人: `%s` | 旧: `%s` → 新: `%s`\n",
			safe(item.Service), safe(item.Env), safe(item.User), safe(item.OldVersion), safe(item.NewVersion))
	}

	return fmt.Sprintf("%s%s**状态**: %s\n**时间**: `%s`\n\n---\n*由 K8s\\\\-CICD Agent 自动发送*",
		header, body, status, time.Now().Format("2006-01-02 15:04:05"))
}

// SendNotification 发送部署通知（现在缓冲添加）
func (bm *BotManager) SendNotification(service, env, user, oldVersion, newVersion string, success bool) error {
	if !bm.Enabled {
		return nil
	}
	bm.AddNotification(service, env, user, oldVersion, newVersion, success)
	return nil
}

// sendMessage 发送消息（使用 MarkdownV2）
func (bm *BotManager) sendMessage(text, parseMode string) (int, error) {
	if !bm.Enabled {
		return 0, fmt.Errorf("Telegram 未启用")
	}
	// 使用预转义的 text
	payload := map[string]interface{}{
		"chat_id":    bm.GroupID,
		"text":       text,
		"parse_mode": parseMode,
	}
	jsonData, _ := json.Marshal(payload)
	resp, err := http.Post(fmt.Sprintf("https://api.telegram.org/bot%s/sendMessage", bm.Token),
		"application/json", bytes.NewBuffer(jsonData))
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()

	var result struct {
		Ok          bool                   `json:"ok"`
		Result      map[string]interface{} `json:"result"`
		ErrorCode   int                    `json:"error_code"`
		Description string                 `json:"description"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return 0, err
	}
	if !result.Ok {
		return 0, fmt.Errorf("Telegram API错误: code=%d, description=%s", result.ErrorCode, result.Description)
	}
	messageID := int(result.Result["message_id"].(float64))
	logrus.Infof(color.GreenString("消息发送成功，message_id=%d"), messageID)
	return messageID, nil
}

// Stop 停止（刷新缓冲）
func (bm *BotManager) Stop() {
	if bm.buffer != nil && bm.buffer.mergeTimer != nil {
		bm.buffer.mergeTimer.Stop()
		bm.flushBuffer() // 强制发送剩余
	}
	logrus.Info(color.GreenString("Telegram BotManager 停止 (缓冲已刷新)"))
}