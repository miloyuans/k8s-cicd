// 文件: telegram/bot.go (完整文件，修复所有编译错误：移除未使用 models 导入；统一 HandleCallback 参数为单 map；移除已删除的 updateChan 字段；保留多 Bot 隔离、单环境查询等所有功能)
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

	"k8s-cicd/approval/client"
	"k8s-cicd/approval/config"

	"github.com/fatih/color"
	"github.com/sirupsen/logrus"
)

// TelegramBot 单个机器人配置
type TelegramBot struct {
	Name         string              // 机器人名称
	Token        string              // Bot Token
	GroupID      string              // 群组ID
	Services     map[string][]string // 服务匹配规则
	RegexMatch   bool                // 是否使用正则匹配
	IsEnabled    bool                // 是否启用
	AllowedUsers []string            // 允许操作的用户
}

// BotManager 机器人管理器
type BotManager struct {
	Bots               map[string]*TelegramBot
	globalAllowedUsers []string
	mongo              *client.MongoClient
	botChannels        map[string]chan map[string]interface{} // 每个 Bot 独立 channel
	stopChan           chan struct{}
	offsets            map[string]int64
	mu                 sync.Mutex
	sentTasks          map[string]bool // task_id + env → bool
	cfg                *config.Config
}

// NewBotManager 创建管理器
func NewBotManager(bots []config.TelegramBot, cfg *config.Config) *BotManager {
	m := &BotManager{
		Bots:               make(map[string]*TelegramBot),
		botChannels:        make(map[string]chan map[string]interface{}),
		stopChan:           make(chan struct{}),
		offsets:            make(map[string]int64),
		sentTasks:          make(map[string]bool),
		cfg:                cfg,
	}

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
			m.botChannels[bot.Name] = make(chan map[string]interface{}, 100)
			m.offsets[bot.Name] = 0
			logrus.Infof(color.GreenString("Telegram机器人 [%s] 已启用"), bot.Name)
		}
	}

	if len(m.Bots) == 0 {
		logrus.Warn("未启用任何Telegram机器人")
	}
	logrus.Info(color.GreenString("k8s-approval BotManager 创建成功"))
	return m
}

// SetMongoClient 注入 Mongo 客户端
func (bm *BotManager) SetMongoClient(mongo *client.MongoClient) {
	bm.mongo = mongo
}

// SetGlobalAllowedUsers 设置全局允许用户
func (bm *BotManager) SetGlobalAllowedUsers(users []string) {
	bm.globalAllowedUsers = users
}

// Start 启动轮询和弹窗
func (bm *BotManager) Start() {
	logrus.Info(color.GreenString("启动 k8s-approval Telegram 服务"))

	// 为每个 Bot 启动独立轮询和处理
	for _, bot := range bm.Bots {
		go bm.startPollingForBot(bot)
		go bm.handleUpdatesForBot(bot)
	}
	go bm.pollPendingTasks()
}

// startPollingForBot 启动单个 Bot 的轮询
func (bm *BotManager) startPollingForBot(bot *TelegramBot) {
	for {
		select {
		case <-bm.stopChan:
			logrus.WithFields(logrus.Fields{
				"bot": bot.Name,
			}).Info(color.GreenString("Telegram 轮询停止"))
			return
		default:
			bm.pollUpdatesForBot(bot)
		}
	}
}

// pollUpdatesForBot 轮询单个 Bot 的 Updates
func (bm *BotManager) pollUpdatesForBot(bot *TelegramBot) {
	bm.mu.Lock()
	offset := bm.offsets[bot.Name]
	bm.mu.Unlock()

	url := fmt.Sprintf("https://api.telegram.org/bot%s/getUpdates?offset=%d&timeout=10", bot.Token, offset)
	resp, err := http.Get(url)
	if err != nil {
		logrus.WithFields(logrus.Fields{
			"bot": bot.Name,
		}).Errorf(color.RedString("Telegram 轮询错误: %v"), err)
		time.Sleep(5 * time.Second)
		return
	}
	defer resp.Body.Close()

	var result struct {
		Ok     bool                       `json:"ok"`
		Result []map[string]interface{}   `json:"result"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		logrus.WithFields(logrus.Fields{
			"bot": bot.Name,
		}).Errorf(color.RedString("解析响应失败: %v"), err)
		return
	}

	for _, update := range result.Result {
		bm.mu.Lock()
		bm.offsets[bot.Name] = int64(update["update_id"].(float64)) + 1
		bm.mu.Unlock()
		bm.botChannels[bot.Name] <- update
	}
}

// handleUpdatesForBot 处理单个 Bot 的 Updates
func (bm *BotManager) handleUpdatesForBot(bot *TelegramBot) {
	for update := range bm.botChannels[bot.Name] {
		if callback, ok := update["callback_query"].(map[string]interface{}); ok {
			bm.HandleCallback(bot, callback) // 修复: 传入 bot 指针
		}
	}
}

// pollPendingTasks 根据 "待确认" 状态 + 配置环境触发弹窗
func (bm *BotManager) pollPendingTasks() {
	ticker := time.NewTicker(bm.cfg.API.QueryInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			logrus.WithFields(logrus.Fields{
				"time":   time.Now().Format("2006-01-02 15:04:05"),
				"method": "pollPendingTasks",
				"confirm_envs": bm.cfg.Query.ConfirmEnvs,
			}).Debug("=== 开始新一轮 pending 任务轮询 (触发条件: 状态=待确认 + 环境在配置中) ===")

			totalSent := 0
			for _, env := range bm.cfg.Query.ConfirmEnvs {
				logrus.WithFields(logrus.Fields{
					"time":   time.Now().Format("2006-01-02 15:04:05"),
					"method": "pollPendingTasks",
					"env":    env,
					"status_filter": "待确认",
				}).Debugf("查询 %s 环境的待确认任务 (配置确认环境 + 状态过滤)", env)

				tasks, err := bm.mongo.GetPendingTasks(env)
				if err != nil {
					logrus.WithFields(logrus.Fields{
						"time":   time.Now().Format("2006-01-02 15:04:05"),
						"method": "pollPendingTasks",
						"env":    env,
					}).Errorf("查询 %s 待确认任务失败: %v", env, err)
					continue
				}

				logrus.WithFields(logrus.Fields{
					"time":   time.Now().Format("2006-01-02 15:04:05"),
					"method": "pollPendingTasks",
					"env":    env,
					"count":  len(tasks),
					"status_filter": "待确认",
				}).Infof("找到 %d 个待弹窗任务 (状态=待确认, 环境=%s)", len(tasks), env)

				for i := range tasks {
					task := &tasks[i]

					if len(task.Environments) == 0 {
						logrus.WithFields(logrus.Fields{
							"time":   time.Now().Format("2006-01-02 15:04:05"),
							"method": "pollPendingTasks",
							"task_id": task.TaskID,
							"service": task.Service,
						}).Warnf("任务 Environments 为空，跳过弹窗")
						continue
					}

					service := task.Service
					if service == "" {
						logrus.WithFields(logrus.Fields{
							"time":   time.Now().Format("2006-01-02 15:04:05"),
							"method": "pollPendingTasks",
							"task_id": task.TaskID,
						}).Warnf("任务 Service 为空，跳过弹窗")
						continue
					}

					bot, err := bm.getBotForService(service)
					if err != nil {
						logrus.WithFields(logrus.Fields{
							"time":   time.Now().Format("2006-01-02 15:04:05"),
							"method": "pollPendingTasks",
							"service": service,
						}).Errorf("找不到匹配的机器人: %v", err)

						defaultBot := bm.getDefaultBot()
						if defaultBot != nil {
							warningMsg := fmt.Sprintf("服务 %s 未匹配任何机器人，无法发送审批弹窗。请检查配置。任务ID: %s", service, task.TaskID)
							_, sendErr := bm.sendMessage(defaultBot, defaultBot.GroupID, warningMsg, "")
							if sendErr != nil {
								logrus.WithFields(logrus.Fields{
									"time":    time.Now().Format("2006-01-02 15:04:05"),
									"method":  "pollPendingTasks",
									"service": service,
									"error":   sendErr.Error(),
								}).Errorf("发送机器人匹配警告失败")
							} else {
								logrus.WithFields(logrus.Fields{
									"time":   time.Now().Format("2006-01-02 15:04:05"),
									"method": "pollPendingTasks",
									"service": service,
								}).Infof("已发送机器人匹配警告到默认群组")
							}
						}
						continue
					}

					taskKey := fmt.Sprintf("%s-%s", task.TaskID, env)
					if bm.sentTasks[taskKey] {
						logrus.WithFields(logrus.Fields{
							"time":   time.Now().Format("2006-01-02 15:04:05"),
							"method": "pollPendingTasks",
							"task_id": task.TaskID,
							"service": service,
							"env":     env,
						}).Debugf("任务已发送过，跳过")
						continue
					}

					keyboard := map[string]interface{}{
						"inline_keyboard": [][]map[string]string{
							{
								{"text": "确认部署", "callback_data": fmt.Sprintf("confirm-%s", task.TaskID)},
								{"text": "拒绝部署", "callback_data": fmt.Sprintf("reject-%s", task.TaskID)},
							},
						},
					}
					messageText := fmt.Sprintf(
						"**部署审批请求**\n\n"+
							"**服务**: %s\n"+
							"**版本**: %s\n"+
							"**环境**: %s\n"+
							"**操作人**: %s\n"+
							"**创建时间**: %s\n\n"+
							"请在 24 小时内操作，否则自动过期。",
						task.Service, task.Version, task.Environment, task.User, task.CreatedAt.Format("2006-01-02 15:04:05"),
					)

					messageID, err := bm.sendMessageWithKeyboard(bot, bot.GroupID, messageText, keyboard, "Markdown")
					if err != nil {
						logrus.WithFields(logrus.Fields{
							"time":    time.Now().Format("2006-01-02 15:04:05"),
							"method":  "pollPendingTasks",
							"task_id": task.TaskID,
							"service": service,
							"env":     env,
							"error":   err.Error(),
						}).Errorf("发送弹窗失败")
						continue
					}

					task.PopupBotName = bot.Name
					task.PopupSent = true
					task.PopupMessageID = messageID
					task.PopupSentAt = time.Now()
					if err := bm.mongo.MarkPopupSent(task.TaskID, messageID, bot.Name); err != nil {
						logrus.WithFields(logrus.Fields{
							"time":    time.Now().Format("2006-01-02 15:04:05"),
							"method":  "pollPendingTasks",
							"task_id": task.TaskID,
							"service": service,
							"env":     env,
							"error":   err.Error(),
						}).Errorf("标记弹窗已发送失败")
					}

					bm.sentTasks[taskKey] = true
					totalSent++

					logrus.WithFields(logrus.Fields{
						"time":     time.Now().Format("2006-01-02 15:04:05"),
						"method":   "pollPendingTasks",
						"task_id":  task.TaskID,
						"service":  service,
						"env":      env,
						"msg_id":   messageID,
						"bot":      bot.Name,
					}).Infof("弹窗发送成功 (消息ID: %d)", messageID)
				}
			}

			logrus.WithFields(logrus.Fields{
				"time":        time.Now().Format("2006-01-02 15:04:05"),
				"method":      "pollPendingTasks",
				"total_sent":  totalSent,
				"confirm_envs": bm.cfg.Query.ConfirmEnvs,
			}).Infof("本轮弹窗发送完成，共发送 %d 个审批请求", totalSent)
		}
	}
}

// HandleCallback 处理回调查询（修复: 接收 bot 指针，移除对 updateChan 的依赖）
func (bm *BotManager) HandleCallback(bot *TelegramBot, callback map[string]interface{}) {
	startTime := time.Now()

	id := callback["id"].(string)
	data := callback["data"].(string)
	user := callback["from"].(map[string]interface{})
	username := user["username"].(string)
	message := callback["message"].(map[string]interface{})
	chatID := fmt.Sprintf("%d", int(message["chat"].(map[string]interface{})["id"].(float64)))
	messageID := int(message["message_id"].(float64))

	parts := strings.Split(data, "-")
	if len(parts) != 2 {
		bm.answerCallback(bot, id, "无效操作")
		return
	}

	action := parts[0]
	taskID := parts[1]

	task, err := bm.mongo.GetTaskByID(taskID)
	if err != nil {
		bm.answerCallback(bot, id, "任务不存在")
		logrus.WithFields(logrus.Fields{
			"time":    time.Now().Format("2006-01-02 15:04:05"),
			"method":  "HandleCallback",
			"task_id": taskID,
		}).Errorf("获取任务失败: %v", err)
		return
	}

	// 校验 Bot 名称一致性
	if task.PopupBotName != bot.Name {
		bm.answerCallback(bot, id, "机器人不匹配")
		logrus.WithFields(logrus.Fields{
			"time":     time.Now().Format("2006-01-02 15:04:05"),
			"method":   "HandleCallback",
			"task_id":  taskID,
			"expected_bot": task.PopupBotName,
			"actual_bot": bot.Name,
		}).Errorf("弹窗 Bot 不匹配")
		return
	}

	if !bm.isUserAllowed(username, bot) {
		bm.answerCallback(bot, id, "您无权限操作")
		logrus.WithFields(logrus.Fields{
			"time":   time.Now().Format("2006-01-02 15:04:05"),
			"method": "HandleCallback",
			"user":   username,
			"task_id": taskID,
		}).Warnf("用户无权限操作")
		return
	}

	var status string
	var feedbackText string
	if action == "confirm" {
		status = "已确认"
		feedbackText = fmt.Sprintf("用户 %s 已批准部署:\n服务: %s\n版本: %s\n环境: %s\n任务ID: %s", username, task.Service, task.Version, task.Environment, taskID)
	} else if action == "reject" {
		status = "已拒绝"
		feedbackText = fmt.Sprintf("用户 %s 已拒绝部署:\n服务: %s\n版本: %s\n环境: %s\n任务ID: %s", username, task.Service, task.Version, task.Environment, taskID)
	} else {
		bm.answerCallback(bot, id, "无效操作")
		return
	}

	if err := bm.mongo.UpdateTaskStatus(taskID, status, username); err != nil {
		bm.answerCallback(bot, id, "更新状态失败")
		logrus.WithFields(logrus.Fields{
			"time":    time.Now().Format("2006-01-02 15:04:05"),
			"method":  "HandleCallback",
			"task_id": taskID,
			"status":  status,
		}).Errorf("更新任务状态失败: %v", err)
		return
	}

	if err := bm.DeleteMessage(bot, chatID, messageID); err != nil {
		logrus.WithFields(logrus.Fields{
			"time":    time.Now().Format("2006-01-02 15:04:05"),
			"method":  "HandleCallback",
			"task_id": taskID,
			"msg_id":  messageID,
		}).Errorf("删除弹窗失败: %v", err)
	} else {
		logrus.WithFields(logrus.Fields{
			"time":    time.Now().Format("2006-01-02 15:04:05"),
			"method":  "HandleCallback",
			"task_id": taskID,
			"msg_id":  messageID,
		}).Infof("已删除原弹窗消息")
	}

	if _, err := bm.sendMessage(bot, bot.GroupID, feedbackText, ""); err != nil {
		logrus.WithFields(logrus.Fields{
			"time":    time.Now().Format("2006-01-02 15:04:05"),
			"method":  "HandleCallback",
			"task_id": taskID,
		}).Errorf("发送反馈消息失败: %v", err)
	} else {
		logrus.WithFields(logrus.Fields{
			"time":    time.Now().Format("2006-01-02 15:04:05"),
			"method":  "HandleCallback",
			"task_id": taskID,
		}).Infof("已发送反馈消息 (操作: %s)", action)
	}

	if action == "reject" {
		logrus.WithFields(logrus.Fields{
			"time":    time.Now().Format("2006-01-02 15:04:05"),
			"method":  "HandleCallback",
			"task_id": taskID,
			"full_task": fmt.Sprintf("%+v", task),
		}).Infof("用户 %s 拒绝部署，准备删除任务 (状态: %s)", username, status)

		if err := bm.mongo.DeleteTask(taskID); err != nil {
			logrus.WithFields(logrus.Fields{
				"time":    time.Now().Format("2006-01-02 15:04:05"),
				"method":  "HandleCallback",
				"task_id": taskID,
			}).Errorf("立即删除任务失败: %v", err)
		} else {
			logrus.WithFields(logrus.Fields{
				"time":    time.Now().Format("2006-01-02 15:04:05"),
				"method":  "HandleCallback",
				"task_id": taskID,
				"full_task": fmt.Sprintf("%+v", task),
			}).Infof("已立即删除 已拒绝 任务")
		}
	}

	bm.answerCallback(bot, id, fmt.Sprintf("操作已执行: %s (弹窗已删除，反馈已发送，状态: %s)", action, status))

	logrus.WithFields(logrus.Fields{
		"time":   time.Now().Format("2006-01-02 15:04:05"),
		"method": "HandleCallback",
		"user":   username,
		"action": action,
		"task_id": taskID,
		"status": status,
		"bot":    bot.Name,
		"took":   time.Since(startTime),
	}).Infof("回调处理完成: 原弹窗删除 + 反馈发送 (状态变更: %s)", status)
}

// answerCallback 响应 Callback Query（传入 bot）
func (bm *BotManager) answerCallback(bot *TelegramBot, id, text string) {
	payload := map[string]interface{}{
		"callback_query_id": id,
		"text":              text,
	}
	jsonData, _ := json.Marshal(payload)
	_, _ = http.Post(fmt.Sprintf("https://api.telegram.org/bot%s/answerCallbackQuery", bot.Token), "application/json", bytes.NewBuffer(jsonData))
}

// isUserAllowed 支持机器人级权限
func (bm *BotManager) isUserAllowed(username string, bot *TelegramBot) bool {
	for _, u := range bm.globalAllowedUsers {
		if u == username {
			return true
		}
	}
	if bot != nil {
		for _, u := range bot.AllowedUsers {
			if u == username {
				return true
			}
		}
	}
	return false
}

// getDefaultBot 获取默认机器人
func (bm *BotManager) getDefaultBot() *TelegramBot {
	for _, bot := range bm.Bots {
		if bot.IsEnabled {
			return bot
		}
	}
	return nil
}

// getBotForService 服务匹配机器人
func (bm *BotManager) getBotForService(service string) (*TelegramBot, error) {
	for _, bot := range bm.Bots {
		for _, serviceList := range bot.Services {
			for _, pattern := range serviceList {
				if bot.RegexMatch {
					if matched, _ := regexp.MatchString(pattern, service); matched {
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

// sendMessage 发送消息
func (bm *BotManager) sendMessage(bot *TelegramBot, chatID, text, parseMode string) (int, error) {
	payload := map[string]interface{}{
		"chat_id":    chatID,
		"text":       text,
	}
	if parseMode != "" {
		payload["parse_mode"] = parseMode
	}
	jsonData, _ := json.Marshal(payload)

	resp, err := http.Post(fmt.Sprintf("https://api.telegram.org/bot%s/sendMessage", bot.Token), "application/json", bytes.NewBuffer(jsonData))
	if err != nil {
		return 0, fmt.Errorf("网络错误: %w", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	var result struct {
		Ok          bool   `json:"ok"`
		ErrorCode   int    `json:"error_code"`
		Description string `json:"description"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return 0, fmt.Errorf("解析响应失败: %v, 响应: %s", err, string(body))
	}

	if !result.Ok {
		return 0, fmt.Errorf("Telegram API 错误: code=%d, desc=%s", result.ErrorCode, result.Description)
	}

	resultMap := make(map[string]interface{})
	if err := json.Unmarshal(body, &resultMap); err != nil {
		return 0, fmt.Errorf("解析 result 失败: %v", err)
	}
	messageID := int(resultMap["result"].(map[string]interface{})["message_id"].(float64))
	return messageID, nil
}

// sendMessageWithKeyboard 发送带键盘消息
func (bm *BotManager) sendMessageWithKeyboard(bot *TelegramBot, chatID, text string, keyboard map[string]interface{}, parseMode string) (int, error) {
	payload := map[string]interface{}{
		"chat_id":      chatID,
		"text":         text,
		"reply_markup": keyboard,
	}
	if parseMode != "" {
		payload["parse_mode"] = parseMode
	}
	jsonData, _ := json.Marshal(payload)

	resp, err := http.Post(fmt.Sprintf("https://api.telegram.org/bot%s/sendMessage", bot.Token), "application/json", bytes.NewBuffer(jsonData))
	if err != nil {
		return 0, fmt.Errorf("网络错误: %w", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	var result struct {
		Ok          bool   `json:"ok"`
		ErrorCode   int    `json:"error_code"`
		Description string `json:"description"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return 0, fmt.Errorf("解析响应失败: %v, 响应: %s", err, string(body))
	}

	if !result.Ok {
		return 0, fmt.Errorf("Telegram API 错误: code=%d, desc=%s", result.ErrorCode, result.Description)
	}

	resultMap := make(map[string]interface{})
	if err := json.Unmarshal(body, &resultMap); err != nil {
		return 0, fmt.Errorf("解析 result 失败: %v", err)
	}
	messageID := int(resultMap["result"].(map[string]interface{})["message_id"].(float64))
	return messageID, nil
}

// DeleteMessage 删除消息
func (bm *BotManager) DeleteMessage(bot *TelegramBot, chatID string, messageID int) error {
	payload := map[string]interface{}{
		"chat_id":    chatID,
		"message_id": messageID,
	}
	jsonData, _ := json.Marshal(payload)
	_, err := http.Post(fmt.Sprintf("https://api.telegram.org/bot%s/deleteMessage", bot.Token), "application/json", bytes.NewBuffer(jsonData))
	return err
}

// Stop 停止
func (bm *BotManager) Stop() {
	close(bm.stopChan)
	logrus.Info(color.GreenString("k8s-approval BotManager 停止"))
}