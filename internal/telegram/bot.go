// bot.go
package telegram

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"k8s-cicd/agent/config"
	"k8s-cicd/agent/models"

	"github.com/fatih/color"
	"github.com/sirupsen/logrus"
)

// TelegramBot 单个Telegram机器人配置
// 用于存储单个机器人的配置信息，从配置文件加载
type TelegramBot struct {
	Name         string              // 机器人名称
	Token        string              // Bot Token
	GroupID      string              // 群组ID
	Services     map[string][]string // 服务匹配规则: prefix -> 服务列表
	RegexMatch   bool                // 是否使用正则匹配
	IsEnabled    bool                // 是否启用该机器人
	AllowedUsers []string            // 机器人特定的允许用户
}

// BotManager 多机器人管理器
// 管理多个Telegram机器人，实现匹配选择和消息处理
type BotManager struct {
	Bots               map[string]*TelegramBot // 机器人映射
	offset             int64                   // Telegram updates offset
	updateChan         chan map[string]interface{} // 更新通道
	stopChan           chan struct{}           // 停止信号通道
	globalAllowedUsers []string            // 全局允许用户
	confirmationChans  sync.Map            // 存储确认通道: key -> confirmationChans
}

type confirmationChans struct {
	confirmChan chan models.DeployRequest
	rejectChan  chan models.StatusRequest
}

// NewBotManager 创建多机器人管理器
// 从配置中加载机器人列表，并初始化启用的机器人
func NewBotManager(bots []config.TelegramBot) *BotManager {
	startTime := time.Now()
	// 步骤1：初始化管理器结构
	m := &BotManager{
		Bots:       make(map[string]*TelegramBot),
		updateChan: make(chan map[string]interface{}, 100),
		stopChan:   make(chan struct{}),
		globalAllowedUsers: make([]string, 0), // 将在调用时设置
	}

	// 步骤2：遍历配置中的机器人，初始化启用的机器人
	for i := range bots {
		if bots[i].IsEnabled {
			bot := &TelegramBot{
				Name:         bots[i].Name,
				Token:        bots[i].Token,
				GroupID:      bots[i].GroupID,
				Services:     bots[i].Services,
				RegexMatch:   bots[i].RegexMatch,
				IsEnabled:    true,
				AllowedUsers: bots[i].AllowedUsers, // 从配置中获取机器人特定的允许用户
			}
			m.Bots[bot.Name] = bot
			logrus.WithFields(logrus.Fields{
				"time":   time.Now().Format("2006-01-02 15:04:05"),
				"method": "NewBotManager",
				"took":   time.Since(startTime),
			}).Infof(color.GreenString("✅ Telegram机器人 [%s] 已启用，允许用户: %v", bot.Name, bot.AllowedUsers))
		}
	}

	// 步骤3：检查是否有启用的机器人
	if len(m.Bots) == 0 {
		logrus.WithFields(logrus.Fields{
			"time":   time.Now().Format("2006-01-02 15:04:05"),
			"method": "NewBotManager",
			"took":   time.Since(startTime),
		}).Warn("⚠️ 未启用任何Telegram机器人")
	}

	// 步骤4：返回管理器实例
	logrus.WithFields(logrus.Fields{
		"time":   time.Now().Format("2006-01-02 15:04:05"),
		"method": "NewBotManager",
		"took":   time.Since(startTime),
	}).Info(color.GreenString("BotManager创建成功"))

	// 启动更新处理
	go m.processUpdateChan()

	return m
}

// SetGlobalAllowedUsers 设置全局允许用户
// 从配置中设置全局允许的用户ID列表
func (bm *BotManager) SetGlobalAllowedUsers(users []string) {
	bm.globalAllowedUsers = users
	logrus.WithFields(logrus.Fields{
		"time":   time.Now().Format("2006-01-02 15:04:05"),
		"method": "SetGlobalAllowedUsers",
	}).Infof(color.GreenString("全局允许用户设置为: %v", users))
}

// StartPolling 启动Telegram Updates轮询
// 启动后台goroutine进行无限轮询更新
func (bm *BotManager) StartPolling() {
	startTime := time.Now()
	// 步骤1：记录启动日志
	logrus.WithFields(logrus.Fields{
		"time":   time.Now().Format("2006-01-02 15:04:05"),
		"method": "StartPolling",
		"took":   time.Since(startTime),
	}).Info(color.GreenString("🔄 启动Telegram Updates轮询"))
	// 步骤2：启动goroutine进行无限轮询
	go func() {
		for {
			select {
			case <-bm.stopChan:
				logrus.WithFields(logrus.Fields{
					"time":   time.Now().Format("2006-01-02 15:04:05"),
					"method": "StartPolling",
					"took":   time.Since(startTime),
				}).Info(color.GreenString("🛑 Telegram轮询已停止"))
				return
			default:
				bm.pollUpdates()
				time.Sleep(1 * time.Second) // 防止频繁轮询导致冲突
			}
		}
	}()
}

// pollUpdates 轮询Telegram Updates
// 通过HTTP GET请求获取Telegram更新
func (bm *BotManager) pollUpdates() {
	startTime := time.Now()
	// 步骤1：获取默认机器人
	bot := bm.getDefaultBot()
	if bot == nil {
		logrus.WithFields(logrus.Fields{
			"time":   time.Now().Format("2006-01-02 15:04:05"),
			"method": "pollUpdates",
			"took":   time.Since(startTime),
		}).Errorf(color.RedString("❌ 无可用机器人，无法轮询"))
		return
	}

	// 步骤2：构建getUpdates请求URL
	url := fmt.Sprintf("https://api.telegram.org/bot%s/getUpdates?offset=%d&timeout=10",
		bot.Token, bm.offset)

	// 步骤3：发送HTTP GET请求
	resp, err := http.Get(url)
	if err != nil {
		logrus.WithFields(logrus.Fields{
			"time":   time.Now().Format("2006-01-02 15:04:05"),
			"method": "pollUpdates",
			"took":   time.Since(startTime),
		}).Errorf(color.RedString("轮询失败: %v", err))
		return
	}
	defer resp.Body.Close()

	// 步骤4：读取响应
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		logrus.WithFields(logrus.Fields{
			"time":   time.Now().Format("2006-01-02 15:04:05"),
			"method": "pollUpdates",
			"took":   time.Since(startTime),
		}).Errorf(color.RedString("读取响应失败: %v", err))
		return
	}

	// 步骤5：解析JSON
	var result map[string]interface{}
	if err := json.Unmarshal(body, &result); err != nil {
		logrus.WithFields(logrus.Fields{
			"time":   time.Now().Format("2006-01-02 15:04:05"),
			"method": "pollUpdates",
			"took":   time.Since(startTime),
		}).Errorf(color.RedString("JSON解析失败: %v", err))
		return
	}

	// 步骤6：处理更新
	if ok, _ := result["ok"].(bool); !ok {
		logrus.WithFields(logrus.Fields{
			"time":   time.Now().Format("2006-01-02 15:04:05"),
			"method": "pollUpdates",
			"took":   time.Since(startTime),
		}).Errorf(color.RedString("Telegram响应失败: %v", result["description"]))
		return
	}

	updates, _ := result["result"].([]interface{})
	for _, u := range updates {
		update, _ := u.(map[string]interface{})
		bm.updateChan <- update
		updateID, _ := update["update_id"].(float64)
		bm.offset = int64(updateID) + 1
	}
}

// processUpdateChan 处理更新通道
// 处理Telegram更新，包括按钮回调
func (bm *BotManager) processUpdateChan() {
	for update := range bm.updateChan {
		callback, ok := update["callback_query"].(map[string]interface{})
		if !ok {
			continue
		}

		data, _ := callback["data"].(string)
		message, _ := callback["message"].(map[string]interface{})
		chat, _ := message["chat"].(map[string]interface{})
		chatID, _ := chat["id"].(float64)
		messageID, _ := message["message_id"].(float64)

		from, _ := callback["from"].(map[string]interface{})
		userID := fmt.Sprintf("%.0f", from["id"].(float64))

		bot := bm.getBotByToken(callback["token"].(string)) // 假设回调包含token
		if bot == nil {
			continue
		}

		if !bm.isUserAllowed(bot, userID) {
			bm.answerCallbackQuery(bot, callback["id"].(string), "无权限操作", true)
			continue
		}

		chans, ok := bm.confirmationChans.Load(data)
		if !ok {
			bm.answerCallbackQuery(bot, callback["id"].(string), "操作已过期", true)
			continue
		}

		cc := chans.(confirmationChans)
		switch data {
		case "confirm":
			cc.confirmChan <- models.DeployRequest{} // 填充实际数据
			bm.answerCallbackQuery(bot, callback["id"].(string), "已确认", true)
		case "reject":
			cc.rejectChan <- models.StatusRequest{} // 填充实际数据
			bm.answerCallbackQuery(bot, callback["id"].(string), "已拒绝", true)
		}

		bm.editMessageReplyMarkup(bot, int64(chatID), int(messageID), nil)
		bm.confirmationChans.Delete(data)
	}
}

// getDefaultBot 获取默认机器人
func (bm *BotManager) getDefaultBot() *TelegramBot {
	for _, bot := range bm.Bots {
		return bot
	}
	return nil
}

// getBotByToken 根据Token获取机器人
func (bm *BotManager) getBotByToken(token string) *TelegramBot {
	for _, bot := range bm.Bots {
		if bot.Token == token {
			return bot
		}
	}
	return nil
}

// isUserAllowed 检查用户是否允许
func (bm *BotManager) isUserAllowed(bot *TelegramBot, userID string) bool {
	for _, u := range bot.AllowedUsers {
		if u == userID {
			return true
		}
	}
	for _, u := range bm.globalAllowedUsers {
		if u == userID {
			return true
		}
	}
	return false
}

// answerCallbackQuery 响应回调查询
func (bm *BotManager) answerCallbackQuery(bot *TelegramBot, callbackID, text string, showAlert bool) error {
	url := fmt.Sprintf("https://api.telegram.org/bot%s/answerCallbackQuery", bot.Token)
	payload := map[string]interface{}{
		"callback_query_id": callbackID,
		"text":              text,
		"show_alert":        showAlert,
	}
	jsonPayload, _ := json.Marshal(payload)
	http.Post(url, "application/json", bytes.NewBuffer(jsonPayload))
	return nil
}

// editMessageReplyMarkup 编辑消息键盘
func (bm *BotManager) editMessageReplyMarkup(bot *TelegramBot, chatID int64, messageID int, replyMarkup interface{}) error {
	url := fmt.Sprintf("https://api.telegram.org/bot%s/editMessageReplyMarkup", bot.Token)
	payload := map[string]interface{}{
		"chat_id":    chatID,
		"message_id": messageID,
	}
	if replyMarkup != nil {
		payload["reply_markup"] = replyMarkup
	}
	jsonPayload, _ := json.Marshal(payload)
	http.Post(url, "application/json", bytes.NewBuffer(jsonPayload))
	return nil
}

// SendConfirmation 发送确认消息
// 发送带有确认/拒绝按钮的消息
func (bm *BotManager) SendConfirmation(service, env, user, version string) (chan models.DeployRequest, chan models.StatusRequest, error) {
	startTime := time.Now()
	bot, err := bm.getBotForService(service)
	if err != nil {
		logrus.WithFields(logrus.Fields{
			"time":   time.Now().Format("2006-01-02 15:04:05"),
			"method": "SendConfirmation",
			"took":   time.Since(startTime),
		}).Errorf(color.RedString("获取机器人失败: %v", err))
		return nil, nil, err
	}

	key := fmt.Sprintf("confirm-%s-%s-%s", service, env, version)
	confirmChan := make(chan models.DeployRequest, 1)
	rejectChan := make(chan models.StatusRequest, 1)
	bm.confirmationChans.Store(key, confirmationChans{confirmChan, rejectChan})

	keyboard := map[string]interface{}{
		"inline_keyboard": [][]map[string]string{
			{
				{"text": "✅ 确认", "callback_data": "confirm"},
				{"text": "❌ 拒绝", "callback_data": "reject"},
			},
		},
	}

	message := bm.generateConfirmationMessage(service, env, user, version)
	_, err = bm.sendMessage(bot, bot.GroupID, message, keyboard)
	if err != nil {
		logrus.WithFields(logrus.Fields{
			"time":   time.Now().Format("2006-01-02 15:04:05"),
			"method": "SendConfirmation",
			"took":   time.Since(startTime),
		}).Errorf(color.RedString("发送确认消息失败: %v", err))
		bm.confirmationChans.Delete(key)
		return nil, nil, err
	}

	logrus.WithFields(logrus.Fields{
		"time":   time.Now().Format("2006-01-02 15:04:05"),
		"method": "SendConfirmation",
		"took":   time.Since(startTime),
	}).Infof(color.GreenString("确认消息发送成功: %s [%s]", service, env))
	return confirmChan, rejectChan, nil
}

// generateConfirmationMessage 生成确认消息
func (bm *BotManager) generateConfirmationMessage(service, env, user, version string) string {
	var message strings.Builder
	message.WriteString("**部署确认**\n\n")
	message.WriteString(fmt.Sprintf("服务: `%s`\n", escapeMarkdownV2(service)))
	message.WriteString(fmt.Sprintf("环境: `%s`\n", escapeMarkdownV2(env)))
	message.WriteString(fmt.Sprintf("版本: `%s`\n", escapeMarkdownV2(version)))
	message.WriteString(fmt.Sprintf("操作人: `%s`\n\n", escapeMarkdownV2(user)))
	message.WriteString("请确认是否部署?")
	return message.String()
}

// SendNotification 发送部署通知
func (bm *BotManager) SendNotification(service, env, user, oldVersion, newVersion string, success bool) error {
	startTime := time.Now()
	bot, err := bm.getBotForService(service)
	if err != nil {
		logrus.WithFields(logrus.Fields{
			"time":   time.Now().Format("2006-01-02 15:04:05"),
			"method": "SendNotification",
			"took":   time.Since(startTime),
		}).Errorf(color.RedString("获取机器人失败: %v", err))
		return err
	}

	message := bm.generateMarkdownMessage(service, env, user, oldVersion, newVersion, success)
	_, err = bm.sendMessage(bot, bot.GroupID, message, nil)
	if err != nil {
		logrus.WithFields(logrus.Fields{
			"time":   time.Now().Format("2006-01-02 15:04:05"),
			"method": "SendNotification",
			"took":   time.Since(startTime),
		}).Errorf(color.RedString("发送通知失败: %v", err))
		return err
	}

	logrus.WithFields(logrus.Fields{
		"time":   time.Now().Format("2006-01-02 15:04:05"),
		"method": "SendNotification",
		"took":   time.Since(startTime),
	}).Infof(color.GreenString("通知发送成功: %s v%s [%s]", service, newVersion, env))
	return nil
}

// generateMarkdownMessage 生成美观的Markdown部署通知
// 生成格式化的Markdown消息用于通知
func (bm *BotManager) generateMarkdownMessage(service, env, user, oldVersion, newVersion string, success bool) string {
	startTime := time.Now()
	// 步骤1：初始化字符串构建器
	var message strings.Builder

	// 步骤2：构建标题
	message.WriteString("**部署通知**\n\n")

	// 步骤3：添加详细信息
	message.WriteString("**服务**: `")
	message.WriteString(escapeMarkdownV2(service))
	message.WriteString("`\n")

	message.WriteString("**环境**: `")
	message.WriteString(escapeMarkdownV2(env))
	message.WriteString("`\n")

	message.WriteString("**操作人**: `")
	message.WriteString(escapeMarkdownV2(user))
	message.WriteString("`\n")

	message.WriteString("**旧版本**: `")
	message.WriteString(escapeMarkdownV2(oldVersion))
	message.WriteString("`\n")

	message.WriteString("**新版本**: `")
	message.WriteString(escapeMarkdownV2(newVersion))
	message.WriteString("`\n")

	// 步骤4：添加状态
	message.WriteString("**状态**: ")
	if success {
		message.WriteString("✅ *部署成功*")
	} else {
		message.WriteString("❌ *部署失败*")
	}
	message.WriteString("\n")

	// 步骤5：添加时间
	message.WriteString("**时间**: `")
	message.WriteString(escapeMarkdownV2(time.Now().Format("2006-01-02 15:04:05")))
	message.WriteString("`\n\n")

	// 步骤6：如果失败，添加回滚信息
	if !success {
		message.WriteString("*自动回滚已完成*\n\n")
	}

	// 步骤7：添加签名
	message.WriteString("---\n")
	message.WriteString("*由 K8s\\-CICD Agent 自动发送*")

	// 步骤8：返回生成的字符串
	logrus.WithFields(logrus.Fields{
		"time":   time.Now().Format("2006-01-02 15:04:05"),
		"method": "generateMarkdownMessage",
		"took":   time.Since(startTime),
	}).Debugf(color.GreenString("生成Markdown消息成功"))
	return message.String()
}

// escapeMarkdownV2 转义MarkdownV2特殊字符
// 转义Telegram MarkdownV2格式中的特殊字符
func escapeMarkdownV2(text string) string {
	reserved := []string{"_", "*", "[", "]", "(", ")", "~", "`", ">", "#", "+", "-", "=", "|", "{", "}", ".", "!"}
	for _, char := range reserved {
		text = strings.ReplaceAll(text, char, "\\"+char)
	}
	return text
}

// sendMessage 发送消息
// 发送Telegram消息，支持可选键盘
func (bm *BotManager) sendMessage(bot *TelegramBot, chatID, text string, replyMarkup interface{}) (map[string]interface{}, error) {
	url := fmt.Sprintf("https://api.telegram.org/bot%s/sendMessage", bot.Token)
	payload := map[string]interface{}{
		"chat_id":    chatID,
		"text":       text,
		"parse_mode": "MarkdownV2",
	}
	if replyMarkup != nil {
		payload["reply_markup"] = replyMarkup
	}
	jsonPayload, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	resp, err := http.Post(url, "application/json", bytes.NewBuffer(jsonPayload))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	var result map[string]interface{}
	json.Unmarshal(body, &result)
	if ok, _ := result["ok"].(bool); !ok {
		return nil, fmt.Errorf("Telegram API错误: %v", result["description"])
	}
	return result, nil
}

// getBotForService 根据服务名选择机器人
// 根据服务名称匹配机器人配置，支持正则或前缀匹配
func (bm *BotManager) getBotForService(service string) (*TelegramBot, error) {
	startTime := time.Now()
	// 步骤1：遍历所有机器人
	for _, bot := range bm.Bots {
		// 步骤2：遍历服务的匹配规则
		for _, serviceList := range bot.Services {
			// 步骤3：遍历服务列表中的模式
			for _, pattern := range serviceList {
				if bot.RegexMatch {
					// 使用正则匹配
					matched, err := regexp.MatchString(pattern, service)
					if err == nil && matched {
						logrus.WithFields(logrus.Fields{
							"time":   time.Now().Format("2006-01-02 15:04:05"),
							"method": "getBotForService",
							"took":   time.Since(startTime),
						}).Infof(color.GreenString("服务 %s 匹配机器人 %s", service, bot.Name))
						return bot, nil
					}
				} else {
					// 使用前缀匹配（忽略大小写）
					if strings.HasPrefix(strings.ToUpper(service), strings.ToUpper(pattern)) {
						logrus.WithFields(logrus.Fields{
							"time":   time.Now().Format("2006-01-02 15:04:05"),
							"method": "getBotForService",
							"took":   time.Since(startTime),
						}).Infof(color.GreenString("服务 %s 匹配机器人 %s", service, bot.Name))
						return bot, nil
					}
				}
			}
		}
	}
	// 步骤4：未匹配，返回错误
	logrus.WithFields(logrus.Fields{
		"time":   time.Now().Format("2006-01-02 15:04:05"),
		"method": "getBotForService",
		"took":   time.Since(startTime),
	}).Errorf(color.RedString("服务 %s 未匹配任何机器人", service))
	return nil, fmt.Errorf("服务 %s 未匹配任何机器人", service)
}

// Stop 停止Telegram轮询
// 关闭轮询通道并停止更新处理
func (bm *BotManager) Stop() {
	startTime := time.Now()
	// 步骤1：关闭停止通道
	close(bm.stopChan)
	close(bm.updateChan)
	logrus.WithFields(logrus.Fields{
		"time":   time.Now().Format("2006-01-02 15:04:05"),
		"method": "Stop",
		"took":   time.Since(startTime),
	}).Infof(color.GreenString("Telegram轮询停止"))
}