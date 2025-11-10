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
	"k8s-cicd/approval/models" // 修复：导入 models 包

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
	updateChan         chan map[string]interface{}
	stopChan           chan struct{}
	offsets            map[string]int64 // per Bot offset
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
		offsets:    make(map[string]int64),
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

	for _, bot := range bm.Bots {
		go bm.startPollingForBot(bot)
	}

	go bm.pollPendingTasks()
	go bm.handleUpdates()
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
		bm.updateChan <- update
	}
}

// pollPendingTasks 轮询待确认任务并发送弹窗
func (bm *BotManager) pollPendingTasks() {
	ticker := time.NewTicker(bm.cfg.API.QueryInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			logrus.WithFields(logrus.Fields{
				"time":         time.Now().Format("2006-01-02 15:04:05"),
				"method":       "pollPendingTasks",
				"confirm_envs": bm.cfg.Query.ConfirmEnvs,
			}).Debug("=== 开始新一轮 pending 任务轮询 ===")

			totalSent := 0
			for _, env := range bm.cfg.Query.ConfirmEnvs {
				tasks, err := bm.mongo.GetPendingTasks(env)
				if err != nil {
					logrus.WithFields(logrus.Fields{
						"time": time.Now().Format("2006-01-02 15:04:05"),
						"method": "pollPendingTasks",
						"env":    env,
					}).Errorf("查询 %s 待确认任务失败: %v", env, err)
					continue
				}

				for _, task := range tasks {
					bm.mu.Lock()
					if bm.sentTasks[task.TaskID] {
						bm.mu.Unlock()
						continue
					}
					bm.mu.Unlock()

					bot, err := bm.getBotForService(task.Service)
					if err != nil {
						logrus.WithFields(logrus.Fields{
							"time":    time.Now().Format("2006-01-02 15:04:05"),
							"method":  "pollPendingTasks",
							"service": task.Service,
						}).Warnf("未匹配到机器人: %v", err)
						continue
					}

					bm.sendPendingTaskPopup(task, bot)
					totalSent++
				}
			}

			logrus.WithFields(logrus.Fields{
				"time":      time.Now().Format("2006-01-02 15:04:05"),
				"method":    "pollPendingTasks",
				"sent":      totalSent,
				"interval":  bm.cfg.API.QueryInterval,
			}).Infof("本轮发送 %d 个弹窗", totalSent)
		case <-bm.stopChan:
			return
		}
	}
}

// sendPendingTaskPopup 发送单个弹窗
func (bm *BotManager) sendPendingTaskPopup(task models.DeployRequest, bot *TelegramBot) {
	startTime := time.Now()
	chatID := bot.GroupID

	text := fmt.Sprintf(
		"部署审批请求\n\n服务: %s\n环境: %s\n版本: %s\n操作人: %s\n\n请确认部署",
		task.Service, task.Environment, task.Version, task.User,
	)

	keyboard := map[string]interface{}{
		"inline_keyboard": [][]map[string]string{
			{
				{"text": "批准", "callback_data": fmt.Sprintf("approve:%s", task.TaskID)},
				{"text": "拒绝", "callback_data": fmt.Sprintf("reject:%s", task.TaskID)},
			},
		},
	}

	messageID, err := bm.sendMessageWithKeyboard(bot, chatID, text, keyboard, "Markdown")
	if err != nil {
		logrus.WithFields(logrus.Fields{
			"time":    time.Now().Format("2006-01-02 15:04:05"),
			"method":  "sendPendingTaskPopup",
			"task_id": task.TaskID,
			"env":     task.Environment,
		}).Errorf("发送弹窗失败: %v", err)
		return
	}

	// 修复：补全 env 参数
	if err := bm.mongo.MarkPopupSent(task.TaskID, messageID, bot.Name, task.Environment); err != nil {
		logrus.WithFields(logrus.Fields{
			"time":    time.Now().Format("2006-01-02 15:04:05"),
			"method":  "sendPendingTaskPopup",
			"task_id": task.TaskID,
			"env":     task.Environment,
		}).Errorf("标记 popup_sent 失败: %v", err)
	}

	bm.mu.Lock()
	bm.sentTasks[task.TaskID] = true
	bm.mu.Unlock()

	logrus.WithFields(logrus.Fields{
		"time":       time.Now().Format("2006-01-02 15:04:05"),
		"method":     "sendPendingTaskPopup",
		"task_id":    task.TaskID,
		"env":        task.Environment,
		"message_id": messageID,
		"bot":        bot.Name,
		"took":       time.Since(startTime),
	}).Info("弹窗发送成功")
}

// handleUpdates 处理 Telegram 更新
func (bm *BotManager) handleUpdates() {
	for update := range bm.updateChan {
		if _, ok := update["callback_query"]; ok {
			bm.HandleCallback(update)
		}
	}
}

// HandleCallback 处理回调
func (bm *BotManager) HandleCallback(update map[string]interface{}) {
	startTime := time.Now()
	callbackQuery, ok := update["callback_query"].(map[string]interface{})
	if !ok {
		return
	}

	data, _ := callbackQuery["data"].(string)
	message, _ := callbackQuery["message"].(map[string]interface{})
	chat, _ := message["chat"].(map[string]interface{})
	chatID := fmt.Sprintf("%.0f", chat["id"].(float64))
	from, _ := callbackQuery["from"].(map[string]interface{})
	userID := fmt.Sprintf("%.0f", from["id"].(float64))
	username := ""
	if u, ok := from["username"].(string); ok {
		username = u
	}
	id := callbackQuery["id"].(string)

	parts := strings.SplitN(data, ":", 2)
	if len(parts) != 2 {
		bm.answerCallback(id, "无效操作")
		return
	}
	action := parts[0]
	taskID := parts[1]

	// 修复：使用 models.DeployRequest
	var task models.DeployRequest
	var env string
	var bot *TelegramBot
	found := false

	// 优先按 Bot 名查找（bot.Name 可能与 env 不一致，fallback 跨环境）
	for botName := range bm.Bots {
		t, err := bm.mongo.GetTaskByID(taskID, botName)
		if err == nil {
			task = *t
			env = task.Environment
			bot = bm.Bots[botName]
			found = true
			break
		}
	}
	if !found {
		// 跨环境搜索
		for envName := range bm.mongo.GetEnvMappings() {
			t, err := bm.mongo.GetTaskByID(taskID, envName)
			if err == nil {
				task = *t
				env = task.Environment
				bot = bm.getDefaultBot()
				found = true
				break
			}
		}
	}
	if !found {
		bm.answerCallback(id, "任务不存在或已过期")
		return
	}

	if !bm.isUserAllowed(username, bot) {
		bm.answerCallback(id, "无权限操作")
		return
	}

	status := "已确认"
	if action == "reject" {
		status = "已拒绝"
	}

	// 修复：补全 env 和 popup_sent
	popupSent := true
	if err := bm.mongo.UpdateTaskStatus(taskID, status, username, env, &popupSent); err != nil {
		bm.answerCallback(id, "操作失败")
		logrus.Errorf("更新任务状态失败: %v", err)
		return
	}

	if msg, ok := message["message_id"].(float64); ok {
		bm.DeleteMessage(bot, chatID, int(msg))
	}

	feedback := fmt.Sprintf("部署已%s\n\n服务: %s\n环境: %s\n版本: %s\n操作人: %s",
		status, task.Service, task.Environment, task.Version, task.User)
	bm.sendMessage(bot, chatID, feedback, "")

	bm.answerCallback(id, fmt.Sprintf("操作已执行: %s", status))

	logrus.WithFields(logrus.Fields{
		"time":    time.Now().Format("2006-01-02 15:04:05"),
		"method":  "HandleCallback",
		"user":    username,
		"user_id": userID,
		"action":  action,
		"task_id": taskID,
		"status":  status,
		"bot":     bot.Name,
		"took":    time.Since(startTime),
	}).Infof("回调处理完成")
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

// answerCallback 响应 Callback Query（新增实现，使用默认 Bot）
func (bm *BotManager) answerCallback(id, text string) {
	defaultBot := bm.getDefaultBot()
	if defaultBot == nil {
		logrus.Warn("无默认 Bot，无法响应 Callback")
		return
	}
	payload := map[string]interface{}{
		"callback_query_id": id,
		"text":              text,
	}
	jsonData, _ := json.Marshal(payload)
	resp, err := http.Post(fmt.Sprintf("https://api.telegram.org/bot%s/answerCallbackQuery", defaultBot.Token), "application/json", bytes.NewBuffer(jsonData))
	if err != nil {
		logrus.Errorf("响应 Callback 失败: %v", err)
	}
	defer resp.Body.Close()
}

// Stop 停止
func (bm *BotManager) Stop() {
	close(bm.stopChan)
	logrus.Info(color.GreenString("k8s-approval BotManager 停止"))
}