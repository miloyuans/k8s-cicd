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
	globalAllowedUsers []string            // 全局允许用户
	confirmationChans  sync.Map            // 存储确认通道: key -> confirmationChans
}

type confirmationChans struct {
	confirmChan chan models.DeployRequest
	rejectChan  chan models.StatusRequest
}

// NewBotManager 创建多机器人管理器
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
func (bm *BotManager) SetGlobalAllowedUsers(users []string) {
	bm.globalAllowedUsers = users
	logrus.WithFields(logrus.Fields{
		"time":   time.Now().Format("2006-01-02 15:04:05"),
		"method": "SetGlobalAllowedUsers",
	}).Infof(color.GreenString("全局允许用户设置为: %v", users))
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
				time.Sleep(1 * time.Second) // 防止频繁轮询导致冲突
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
		}).Errorf(color.RedString("Telegram API错误: %v", result["description"]))
		return
	}

	updates, _ := result["result"].([]interface{})
	for _, u := range updates {
		update, _ := u.(map[string]interface{})
		bm.updateChan <- update
		if updateID, _ := update["update_id"].(float64); updateID >= float64(bm.offset) {
			bm.offset = int64(updateID) + 1
		}
	}
}

// processUpdateChan 处理更新通道
func (bm *BotManager) processUpdateChan() {
	for update := range bm.updateChan {
		if callback, ok := update["callback_query"].(map[string]interface{}); ok {
			bot := bm.getDefaultBot() // 假设默认机器人，或根据context
			data, _ := callback["data"].(string)
			message, _ := callback["message"].(map[string]interface{})
			chat, _ := message["chat"].(map[string]interface{})
			chatID, _ := chat["id"].(float64)
			messageID, _ := message["message_id"].(float64)
			if strings.HasPrefix(data, "confirm:") {
				key := data[8:]
				if val, ok := bm.confirmationChans.LoadAndDelete(key); ok {
					chans := val.(confirmationChans)
					parts := strings.Split(key, ":")
					if len(parts) == 4 {
						service, env, version, user := parts[0], parts[1], parts[2], parts[3]
						chans.confirmChan <- models.DeployRequest{
							Service:      service,
							Environments: []string{env},
							Version:      version,
							User:         user,
						}
						close(chans.confirmChan)
						close(chans.rejectChan)
						bm.DeleteMessage(bot, fmt.Sprintf("%d", int64(chatID)), int64(messageID))
						time.Sleep(30 * time.Minute)
						bm.DeleteMessage(bot, fmt.Sprintf("%d", int64(chatID)), int64(messageID))
					}
				}
			} else if strings.HasPrefix(data, "reject:") {
				key := data[7:]
				if val, ok := bm.confirmationChans.LoadAndDelete(key); ok {
					chans := val.(confirmationChans)
					parts := strings.Split(key, ":")
					if len(parts) == 4 {
						service, env, version, user := parts[0], parts[1], parts[2], parts[3]
						chans.rejectChan <- models.StatusRequest{
							Service:     service,
							Environment: env,
							Version:     version,
							User:        user,
						}
						close(chans.confirmChan)
						close(chans.rejectChan)
						bm.DeleteMessage(bot, fmt.Sprintf("%d", int64(chatID)), int64(messageID))
						time.Sleep(30 * time.Minute)
						bm.DeleteMessage(bot, fmt.Sprintf("%d", int64(chatID)), int64(messageID))
					}
				}
			}
			// 应答回调查询
			queryID, _ := callback["id"].(string)
			bm.answerCallbackQuery(bot, queryID)
		}
	}
}

// answerCallbackQuery 应答回调查询
func (bm *BotManager) answerCallbackQuery(bot *TelegramBot, queryID string) {
	url := fmt.Sprintf("https://api.telegram.org/bot%s/answerCallbackQuery", bot.Token)
	payload := map[string]string{"callback_query_id": queryID}
	jsonPayload, _ := json.Marshal(payload)
	http.Post(url, "application/json", bytes.NewBuffer(jsonPayload))
	// 忽略错误
}

// SendConfirmation 发送确认弹窗
func (bm *BotManager) SendConfirmation(service, env, version, user string, confirmChan chan models.DeployRequest, rejectChan chan models.StatusRequest) error {
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
	for _, uid := range bm.globalAllowedUsers {
		mentions.WriteString("@")
		mentions.WriteString(uid)
		mentions.WriteString(" ")
	}

	// 步骤3：构建确认消息文本，包括@用户，并转义
	message := fmt.Sprintf("*🛡️ 部署确认*\n\n"+
		"**服务**: `%s`\n"+
		"**环境**: `%s`\n"+
		"**版本**: `%s`\n"+
		"**用户**: `%s`\n\n"+
		"*请选择操作*\n\n"+
		"通知: %s", escapeCode(service), escapeCode(env), escapeCode(version), escapeCode(user), mentions.String())

	// 步骤4：构建内联键盘
	key := fmt.Sprintf("%s:%s:%s:%s", service, env, version, user)
	callbackDataConfirm := "confirm:" + key
	callbackDataReject := "reject:" + key

	keyboard := map[string]interface{}{
		"inline_keyboard": [][]map[string]string{
			{
				{"text": "✅ 确认部署", "callback_data": callbackDataConfirm},
				{"text": "❌ 拒绝部署", "callback_data": callbackDataReject},
			},
		},
	}

	// 存储通道
	bm.confirmationChans.Store(key, confirmationChans{confirmChan: confirmChan, rejectChan: rejectChan})

	// 步骤5：发送带键盘的消息
	_, err = bm.sendMessage(bot, bot.GroupID, message, keyboard)
	if err != nil {
		bm.confirmationChans.Delete(key) // 清理
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

// getDefaultBot 获取默认机器人
func (bm *BotManager) getDefaultBot() *TelegramBot {
	for _, bot := range bm.Bots {
		return bot
	}
	return nil
}

// SendNotification 发送部署通知
func (bm *BotManager) SendNotification(service, env, user, oldVersion, newVersion string, success bool) error {
	startTime := time.Now()
	// 步骤1：获取匹配的机器人
	bot, err := bm.getBotForService(service)
	if err != nil {
		logrus.WithFields(logrus.Fields{
			"time":   time.Now().Format("2006-01-02 15:04:05"),
			"method": "SendNotification",
			"took":   time.Since(startTime),
		}).Errorf(color.RedString("发送通知失败: %v", err))
		return err
	}

	// 步骤2：验证GroupID
	if bot.GroupID == "" {
		logrus.WithFields(logrus.Fields{
			"time":   time.Now().Format("2006-01-02 15:04:05"),
			"method": "SendNotification",
			"took":   time.Since(startTime),
		}).Errorf(color.RedString("发送通知失败: 机器人 [%s] 的GroupID为空", bot.Name))
		return fmt.Errorf("GroupID为空")
	}

	// 步骤3：生成通知消息
	message := bm.generateMarkdownMessage(service, env, user, oldVersion, newVersion, success)

	// 步骤4：发送通知
	_, err = bm.sendMessage(bot, bot.GroupID, message, nil)
	if err != nil {
		// 回退到纯文本
		logrus.WithFields(logrus.Fields{
			"time":   time.Now().Format("2006-01-02 15:04:05"),
			"method": "SendNotification",
			"took":   time.Since(startTime),
		}).Warnf(color.YellowString("MarkdownV2通知失败，尝试纯文本: %v", err))
		message = fmt.Sprintf("部署通知\n服务: %s\n环境: %s\n操作人: %s\n旧版本: %s\n新版本: %s\n状态: %s\n时间: %s",
			service, env, user, oldVersion, newVersion, map[bool]string{true: "成功", false: "失败"}[success], time.Now().Format("2006-01-02 15:04:05"))
		_, err = bm.sendMessage(bot, bot.GroupID, message, nil)
		if err != nil {
			logrus.WithFields(logrus.Fields{
				"time":   time.Now().Format("2006-01-02 15:04:05"),
				"method": "SendNotification",
				"took":   time.Since(startTime),
			}).Errorf(color.RedString("发送通知失败: %v", err))
			return err
		}
	}

	logrus.WithFields(logrus.Fields{
		"time":   time.Now().Format("2006-01-02 15:04:05"),
		"method": "SendNotification",
		"took":   time.Since(startTime),
	}).Infof(color.GreenString("通知发送成功: %s v%s [%s]", service, newVersion, env))
	return nil
}

// generateMarkdownMessage 生成美观的Markdown部署通知
func (bm *BotManager) generateMarkdownMessage(service, env, user, oldVersion, newVersion string, success bool) string {
	startTime := time.Now()
	// 步骤1：初始化字符串构建器
	var message strings.Builder

	// 步骤2：构建标题
	message.WriteString("**部署通知**\n\n")

	// 步骤3：添加详细信息
	message.WriteString("**服务**: `")
	message.WriteString(escapeCode(service))
	message.WriteString("`\n")

	message.WriteString("**环境**: `")
	message.WriteString(escapeCode(env))
	message.WriteString("`\n")

	message.WriteString("**操作人**: `")
	message.WriteString(escapeCode(user))
	message.WriteString("`\n")

	message.WriteString("**旧版本**: `")
	message.WriteString(escapeCode(oldVersion))
	message.WriteString("`\n")

	message.WriteString("**新版本**: `")
	message.WriteString(escapeCode(newVersion))
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
	message.WriteString(escapeCode(time.Now().Format("2006-01-02 15:04:05")))
	message.WriteString("`\n\n")

	// 步骤6：如果失败，添加回滚信息
	if !success {
		message.WriteString("*自动回滚已完成*\n\n")
	}

	// 步骤7：添加签名
	message.WriteString("\-\\-\\-\n")
	message.WriteString("*由 K8s\\-CICD Agent 自动发送*")

	// 步骤8：返回生成的字符串
	logrus.WithFields(logrus.Fields{
		"time":   time.Now().Format("2006-01-02 15:04:05"),
		"method": "generateMarkdownMessage",
		"took":   time.Since(startTime),
	}).Debugf(color.GreenString("生成Markdown消息成功"))
	return message.String()
}

// escapeNormal 转义正常文本的MarkdownV2特殊字符
func escapeNormal(text string) string {
	reserved := []string{"_", "*", "[", "]", "(", ")", "~", "`", ">", "#", "+", "-", "=", "|", "{", "}", ".", "!"}
	for _, char := range reserved {
		text = strings.ReplaceAll(text, char, "\\"+char)
	}
	return text
}

// escapeCode 转义inline code的MarkdownV2特殊字符
func escapeCode(text string) string {
	reserved := []string{"`", "\\"}
	for _, char := range reserved {
		text = strings.ReplaceAll(text, char, "\\"+char)
	}
	return text
}

// sendMessage 发送消息
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

// getBotForService 根据服务获取机器人
func (bm *BotManager) getBotForService(service string) (*TelegramBot, error) {
	for _, bot := range bm.Bots {
		for prefix, services := range bot.Services {
			if bot.RegexMatch {
				if matched, _ := regexp.MatchString(prefix, service); matched {
					return bot, nil
				}
			} else if strings.HasPrefix(service, prefix) {
				for _, s := range services {
					if s == service {
						return bot, nil
					}
				}
			}
		}
	}
	return nil, fmt.Errorf("服务 %s 未匹配任何机器人", service)
}

// DeleteMessage 删除消息
func (bm *BotManager) DeleteMessage(bot *TelegramBot, chatID string, messageID int64) error {
	startTime := time.Now()
	url := fmt.Sprintf("https://api.telegram.org/bot%s/deleteMessage", bot.Token)
	payload := map[string]interface{}{
		"chat_id":    chatID,
		"message_id": messageID,
	}
	jsonPayload, err := json.Marshal(payload)
	if err != nil {
		logrus.WithFields(logrus.Fields{
			"time":   time.Now().Format("2006-01-02 15:04:05"),
			"method": "DeleteMessage",
			"took":   time.Since(startTime),
		}).Errorf(color.RedString("JSON序列化失败: %v", err))
		return err
	}
	resp, err := http.Post(url, "application/json", bytes.NewBuffer(jsonPayload))
	if err != nil {
		logrus.WithFields(logrus.Fields{
			"time":   time.Now().Format("2006-01-02 15:04:05"),
			"method": "DeleteMessage",
			"took":   time.Since(startTime),
		}).Errorf(color.RedString("HTTP请求失败: %v", err))
		return err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		logrus.WithFields(logrus.Fields{
			"time":   time.Now().Format("2006-01-02 15:04:05"),
			"method": "DeleteMessage",
			"took":   time.Since(startTime),
		}).Errorf(color.RedString("读取响应失败: %v", err))
		return err
	}
	var result map[string]interface{}
	json.Unmarshal(body, &result)
	if ok, _ := result["ok"].(bool); !ok {
		logrus.WithFields(logrus.Fields{
			"time":   time.Now().Format("2006-01-02 15:04:05"),
			"method": "DeleteMessage",
			"took":   time.Since(startTime),
		}).Errorf(color.RedString("Telegram API错误: %v", result["description"]))
		return fmt.Errorf("Telegram API错误: %v", result["description"])
	}
	logrus.WithFields(logrus.Fields{
		"time":   time.Now().Format("2006-01-02 15:04:05"),
		"method": "DeleteMessage",
		"took":   time.Since(startTime),
	}).Infof(color.GreenString("消息删除成功"))
	return nil
}

// Stop 停止Telegram轮询
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