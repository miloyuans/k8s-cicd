// 文件: bot.go (完整文件，优化: 支持多 Bot 独立轮询以捕获所有 Callback；在 pollPendingTasks 发送时设置 PopupBotName 并传入 MarkPopupSent；在 HandleCallback 从任务获取 PopupBotName 获取 Bot 用于 Delete/send；新增 startPollingForBot 和 pollUpdatesForBot，支持 per Bot offset)
// 文件: telegram/bot.go
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

// ---------- 结构体 ----------
type TelegramBot struct {
	Name         string              // 机器人名称
	Token        string              // Bot Token
	GroupID      string              // 群组ID
	Services     map[string][]string // 服务匹配规则
	RegexMatch   bool                // 是否使用正则匹配
	IsEnabled    bool                // 是否启用
	AllowedUsers []string            // 允许操作的用户
}

type BotManager struct {
	Bots          map[string]*TelegramBot
	globalAllowed []string
	mongo         *client.MongoClient
	botChannels   map[string]chan map[string]interface{} // 每个 Bot 独立 channel
	stopChan      chan struct{}
	offsets       map[string]int64
	mu            sync.Mutex
	sentTasks     map[string]bool // task_id + env → bool
	cfg           *config.Config
}

// ---------- 初始化 ----------
func NewBotManager(bots []config.TelegramBot, cfg *config.Config) *BotManager {
	bm := &BotManager{
		Bots:        make(map[string]*TelegramBot),
		botChannels: make(map[string]chan map[string]interface{}),
		stopChan:    make(chan struct{}),
		offsets:     make(map[string]int64),
		sentTasks:   make(map[string]bool),
		cfg:         cfg,
	}

	for i := range bots {
		if !bots[i].IsEnabled {
			continue
		}
		bot := &TelegramBot{
			Name:         bots[i].Name,
			Token:        bots[i].Token,
			GroupID:      bots[i].GroupID,
			Services:     bots[i].Services,
			RegexMatch:   bots[i].RegexMatch,
			IsEnabled:    true,
			AllowedUsers: bots[i].AllowedUsers,
		}
		bm.Bots[bot.Name] = bot
		bm.botChannels[bot.Name] = make(chan map[string]interface{}, 100)
		bm.offsets[bot.Name] = 0
		logrus.Infof(color.GreenString("Telegram机器人 [%s] 已启用"), bot.Name)
	}
	if len(bm.Bots) == 0 {
		logrus.Warn("未启用任何Telegram机器人")
	}
	logrus.Info(color.GreenString("BotManager 创建成功"))
	return bm
}

// ---------- 依赖注入 ----------
func (bm *BotManager) SetMongoClient(m *client.MongoClient) { bm.mongo = m }
func (bm *BotManager) SetGlobalAllowedUsers(u []string)      { bm.globalAllowed = u }

// ---------- 启动 ----------
func (bm *BotManager) Start() {
	logrus.Info(color.GreenString("启动 k8s-approval Telegram 服务"))

	// 每个 Bot 独立轮询 + 独立处理
	for _, bot := range bm.Bots {
		go bm.startPollingForBot(bot)   // 轮询
		go bm.handleUpdatesForBot(bot)  // 处理 callback
	}
	go bm.pollPendingTasks() // 统一发送弹窗（只负责匹配 Bot）
}

// ---------- 轮询（每个 Bot） ----------
func (bm *BotManager) startPollingForBot(bot *TelegramBot) {
	for {
		select {
		case <-bm.stopChan:
			logrus.WithFields(logrus.Fields{"bot": bot.Name}).
				Info(color.GreenString("轮询停止"))
			return
		default:
			bm.pollUpdatesForBot(bot)
		}
	}
}

func (bm *BotManager) pollUpdatesForBot(bot *TelegramBot) {
	bm.mu.Lock()
	offset := bm.offsets[bot.Name]
	bm.mu.Unlock()

	url := fmt.Sprintf("https://api.telegram.org/bot%s/getUpdates?offset=%d&timeout=10", bot.Token, offset)
	resp, err := http.Get(url)
	if err != nil {
		logrus.WithFields(logrus.Fields{"bot": bot.Name}).
			Errorf(color.RedString("轮询错误: %v"), err)
		time.Sleep(5 * time.Second)
		return
	}
	defer resp.Body.Close()

	var result struct {
		Ok     bool                       `json:"ok"`
		Result []map[string]interface{}   `json:"result"`
	}
	if json.NewDecoder(resp.Body).Decode(&result); err != nil {
		logrus.WithFields(logrus.Fields{"bot": bot.Name}).
			Errorf(color.RedString("解析响应失败: %v"), err)
		return
	}

	for _, upd := range result.Result {
		bm.mu.Lock()
		bm.offsets[bot.Name] = int64(upd["update_id"].(float64)) + 1
		bm.mu.Unlock()
		// 直接投递到该 Bot 专属 channel
		bm.botChannels[bot.Name] <- upd
	}
}

// ---------- 回调处理（每个 Bot） ----------
func (bm *BotManager) handleUpdatesForBot(bot *TelegramBot) {
	for upd := range bm.botChannels[bot.Name] {
		if cb, ok := upd["callback_query"].(map[string]interface{}); ok {
			bm.HandleCallback(bot, cb) // 传入 bot，彻底隔离
		}
	}
}

// ---------- 弹窗发送（统一入口） ----------
func (bm *BotManager) pollPendingTasks() {
	ticker := time.NewTicker(bm.cfg.API.QueryInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			bm.sendPendingPopups()
		}
	}
}

func (bm *BotManager) sendPendingPopups() {
	totalSent := 0
	for _, env := range bm.cfg.Query.ConfirmEnvs {
		tasks, err := bm.mongo.GetPendingTasks(env)
		if err != nil {
			logrus.Errorf("查询 %s 待确认任务失败: %v", env, err)
			continue
		}
		for i := range tasks {
			task := &tasks[i]
			if len(task.Environments) == 0 || task.Service == "" {
				continue
			}
			bot, err := bm.getBotForService(task.Service)
			if err != nil {
				// 警告使用默认 Bot
				bm.warnNoBot(task)
				continue
			}
			key := task.TaskID + "-" + env
			if bm.sentTasks[key] {
				continue
			}

			// 构建键盘
			keyboard := map[string]interface{}{
				"inline_keyboard": [][]map[string]string{
					{
						{"text": "确认部署", "callback_data": "confirm-" + task.TaskID},
						{"text": "拒绝部署", "callback_data": "reject-" + task.TaskID},
					},
				},
			}
			text := fmt.Sprintf("**部署审批请求**\n\n**服务**: %s\n**版本**: %s\n**环境**: %s\n**操作人**: %s\n**创建时间**: %s\n\n请在 24 小时内操作。",
				task.Service, task.Version, task.Environment, task.User,
				task.CreatedAt.Format("2006-01-02 15:04:05"))

			msgID, err := bm.sendMessageWithKeyboard(bot, bot.GroupID, text, keyboard, "Markdown")
			if err != nil {
				logrus.Errorf("发送弹窗失败 task=%s: %v", task.TaskID, err)
				continue
			}

			// 记录 Bot 名称
			task.PopupBotName = bot.Name
			task.PopupSent = true
			task.PopupMessageID = msgID
			task.PopupSentAt = time.Now()
			_ = bm.mongo.MarkPopupSent(task.TaskID, msgID, bot.Name)

			bm.sentTasks[key] = true
			totalSent++
			logrus.Infof("弹窗发送成功 bot=%s task=%s msgID=%d", bot.Name, task.TaskID, msgID)
		}
	}
	if totalSent > 0 {
		logrus.Infof("本轮发送 %d 个审批弹窗", totalSent)
	}
}

// ---------- 回调核心 ----------
func (bm *BotManager) HandleCallback(bot *TelegramBot, callback map[string]interface{}) {
	start := time.Now()

	id := callback["id"].(string)
	data := callback["data"].(string)
	user := callback["from"].(map[string]interface{})
	username := user["username"].(string)
	userID := int(user["id"].(float64))

	// 安全获取 chat_id（float64 → string）
	chat := callback["message"].(map[string]interface{})["chat"].(map[string]interface{})
	chatID := fmt.Sprintf("%d", int(chat["id"].(float64)))
	messageID := int(callback["message"].(map[string]interface{})["message_id"].(float64))

	parts := strings.Split(data, "-")
	if len(parts) != 2 {
		bm.answerCallback(bot, id, "无效操作")
		return
	}
	action, taskID := parts[0], parts[1]

	task, err := bm.mongo.GetTaskByID(taskID)
	if err != nil {
		bm.answerCallback(bot, id, "任务不存在")
		return
	}
	// 再次校验 Bot 名称（防御性）
	if task.PopupBotName != bot.Name {
		bm.answerCallback(bot, id, "机器人不匹配")
		return
	}
	if !bm.isUserAllowed(username, bot) {
		bm.answerCallback(bot, id, "无权限")
		return
	}

	var status, feedback string
	if action == "confirm" {
		status = "已确认"
		feedback = fmt.Sprintf("用户 %s 已批准部署:\n服务: %s\n版本: %s\n环境: %s\n任务ID: %s",
			username, task.Service, task.Version, task.Environment, taskID)
	} else if action == "reject" {
		status = "已拒绝"
		feedback = fmt.Sprintf("用户 %s 已拒绝部署:\n服务: %s\n版本: %s\n环境: %s\n任务ID: %s",
			username, task.Service, task.Version, task.Environment, taskID)
	} else {
		bm.answerCallback(bot, id, "无效操作")
		return
	}

	// 更新状态
	if err := bm.mongo.UpdateTaskStatus(taskID, status, username); err != nil {
		bm.answerCallback(bot, id, "状态更新失败")
		return
	}

	// 删除原弹窗
	_ = bm.DeleteMessage(bot, chatID, messageID)

	// 发送反馈
	_, _ = bm.sendMessage(bot, bot.GroupID, feedback, "")

	// 拒绝立即删除任务
	if action == "reject" {
		_ = bm.mongo.DeleteTask(taskID)
	}

	bm.answerCallback(bot, id, fmt.Sprintf("操作已执行: %s", action))

	logrus.WithFields(logrus.Fields{
		"bot":     bot.Name,
		"user":    username,
		"task_id": taskID,
		"action":  action,
		"took":    time.Since(start),
	}).Infof("回调处理完成")
}

// ---------- 辅助 ----------
func (bm *BotManager) answerCallback(bot *TelegramBot, id, text string) {
	payload := map[string]interface{}{
		"callback_query_id": id,
		"text":              text,
	}
	b, _ := json.Marshal(payload)
	_, _ = http.Post(fmt.Sprintf("https://api.telegram.org/bot%s/answerCallbackQuery", bot.Token),
		"application/json", bytes.NewBuffer(b))
}

func (bm *BotManager) warnNoBot(task *models.DeployRequest) {
	defaultBot := bm.getDefaultBot()
	if defaultBot == nil {
		return
	}
	msg := fmt.Sprintf("服务 %s 未匹配机器人，无法弹窗。TaskID: %s", task.Service, task.TaskID)
	_, _ = bm.sendMessage(defaultBot, defaultBot.GroupID, msg, "")
}

// ---------- 其余工具函数保持不变 ----------
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