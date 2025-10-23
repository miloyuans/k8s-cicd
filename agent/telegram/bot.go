package telegram

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"k8s-cicd/agent/config"
	"k8s-cicd/agent/models"

	"github.com/fatih/color"
	"github.com/sirupsen/logrus"
)

// TelegramBot 单个Telegram机器人配置
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
	Bots        map[string]*TelegramBot
	offset      int64 // Telegram updates offset
	updateChan  chan map[string]interface{}
	stopChan    chan struct{}
}

// NewBotManager 创建多机器人管理器
func NewBotManager(bots []config.TelegramBot) *BotManager {
	m := &BotManager{
		Bots:       make(map[string]*TelegramBot),
		updateChan: make(chan map[string]interface{}, 100),
		stopChan:   make(chan struct{}),
	}

	// 步骤1：初始化启用的机器人
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
			logrus.Infof("✅ Telegram机器人 [%s] 已启用", bot.Name)
		}
	}

	if len(m.Bots) == 0 {
		logrus.Warn("⚠️ 未启用任何Telegram机器人")
	}

	return m
}

// StartPolling 启动Telegram Updates轮询
func (bm *BotManager) StartPolling() {
	logrus.Info("🔄 启动Telegram Updates轮询")
	go func() {
		for {
			select {
			case <-bm.stopChan:
				logrus.Info("🛑 Telegram轮询已停止")
				return
			default:
				bm.pollUpdates()
			}
		}
	}()
}

// pollUpdates 轮询Telegram Updates
func (bm *BotManager) pollUpdates() {
	// 步骤1：构建getUpdates请求
	url := fmt.Sprintf("https://api.telegram.org/bot%s/getUpdates?offset=%d&timeout=10", 
		bm.getDefaultBot().Token, bm.offset)

	resp, err := http.Get(url)
	if err != nil {
		logrus.Errorf("❌ Telegram轮询网络错误: %v", err)
		time.Sleep(5 * time.Second)
		return
	}
	defer resp.Body.Close()

	// 步骤2：解析响应
	var result struct {
		Ok     bool                  `json:"ok"`
		Result []map[string]interface{} `json:"result"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		logrus.Errorf("❌ Telegram响应解析失败: %v", err)
		return
	}

	// 步骤3：处理每个update
	for _, update := range result.Result {
		bm.offset = int64(update["update_id"].(float64)) + 1
		bm.updateChan <- update
	}
}

// getDefaultBot 获取默认机器人（第一个启用的）
func (bm *BotManager) getDefaultBot() *TelegramBot {
	for _, bot := range bm.Bots {
		if bot.IsEnabled {
			return bot
		}
	}
	return nil
}

// PollUpdates 阻塞式处理Updates（供Agent调用）
func (bm *BotManager) PollUpdates(allowedUsers []int64, confirmChan chan models.DeployRequest, rejectChan chan models.StatusRequest) {
	logrus.Info("📡 开始处理Telegram回调")
	for {
		select {
		case update := <-bm.updateChan:
			bm.HandleCallback(update, allowedUsers, confirmChan, rejectChan)
		case <-bm.stopChan:
			return
		}
	}
}

// HandleCallback 处理回调查询
func (bm *BotManager) HandleCallback(update map[string]interface{}, allowedUsers []int64, confirmChan chan models.DeployRequest, rejectChan chan models.StatusRequest) {
	// 步骤1：检查是否为callback_query
	if _, ok := update["callback_query"]; !ok {
		return
	}

	callback := update["callback_query"].(map[string]interface{})
	userIDFloat := callback["from"].(map[string]interface{})["id"].(float64)
	userID := int64(userIDFloat)
	data := callback["data"].(string)

	logrus.Infof("🔘 收到回调: user=%d, data=%s", userID, data)

	// 步骤2：用户ID过滤
	allowed := false
	for _, uid := range allowedUsers {
		if uid == userID {
			allowed = true
			break
		}
	}
	if !allowed {
		logrus.Warnf("⚠️ 无效用户ID: %d", userID)
		return
	}

	// 步骤3：解析回调数据
	parts := strings.Split(data, ":")
	if len(parts) != 5 {
		logrus.Errorf("❌ 回调数据格式错误: %s", data)
		return
	}

	action, service, env, version, user := parts[0], parts[1], parts[2], parts[3], parts[4]

	// 步骤4：删除原弹窗
	message := callback["message"].(map[string]interface{})
	messageID := int(message["message_id"].(float64))
	chatID := fmt.Sprintf("%v", message["chat"].(map[string]interface{})["id"])

	bm.DeleteMessage(bm.getDefaultBot(), chatID, messageID)

	// 步骤5：发送反馈消息
	resultText := fmt.Sprintf("✅ 用户 @%d %s 部署请求: *%s* v`%s` 在 `%s`", 
		userID, action, service, version, env)
	bm.SendSimpleMessage(bm.getDefaultBot(), chatID, resultText, "Markdown")

	// 步骤6：处理确认/拒绝
	if action == "confirm" {
		task := models.DeployRequest{
			Service:      service,
			Environments: []string{env},
			Version:      version,
			User:         user,
			Status:       "pending",
		}
		confirmChan <- task
		logrus.Infof("✅ 任务确认: %s v%s [%s]", service, version, env)
	} else if action == "reject" {
		statusReq := models.StatusRequest{
			Service:     service,
			Environment: env,
			Version:     version,
			User:        user,
			Status:      "rejected",
		}
		rejectChan <- statusReq
		logrus.Infof("❌ 任务拒绝: %s v%s [%s]", service, version, env)
	}
}

// SendConfirmation 发送确认弹窗（支持多机器人）
func (bm *BotManager) SendConfirmation(service, env, user, version string, allowedUsers []int64) error {
	// 步骤1：选择匹配的机器人
	bot, err := bm.getBotForService(service)
	if err != nil {
		return fmt.Errorf("未找到匹配机器人: %s", service)
	}

	// 步骤2：生成@提醒
	var mentions strings.Builder
	for _, uid := range allowedUsers {
		mentions.WriteString(fmt.Sprintf("@%d ", uid))
	}

	// 步骤3：构建消息和键盘
	messageText := fmt.Sprintf("%s请确认部署:\n**服务**: `%s`\n**环境**: `%s`\n**版本**: `%s`\n**操作人**: `%s`", 
		mentions.String(), service, env, version, user)

	keyboard := map[string]interface{}{
		"inline_keyboard": [][]map[string]string{
			{
				{"text": "✅ 确认部署", "callback_data": fmt.Sprintf("confirm:%s:%s:%s:%s", service, env, version, user)},
				{"text": "❌ 拒绝部署", "callback_data": fmt.Sprintf("reject:%s:%s:%s:%s", service, env, version, user)},
			},
		},
	}

	// 步骤4：发送消息
	messageID, err := bm.sendMessageWithKeyboard(bot, bot.GroupID, messageText, keyboard, "Markdown")
	if err != nil {
		return err
	}

	// 步骤5：24小时后自动删除
	go func() {
		time.Sleep(24 * time.Hour)
		bm.DeleteMessage(bot, bot.GroupID, messageID)
		logrus.Infof("🗑️ 自动删除确认弹窗: %s", messageID)
	}()

	green := color.New(color.FgGreen)
	green.Printf("✅ 确认弹窗发送成功: %s v%s [%s]\n", service, version, env)
	return nil
}

// SendNotification 发送部署结果通知（支持多机器人）
func (bm *BotManager) SendNotification(service, env, user, oldVersion, newVersion string, success bool) error {
	// 步骤1：选择机器人
	bot, err := bm.getBotForService(service)
	if err != nil {
		return err
	}

	// 步骤2：生成美观的消息
	message := bm.generateMarkdownMessage(service, env, user, oldVersion, newVersion, success)

	// 步骤3：发送通知
	_, err = bm.sendMessage(bot, bot.GroupID, message, "MarkdownV2")
	if err != nil {
		return err
	}

	green := color.New(color.FgGreen)
	green.Printf("✅ 部署通知发送成功: %s -> %s [%s]\n", oldVersion, newVersion, service)
	return nil
}

// sendMessage 发送普通消息
func (bm *BotManager) sendMessage(bot *TelegramBot, chatID, text, parseMode string) (int, error) {
	payload := map[string]interface{}{
		"chat_id":   chatID,
		"text":      text,
		"parse_mode": parseMode,
	}

	jsonData, _ := json.Marshal(payload)
	resp, err := http.Post(fmt.Sprintf("https://api.telegram.org/bot%s/sendMessage", bot.Token),
		"application/json", bytes.NewBuffer(jsonData))
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()

	var result struct {
		Ok     bool                  `json:"ok"`
		Result map[string]interface{} `json:"result"`
	}
	json.NewDecoder(resp.Body).Decode(&result)

	if !result.Ok {
		return 0, fmt.Errorf("Telegram API错误")
	}

	messageID := int(result.Result["message_id"].(float64))
	return messageID, nil
}

// sendMessageWithKeyboard 发送带键盘的消息
func (bm *BotManager) sendMessageWithKeyboard(bot *TelegramBot, chatID, text string, keyboard, parseMode map[string]interface{}) (int, error) {
	payload := map[string]interface{}{
		"chat_id":      chatID,
		"text":         text,
		"reply_markup": keyboard,
		"parse_mode":   parseMode,
	}

	jsonData, _ := json.Marshal(payload)
	resp, err := http.Post(fmt.Sprintf("https://api.telegram.org/bot%s/sendMessage", bot.Token),
		"application/json", bytes.NewBuffer(jsonData))
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()

	var result struct {
		Ok     bool                  `json:"ok"`
		Result map[string]interface{} `json:"result"`
	}
	json.NewDecoder(resp.Body).Decode(&result)

	if !result.Ok {
		return 0, fmt.Errorf("Telegram API错误")
	}

	messageID := int(result.Result["message_id"].(float64))
	return messageID, nil
}

// SendSimpleMessage 发送简单反馈消息
func (bm *BotManager) SendSimpleMessage(bot *TelegramBot, chatID, text, parseMode string) error {
	_, err := bm.sendMessage(bot, chatID, text, parseMode)
	return err
}

// DeleteMessage 删除指定消息
func (bm *BotManager) DeleteMessage(bot *TelegramBot, chatID string, messageID int) error {
	payload := map[string]interface{}{
		"chat_id":    chatID,
		"message_id": messageID,
	}
	jsonData, _ := json.Marshal(payload)

	resp, _ := http.Post(fmt.Sprintf("https://api.telegram.org/bot%s/deleteMessage", bot.Token),
		"application/json", bytes.NewBuffer(jsonData))
	return nil
}

// getBotForService 根据服务名选择机器人
func (bm *BotManager) getBotForService(service string) (*TelegramBot, error) {
	for _, bot := range bm.Bots {
		for prefix, serviceList := range bot.Services {
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

// Stop 停止Telegram轮询
func (bm *BotManager) Stop() {
	close(bm.stopChan)
}

// generateMarkdownMessage 生成美观的Markdown部署通知
func (bm *BotManager) generateMarkdownMessage(service, env, user, oldVersion, newVersion string, success bool) string {
	var message strings.Builder

	// 标题
	message.WriteString("*🚀 ")
	message.WriteString(service)
	message.WriteString(" 部署 ")
	if success {
		message.WriteString("成功*")
	} else {
		message.WriteString("失败*")
	}
	message.WriteString("\n\n")

	// 详细信息
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
	message.WriteString("`\n\n")

	// 回滚信息
	if !success {
		message.WriteString("*🔄 自动回滚已完成*\n\n")
	}

	// 签名
	message.WriteString("---\n")
	message.WriteString("*由 K8s-CICD Agent 自动发送*")

	return message.String()
}