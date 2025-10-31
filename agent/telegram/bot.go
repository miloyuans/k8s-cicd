// 修改后的 telegram/bot.go：增强 generateMarkdownMessage 模板，明确包含回滚信息（失败时）。

package telegram

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"k8s-cicd/agent/config" // 新增导入 config

	"github.com/fatih/color"
	"github.com/sirupsen/logrus"
)

// BotManager 简化版：仅发送通知
type BotManager struct {
	Token   string // Bot Token
	GroupID string // 群组ID
	Enabled bool   // 是否启用
}

// NewBotManager 创建 BotManager（从配置读取）
func NewBotManager(cfg *config.TelegramConfig) *BotManager {
	if cfg == nil || !cfg.Enabled || cfg.Token == "" || cfg.GroupID == "" {
		logrus.Warn(color.YellowString("Telegram 配置无效，通知功能禁用"))
		return &BotManager{Enabled: false}
	}

	bm := &BotManager{
		Token:   cfg.Token,
		GroupID: cfg.GroupID,
		Enabled: true,
	}
	logrus.Info(color.GreenString("Telegram BotManager 创建成功（通知专用）"))
	return bm
}

// escapeMarkdownV2 转义 MarkdownV2
func escapeMarkdownV2(text string) string {
	escapeChars := []rune{'_', '*', '[', ']', '(', ')', '~', '`', '>', '#', '+', '-', '=', '|', '{', '}', '.', '!'}
	var result strings.Builder
	for _, c := range text {
		if containsRune(escapeChars, c) {
			result.WriteString("\\")
		}
		result.WriteRune(c)
	}
	return result.String()
}

func containsRune(slice []rune, r rune) bool {
	for _, s := range slice {
		if s == r {
			return true
		}
	}
	return false
}

// sendMessage 发送消息
func (bm *BotManager) sendMessage(text, parseMode string) (int, error) {
	if !bm.Enabled {
		return 0, fmt.Errorf("Telegram 未启用")
	}
	text = escapeMarkdownV2(text)
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

// generateMarkdownMessage 生成通知（优化：失败时明确包含回滚信息）
func (bm *BotManager) generateMarkdownMessage(service, env, user, oldVersion, newVersion string, success bool) string {
	safe := escapeMarkdownV2
	if success {
		status := "Success *部署成功*"
		return fmt.Sprintf("*Deployment %s %s*\n\n"+
			"**服务**: `%s`\n"+
			"**环境**: `%s`\n"+
			"**操作人**: `%s`\n"+
			"**旧版本**: `%s`\n"+
			"**新版本**: `%s`\n"+
			"**状态**: %s\n"+
			"**时间**: `%s`\n\n"+
			"---\n"+
			"*由 K8s\\-CICD Agent 自动发送*",
			safe(service), map[bool]string{true: "成功", false: "失败"}[success],
			safe(service), safe(env), safe(user), safe(oldVersion), safe(newVersion), status,
			time.Now().Format("2006-01-02 15:04:05"))
	} else {
		// 失败时：明确提到回滚到旧版本
		status := "Failure *部署失败\\-已回滚到旧版本*"
		return fmt.Sprintf("*Deployment %s %s*\n\n"+
			"**服务**: `%s`\n"+
			"**环境**: `%s`\n"+
			"**操作人**: `%s`\n"+
			"**旧版本**: `%s` (回滚成功)\n"+
			"**新版本**: `%s` (部署异常，Pod 状态异常)\n"+
			"**状态**: %s\n"+
			"**时间**: `%s`\n\n"+
			"---\n"+
			"*由 K8s\\-CICD Agent 自动发送*",
			safe(service), map[bool]string{true: "成功", false: "失败"}[success],
			safe(service), safe(env), safe(user), safe(oldVersion), safe(newVersion), status,
			time.Now().Format("2006-01-02 15:04:05"))
	}
}

// SendNotification 发送部署通知
func (bm *BotManager) SendNotification(service, env, user, oldVersion, newVersion string, success bool) error {
	if !bm.Enabled {
		return nil // 无需错误，静默跳过
	}
	startTime := time.Now()

	message := bm.generateMarkdownMessage(service, env, user, oldVersion, newVersion, success)

	_, err := bm.sendMessage(message, "MarkdownV2")
	if err != nil {
		logrus.WithFields(logrus.Fields{
			"time":   time.Now().Format("2006-01-02 15:04:05"),
			"method": "SendNotification",
			"took":   time.Since(startTime),
		}).Errorf(color.RedString("发送通知失败: %v"), err)
		return err
	}

	logrus.WithFields(logrus.Fields{
		"time":   time.Now().Format("2006-01-02 15:04:05"),
		"method": "SendNotification",
		"took":   time.Since(startTime),
	}).Infof(color.GreenString("部署通知发送成功: %s -> %s [%s] (状态: %s)"), oldVersion, newVersion, service, map[bool]string{true: "成功", false: "失败 (已回滚)"}[success])
	return nil
}

// Stop 空实现
func (bm *BotManager) Stop() {
	logrus.Info(color.GreenString("Telegram BotManager 停止"))
}