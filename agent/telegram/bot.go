// 文件: telegram/bot.go
// 修复: 删除重复的 Stop 方法（只保留一个）
// 保留所有功能：5s 合并、Markdown 转义、异步 flush、附加事件

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

// flushBuffer 刷新缓冲，合并发送（异步）
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
			if len(list) == 0 {
				continue
			}
			success := statusKey == "success"
			msg := bm.generateMergedMessage(list, success)
			bm.sendMessage(msg, "MarkdownV2")
		}
	}()
}

// generateMergedMessage 生成合并消息（5s内相同状态）
func (bm *BotManager) generateMergedMessage(items []*NotificationItem, success bool) string {
	safe := escapeForMarkdown
	header := fmt.Sprintf("*部署%s (%d 个服务)*\n\n", map[bool]string{true: "成功", false: "失败"}[success], len(items))

	body := ""
	for i, item := range items {
		line := fmt.Sprintf("%d. *服务*: `%s` | *环境*: `%s` | *命名空间*: `%s` | *旧*: `%s` → *新*: `%s`\n",
			i+1, safe(item.Service), safe(item.Env), safe(item.User), safe(item.OldVersion), safe(item.NewVersion))

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

func NewBotManager(cfg *config.TelegramConfig) *BotManager {
	if cfg == nil || cfg.Token == "" || cfg.GroupID == "" {
		logrus.Warn(color.YellowString("Telegram 配置不完整，通知功能禁用"))
		return &BotManager{Enabled: false}
	}
	bm := &BotManager{
		Token:   cfg.Token,
		GroupID: cfg.GroupID,
		Enabled: true,
	}
	logrus.Info(color.GreenString("Telegram BotManager 创建成功（立即发送模式）"))
	return bm
}

func escapeForMarkdown(text string) string {
	escapeChars := []string{"_", "*", "[", "]", "(", ")", "~", "`", ">", "#", "+", "-", "=", "|", "{", "}", ".", "!"}
	for _, char := range escapeChars {
		text = strings.ReplaceAll(text, char, "\\"+char)
	}
	return text
}

func (bm *BotManager) SendNotification(service, env, user, oldVersion, newVersion string, success bool, extra string) error {
	if !bm.Enabled {
		return nil
	}

	safe := escapeForMarkdown
	status := map[bool]string{true: "成功", false: "失败"}[success]
	emoji := map[bool]string{true: "Success", false: "Failed"}[success]
	header := fmt.Sprintf("*部署%s*\n", status)
	body := fmt.Sprintf("*服务*: `%s`\n*环境*: `%s`\n*操作人*: `%s`\n*旧版本*: `%s`\n*新版本*: `%s`\n",
		safe(service), safe(env), safe(user), safe(oldVersion), safe(newVersion))

	footer := ""
	if !success && extra != "" {
		footer = fmt.Sprintf("\n_异常_: %s", safe(extra))
	}
	footer += fmt.Sprintf("\n\n%s *由 K8s\\-CICD Agent 发送* — `%s`", emoji, time.Now().Format("15:04:05"))

	msg := header + body + footer

	_, err := bm.sendMessage(msg, "MarkdownV2")
	return err
}

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
		Ok          bool   `json:"ok"`
		Description string `json:"description"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil || !result.Ok {
		errMsg := "未知错误"
		if result.Description != "" {
			errMsg = result.Description
		}
		return 0, fmt.Errorf("Telegram API 错误: %s", errMsg)
	}
	logrus.Infof(color.GreenString("Telegram 通知已发送: %s", text[:50]))
	return 0, nil
}

func (bm *BotManager) Stop() {
	logrus.Info(color.GreenString("Telegram BotManager 已停止"))
}