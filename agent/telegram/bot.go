// 文件: telegram/bot.go
// 优化: 移除 5s 缓冲逻辑，立即发送通知
// 修复: 删除 AddNotification、flushBuffer、generateMergedMessage 等残留函数
// 功能: Telegram 通知（MarkdownV2 格式，立即发送）

package telegram

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"k8s-cicd/agent/config"

	"github.com/fatih/color"
	"github.com/sirupsen/logrus"
)

// BotManager 简化版：仅发送通知（无缓冲）
type BotManager struct {
	Token   string // Bot Token
	GroupID string // 群组ID
	Enabled bool   // 是否启用
}

// NewBotManager 创建 BotManager（配置文件 token 和 group_id 存在即启用）
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

// escapeForMarkdown 简单 Markdown 转义函数
func escapeForMarkdown(text string) string {
	escapeChars := []string{"_", "*", "[", "]", "(", ")", "~", "`", ">", "#", "+", "-", "=", "|", "{", "}", ".", "!"}
	for _, char := range escapeChars {
		text = strings.ReplaceAll(text, char, "\\"+char)
	}
	return text
}

// SendNotification 发送部署通知（立即发送，MarkdownV2 格式）
func (bm *BotManager) SendNotification(service, env, user, oldVersion, newVersion string, success bool, extra string) error {
	if !bm.Enabled {
		return nil
	}

	safe := escapeForMarkdown
	status := map[bool]string{true: "成功", false: "失败"}[success]
	emoji := map[bool]string{true: "✅", false: "❌"}[success]
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
	jsonData, err := json.Marshal(payload)
	if err != nil {
		return 0, fmt.Errorf("JSON 序列化失败: %v", err)
	}
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

// Stop 停止 BotManager
func (bm *BotManager) Stop() {
	logrus.Info(color.GreenString("Telegram BotManager 已停止"))
}