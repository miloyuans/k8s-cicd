// bot.go
package telegram

import (
	"bytes"
	"context"
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

// 时间格式常量
const timeFormat = "2006-01-02 15:04:05"

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
	ctx                context.Context         // 上下文
	cancel             context.CancelFunc      // 取消函数
}

// NewBotManager 创建多机器人管理器
func NewBotManager(bots []config.TelegramBot) *BotManager {
	startTime := time.Now()
	// 步骤1：初始化管理器结构
	ctx, cancel := context.WithCancel(context.Background())
	m := &BotManager{
		Bots:               make(map[string]*TelegramBot),
		updateChan:         make(chan map[string]interface{}, 100),
		stopChan:           make(chan struct{}),
		globalAllowedUsers: make([]string, 0),
		ctx:                ctx,
		cancel:             cancel,
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
				"time":   time.Now().Format(timeFormat),
				"method": "NewBotManager",
				"took":   time.Since(startTime),
			}).Infof(color.GreenString("✅ Telegram机器人 [%s] 已启用", bot.Name))
		}
	}

	// 步骤3：检查是否有启用的机器人
	if len(m.Bots) == 0 {
		logrus.WithFields(logrus.Fields{
			"time":   time.Now().Format(timeFormat),
			"method": "NewBotManager",
			"took":   time.Since(startTime),
		}).Warn("⚠️ 未启用任何Telegram机器人")
	}

	// 步骤4：返回管理器实例
	logrus.WithFields(logrus.Fields{
		"time":   time.Now().Format(timeFormat),
		"method": "NewBotManager",
		"took":   time.Since(startTime),
	}).Info(color.GreenString("BotManager创建成功"))
	return m
}

// SetGlobalAllowedUsers 设置全局允许用户
func (m *BotManager) SetGlobalAllowedUsers(users []string) {
	m.globalAllowedUsers = users
	logrus.WithFields(logrus.Fields{
		"time":   time.Now().Format(timeFormat),
		"method": "SetGlobalAllowedUsers",
	}).Infof("全局允许用户设置为: %v", users)
}

// StartPolling 启动Telegram Updates轮询
func (m *BotManager) StartPolling() {
	startTime := time.Now()
	// 步骤1：记录启动日志
	logrus.WithFields(logrus.Fields{
		"time":   time.Now().Format(timeFormat),
		"method": "StartPolling",
		"took":   time.Since(startTime),
	}).Info(color.GreenString("🔄 启动Telegram Updates轮询"))
	// 步骤2：启动goroutine进行无限轮询
	go func() {
		for {
			select {
			case <-m.ctx.Done():
				logrus.WithFields(logrus.Fields{
					"time":   time.Now().Format(timeFormat),
					"method": "StartPolling",
					"took":   time.Since(startTime),
				}).Info(color.GreenString("🛑 Telegram轮询已停止"))
				return
			default:
				m.pollUpdates()
				time.Sleep(1 * time.Second) // 避免忙等
			}
		}
	}()
}

// pollUpdates 轮询Telegram Updates
func (m *BotManager) pollUpdates() {
	startTime := time.Now()
	// 步骤1：获取默认机器人
	bot := m.getDefaultBot()
	if bot == nil {
		logrus.WithFields(logrus.Fields{
			"time":   time.Now().Format(timeFormat),
			"method": "pollUpdates",
			"took":   time.Since(startTime),
		}).Errorf(color.RedString("❌ 无可用机器人，无法轮询"))
		return
	}

	// 步骤2：构建getUpdates请求URL
	url := fmt.Sprintf("https://api.telegram.org/bot%s/getUpdates?offset=%d&timeout=10", bot.Token, m.offset)

	// 步骤3：发送HTTP GET请求（带重试）
	var resp *http.Response
	var err error
	for attempt := 0; attempt < 3; attempt++ {
		resp, err = http.Get(url)
		if err == nil {
			break
		}
		logrus.WithFields(logrus.Fields{
			"time":   time.Now().Format(timeFormat),
			"method": "pollUpdates",
			"took":   time.Since(startTime),
		}).Warnf("Telegram轮询网络错误，第 %d 次重试: %v", attempt+1, err)
		time.Sleep(2 * time.Second)
	}
	if err != nil {
		logrus.WithFields(logrus.Fields{
			"time":   time.Now().Format(timeFormat),
			"method": "pollUpdates",
			"took":   time.Since(startTime),
		}).Errorf(color.RedString("❌ Telegram轮询网络错误: %v", err))
		return
	}
	defer resp.Body.Close()

	// 步骤4：解析响应JSON
	var result struct {
		Ok          bool                      `json:"ok"`
		Result      []map[string]interface{}  `json:"result"`
		ErrorCode   int                       `json:"error_code"`
		Description string                    `json:"description"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		logrus.WithFields(logrus.Fields{
			"time":   time.Now().Format(timeFormat),
			"method": "pollUpdates",
			"took":   time.Since(startTime),
		}).Errorf(color.RedString("❌ 解析Telegram响应失败: %v", err))
		return
	}

	// 步骤5：检查响应是否成功
	if !result.Ok {
		logrus.WithFields(logrus.Fields{
			"time":   time.Now().Format(timeFormat),
			"method": "pollUpdates",
			"took":   time.Since(startTime),
			"data": logrus.Fields{
				"error_code": result.ErrorCode,
				"description": result.Description,
			},
		}).Errorf(color.RedString("❌ Telegram API错误"))
		return
	}

	// 步骤6：处理更新
	for _, update := range result.Result {
		m.offset = int64(update["update_id"].(float64)) + 1
		select {
		case m.updateChan <- update:
			logrus.WithFields(logrus.Fields{
				"time":   time.Now().Format(timeFormat),
				"method": "pollUpdates",
				"took":   time.Since(startTime),
			}).Debugf("处理Telegram更新: %v", update["update_id"])
		default:
			logrus.WithFields(logrus.Fields{
				"time":   time.Now().Format(timeFormat),
				"method": "pollUpdates",
				"took":   time.Since(startTime),
			}).Warn("更新通道已满，丢弃更新: %v", update["update_id"])
		}
	}
}

// getDefaultBot 获取默认机器人
func (m *BotManager) getDefaultBot() *TelegramBot {
	for _, bot := range m.Bots {
		return bot
	}
	return nil
}

// SendNotification 发送部署通知
func (m *BotManager) SendNotification(service, env, user, oldVersion, newVersion string, success bool) error {
	startTime := time.Now()
	// 步骤1：选择机器人
	bot, err := m.getBotForService(service)
	if err != nil {
		logrus.WithFields(logrus.Fields{
			"time":   time.Now().Format(timeFormat),
			"method": "SendNotification",
			"took":   time.Since(startTime),
		}).Errorf(color.RedString("发送通知失败: %v", err))
		return err
	}

	// 步骤2：生成消息
	message := m.generateMarkdownMessage(service, env, user, oldVersion, newVersion, success)

	// 步骤3：发送消息（无 replyMarkup）
	return m.sendMessage(bot, message, bot.GroupID, "")
}

// SendPopup 发送弹窗确认消息
func (m *BotManager) SendPopup(task models.DeployRequest) error {
	startTime := time.Now()
	// 步骤1：选择机器人
	bot, err := m.getBotForService(task.Service)
	if err != nil {
		logrus.WithFields(logrus.Fields{
			"time":   time.Now().Format(timeFormat),
			"method": "SendPopup",
			"took":   time.Since(startTime),
		}).Errorf(color.RedString("发送弹窗失败: %v", err))
		return err
	}

	// 步骤2：生成消息
	message := fmt.Sprintf(
		"*🚀 部署请求: %s*\n\n**服务**: `%s`\n**环境**: `%s`\n**版本**: `%s`\n**操作人**: `%s`\n**时间**: `%s`\n\n请确认是否继续部署？",
		task.Service, task.Service, task.Environments[0], task.Version, task.User, time.Now().Format(timeFormat))

	// 步骤3：生成 inline keyboard
	replyMarkup := map[string]interface{}{
		"inline_keyboard": [][]map[string]interface{}{
			{
				{"text": "✅ 确认", "callback_data": fmt.Sprintf("confirm:%s:%s:%s:%s", task.Service, task.Version, task.Environments[0], task.User)},
				{"text": "❌ 拒绝", "callback_data": fmt.Sprintf("reject:%s:%s:%s:%s", task.Service, task.Version, task.Environments[0], task.User)},
			},
		},
	}
	replyMarkupJSON, err := json.Marshal(replyMarkup)
	if err != nil {
		logrus.WithFields(logrus.Fields{
			"time":   time.Now().Format(timeFormat),
			"method": "SendPopup",
			"took":   time.Since(startTime),
		}).Errorf(color.RedString("生成 inline keyboard 失败: %v", err))
		return err
	}

	// 步骤4：发送弹窗消息
	return m.sendMessage(bot, message, bot.GroupID, string(replyMarkupJSON))
}

// DeleteMessage 发送删除消息请求
func (m *BotManager) DeleteMessage(bot *TelegramBot, messageID int) error {
	startTime := time.Now()
	// 步骤1：构建删除请求
	url := fmt.Sprintf("https://api.telegram.org/bot%s/deleteMessage", bot.Token)
	reqBody, err := json.Marshal(map[string]interface{}{
		"chat_id":    bot.GroupID,
		"message_id": messageID,
	})
	if err != nil {
		logrus.WithFields(logrus.Fields{
			"time":   time.Now().Format(timeFormat),
			"method": "DeleteMessage",
			"took":   time.Since(startTime),
		}).Errorf(color.RedString("序列化删除请求失败: %v", err))
		return err
	}

	// 步骤2：发送HTTP POST请求
	resp, err := http.Post(url, "application/json", bytes.NewBuffer(reqBody))
	if err != nil {
		logrus.WithFields(logrus.Fields{
			"time":   time.Now().Format(timeFormat),
			"method": "DeleteMessage",
			"took":   time.Since(startTime),
		}).Errorf(color.RedString("删除消息网络错误: %v", err))
		return err
	}
	defer resp.Body.Close()

	// 步骤3：解析响应
	var result struct {
		Ok          bool   `json:"ok"`
		ErrorCode   int    `json:"error_code"`
		Description string `json:"description"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		logrus.WithFields(logrus.Fields{
			"time":   time.Now().Format(timeFormat),
			"method": "DeleteMessage",
			"took":   time.Since(startTime),
		}).Errorf(color.RedString("解析响应失败: %v", err))
		return err
	}

	// 步骤4：检查响应是否成功
	if !result.Ok {
		logrus.WithFields(logrus.Fields{
			"time":   time.Now().Format(timeFormat),
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
		"time":   time.Now().Format(timeFormat),
		"method": "DeleteMessage",
		"took":   time.Since(startTime),
	}).Infof(color.GreenString("消息删除成功，message_id=%d", messageID))
	return nil
}

// getBotForService 根据服务名选择机器人
func (m *BotManager) getBotForService(service string) (*TelegramBot, error) {
	startTime := time.Now()
	// 步骤1：遍历所有机器人
	for _, bot := range m.Bots {
		// 步骤2：遍历服务的匹配规则
		for _, serviceList := range bot.Services {
			// 步骤3：遍历服务列表中的模式
			for _, pattern := range serviceList {
				if bot.RegexMatch {
					// 使用正则匹配
					matched, err := regexp.MatchString(pattern, service)
					if err == nil && matched {
						logrus.WithFields(logrus.Fields{
							"time":   time.Now().Format(timeFormat),
							"method": "getBotForService",
							"took":   time.Since(startTime),
						}).Infof(color.GreenString("服务 %s 匹配机器人 %s", service, bot.Name))
						return bot, nil
					}
				} else {
					// 使用前缀匹配（忽略大小写）
					if strings.HasPrefix(strings.ToUpper(service), strings.ToUpper(pattern)) {
						logrus.WithFields(logrus.Fields{
							"time":   time.Now().Format(timeFormat),
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
		"time":   time.Now().Format(timeFormat),
		"method": "getBotForService",
		"took":   time.Since(startTime),
	}).Errorf(color.RedString("服务 %s 未匹配任何机器人", service))
	return nil, fmt.Errorf("服务 %s 未匹配任何机器人", service)
}

// Stop 停止Telegram轮询
func (m *BotManager) Stop() {
	startTime := time.Now()
	// 步骤1：关闭停止通道
	m.cancel()
	close(m.stopChan)
	logrus.WithFields(logrus.Fields{
		"time":   time.Now().Format(timeFormat),
		"method": "Stop",
		"took":   time.Since(startTime),
	}).Infof(color.GreenString("Telegram轮询停止"))
}

// generateMarkdownMessage 生成美观的Markdown部署通知
func (m *BotManager) generateMarkdownMessage(service, env, user, oldVersion, newVersion string, success bool) string {
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
	message.WriteString(time.Now().Format(timeFormat))
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
		"time":   time.Now().Format(timeFormat),
		"method": "generateMarkdownMessage",
		"took":   time.Since(startTime),
	}).Debugf(color.GreenString("生成Markdown消息成功"))
	return message.String()
}

// sendMessage 发送Telegram消息
func (m *BotManager) sendMessage(bot *TelegramBot, message, chatID, replyMarkup string) error {
	startTime := time.Now()
	// 步骤1：构建请求
	url := fmt.Sprintf("https://api.telegram.org/bot%s/sendMessage", bot.Token)
	body := map[string]interface{}{
		"chat_id":    chatID,
		"text":       message,
		"parse_mode": "Markdown",
	}
	if replyMarkup != "" {
		body["reply_markup"] = json.RawMessage(replyMarkup)
	}
	reqBody, err := json.Marshal(body)
	if err != nil {
		logrus.WithFields(logrus.Fields{
			"time":   time.Now().Format(timeFormat),
			"method": "sendMessage",
			"took":   time.Since(startTime),
		}).Errorf(color.RedString("序列化消息失败: %v", err))
		return err
	}

	// 步骤2：发送HTTP POST请求
	resp, err := http.Post(url, "application/json", bytes.NewBuffer(reqBody))
	if err != nil {
		logrus.WithFields(logrus.Fields{
			"time":   time.Now().Format(timeFormat),
			"method": "sendMessage",
			"took":   time.Since(startTime),
		}).Errorf(color.RedString("发送消息网络错误: %v", err))
		return err
	}
	defer resp.Body.Close()

	// 步骤3：解析响应
	var result struct {
		Ok          bool   `json:"ok"`
		ErrorCode   int    `json:"error_code"`
		Description string `json:"description"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		logrus.WithFields(logrus.Fields{
			"time":   time.Now().Format(timeFormat),
			"method": "sendMessage",
			"took":   time.Since(startTime),
		}).Errorf(color.RedString("解析响应失败: %v", err))
		return err
	}

	// 步骤4：检查响应
	if !result.Ok {
		logrus.WithFields(logrus.Fields{
			"time":   time.Now().Format(timeFormat),
			"method": "sendMessage",
			"took":   time.Since(startTime),
			"data": logrus.Fields{
				"error_code": result.ErrorCode,
				"description": result.Description,
			},
		}).Errorf(color.RedString("Telegram API错误"))
		return fmt.Errorf("Telegram API错误: code=%d, description=%s", result.ErrorCode, result.Description)
	}

	logrus.WithFields(logrus.Fields{
		"time":   time.Now().Format(timeFormat),
		"method": "sendMessage",
		"took":   time.Since(startTime),
	}).Infof(color.GreenString("消息发送成功"))
	return nil
}