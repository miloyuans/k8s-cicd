// bot.go
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
	"k8s-cicd/agent/models"

	"github.com/fatih/color"
	"github.com/sirupsen/logrus"
)

// TelegramBot 单个Telegram机器人配置
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
type BotManager struct {
	Bots               map[string]*TelegramBot // 机器人映射
	offset             int64                   // Telegram updates offset
	updateChan         chan map[string]interface{} // 更新通道
	stopChan           chan struct{}           // 停止信号通道
	globalAllowedUsers []string                // 全局允许用户
}

// NewBotManager 创建多机器人管理器
func NewBotManager(bots []config.TelegramBot) *BotManager {
	startTime := time.Now()
	// 步骤1：初始化管理器结构
	m := &BotManager{
		Bots:               make(map[string]*TelegramBot),
		updateChan:         make(chan map[string]interface{}, 100),
		stopChan:           make(chan struct{}),
		globalAllowedUsers: make([]string, 0),
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
				AllowedUsers: bots[i].AllowedUsers,
			}
			m.Bots[bot.Name] = bot
			logrus.WithFields(logrus.Fields{
				"time":   time.Now().Format("2006-01-02 15:04:05"),
				"method": "NewBotManager",
				"took":   time.Since(startTime),
			}).Infof(color.GreenString("✅ Telegram机器人 [%s] 已启用", bot.Name))
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
	return m
}

// SetGlobalAllowedUsers 设置全局允许用户
func (bm *BotManager) SetGlobalAllowedUsers(users []string) {
	bm.globalAllowedUsers = users
}

// StartPolling 启动Telegram Updates轮询
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
			}
		}
	}()
}

// pollUpdates 轮询Telegram Updates
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
		}).Errorf(color.RedString("❌ Telegram轮询网络错误: %v", err))
		time.Sleep(5 * time.Second)
		return
	}
	defer resp.Body.Close()

	// 步骤4：解析响应JSON
	var result struct {
		Ok     bool                       `json:"ok"`
		Result []map[string]interface{}   `json:"result"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		logrus.WithFields(logrus.Fields{
			"time":   time.Now().Format("2006-01-02 15:04:05"),
			"method": "pollUpdates",
			"took":   time.Since(startTime),
		}).Errorf(color.RedString("❌ Telegram响应解析失败: %v", err))
		return
	}

	// 步骤5：处理每个update，并更新offset
	for _, update := range result.Result {
		bm.offset = int64(update["update_id"].(float64)) + 1
		bm.updateChan <- update
	}
	logrus.WithFields(logrus.Fields{
		"time":   time.Now().Format("2006-01-02 15:04:05"),
		"method": "pollUpdates",
		"took":   time.Since(startTime),
	}).Infof(color.GreenString("轮询完成，收到 %d 个更新", len(result.Result)))
}

// getDefaultBot 获取默认机器人（第一个启用的）
func (bm *BotManager) getDefaultBot() *TelegramBot {
	startTime := time.Now()
	// 步骤1：遍历机器人映射
	for _, bot := range bm.Bots {
		// 步骤2：返回第一个启用的机器人
		if bot.IsEnabled {
			logrus.WithFields(logrus.Fields{
				"time":   time.Now().Format("2006-01-02 15:04:05"),
				"method": "getDefaultBot",
				"took":   time.Since(startTime),
			}).Debug(color.GreenString("获取默认机器人: %s", bot.Name))
			return bot
		}
	}
	// 步骤3：如果没有，返回nil
	logrus.WithFields(logrus.Fields{
		"time":   time.Now().Format("2006-01-02 15:04:05"),
		"method": "getDefaultBot",
		"took":   time.Since(startTime),
	}).Warn("未找到默认机器人")
	return nil
}

// PollUpdates 阻塞式处理Updates（供Agent调用）
func (bm *BotManager) PollUpdates(confirmChan chan models.DeployRequest, rejectChan chan models.StatusRequest) {
	startTime := time.Now()
	// 步骤1：记录启动日志
	logrus.WithFields(logrus.Fields{
		"time":   time.Now().Format("2006-01-02 15:04:05"),
		"method": "PollUpdates",
		"took":   time.Since(startTime),
	}).Info(color.GreenString("📡 开始处理Telegram回调"))
	for {
		select {
		case update := <-bm.updateChan:
			// 处理更新
			bm.HandleCallback(update, confirmChan, rejectChan)
		case <-bm.stopChan:
			logrus.WithFields(logrus.Fields{
				"time":   time.Now().Format("2006-01-02 15:04:05"),
				"method": "PollUpdates",
				"took":   time.Since(startTime),
			}).Info(color.GreenString("Telegram回调处理停止"))
			return
		}
	}
}

// HandleCallback 处理回调查询
func (bm *BotManager) HandleCallback(update map[string]interface{}, confirmChan chan models.DeployRequest, rejectChan chan models.StatusRequest) {
	startTime := time.Now()
	// 步骤1：检查是否为callback_query
	if _, ok := update["callback_query"]; !ok {
		return
	}

	// 步骤2：提取回调数据
	callback := update["callback_query"].(map[string]interface{})
	from, ok := callback["from"].(map[string]interface{})
	if !ok {
		logrus.WithFields(logrus.Fields{
			"time":   time.Now().Format("2006-01-02 15:04:05"),
			"method": "HandleCallback",
			"took":   time.Since(startTime),
		}).Errorf(color.RedString("无法解析用户信息"))
		return
	}
	userName, ok := from["username"].(string)
	if !ok {
		logrus.WithFields(logrus.Fields{
			"time":   time.Now().Format("2006-01-02 15:04:05"),
			"method": "HandleCallback",
			"took":   time.Since(startTime),
		}).Errorf(color.RedString("无法解析用户名"))
		return
	}
	data, ok := callback["data"].(string)
	if !ok {
		logrus.WithFields(logrus.Fields{
			"time":   time.Now().Format("2006-01-02 15:04:05"),
			"method": "HandleCallback",
			"took":   time.Since(startTime),
		}).Errorf(color.RedString("无法解析回调数据"))
		return
	}

	// 步骤3：记录回调日志
	logrus.WithFields(logrus.Fields{
		"time":   time.Now().Format("2006-01-02 15:04:05"),
		"method": "HandleCallback",
		"took":   time.Since(startTime),
	}).Infof(color.GreenString("收到回调: username=%s, data=%s", userName, data))

	// 步骤4：用户用户名过滤
	allowed := false
	for _, username := range bm.globalAllowedUsers {
		if username == userName {
			allowed = true
			break
		}
	}
	if !allowed {
		logrus.WithFields(logrus.Fields{
			"time":   time.Now().Format("2006-01-02 15:04:05"),
			"method": "HandleCallback",
			"took":   time.Since(startTime),
		}).Warnf("无效用户名: %s", userName)
		return
	}

	// 步骤5：解析回调数据
	parts := strings.Split(data, ":")
	if len(parts) != 5 {
		logrus.WithFields(logrus.Fields{
			"time":   time.Now().Format("2006-01-02 15:04:05"),
			"method": "HandleCallback",
			"took":   time.Since(startTime),
		}).Errorf(color.RedString("回调数据格式错误: %s", data))
		return
	}

	// 步骤6：提取行动和服务信息
	action, service, env, version, user := parts[0], parts[1], parts[2], parts[3], parts[4]

	// 步骤7：删除原弹窗消息
	message := callback["message"].(map[string]interface{})
	messageID := int(message["message_id"].(float64))
	chatID := fmt.Sprintf("%v", message["chat"].(map[string]interface{})["id"])
	err := bm.DeleteMessage(bm.getDefaultBot(), chatID, messageID)
	if err != nil {
		logrus.WithFields(logrus.Fields{
			"time":   time.Now().Format("2006-01-02 15:04:05"),
			"method": "HandleCallback",
			"took":   time.Since(startTime),
		}).Errorf(color.RedString("删除原弹窗消息失败: %v", err))
	}

	// 步骤8：构建反馈消息文本
	resultText := fmt.Sprintf("✅ 用户 @%s %s 部署请求: *%s* v`%s` 在 `%s`",
		userName, action, service, version, env)

	// 步骤9：发送反馈消息
	feedbackMessageID, err := bm.sendMessage(bm.getDefaultBot(), chatID, resultText, nil, "MarkdownV2")
	if err != nil {
		logrus.WithFields(logrus.Fields{
			"time":   time.Now().Format("2006-01-02 15:04:05"),
			"method": "HandleCallback",
			"took":   time.Since(startTime),
		}).Errorf(color.RedString("发送反馈消息失败: %v", err))
	} else {
		// 步骤10：定时删除反馈消息（例如30秒后）
		go func() {
			time.Sleep(30 * time.Second)
			err := bm.DeleteMessage(bm.getDefaultBot(), chatID, feedbackMessageID)
			if err != nil {
				logrus.WithFields(logrus.Fields{
					"time":   time.Now().Format("2006-01-02 15:04:05"),
					"method": "HandleCallback",
					"took":   time.Since(startTime),
				}).Errorf(color.RedString("删除反馈消息失败: %v", err))
			} else {
				logrus.WithFields(logrus.Fields{
					"time":   time.Now().Format("2006-01-02 15:04:05"),
					"method": "HandleCallback",
					"took":   time.Since(startTime),
				}).Infof(color.GreenString("反馈消息自动删除: message_id=%d", feedbackMessageID))
			}
		}()
	}

	// 步骤11：根据行动处理确认或拒绝
	if action == "confirm" {
		task := models.DeployRequest{
			Service:      service,
			Environments: []string{env},
			Version:      version,
			User:         user,
			Status:       "pending",
		}
		confirmChan <- task
	} else if action == "reject" {
		status := models.StatusRequest{
			Service:     service,
			Version:     version,
			Environment: env,
			User:        user,
			Status:      "no_action",
		}
		rejectChan <- status
	}
	logrus.WithFields(logrus.Fields{
		"time":   time.Now().Format("2006-01-02 15:04:05"),
		"method": "HandleCallback",
		"took":   time.Since(startTime),
	}).Infof(color.GreenString("回调处理完成: action=%s, service=%s, env=%s", action, service, env))
}

// SendConfirmation 发送确认弹窗
func (bm *BotManager) SendConfirmation(service, env, user, version string, allowedUsers []string) error {
	startTime := time.Now()
	// 步骤1：根据服务选择机器人
	bot, err := bm.getBotForService(service)
	if err != nil {
		logrus.WithFields(logrus.Fields{
			"time":   time.Now().Format("2006-01-02 15:04:05"),
			"method": "SendConfirmation",
			"took":   time.Since(startTime),
		}).Errorf(color.RedString("选择机器人失败: %v", err))
		return err
	}

	// 步骤2：构建@用户列表
	var mentions strings.Builder
	for _, uid := range allowedUsers {
		mentions.WriteString("@")
		mentions.WriteString(uid)
		mentions.WriteString(" ")
	}

	// 步骤3：构建确认消息文本，包括@用户
	message := fmt.Sprintf("*🛡️ 部署确认*\n\n"+
		"**服务**: `%s`\n"+
		"**环境**: `%s`\n"+
		"**版本**: `%s`\n"+
		"**用户**: `%s`\n\n"+
		"*请选择操作*\n\n"+
		"通知: %s", service, env, version, user, mentions.String())

	// 步骤4：构建内联键盘
	callbackDataConfirm := fmt.Sprintf("confirm:%s:%s:%s:%s", service, env, version, user)
	callbackDataReject := fmt.Sprintf("reject:%s:%s:%s:%s", service, env, version, user)

	keyboard := map[string]interface{}{
		"inline_keyboard": [][]map[string]string{
			{
				{"text": "✅ 确认部署", "callback_data": callbackDataConfirm},
				{"text": "❌ 拒绝部署", "callback_data": callbackDataReject},
			},
		},
	}

	// 步骤5：发送带键盘的消息
	_, err = bm.sendMessage(bot, bot.GroupID, message, keyboard, "MarkdownV2")
	if err != nil {
		logrus.WithFields(logrus.Fields{
			"time":   time.Now().Format("2006-01-02 15:04:05"),
			"method": "SendConfirmation",
			"took":   time.Since(startTime),
		}).Errorf(color.RedString("发送弹窗失败: %v", err))
		return err
	}

	// 步骤6：记录发送成功日志
	logrus.WithFields(logrus.Fields{
		"time":   time.Now().Format("2006-01-02 15:04:05"),
		"method": "SendConfirmation",
		"took":   time.Since(startTime),
	}).Infof(color.GreenString("确认弹窗发送成功: %s v%s [%s]", service, version, env))
	return nil
}

// SendNotification 发送部署通知
func (bm *BotManager) SendNotification(service, env, user, oldVersion, newVersion string, success bool) error {
	startTime := time.Now()
	// 步骤1：根据服务选择机器人
	bot, err := bm.getBotForService(service)
	if err != nil {
		logrus.WithFields(logrus.Fields{
			"time":   time.Now().Format("2006-01-02 15:04:05"),
			"method": "SendNotification",
			"took":   time.Since(startTime),
		}).Errorf(color.RedString("选择机器人失败: %v", err))
		return err
	}

	// 步骤2：生成Markdown消息
	message := bm.generateMarkdownMessage(service, env, user, oldVersion, newVersion, success)

	// 步骤3：发送消息
	_, err = bm.sendMessage(bot, bot.GroupID, message, "MarkdownV2")
	if err != nil {
		logrus.WithFields(logrus.Fields{
			"time":   time.Now().Format("2006-01-02 15:04:05"),
			"method": "SendNotification",
			"took":   time.Since(startTime),
		}).Errorf(color.RedString("发送通知失败: %v", err))
		return err
	}

	// 步骤4：记录发送成功日志
	logrus.WithFields(logrus.Fields{
		"time":   time.Now().Format("2006-01-02 15:04:05"),
		"method": "SendNotification",
		"took":   time.Since(startTime),
	}).Infof(color.GreenString("部署通知发送成功: %s -> %s [%s]", oldVersion, newVersion, service))
	return nil
}

// sendMessage 发送普通消息
func (bm *BotManager) sendMessage(bot *TelegramBot, chatID, text, parseMode string) (int, error) {
	startTime := time.Now()
	// 步骤1：构建payload
	payload := map[string]interface{}{
		"chat_id":    chatID,
		"text":       text,
		"parse_mode": parseMode,
	}

	// 步骤2：序列化JSON
	jsonData, err := json.Marshal(payload)
	if err != nil {
		logrus.WithFields(logrus.Fields{
			"time":   time.Now().Format("2006-01-02 15:04:05"),
			"method": "sendMessage",
			"took":   time.Since(startTime),
		}).Errorf(color.RedString("JSON序列化失败: %v", err))
		return 0, err
	}

	// 步骤3：发送POST请求
	resp, err := http.Post(fmt.Sprintf("https://api.telegram.org/bot%s/sendMessage", bot.Token),
		"application/json", bytes.NewBuffer(jsonData))
	if err != nil {
		logrus.WithFields(logrus.Fields{
			"time":   time.Now().Format("2006-01-02 15:04:05"),
			"method": "sendMessage",
			"took":   time.Since(startTime),
		}).Errorf(color.RedString("发送消息失败: %v", err))
		return 0, err
	}
	defer resp.Body.Close()

	// 步骤4：解析响应
	var result struct {
		Ok          bool                   `json:"ok"`
		Result      map[string]interface{} `json:"result"`
		ErrorCode   int                    `json:"error_code"`
		Description string                 `json:"description"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		logrus.WithFields(logrus.Fields{
			"time":   time.Now().Format("2006-01-02 15:04:05"),
			"method": "sendMessage",
			"took":   time.Since(startTime),
		}).Errorf(color.RedString("解析响应失败: %v", err))
		return 0, err
	}

	// 步骤5：检查响应是否成功
	if !result.Ok {
		logrus.WithFields(logrus.Fields{
			"time":   time.Now().Format("2006-01-02 15:04:05"),
			"method": "sendMessage",
			"took":   time.Since(startTime),
			"data": logrus.Fields{
				"error_code": result.ErrorCode,
				"description": result.Description,
			},
		}).Errorf(color.RedString("Telegram API错误"))
		return 0, fmt.Errorf("Telegram API错误: code=%d, description=%s", result.ErrorCode, result.Description)
	}

	// 步骤6：提取并返回message ID
	messageID := int(result.Result["message_id"].(float64))
	logrus.WithFields(logrus.Fields{
		"time":   time.Now().Format("2006-01-02 15:04:05"),
		"method": "sendMessage",
		"took":   time.Since(startTime),
	}).Infof(color.GreenString("消息发送成功，message_id=%d", messageID))
	return messageID, nil
}

// sendMessageWithKeyboard 发送带键盘的消息
func (bm *BotManager) sendMessageWithKeyboard(bot *TelegramBot, chatID, text string, keyboard map[string]interface{}, parseMode string) (int, error) {
	startTime := time.Now()
	// 步骤1：构建payload
	payload := map[string]interface{}{
		"chat_id":      chatID,
		"text":         text,
		"reply_markup": keyboard,
		"parse_mode":   parseMode,
	}

	// 步骤2：序列化JSON
	jsonData, err := json.Marshal(payload)
	if err != nil {
		logrus.WithFields(logrus.Fields{
			"time":   time.Now().Format("2006-01-02 15:04:05"),
			"method": "sendMessageWithKeyboard",
			"took":   time.Since(startTime),
		}).Errorf(color.RedString("JSON序列化失败: %v", err))
		return 0, err
	}

	// 步骤3：发送POST请求
	resp, err := http.Post(fmt.Sprintf("https://api.telegram.org/bot%s/sendMessage", bot.Token),
		"application/json", bytes.NewBuffer(jsonData))
	if err != nil {
		logrus.WithFields(logrus.Fields{
			"time":   time.Now().Format("2006-01-02 15:04:05"),
			"method": "sendMessageWithKeyboard",
			"took":   time.Since(startTime),
		}).Errorf(color.RedString("发送消息失败: %v", err))
		return 0, err
	}
	defer resp.Body.Close()

	// 步骤4：解析响应
	var result struct {
		Ok          bool                   `json:"ok"`
		Result      map[string]interface{} `json:"result"`
		ErrorCode   int                    `json:"error_code"`
		Description string                 `json:"description"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		logrus.WithFields(logrus.Fields{
			"time":   time.Now().Format("2006-01-02 15:04:05"),
			"method": "sendMessageWithKeyboard",
			"took":   time.Since(startTime),
		}).Errorf(color.RedString("解析响应失败: %v", err))
		return 0, err
	}

	// 步骤5：检查响应是否成功
	if !result.Ok {
		logrus.WithFields(logrus.Fields{
			"time":   time.Now().Format("2006-01-02 15:04:05"),
			"method": "sendMessageWithKeyboard",
			"took":   time.Since(startTime),
			"data": logrus.Fields{
				"error_code": result.ErrorCode,
				"description": result.Description,
			},
		}).Errorf(color.RedString("Telegram API错误"))
		return 0, fmt.Errorf("Telegram API错误: code=%d, description=%s", result.ErrorCode, result.Description)
	}

	// 步骤6：提取并返回message ID
	messageID := int(result.Result["message_id"].(float64))
	logrus.WithFields(logrus.Fields{
		"time":   time.Now().Format("2006-01-02 15:04:05"),
		"method": "sendMessageWithKeyboard",
		"took":   time.Since(startTime),
	}).Infof(color.GreenString("带键盘消息发送成功，message_id=%d", messageID))
	return messageID, nil
}

// SendSimpleMessage 发送简单反馈消息
func (bm *BotManager) SendSimpleMessage(bot *TelegramBot, chatID, text, parseMode string) (int, error) {
	// 步骤1：调用sendMessage发送消息
	return bm.sendMessage(bot, chatID, text, nil, parseMode)
}

// DeleteMessage 删除指定消息
func (bm *BotManager) DeleteMessage(bot *TelegramBot, chatID string, messageID int) error {
	startTime := time.Now()
	// 步骤1：构建payload
	payload := map[string]interface{}{
		"chat_id":    chatID,
		"message_id": messageID,
	}

	// 步骤2：序列化JSON
	jsonData, err := json.Marshal(payload)
	if err != nil {
		logrus.WithFields(logrus.Fields{
			"time":   time.Now().Format("2006-01-02 15:04:05"),
			"method": "DeleteMessage",
			"took":   time.Since(startTime),
		}).Errorf(color.RedString("JSON序列化失败: %v", err))
		return err
	}

	// 步骤3：发送POST请求
	resp, err := http.Post(fmt.Sprintf("https://api.telegram.org/bot%s/deleteMessage", bot.Token),
		"application/json", bytes.NewBuffer(jsonData))
	if err != nil {
		logrus.WithFields(logrus.Fields{
			"time":   time.Now().Format("2006-01-02 15:04:05"),
			"method": "DeleteMessage",
			"took":   time.Since(startTime),
		}).Errorf(color.RedString("删除消息失败: %v", err))
		return err
	}
	defer resp.Body.Close()

	// 步骤4：解析响应
	var result struct {
		Ok          bool   `json:"ok"`
		ErrorCode   int    `json:"error_code"`
		Description string `json:"description"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		logrus.WithFields(logrus.Fields{
			"time":   time.Now().Format("2006-01-02 15:04:05"),
			"method": "DeleteMessage",
			"took":   time.Since(startTime),
		}).Errorf(color.RedString("解析响应失败: %v", err))
		return err
	}

	// 步骤5：检查响应是否成功
	if !result.Ok {
		logrus.WithFields(logrus.Fields{
			"time":   time.Now().Format("2006-01-02 15:04:05"),
			"method": "DeleteMessage",
			"took":   time.Since(startTime),
			"data": logrus.Fields{
				"error_code": result.ErrorCode,
				"description": result.Description,
			},
		}).Errorf(color.RedString("Telegram API错误"))
		return fmt.Errorf("Telegram API错误: code=%d, description=%s", result.ErrorCode, result.Description)
	}

	logrus.WithFields(logrus.Fields{
		"time":   time.Now().Format("2006-01-02 15:04:05"),
		"method": "DeleteMessage",
		"took":   time.Since(startTime),
	}).Infof(color.GreenString("消息删除成功，message_id=%d", messageID))
	return nil
}

// getBotForService 根据服务名选择机器人
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
func (bm *BotManager) Stop() {
	startTime := time.Now()
	// 步骤1：关闭停止通道
	close(bm.stopChan)
	logrus.WithFields(logrus.Fields{
		"time":   time.Now().Format("2006-01-02 15:04:05"),
		"method": "Stop",
		"took":   time.Since(startTime),
	}).Infof(color.GreenString("Telegram轮询停止"))
}

// generateMarkdownMessage 生成美观的Markdown部署通知
func (bm *BotManager) generateMarkdownMessage(service, env, user, oldVersion, newVersion string, success bool) string {
	startTime := time.Now()
	// 步骤1：初始化字符串构建器
	var message strings.Builder

	// 步骤2：构建标题
	message.WriteString("*🚀 ")
	message.WriteString(service)
	message.WriteString(" 部署 ")
	if success {
		message.WriteString("成功*")
	} else {
		message.WriteString("失败*")
	}
	message.WriteString("\n\n")

	// 步骤3：添加详细信息
	message.WriteString("**服务**: `")
	message.WriteString(service)
	message.WriteString("`\n")

	message.WriteString("**环境**: `")
	message.WriteString(env)
	message.WriteString("`\n")

	message.WriteString("**操作人**: `")
	message.WriteString(user)
	message.WriteString("`\n")

	message.WriteString("**旧版本**: `")
	message.WriteString(oldVersion)
	message.WriteString("`\n")

	message.WriteString("**新版本**: `")
	message.WriteString(newVersion)
	message.WriteString("`\n")

	// 步骤4：添加状态
	message.WriteString("**状态**: ")
	if success {
		message.WriteString("✅ *部署成功*")
	} else {
		message.WriteString("❌ *部署失败-已回滚*")
	}
	message.WriteString("\n")

	// 步骤5：添加时间
	message.WriteString("**时间**: `")
	message.WriteString(time.Now().Format("2006-01-02 15:04:05"))
	message.WriteString("`\n\n")

	// 步骤6：如果失败，添加回滚信息
	if !success {
		message.WriteString("*🔄 自动回滚已完成*\n\n")
	}

	// 步骤7：添加签名
	message.WriteString("---\n")
	message.WriteString("*由 K8s-CICD Agent 自动发送*")

	// 步骤8：返回生成的字符串
	logrus.WithFields(logrus.Fields{
		"time":   time.Now().Format("2006-01-02 15:04:05"),
		"method": "generateMarkdownMessage",
		"took":   time.Since(startTime),
	}).Debugf(color.GreenString("生成Markdown消息成功"))
	return message.String()
}