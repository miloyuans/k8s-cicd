package telegram

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"text/template"
	"time"

	"k8s-cicd/agent/config"
	"github.com/sirupsen/logrus"
)

// TelegramBot 单个Telegram机器人
type TelegramBot struct {
	Name       string
	Token      string
	GroupID    string
	Services   map[string][]string
	RegexMatch bool
	IsEnabled  bool
}

// BotManager 多机器人管理器
type BotManager struct {
	Bots map[string]*TelegramBot
}

// NewBotManager 创建多机器人管理器
func NewBotManager(bots []config.TelegramBot) *BotManager {
	// 步骤1：初始化bots映射
	m := &BotManager{Bots: make(map[string]*TelegramBot)}
	
	for i := range bots {
		if bots[i].IsEnabled {
			bot := &TelegramBot{
				Name:       bots[i].Name,
				Token:      bots[i].Token,
				GroupID:    bots[i].GroupID,
				Services:   bots[i].Services,
				RegexMatch: bots[i].RegexMatch,
				IsEnabled:  true,
			}
			m.Bots[bot.Name] = bot
			logrus.Infof("Telegram机器人 [%s] 已启用", bot.Name)
		}
	}
	
	return m
}

// SendNotification 发送部署通知（多机器人智能选择）
func (bm *BotManager) SendNotification(service, env, user, oldVersion, newVersion string, success bool) error {
	// 步骤1：根据服务名选择合适的机器人
	bot, err := bm.getBotForService(service)
	if err != nil {
		logrus.Warnf("未找到匹配的Telegram机器人: %v", err)
		return err
	}

	// 步骤2：生成Markdown消息
	message := bm.generateMarkdownMessage(service, env, user, oldVersion, newVersion, success)

	// 步骤3：发送HTTP请求
	payload := map[string]interface{}{
		"chat_id":   bot.GroupID,
		"text":      message,
		"parse_mode": "MarkdownV2",
	}

	jsonData, _ := json.Marshal(payload)
	
	resp, err := http.Post(fmt.Sprintf("https://api.telegram.org/bot%s/sendMessage", bot.Token), 
		"application/json", bytes.NewBuffer(jsonData))
	if err != nil {
		return fmt.Errorf("Telegram发送失败: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		logrus.Errorf("Telegram API返回错误: %s", resp.Status)
		return fmt.Errorf("发送失败，状态码: %d", resp.StatusCode)
	}

	logrus.Infof("Telegram通知发送成功: %s -> %s (%s)", service, newVersion, success)
	return nil
}

// getBotForService 根据服务名选择机器人
func (bm *BotManager) getBotForService(service string) (*TelegramBot, error) {
	for _, bot := range bm.Bots {
		for _, serviceList := range bot.Services {
			for _, pattern := range serviceList {
				// 正则匹配
				if bot.RegexMatch {
					matched, err := regexp.MatchString(pattern, service)
					if err == nil && matched {
						return bot, nil
					}
				} else {
					// 前缀匹配
					if strings.HasPrefix(strings.ToUpper(service), strings.ToUpper(pattern)) {
						return bot, nil
					}
				}
			}
		}
	}
	return nil, fmt.Errorf("服务 %s 未匹配任何机器人", service)
}

// generateMarkdownMessage 生成美观的Markdown通知消息
func (bm *BotManager) generateMarkdownMessage(service, env, user, oldVersion, newVersion string, success bool) string {
	// 步骤1：定义消息模板
	tmpl := `
*🚀 {{.Service}} 部署 {{.Status}}*

**服务**: \`{{.Service}}\`
**环境**: \`{{.Environment}}\`
**操作人**: \`{{.User}}\`
**旧版本**: \`{{.OldVersion}}\`
**新版本**: \`{{.NewVersion}}\`
**状态**: {{.StatusEmoji}} *{{.StatusText}}*
**时间**: \`{{.Time}}\`

{{if not .Success}}
*🔄 自动回滚已完成*
{{end}}

---
*由 K8s-CICD Agent 自动发送*
`

	// 步骤2：解析模板
	t, err := template.New("notification").Parse(tmpl)
	if err != nil {
		logrus.Error("模板解析失败: ", err)
		return "部署通知"
	}

	// 步骤3：准备模板数据
	data := struct {
		Service     string
		Environment string
		User        string
		OldVersion  string
		NewVersion  string
		Success     bool
		StatusEmoji string
		StatusText  string
		Time        string
	}{
		Service:     service,
		Environment: env,
		User:        user,
		OldVersion:  oldVersion,
		NewVersion:  newVersion,
		Success:     success,
		Time:        time.Now().Format("2006-01-02 15:04:05"),
	}

	// 步骤4：设置状态信息
	if success {
		data.StatusEmoji = "✅"
		data.StatusText = "部署成功"
	} else {
		data.StatusEmoji = "❌"
		data.StatusText = "部署失败-已回滚"
	}

	// 步骤5：执行模板
	var buf bytes.Buffer
	err = t.Execute(&buf, data)
	if err != nil {
		logrus.Error("模板执行失败: ", err)
		return "部署通知"
	}

	// 步骤6：转义Markdown特殊字符
	message := buf.String()
	message = strings.ReplaceAll(message, "_", "\\_")
	message = strings.ReplaceAll(message, "*", "\\*")
	message = strings.ReplaceAll(message, "[", "\\[")
	message = strings.ReplaceAll(message, "]", "\\]")

	return message
}