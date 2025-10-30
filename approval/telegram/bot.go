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
	"github.com/google/uuid"
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

// pollPendingTasks 周期性查询待弹窗任务（只发一次）
func (bm *BotManager) pollPendingTasks() {
	ticker := time.NewTicker(bm.cfg.API.QueryInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			for _, env := range bm.cfg.Query.ConfirmEnvs {
				tasks, err := bm.mongo.GetPendingTasks(env)
				if err != nil {
					logrus.Errorf("查询 %s 待确认任务失败: %v", env, err)
					continue
				}

				for i := range tasks {
					task := &tasks[i]

					bm.mu.Lock()
					if bm.sentTasks[task.TaskID] {
						bm.mu.Unlock()
						continue
					}
					bm.sentTasks[task.TaskID] = true
					bm.mu.Unlock()

					go func(t *models.DeployRequest) {
						startTime := time.Now()

						if err := bm.sendConfirmation(t); err != nil {
							logrus.WithFields(logrus.Fields{
								"time":     time.Now().Format("2006-01-02 15:04:05"),
								"method":   "pollPendingTasks",
								"task_id":  t.TaskID,
								"took":     time.Since(startTime),
							}).Errorf("弹窗发送失败: %v", err)

							bm.mu.Lock()
							delete(bm.sentTasks, t.TaskID)
							bm.mu.Unlock()
							return
						}

						if err := bm.mongo.MarkPopupSent(t.TaskID, 0); err != nil {
							logrus.Warnf("标记弹窗已发送失败: %v", err)
						}

						logrus.WithFields(logrus.Fields{
							"time":    time.Now().Format("2006-01-02 15:04:05"),
							"method":  "pollPendingTasks",
							"task_id": t.TaskID,
							"took":    time.Since(startTime),
						}).Infof("弹窗发送成功")
					}(task)
				}
			}
		case <-bm.stopChan:
			logrus.Info("停止查询待弹窗任务")
			return
		}
	}
}

// sendConfirmation 发送确认弹窗（安全模板）
func (bm *BotManager) sendConfirmation(task *models.DeployRequest) error {
	startTime := time.Now()
	env := task.Environments[0]

	bot, err := bm.getBotForService(task.Service)
	if err != nil {
		return err
	}

	text := fmt.Sprintf(
		"部署确认:\n服务: %s\n版本: %s\n环境: %s\n用户: %s\n时间: %s",
		task.Service, task.Version, env, task.User, task.CreatedAt.Format("2006-01-02 15:04:05"),
	)

	keyboard := map[string]interface{}{
		"inline_keyboard": [][]map[string]string{{
			{"text": "✅ 确认", "callback_data": fmt.Sprintf("confirm_%s", task.TaskID)},
			{"text": "❌ 拒绝", "callback_data": fmt.Sprintf("reject_%s", task.TaskID)},
		}},
	}

	messageID, err := bm.sendMessageWithKeyboard(bot, bot.GroupID, text, keyboard, "Markdown")
	if err != nil {
		return err
	}

	// 更新 messageID
	bm.mongo.MarkPopupSent(task.TaskID, messageID)

	logrus.WithFields(logrus.Fields{
		"time":    time.Now().Format("2006-01-02 15:04:05"),
		"method":  "sendConfirmation",
		"task_id": task.TaskID,
		"took":    time.Since(startTime),
	}).Infof("确认弹窗发送成功")
	return nil
}

// HandleCallback 处理回调（优化：调整状态和反馈消息，拒绝时立即删除）
func (bm *BotManager) HandleCallback(callback map[string]interface{}) {
	startTime := time.Now()

	user := callback["from"].(map[string]interface{})
	userName := user["username"].(string)

	if !bm.isUserAllowed(userName) {
		logrus.Warnf("未授权用户尝试操作: @%s", userName)
		return
	}

	data := callback["data"].(string)
	parts := strings.SplitN(data, "_", 2)
	if len(parts) != 2 {
		return
	}
	action, taskID := parts[0], parts[1]

	task, err := bm.mongo.GetTaskByID(taskID)
	if err != nil {
		logrus.Warnf("任务未找到 task_id=%s: %v", taskID, err)
		return
	}

	message := callback["message"].(map[string]interface{})
	messageID := int(message["message_id"].(float64))
	chatID := fmt.Sprintf("%v", message["chat"].(map[string]interface{})["id"])
	bot, _ := bm.getBotForService(task.Service)
	bm.DeleteMessage(bot, chatID, messageID)

	actionText := map[string]string{"confirm": "同意", "reject": "拒绝"}[action]
	queueText := map[string]string{"confirm": "执行", "reject": "删除"}[action]
	feedback := fmt.Sprintf("任务已经由用户 @%s %s %s v%s 在 %s 进入发布队列 %s中",
		userName, actionText, task.Service, task.Version, task.Environments[0], queueText)

	feedbackID, _ := bm.sendMessage(bot, chatID, feedback, "")
	go func() {
		time.Sleep(30 * time.Second)
		bm.DeleteMessage(bot, chatID, feedbackID)
	}()

	newStatus := "confirmed"
	if action == "reject" {
		newStatus = "rejected"
	}
	if err := bm.mongo.UpdateTaskStatus(taskID, newStatus); err != nil {
		logrus.Errorf("更新任务状态失败: %v", err)
	}

	// 拒绝时立即执行删除操作
	if action == "reject" {
		if err := bm.mongo.DeleteTask(taskID); err != nil {
			logrus.Errorf("立即删除任务失败: %v", err)
		}
	}

	logrus.WithFields(logrus.Fields{
		"time":   time.Now().Format("2006-01-02 15:04:05"),
		"method": "HandleCallback",
		"took":   time.Since(startTime),
	}).Infof("回调处理完成: %s task_id=%s", action, taskID)
}

// isUserAllowed 检查用户权限
func (bm *BotManager) isUserAllowed(username string) bool {
	for _, u := range bm.globalAllowedUsers {
		if u == username {
			return true
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