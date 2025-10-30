// 文件: bot.go (完整文件，优化了 HandleCallback 函数：调整状态、反馈消息，并添加立即删除操作)
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
	"k8s-cicd/approval/models"

	"github.com/fatih/color"
	//"github.com/google/uuid"
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
	updateChan         chan map[string]interface{}
	stopChan           chan struct{}
	offset             int64
	mu                 sync.Mutex
	sentTasks          map[string]bool // task_id -> sent (内存防重)
	cfg                *config.Config
}

// NewBotManager 创建管理器
func NewBotManager(bots []config.TelegramBot, cfg *config.Config) *BotManager {
	m := &BotManager{
		Bots:       make(map[string]*TelegramBot),
		updateChan: make(chan map[string]interface{}, 100),
		stopChan:   make(chan struct{}),
		offset:     0,
		sentTasks:  make(map[string]bool),
		cfg:        cfg,
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
	go bm.startPolling()
	go bm.pollPendingTasks()
	go bm.handleUpdates() // 新增: 处理回调查询
}

// startPolling 启动 Telegram Updates 轮询
func (bm *BotManager) startPolling() {
	for {
		select {
		case <-bm.stopChan:
			logrus.Info(color.GreenString("Telegram 轮询停止"))
			return
		default:
			bm.pollUpdates()
		}
	}
}

// pollUpdates 轮询 Telegram Updates
func (bm *BotManager) pollUpdates() {
	bot := bm.getDefaultBot()
	if bot == nil {
		time.Sleep(5 * time.Second)
		return
	}

	url := fmt.Sprintf("https://api.telegram.org/bot%s/getUpdates?offset=%d&timeout=10", bot.Token, bm.offset)
	resp, err := http.Get(url)
	if err != nil {
		logrus.Errorf(color.RedString("Telegram 轮询错误: %v"), err)
		time.Sleep(5 * time.Second)
		return
	}
	defer resp.Body.Close()

	var result struct {
		Ok     bool                       `json:"ok"`
		Result []map[string]interface{}   `json:"result"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		logrus.Errorf(color.RedString("解析响应失败: %v"), err)
		return
	}

	for _, update := range result.Result {
		bm.offset = int64(update["update_id"].(float64)) + 1
		bm.updateChan <- update
	}
}

// 新增: contains 函数 (从 agent.go 复制，供 bot.go 使用)
func contains(slice []string, item string) bool {
	for _, s := range slice {
		if s == item {
			return true
		}
	}
	return false
}

// 修复: pollPendingTasks 增强触发逻辑，确保只在配置环境中触发，并打印用户信息相关日志
func (bm *BotManager) pollPendingTasks() {
	ticker := time.NewTicker(bm.cfg.API.QueryInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			logrus.WithFields(logrus.Fields{
				"time":   time.Now().Format("2006-01-02 15:04:05"),
				"method": "pollPendingTasks",
				"confirm_envs": bm.cfg.Query.ConfirmEnvs, // 打印配置环境
			}).Debug("=== 开始新一轮 pending 任务轮询 ===")

			totalSent := 0
			for _, env := range bm.cfg.Query.ConfirmEnvs { // 严格使用配置的 confirm_envs
				logrus.WithFields(logrus.Fields{
					"time":   time.Now().Format("2006-01-02 15:04:05"),
					"method": "pollPendingTasks",
					"env":    env,
				}).Debugf("查询 %s 环境的 pending 任务 (配置确认环境)", env)

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
				}).Infof("找到 %d 个待弹窗 pending 任务", len(tasks))

				for i := range tasks {
					task := &tasks[i]

					// 修复2: 检查任务环境是否在配置中（冗余检查）
					if !contains(bm.cfg.Query.ConfirmEnvs, task.Environments[0]) {
						logrus.WithFields(logrus.Fields{
							"time":   time.Now().Format("2006-01-02 15:04:05"),
							"method": "pollPendingTasks",
							"task_id": task.TaskID,
							"env":     task.Environments[0],
						}).Warnf("任务环境 %s 不在确认列表中，跳过弹窗", task.Environments[0])
						continue
					}

					bm.mu.Lock()
					if bm.sentTasks[task.TaskID] {
						logrus.WithFields(logrus.Fields{
							"time":   time.Now().Format("2006-01-02 15:04:05"),
							"method": "pollPendingTasks",
							"task_id": task.TaskID,
						}).Debugf("任务已发送弹窗，跳过: %s", task.TaskID)
						bm.mu.Unlock()
						continue
					}
					bm.sentTasks[task.TaskID] = true
					bm.mu.Unlock()

					logrus.WithFields(logrus.Fields{
						"time":   time.Now().Format("2006-01-02 15:04:05"),
						"method": "pollPendingTasks",
						"task_id": task.TaskID,
						"env":     task.Environments[0],
					}).Infof("准备发送弹窗: %s (环境: %s)", task.TaskID, task.Environments[0])

					// 获取 bot 并检查 allowed_users
					bot, err := bm.getBotForService(task.Service)
					if err != nil || len(bot.AllowedUsers) == 0 {
						logrus.WithFields(logrus.Fields{
							"time":   time.Now().Format("2006-01-02 15:04:05"),
							"method": "pollPendingTasks",
							"task_id": task.TaskID,
							"service": task.Service,
							"allowed_users": bot.AllowedUsers,
						}).Warnf("无匹配机器人或 allowed_users 为空，跳过弹窗: %v", err)
						bm.mu.Lock()
						delete(bm.sentTasks, task.TaskID)
						bm.mu.Unlock()
						continue
					}

					logrus.WithFields(logrus.Fields{
						"time":     time.Now().Format("2006-01-02 15:04:05"),
						"method":   "pollPendingTasks",
						"task_id":  task.TaskID,
						"bot":      bot.Name,
						"allowed_users": bot.AllowedUsers, // 打印用户信息
					}).Debugf("使用机器人 %s 发送弹窗，通知用户: %v", bot.Name, bot.AllowedUsers)

					startTime := time.Now()
					if err := bm.sendConfirmation(task); err != nil {
						logrus.WithFields(logrus.Fields{
							"time":     time.Now().Format("2006-01-02 15:04:05"),
							"method":   "pollPendingTasks",
							"task_id":  task.TaskID,
							"took":     time.Since(startTime),
						}).Errorf("弹窗发送失败: %v", err)

						bm.mu.Lock()
						delete(bm.sentTasks, task.TaskID)
						bm.mu.Unlock()
						continue
					}

					totalSent++
					logrus.WithFields(logrus.Fields{
						"time":     time.Now().Format("2006-01-02 15:04:05"),
						"method":   "pollPendingTasks",
						"task_id":  task.TaskID,
						"took":     time.Since(startTime),
						"allowed_users": bot.AllowedUsers,
					}).Infof("弹窗发送成功，通知用户: %v", bot.AllowedUsers)
				}
			}

			logrus.WithFields(logrus.Fields{
				"time":     time.Now().Format("2006-01-02 15:04:05"),
				"method":   "pollPendingTasks",
				"sent_count": totalSent,
				"confirm_envs": bm.cfg.Query.ConfirmEnvs,
			}).Debugf("本轮轮询发送 %d 个弹窗 (配置环境: %v)", totalSent, bm.cfg.Query.ConfirmEnvs)
		case <-bm.stopChan:
			return
		}
	}
}

// 修改: sendConfirmation 添加 @用户和完整日志
func (bm *BotManager) sendConfirmation(task *models.DeployRequest) error {
	startTime := time.Now()
	bot, err := bm.getBotForService(task.Service)
	if err != nil {
		logrus.WithFields(logrus.Fields{
			"time":    time.Now().Format("2006-01-02 15:04:05"),
			"method":  "sendConfirmation",
			"task_id": task.TaskID,
		}).Errorf("未找到匹配机器人: %v", err)
		return fmt.Errorf("未找到匹配机器人: %v", err)
	}

	logrus.WithFields(logrus.Fields{
		"time":    time.Now().Format("2006-01-02 15:04:05"),
		"method":  "sendConfirmation",
		"task_id": task.TaskID,
		"bot":     bot.Name,
		"allowed_users": bot.AllowedUsers, // 打印 allowed_users
	}).Debugf("匹配到机器人 %s, allowed_users: %v", bot.Name, bot.AllowedUsers)

	// 构建 @用户列表 (简化: 使用 username @，实际生产需获取 user_id 并用 <a href="tg://user?id=ID">@user</a>)
	var mentionText string
	if len(bot.AllowedUsers) > 0 {
		mentionText = fmt.Sprintf(" <b>@%s</b> 请审批！", strings.Join(bot.AllowedUsers, " "))
	} else {
		logrus.Warn("机器人无 allowed_users，弹窗不@任何人")
	}

	text := fmt.Sprintf(`
🔔 <b>部署审批请求</b>%s

<b>服务:</b> <code>%s</code>
<b>版本:</b> <code>%s</code>
<b>环境:</b> <code>%s</code>
<b>命名空间:</b> <code>%s</code>
<b>发起人:</b> <code>%s</code>
<b>任务ID:</b> <code>%s</code>

请在 <b>%v</b> 内确认部署：
    `, mentionText, task.Service, task.Version, task.Environments[0], task.Namespace, task.User, task.TaskID, bm.cfg.Telegram.ConfirmTimeout)

	keyboard := map[string]interface{}{
		"inline_keyboard": [][]map[string]interface{}{
			{
				{"text": "✅ 确认部署", "callback_data": fmt.Sprintf("confirm:%s", task.TaskID)},
				{"text": "❌ 拒绝部署", "callback_data": fmt.Sprintf("reject:%s", task.TaskID)},
			},
		},
	}

	logrus.WithFields(logrus.Fields{
		"time":    time.Now().Format("2006-01-02 15:04:05"),
		"method":  "sendConfirmation",
		"task_id": task.TaskID,
	}).Debugf("准备发送消息文本: %s", text) // 打印消息文本

	messageID, err := bm.sendMessageWithKeyboard(bot, bot.GroupID, text, keyboard, "HTML")
	if err != nil {
		logrus.WithFields(logrus.Fields{
			"time":    time.Now().Format("2006-01-02 15:04:05"),
			"method":  "sendConfirmation",
			"task_id": task.TaskID,
			"took":    time.Since(startTime),
		}).Errorf("发送弹窗失败: %v", err)
		return err
	}

	logrus.WithFields(logrus.Fields{
		"time":      time.Now().Format("2006-01-02 15:04:05"),
		"method":    "sendConfirmation",
		"task_id":   task.TaskID,
		"message_id": messageID,
		"mentions":  bot.AllowedUsers,
		"took":      time.Since(startTime),
	}).Infof("弹窗发送成功: @%s", strings.Join(bot.AllowedUsers, " "))

	// 标记已发送
	if err := bm.mongo.MarkPopupSent(task.TaskID, messageID); err != nil {
		logrus.WithFields(logrus.Fields{
			"time":    time.Now().Format("2006-01-02 15:04:05"),
			"method":  "sendConfirmation",
			"task_id": task.TaskID,
		}).Warnf("标记弹窗已发送失败: %v", err)
	}

	return nil
}

// 新增: handleUpdates 处理 updateChan 中的 callback_query
func (bm *BotManager) handleUpdates() {
	logrus.Info("启动 Telegram Updates 处理协程")
	for update := range bm.updateChan {
		if callback, ok := update["callback_query"].(map[string]interface{}); ok {
			go bm.HandleCallback(callback)
		}
		// 可选: 处理其他类型如 message
	}
}

// 新增: getBotForTask 根据任务获取 bot (简化: 用 getBotForService)
func (bm *BotManager) getBotForTask(taskID string) (*TelegramBot, error) {
	// 解析 taskID 获取 service (假设格式 service-version-env)
	parts := strings.Split(taskID, "-")
	if len(parts) < 1 {
		return nil, fmt.Errorf("无效 taskID")
	}
	return bm.getBotForService(parts[0])
}

// 新增辅助: answerCallback 响应回调
func (bm *BotManager) answerCallback(callbackID, text string) {
	bot := bm.getDefaultBot()
	if bot == nil {
		return
	}
	payload := map[string]interface{}{
		"callback_query_id": callbackID,
		"text":              text,
		"show_alert":        false,
	}
	jsonData, _ := json.Marshal(payload)
	http.Post(fmt.Sprintf("https://api.telegram.org/bot%s/answerCallbackQuery", bot.Token), "application/json", bytes.NewBuffer(jsonData))
}

// 修复: HandleCallback - 用户选择后立即删除原弹窗，发送反馈（不删除），并更新状态 使用中文状态更新
func (bm *BotManager) HandleCallback(data map[string]interface{}) {
	startTime := time.Now()
	id := data["id"].(string)
	callbackData := data["data"].(string)

	// 解析 callback_data: action:task_id
	parts := strings.Split(callbackData, ":")
	if len(parts) != 2 {
		bm.answerCallback(id, "无效操作")
		return
	}
	action, taskID := parts[0], parts[1]

	// 提取用户
	from := data["from"].(map[string]interface{})
	username := ""
	userID := 0
	if u, ok := from["username"].(string); ok && u != "" {
		username = u
	}
	if uid, ok := from["id"].(float64); ok {
		userID = int(uid)
	}

	// 获取 bot 并检查权限
	bot, err := bm.getBotForTask(taskID)
	if err != nil {
		bm.answerCallback(id, "机器人未找到")
		return
	}
	if !bm.isUserAllowed(username, bot) {
		bm.answerCallback(id, "无权限操作")
		logrus.WithFields(logrus.Fields{
			"time":    time.Now().Format("2006-01-02 15:04:05"),
			"method":  "HandleCallback",
			"user":    username,
			"user_id": userID,
			"action":  action,
			"task_id": taskID,
		}).Warnf("权限拒绝")
		return
	}

	// 获取任务完整数据
	task, err := bm.mongo.GetTaskByID(taskID)
	if err != nil {
		bm.answerCallback(id, "任务不存在")
		return
	}

	// 更新状态 - 使用中文状态
	status := "已确认"
	if action == "reject" {
		status = "已拒绝"
	}
	if err := bm.mongo.UpdateTaskStatus(taskID, status, username); err != nil {
		bm.answerCallback(id, "状态更新失败")
		return
	}

	// 立即删除原弹窗消息（使用 popup_message_id）
	if task.PopupMessageID > 0 {
		if err := bm.DeleteMessage(bot, bot.GroupID, task.PopupMessageID); err != nil {
			logrus.WithFields(logrus.Fields{
				"time":    time.Now().Format("2006-01-02 15:04:05"),
				"method":  "HandleCallback",
				"task_id": taskID,
				"message_id": task.PopupMessageID,
			}).Errorf("删除原弹窗失败: %v", err)
		} else {
			logrus.WithFields(logrus.Fields{
				"time":    time.Now().Format("2006-01-02 15:04:05"),
				"method":  "HandleCallback",
				"task_id": taskID,
				"message_id": task.PopupMessageID,
			}).Infof("原弹窗已立即删除")
		}
	} else {
		logrus.WithFields(logrus.Fields{
			"time":    time.Now().Format("2006-01-02 15:04:05"),
			"method":  "HandleCallback",
			"task_id": taskID,
		}).Warnf("无弹窗 message_id，无法删除")
	}

	// 发送结果反馈信息（不删除）
	chatID := bot.GroupID
	feedbackText := fmt.Sprintf("✅ <b>%s 部署请求</b>\n\n服务: <code>%s</code>\n版本: <code>%s</code>\n环境: <code>%s</code>\n操作人: <code>%s</code>\n任务ID: <code>%s</code>\n状态: <code>%s</code>", 
		strings.ToUpper(action), task.Service, task.Version, task.Environments[0], username, taskID, status)
	feedbackID, err := bm.sendMessage(bot, chatID, feedbackText, "HTML")
	if err != nil {
		logrus.WithFields(logrus.Fields{
			"time":    time.Now().Format("2006-01-02 15:04:05"),
			"method":  "HandleCallback",
			"task_id": taskID,
		}).Errorf("结果反馈发送失败: %v", err)
	} else {
		logrus.WithFields(logrus.Fields{
			"time":     time.Now().Format("2006-01-02 15:04:05"),
			"method":   "HandleCallback",
			"task_id":  taskID,
			"feedback_id": feedbackID,
		}).Infof("结果反馈已发送 (不删除): %s", feedbackText)
	}

	// 拒绝时立即删除任务 + 打印完整任务数据
	if action == "reject" {
		logrus.WithFields(logrus.Fields{
			"time":    time.Now().Format("2006-01-02 15:04:05"),
			"method":  "HandleCallback",
			"task_id": taskID,
			"full_task": fmt.Sprintf("%+v", task), // 完整任务数据日志
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

	bm.answerCallback(id, fmt.Sprintf("操作已执行: %s (弹窗已删除，反馈已发送，状态: %s)", action, status))

	logrus.WithFields(logrus.Fields{
		"time":   time.Now().Format("2006-01-02 15:04:05"),
		"method": "HandleCallback",
		"user":   username,
		"user_id": userID,
		"action": action,
		"task_id": taskID,
		"status": status,
		"took":   time.Since(startTime),
	}).Infof("回调处理完成: 原弹窗删除 + 反馈发送 (状态变更: %s)", status)
}

// 修改: isUserAllowed 支持机器人级权限
func (bm *BotManager) isUserAllowed(username string, bot *TelegramBot) bool {
	// 全局用户
	for _, u := range bm.globalAllowedUsers {
		if u == username {
			return true
		}
	}
	// 机器人级用户
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

// sendMessage 发送消息（增强错误解析）
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