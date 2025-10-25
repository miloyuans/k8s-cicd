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
	Name       string              // 机器人名称
	Token      string              // Bot Token
	GroupID    string              // 群组ID
	Services   map[string][]string // 服务匹配规则: prefix -> 服务列表
	RegexMatch bool                // 是否使用正则匹配
	IsEnabled  bool                // 是否启用该机器人
}

// BotManager 多机器人管理器
type BotManager struct {
	Bots       map[string]*TelegramBot // 机器人映射
	offset     int64                   // Telegram updates offset
	updateChan chan map[string]interface{} // 更新通道
	stopChan   chan struct{}           // 停止信号通道
}

// NewBotManager 创建多机器人管理器
func NewBotManager(bots []config.TelegramBot) *BotManager {
	startTime := time.Now()
	// 步骤1：初始化管理器结构
	m := &BotManager{
		Bots:       make(map[string]*TelegramBot),
		updateChan: make(chan map[string]interface{}, 100),
		stopChan:   make(chan struct{}),
	}

	// 步骤2：遍历配置中的机器人，初始化启用的机器人
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

// escapeMarkdownV2 转义MarkdownV2的保留字符
func escapeMarkdownV2(text string) string {
	reservedChars := []string{"_", "*", "[", "]", "(", ")", "~", "`", ">", "#", "+", "-", "=", "|", "{", "}", ".", "!"}
	for _, char := range reservedChars {
		text = strings.ReplaceAll(text, char, "\\"+char)
	}
	return text
}

// SendConfirmation 发送确认弹窗
func (bm *BotManager) SendConfirmation(service, env, user, version string, confirmChan chan models.DeployRequest, rejectChan chan models.StatusRequest) {
	startTime := time.Now()
	// 步骤1：获取匹配的机器人
	bot, err := bm.getBotForService(service)
	if err != nil {
		logrus.WithFields(logrus.Fields{
			"time":   time.Now().Format("2006-01-02 15:04:05"),
			"method": "SendConfirmation",
			"took":   time.Since(startTime),
		}).Errorf(color.RedString("发送确认失败: %v", err))
		return
	}

	// 步骤2：验证GroupID
	if bot.GroupID == "" {
		logrus.WithFields(logrus.Fields{
			"time":   time.Now().Format("2006-01-02 15:04:05"),
			"method": "SendConfirmation",
			"took":   time.Since(startTime),
		}).Errorf(color.RedString("发送确认失败: 机器人 [%s] 的GroupID为空", bot.Name))
		return
	}

	// 步骤3：构造确认消息并转义
	message := fmt.Sprintf("*确认部署* `%s` *到* `%s`? *(用户:* `%s`*, 版本:* `%s`*)*",
		escapeMarkdownV2(service),
		escapeMarkdownV2(env),
		escapeMarkdownV2(user),
		escapeMarkdownV2(version))
	keyboard := map[string]interface{}{
		"inline_keyboard": [][]map[string]string{
			{
				{"text": "确认", "callback_data": fmt.Sprintf("confirm:%s:%s:%s:%s",
					escapeMarkdownV2(service),
					escapeMarkdownV2(env),
					escapeMarkdownV2(user),
					escapeMarkdownV2(version))},
				{"text": "拒绝", "callback_data": fmt.Sprintf("reject:%s:%s:%s:%s",
					escapeMarkdownV2(service),
					escapeMarkdownV2(env),
					escapeMarkdownV2(user),
					escapeMarkdownV2(version))},
			},
		},
	}

	// 步骤4：发送确认消息并记录发送的文本
	logrus.WithFields(logrus.Fields{
		"time":   time.Now().Format("2006-01-02 15:04:05"),
		"method": "SendConfirmation",
		"took":   time.Since(startTime),
		"data": logrus.Fields{
			"message": message,
		},
	}).Debugf(color.GreenString("准备发送确认消息"))
	respMessageID, err := bm.sendMessage(bot, bot.GroupID, message, keyboard)
	if err != nil {
		logrus.WithFields(logrus.Fields{
			"time":   time.Now().Format("2006-01-02 15:04:05"),
			"method": "SendConfirmation",
			"took":   time.Since(startTime),
		}).Errorf(color.RedString("发送确认弹窗失败: %v", err))
		return
	}

	// 步骤5：处理回调
	go func() {
		update := <-bm.updateChan
		if callback, ok := update["callback_query"].(map[string]interface{}); ok {
			data, ok := callback["data"].(string)
			if !ok {
				logrus.WithFields(logrus.Fields{
					"time":   time.Now().Format("2006-01-02 15:04:05"),
					"method": "SendConfirmation",
					"took":   time.Since(startTime),
				}).Errorf(color.RedString("回调数据无效"))
				return
			}
			parts := strings.Split(data, ":")
			if len(parts) != 5 {
				logrus.WithFields(logrus.Fields{
					"time":   time.Now().Format("2006-01-02 15:04:05"),
					"method": "SendConfirmation",
					"took":   time.Since(startTime),
				}).Errorf(color.RedString("回调数据格式错误: %s", data))
				return
			}
			action := parts[0]

			// 步骤6：删除确认消息
			if err := bm.DeleteMessage(bot, bot.GroupID, respMessageID); err != nil {
				logrus.WithFields(logrus.Fields{
					"time":   time.Now().Format("2006-01-02 15:04:05"),
					"method": "SendConfirmation",
					"took":   time.Since(startTime),
				}).Errorf(color.RedString("删除确认消息失败: %v", err))
			}

			// 步骤7：根据动作处理
			if action == "confirm" {
				confirmChan <- models.DeployRequest{
					Service:      service,
					Environments: []string{env},
					Version:      version,
					User:         user,
					Status:       "pending",
				}
				feedbackID, err := bm.sendMessage(bot, bot.GroupID, "部署确认", nil)
				if err != nil {
					logrus.WithFields(logrus.Fields{
						"time":   time.Now().Format("2006-01-02 15:04:05"),
						"method": "SendConfirmation",
						"took":   time.Since(startTime),
					}).Errorf(color.RedString("发送确认反馈失败: %v", err))
				} else {
					// 自动删除反馈消息
					time.AfterFunc(30*time.Second, func() {
						bm.DeleteMessage(bot, bot.GroupID, feedbackID)
					})
				}
			} else if action == "reject" {
				rejectChan <- models.StatusRequest{
					Service:     service,
					Environment: env,
					Version:     version,
					User:        user,
					Status:      "no_action",
				}
				feedbackID, err := bm.sendMessage(bot, bot.GroupID, "部署拒绝", nil)
				if err != nil {
					logrus.WithFields(logrus.Fields{
						"time":   time.Now().Format("2006-01-02 15:04:05"),
						"method": "SendConfirmation",
						"took":   time.Since(startTime),
					}).Errorf(color.RedString("发送拒绝反馈失败: %v", err))
				} else {
					// 自动删除反馈消息
					time.AfterFunc(30*time.Second, func() {
						bm.DeleteMessage(bot, bot.GroupID, feedbackID)
					})
				}
			}
			logrus.WithFields(logrus.Fields{
				"time":   time.Now().Format("2006-01-02 15:04:05"),
				"method": "SendConfirmation",
				"took":   time.Since(startTime),
			}).Infof(color.GreenString("回调处理完成: action=%s, service=%s, env=%s", action, service, env))
		}
	}()
}

// sendMessage 发送Telegram消息
func (bm *BotManager) sendMessage(bot *TelegramBot, chatID, text string, replyMarkup map[string]interface{}) (int, error) {
	startTime := time.Now()
	// 步骤1：验证chatID
	if chatID == "" {
		logrus.WithFields(logrus.Fields{
			"time":   time.Now().Format("2006-01-02 15:04:05"),
			"method": "sendMessage",
			"took":   time.Since(startTime),
		}).Errorf(color.RedString("发送消息失败: chatID为空"))
		return 0, fmt.Errorf("chatID为空")
	}

	// 步骤2：构造请求数据并转义文本
	reqData := map[string]interface{}{
		"chat_id":    chatID,
		"text":       escapeMarkdownV2(text),
		"parse_mode": "MarkdownV2",
	}
	if replyMarkup != nil {
		reqData["reply_markup"] = replyMarkup
	}
	jsonData, err := json.Marshal(reqData)
	if err != nil {
		logrus.WithFields(logrus.Fields{
			"time":   time.Now().Format("2006-01-02 15:04:05"),
			"method": "sendMessage",
			"took":   time.Since(startTime),
		}).Errorf(color.RedString("发送消息失败: JSON序列化错误: %v", err))
		return 0, err
	}

	// 步骤3：发送HTTP POST请求
	url := fmt.Sprintf("https://api.telegram.org/bot%s/sendMessage", bot.Token)
	resp, err := http.Post(url, "application/json", bytes.NewBuffer(jsonData))
	if err != nil {
		logrus.WithFields(logrus.Fields{
			"time":   time.Now().Format("2006-01-02 15:04:05"),
			"method": "sendMessage",
			"took":   time.Since(startTime),
		}).Errorf(color.RedString("发送消息失败: HTTP请求错误: %v", err))
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
		}).Errorf(color.RedString("发送消息失败: 响应解析错误: %v", err))
		return 0, err
	}

	// 步骤5：检查响应状态
	if !result.Ok {
		logrus.WithFields(logrus.Fields{
			"time":   time.Now().Format("2006-01-02 15:04:05"),
			"method": "sendMessage",
			"took":   time.Since(startTime),
			"data": logrus.Fields{
				"error_code":   result.ErrorCode,
				"description": result.Description,
			},
		}).Errorf(color.RedString("发送消息失败: Telegram API错误"))
		return 0, fmt.Errorf("Telegram API错误: code=%d, description=%s", result.ErrorCode, result.Description)
	}

	// 步骤6：提取消息ID
	messageID := int(result.Result["message_id"].(float64))
	logrus.WithFields(logrus.Fields{
		"time":   time.Now().Format("2006-01-02 15:04:05"),
		"method": "sendMessage",
		"took":   time.Since(startTime),
	}).Infof(color.GreenString("消息发送成功，message_id=%d", messageID))
	return messageID, nil
}

// DeleteMessage 删除Telegram消息
func (bm *BotManager) DeleteMessage(bot *TelegramBot, chatID string, messageID int) error {
	startTime := time.Now()
	// 步骤1：验证chatID
	if chatID == "" {
		logrus.WithFields(logrus.Fields{
			"time":   time.Now().Format("2006-01-02 15:04:05"),
			"method": "DeleteMessage",
			"took":   time.Since(startTime),
		}).Errorf(color.RedString("删除消息失败: chatID为空"))
		return fmt.Errorf("chatID为空")
	}

	// 步骤2：构造请求数据
	reqData := map[string]interface{}{
		"chat_id":    chatID,
		"message_id": messageID,
	}
	jsonData, err := json.Marshal(reqData)
	if err != nil {
		logrus.WithFields(logrus.Fields{
			"time":   time.Now().Format("2006-01-02 15:04:05"),
			"method": "DeleteMessage",
			"took":   time.Since(startTime),
		}).Errorf(color.RedString("删除消息失败: JSON序列化错误: %v", err))
		return err
	}

	// 步骤3：发送HTTP POST请求
	url := fmt.Sprintf("https://api.telegram.org/bot%s/deleteMessage", bot.Token)
	resp, err := http.Post(url, "application/json", bytes.NewBuffer(jsonData))
	if err != nil {
		logrus.WithFields(logrus.Fields{
			"time":   time.Now().Format("2006-01-02 15:04:05"),
			"method": "DeleteMessage",
			"took":   time.Since(startTime),
		}).Errorf(color.RedString("删除消息失败: HTTP请求错误: %v", err))
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
		}).Errorf(color.RedString("删除消息失败: 响应解析错误: %v", err))
		return err
	}

	// 步骤5：检查响应状态
	if !result.Ok {
		logrus.WithFields(logrus.Fields{
			"time":   time.Now().Format("2006-01-02 15:04:05"),
			"method": "DeleteMessage",
			"took":   time.Since(startTime),
			"data": logrus.Fields{
				"error_code":   result.ErrorCode,
				"description": result.Description,
			},
		}).Errorf(color.RedString("删除消息失败: Telegram API错误"))
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
func (bm *BotManager) generateMarkdownMessage(service, env, user, oldVersion, newVersion string, success bool) string {
	startTime := time.Now()
	// 步骤1：初始化字符串构建器
	var message strings.Builder

	// 步骤2：构建标题
	message.WriteString("*🚀 ")
	message.WriteString(escapeMarkdownV2(service))
	message.WriteString(" 部署 ")
	if success {
		message.WriteString("成功*")
	} else {
		message.WriteString("失败*")
	}
	message.WriteString("\n\n")

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
		message.WriteString("❌ *部署失败\\-已回滚*")
	}
	message.WriteString("\n")

	// 步骤5：添加时间
	message.WriteString("**时间**: `")
	message.WriteString(escapeMarkdownV2(time.Now().Format("2006-01-02 15:04:05")))
	message.WriteString("`\n\n")

	// 步骤6：如果失败，添加回滚信息
	if !success {
		message.WriteString("*🔄 自动回滚已完成*\n\n")
	}

	// 步骤7：添加签名
	message.WriteString("\\-\\-\\-\n")
	message.WriteString("*由 K8s\\-CICD Agent 自动发送*")

	// 步骤8：返回生成的字符串
	logrus.WithFields(logrus.Fields{
		"time":   time.Now().Format("2006-01-02 15:04:05"),
		"method": "generateMarkdownMessage",
		"took":   time.Since(startTime),
	}).Debugf(color.GreenString("生成Markdown消息成功"))
	return message.String()
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