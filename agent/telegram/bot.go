// 文件: telegram/bot.go
// 修改: 
// 1. 完全基于原代码，保留 5s 合并缓冲机制
// 2. 简化模板：仅保留服务、环境、命名空间、新/旧版本、结果
// 3. 支持附加事件信息（extra）
// 4. 使用 safe 转义函数，避免 Markdown 转义问题
// 5. 合并发送时：相同状态合并，附加事件分别列出
// 6. 保留所有原有功能：Stop、flushBuffer、sendMessage 等

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

// NotificationItem 通知项（新增 extra 字段）
type NotificationItem struct {
	Service    string
	Env        string
	User       string
	OldVersion string
	NewVersion string
	Success    bool
	Extra      string // 附加事件信息
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
	escapeChars := []string{"_", "*", "[", "]", "(", ")", "~", "`", ">", "#", "+", "-", "=", "|", "{", "}", ".", "!"}
	for _, char := range escapeChars {
		text = strings.ReplaceAll(text, char, "\\"+char)
	}
	return text
}

// AddNotification 添加通知到缓冲（支持 extra）
func (bm *BotManager) AddNotification(service, env, user, oldVersion, newVersion string, success bool, extra string) {
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
		Extra:      extra,
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
// 文件: telegram/bot.go
// 修改: flushBuffer 异步发送，避免阻塞 Stop

func (bm *BotManager) flushBuffer() {
	bm.buffer.mu.Lock()
	items := make(map[string][]*NotificationItem)
	for k, v := range bm.buffer.buffers {
		items[k] = append([]*NotificationItem{}, v...)
		bm.buffer.buffers[k] = []*NotificationItem{}
	}
	bm.buffer.mu.Unlock()

	// 异步发送
	go func() {
		for statusKey, list := range items {
			if len(list) == 0 { continue }
			success := statusKey == "success"
			msg := bm.generateMergedMessage(list, success)
			bm.sendMessage(msg, "MarkdownV2")
		}
	}()
}

func (bm *BotManager) Stop() {
	if bm.buffer != nil && bm.buffer.mergeTimer != nil {
		bm.buffer.mergeTimer.Stop()
		bm.flushBuffer() // 异步
	}
	logrus.Info(color.GreenString("Telegram BotManager 停止"))
}

// generateMergedMessage 生成合并消息（5s内相同状态）
func (bm *BotManager) generateMergedMessage(items []*NotificationItem, success bool) string {
	safe := escapeForMarkdown
	header := fmt.Sprintf("*部署%s (%d 个服务)*\n\n", map[bool]string{true: "成功", false: "失败"}[success], len(items))

	body := ""
	for i, item := range items {
		// 基础信息
		line := fmt.Sprintf("%d. *服务*: `%s` | *环境*: `%s` | *命名空间*: `%s` | *旧*: `%s` → *新*: `%s`\n",
			i+1, safe(item.Service), safe(item.Env), safe(item.User), safe(item.OldVersion), safe(item.NewVersion))

		// 附加事件（仅失败时）
		if !success && item.Extra != "" {
			line += fmt.Sprintf("   _异常事件_: %s\n", safe(item.Extra))
		}
		body += line
	}

	status := map[bool]string{true: "*部署成功*", false: "*部署失败，已回滚*"}[success]
	footer := fmt.Sprintf("\n**状态**: %s\n**时间**: `%s`\n\n---\n*由 K8s\\-CICD Agent 自动发送*",
		status, time.Now().Format("2006-01-02 15:04:05"))

	return header + body + footer
}

// SendNotification 发送部署通知（现在缓冲添加）
func (bm *BotManager) SendNotification(service, env, user, oldVersion, newVersion string, success bool, extra string) error {
	if !bm.Enabled {
		return nil
	}
	bm.AddNotification(service, env, user, oldVersion, newVersion, success, extra)
	return nil
}

// sendMessage 发送消息（使用 MarkdownV2）
func (bm *BotManager) sendMessage(text, parseMode string) (int, error) {
	if !bm.Enabled {
		return 0, fmt.Errorf("Telegram 未启用")
	}
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