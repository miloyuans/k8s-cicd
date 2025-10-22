package telegram

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strings"
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

	green.Printf("✅ Telegram通知发送成功: %s\n", service)

	return nil
}

// getBotForService 根据服务名选择机器人
func (bm *BotManager) getBotForService(service string) (*TelegramBot, error) {
	for _, bot := range bm.Bots {
		for _, serviceList := range bot.Services {
			for _, pattern := range serviceList {
				if bot.RegexMatch {
					matched, err := regexp.MatchString(pattern, service)
					if err == nil && matched {
						return bot, nil
					}
				} else {
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
	escapeChars := []string{"_", "*", "[", "]", "(", ")", "~", ">", "#", "+", "-", "=", "|", "{", "}", ".", "!"}
	
	for _, char := range escapeChars {
		text = strings.ReplaceAll(text, char, "\\"+char)
	}
	return text
}

// generateMarkdownMessage 生成美观的Markdown通知消息（完全重写，无模板）
func (bm *BotManager) generateMarkdownMessage(service, env, user, oldVersion, newVersion string, success bool) string {
	// 步骤1：构建基础消息（完全使用字符串拼接，避免模板语法）
	var message strings.Builder
	
	// 标题
	message.WriteString("*")
	message.WriteString("🚀 ")
	message.WriteString(service)
	message.WriteString(" 部署 ")
	if success {
		message.WriteString("成功")
	} else {
		message.WriteString("失败")
	}
	message.WriteString("*")
	message.WriteString("\n\n")
	
	// 服务信息
	message.WriteString("**服务**: `")
	message.WriteString(service)
	message.WriteString("`")
	message.WriteString("\n")
	
	// 环境信息
	message.WriteString("**环境**: `")
	message.WriteString(env)
	message.WriteString("`")
	message.WriteString("\n")
	
	// 操作人
	message.WriteString("**操作人**: `")
	message.WriteString(user)
	message.WriteString("`")
	message.WriteString("\n")
	
	// 旧版本
	message.WriteString("**旧版本**: `")
	message.WriteString(oldVersion)
	message.WriteString("`")
	message.WriteString("\n")
	
	// 新版本
	message.WriteString("**新版本**: `")
	message.WriteString(newVersion)
	message.WriteString("`")
	message.WriteString("\n")
	
	// 状态
	message.WriteString("**状态**: ")
	if success {
		message.WriteString("✅ *部署成功*")
	} else {
		message.WriteString("❌ *部署失败-已回滚*")
	}
	message.WriteString("\n")
	
	// 时间
	message.WriteString("**时间**: `")
	message.WriteString(time.Now().Format("2006-01-02 15:04:05"))
	message.WriteString("`")
	message.WriteString("\n\n")
	
	// 回滚信息
	if !success {
		message.WriteString("*🔄 自动回滚已完成*")
		message.WriteString("\n\n")
	}
	
	// 分隔线和签名
	message.WriteString("---")
	message.WriteString("\n")
	message.WriteString("*由 K8s-CICD Agent 自动发送*")

	// 步骤2：转义非代码块的特殊字符
	lines := strings.Split(message.String(), "\n")
	for i, line := range lines {
		// 如果行包含代码块标记 `，跳过转义
		if strings.Contains(line, "`") {
			lines[i] = line
		} else {
			lines[i] = bm.escapeMarkdownV2(line)
		}
	}
	
	return strings.Join(lines, "\n")
}