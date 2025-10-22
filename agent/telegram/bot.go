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

	"github.com/fatih/color"
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

// SendNotification 发送部署通知
func (bm *BotManager) SendNotification(service, env, user, oldVersion, newVersion string, success bool) error {
	// 步骤1：选择机器人
	bot, err := bm.getBotForService(service)
	if err != nil {
		red := color.New(color.FgRed)
		red.Printf("未找到匹配机器人: %s\n", service)
		return err
	}

	green := color.New(color.FgGreen)
	green.Printf("使用机器人 [%s] 发送通知\n", bot.Name)

	// 步骤2：生成消息
	message := bm.generateMarkdownMessage(service, env, user, oldVersion, newVersion, success)

	// 步骤3：发送请求
	payload := map[string]interface{}{
		"chat_id":   bot.GroupID,
		"text":      message,
		"parse_mode": "MarkdownV2",
	}

	jsonData, _ := json.Marshal(payload)
	
	resp, err := http.Post(fmt.Sprintf("https://api.telegram.org/bot%s/sendMessage", bot.Token), 
		"application/json", bytes.NewBuffer(jsonData))
	if err != nil {
		red := color.New(color.FgRed)
		red.Printf("Telegram发送失败: %v\n", err)
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		red := color.New(color.FgRed)
		red.Printf("Telegram API错误: %d\n", resp.StatusCode)
		return fmt.Errorf("发送失败，状态码: %d", resp.StatusCode)
	}

	green := color.New(color.FgGreen)
	green.Printf("✅ Telegram通知发送成功: %s\n", service)

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

// escapeMarkdownV2 转义MarkdownV2特殊字符
func (bm *BotManager) escapeMarkdownV2(text string) string {
	escapeChars := []string{
		"_", "*", "[", "]", "(", ")", "~", "`", ">", "#", "+", "-", "=", "|", "{", "}", ".", "!",
	}
	
	for _, char := range escapeChars {
		text = strings.ReplaceAll(text, char, "\\"+char)
	}
	return text
}

// generateMarkdownMessage 生成美观的Markdown通知消息
func (bm *BotManager) generateMarkdownMessage(service, env, user, oldVersion, newVersion string, success bool) string {
	// 步骤1：定义消息模板（修复：使用raw string literal，避免转义问题）
	tmpl := `*🚀 ` + service + ` 部署 {{.Status}}*\n\n` +
		`**服务**: \`{{.Service}}\`\n` +
		`**环境**: \`{{.Environment}}\`\n` +
		`**操作人**: \`{{.User}}\`\n` +
		`**旧版本**: \`{{.OldVersion}}\`\n` +
		`**新版本**: \`{{.NewVersion}}\`\n` +
		`**状态**: {{.StatusEmoji}} *{{.StatusText}}*\n` +
		`**时间**: \`{{.Time}}\`\n\n`

	if !success {
		tmpl += `*🔄 自动回滚已完成*\n\n`
	}

	tmpl += `---\n*由 K8s-CICD Agent 自动发送*`

	// 步骤2：解析模板
	t, err := template.New("notification").Parse(tmpl)
	if err != nil {
		logrus.Error("模板解析失败: ", err)
		return "部署通知"
	}

	// 步骤3：准备模板数据
	data := struct {
		Service      string
		Environment  string
		User         string
		OldVersion   string
		NewVersion   string
		Success      bool
		StatusEmoji  string
		StatusText   string
		Time         string
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

	// 步骤6：转义Markdown特殊字符（仅转义非代码块内容）
	message := buf.String()
	
	// 分离代码块和普通文本
	lines := strings.Split(message, "\n")
	for i, line := range lines {
		// 如果是代码块行（包含反引号），不转义
		if strings.Contains(line, "`") {
			lines[i] = line
		} else {
			lines[i] = bm.escapeMarkdownV2(line)
		}
	}
	
	return strings.Join(lines, "\n")
}