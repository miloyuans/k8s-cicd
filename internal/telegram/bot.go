package telegram

import (
	"encoding/json"
	"fmt"
	"k8s-cicd/internal/storage"
	"net/http"
	"sort"
	"strings"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"github.com/sirupsen/logrus"
)

// Bot 封装 Telegram 机器人功能
type Bot struct {
	bot        *tgbotapi.BotAPI
	storage    *storage.RedisStorage
	logger     *logrus.Logger
	httpClient *http.Client
}

// UserState 保存用户交互状态
type UserState struct {
	Step         int      // 当前交互步骤
	Service      string   // 选择的服务
	Environments []string // 选择的环境
	Version      string   // 输入的版本号
	ChatID       int64    // 用户聊天 ID
	UserID       int64    // 用户 ID（Telegram 用户 ID）
	Messages     []int    // 交互消息 ID 列表
	LastMsgID    int      // 最后用户消息 ID（用于回复）
}

// NewBot 初始化 Telegram 机器人
func NewBot(token string) (*Bot, error) {
	bot, err := tgbotapi.NewBotAPI(token)
	if err != nil {
		return nil, err
	}

	redisStorage, err := storage.NewRedisStorage("localhost:6379")
	if err != nil {
		return nil, err
	}

	logger := logrus.New()
	return &Bot{
		bot:        bot,
		storage:    redisStorage,
		logger:     logger,
		httpClient: &http.Client{Timeout: 10 * time.Second},
	}, nil
}

// Start 启动 Telegram 机器人，处理消息和回调
func (b *Bot) Start() {
	b.logger.Info("启动 Telegram 机器人")
	u := tgbotapi.NewUpdate(0)
	u.Timeout = 60

	updates := b.bot.GetUpdatesChan(u)
	for update := range updates {
		if update.Message != nil {
			b.handleMessage(update.Message)
		} else if update.CallbackQuery != nil {
			b.handleCallback(update.CallbackQuery)
		}
	}
}

// getServices 从 Redis 获取服务列表
func (b *Bot) getServices() ([]string, error) {
	data, err := b.storage.Get("services")
	if err != nil || data == "" {
		b.logger.Warn("从 Redis 获取服务列表失败，无数据")
		return nil, fmt.Errorf("服务列表未初始化，请等待外部服务推送")
	}
	var services []string
	if err := json.Unmarshal([]byte(data), &services); err != nil {
		b.logger.Errorf("解析服务列表失败: %v", err)
		return nil, err
	}
	return services, nil
}

// getEnvironments 从 Redis 获取环境列表
func (b *Bot) getEnvironments() ([]string, error) {
	data, err := b.storage.Get("environments")
	if err != nil || data == "" {
		b.logger.Warn("从 Redis 获取环境列表失败，无数据")
		return nil, fmt.Errorf("环境列表未初始化，请等待外部服务推送")
	}
	var envs []string
	if err := json.Unmarshal([]byte(data), &envs); err != nil {
		b.logger.Errorf("解析环境列表失败: %v", err)
		return nil, err
	}
	return envs, nil
}

// handleMessage 处理用户文本输入
func (b *Bot) handleMessage(msg *tgbotapi.Message) {
	chatID := msg.Chat.ID
	b.logger.Infof("收到用户 %d 的消息: %s", chatID, msg.Text)

	// 检查是否输入 "ai" 触发交互
	if strings.ToLower(msg.Text) == "ai" {
		b.logger.Infof("用户 %d 触发交互，输入: %s", chatID, msg.Text)
		b.startInteraction(chatID, msg.From.ID)
		return
	}

	// 获取用户状态，仅处理交互相关消息
	state, err := b.getState(fmt.Sprintf("user:%d", chatID))
	if err != nil {
		b.logger.Warnf("用户 %d 无交互状态，忽略消息: %s", chatID, msg.Text)
		return
	}

	switch state.Step {
	case 3: // 输入版本号
		b.logger.Infof("用户 %d 输入版本号: %s", chatID, msg.Text)
		if b.isVersionUnique(state.Service, msg.Text) {
			state.Version = msg.Text
			state.Step = 4
			state.LastMsgID = msg.MessageID // 记录版本输入消息 ID
			b.SaveState(fmt.Sprintf("user:%d", chatID), state)
			b.showConfirmation(chatID, &state)
		} else {
			b.logger.Warnf("用户 %d 输入的版本号 %s 在服务 %s 下已存在", chatID, msg.Text, state.Service)
			b.SendMessage(chatID, fmt.Sprintf("此服务 %s 下版本号 %s 已存在，请输入新版本号：", state.Service, msg.Text), nil)
		}
	default:
		b.logger.Warnf("用户 %d 的消息 %s 非交互相关，忽略", chatID, msg.Text)
	}
}

// startInteraction 开始交互流程，显示服务选择弹窗
func (b *Bot) startInteraction(chatID, userID int64) {
	b.logger.Infof("用户 %d (UserID: %d) 开始交互流程", chatID, userID)
	// 初始化用户状态
	state := UserState{
		Step:   1,
		ChatID: chatID,
		UserID: userID,
	}
	b.SaveState(fmt.Sprintf("user:%d", chatID), state)

	// 服务选择弹窗
	b.showServiceSelection(chatID, &state)
}

// showServiceSelection 显示服务选择弹窗
func (b *Bot) showServiceSelection(chatID int64, state *UserState) {
	services, err := b.getServices()
	if err != nil {
		b.logger.Warnf("用户 %d 获取服务列表失败: %v", chatID, err)
		b.SendMessage(chatID, err.Error(), nil)
		b.deleteState(fmt.Sprintf("user:%d", chatID))
		return
	}

	cols := 3
	if len(services) < cols {
		cols = len(services)
	}
	var buttons [][]tgbotapi.InlineKeyboardButton
	var row []tgbotapi.InlineKeyboardButton
	for i, svc := range services {
		displayText := svc
		if state.Service == svc {
			displayText = fmt.Sprintf("<b>✅ %s</b>", svc)
		}
		row = append(row, tgbotapi.NewInlineKeyboardButtonData(displayText, fmt.Sprintf("service:%s", svc)))
		if len(row) == cols || i == len(services)-1 {
			buttons = append(buttons, row)
			row = []tgbotapi.InlineKeyboardButton{}
		}
	}
	buttons = append(buttons, []tgbotapi.InlineKeyboardButton{
		tgbotapi.NewInlineKeyboardButtonData("下一步 / Next", "next_service"),
		tgbotapi.NewInlineKeyboardButtonData("取消 / Cancel", "cancel"),
	})
	keyboard := tgbotapi.NewInlineKeyboardMarkup(buttons...)
	msgText := fmt.Sprintf("请选择服务（单选）:\n当前选中: %s", state.Service)
	var sentMsg tgbotapi.Message
	if len(state.Messages) > 0 {
		lastMsgID := state.Messages[len(state.Messages)-1]
		editMsg := tgbotapi.NewEditMessageText(chatID, lastMsgID, msgText)
		editMsg.ReplyMarkup = &keyboard
		editMsg.ParseMode = "HTML"
		var err error
		sentMsg, err = b.bot.Send(editMsg)
		if err != nil {
			b.logger.Errorf("编辑服务选择消息失败，用户 %d: %v", chatID, err)
		} else {
			state.Messages[len(state.Messages)-1] = sentMsg.MessageID
			b.SaveState(fmt.Sprintf("user:%d", chatID), state)
		}
	} else {
		msg := tgbotapi.NewMessage(chatID, msgText)
		msg.ReplyMarkup = keyboard
		msg.ParseMode = "HTML"
		var err error
		sentMsg, err = b.bot.Send(msg)
		if err != nil {
			b.logger.Errorf("发送服务选择消息失败，用户 %d: %v", chatID, err)
		} else {
			state.Messages = append(state.Messages, sentMsg.MessageID)
			b.SaveState(fmt.Sprintf("user:%d", chatID), state)
		}
	}
	b.logger.Infof("用户 %d 显示服务选择弹窗，消息 ID: %d", chatID, sentMsg.MessageID)
}

// showEnvironmentSelection 显示环境选择弹窗
func (b *Bot) showEnvironmentSelection(chatID int64, state *UserState) {
	environments, err := b.getEnvironments()
	if err != nil {
		b.logger.Warnf("用户 %d 获取环境列表失败: %v", chatID, err)
		b.SendMessage(chatID, err.Error(), nil)
		b.deleteState(fmt.Sprintf("user:%d", chatID))
		return
	}

	cols := 3
	if len(environments) < cols {
		cols = len(environments)
	}
	var buttons [][]tgbotapi.InlineKeyboardButton
	var row []tgbotapi.InlineKeyboardButton
	for i, env := range environments {
		displayText := env
		if contains(state.Environments, env) {
			displayText = fmt.Sprintf("<b>✅ %s</b>", env)
		}
		row = append(row, tgbotapi.NewInlineKeyboardButtonData(displayText, fmt.Sprintf("env:%s", env)))
		if len(row) == cols || i == len(environments)-1 {
			buttons = append(buttons, row)
			row = []tgbotapi.InlineKeyboardButton{}
		}
	}
	buttons = append(buttons, []tgbotapi.InlineKeyboardButton{
		tgbotapi.NewInlineKeyboardButtonData("下一步 / Next", "next_env"),
		tgbotapi.NewInlineKeyboardButtonData("取消 / Cancel", "cancel"),
	})
	keyboard := tgbotapi.NewInlineKeyboardMarkup(buttons...)
	msgText := fmt.Sprintf("请选择环境（可多选）:\n当前选中: %s", strings.Join(state.Environments, ", "))
	var sentMsg tgbotapi.Message
	if len(state.Messages) > 0 {
		lastMsgID := state.Messages[len(state.Messages)-1]
		editMsg := tgbotapi.NewEditMessageText(chatID, lastMsgID, msgText)
		editMsg.ReplyMarkup = &keyboard
		editMsg.ParseMode = "HTML"
		var err error
		sentMsg, err = b.bot.Send(editMsg)
		if err != nil {
			b.logger.Errorf("编辑环境选择消息失败，用户 %d: %v", chatID, err)
		} else {
			state.Messages[len(state.Messages)-1] = sentMsg.MessageID
			b.SaveState(fmt.Sprintf("user:%d", chatID), state)
		}
	} else {
		msg := tgbotapi.NewMessage(chatID, msgText)
		msg.ReplyMarkup = keyboard
		msg.ParseMode = "HTML"
		var err error
		sentMsg, err = b.bot.Send(msg)
		if err != nil {
			b.logger.Errorf("发送环境选择消息失败，用户 %d: %v", chatID, err)
		} else {
			state.Messages = append(state.Messages, sentMsg.MessageID)
			b.SaveState(fmt.Sprintf("user:%d", chatID), state)
		}
	}
	b.logger.Infof("用户 %d 显示环境选择弹窗，消息 ID: %d", chatID, sentMsg.MessageID)
}

// showConfirmation 显示数据确认弹窗
func (b *Bot) showConfirmation(chatID int64, state *UserState) {
	b.logger.Infof("用户 %d 显示确认弹窗: 服务=%s, 环境=%v, 版本=%s", chatID, state.Service, state.Environments, state.Version)
	msgText := fmt.Sprintf("请确认以下信息：\n服务: %s\n环境: %s\n版本: %s", state.Service, strings.Join(state.Environments, ", "), state.Version)
	keyboard := b.CreateYesNoKeyboard("confirm")
	var sentMsg tgbotapi.Message
	if len(state.Messages) > 0 {
		lastMsgID := state.Messages[len(state.Messages)-1]
		editMsg := tgbotapi.NewEditMessageText(chatID, lastMsgID, msgText)
		editMsg.ReplyMarkup = &keyboard
		editMsg.ReplyToMessageID = state.LastMsgID // 回复版本输入消息
		var err error
		sentMsg, err = b.bot.Send(editMsg)
		if err != nil {
			b.logger.Errorf("编辑确认消息失败，用户 %d: %v", chatID, err)
			// 回退到发送新消息
			msg := tgbotapi.NewMessage(chatID, msgText)
			msg.ReplyMarkup = keyboard
			msg.ReplyToMessageID = state.LastMsgID
			sentMsg, err = b.bot.Send(msg)
			if err != nil {
				b.logger.Errorf("发送确认消息失败，用户 %d: %v", chatID, err)
			} else {
				state.Messages = append(state.Messages, sentMsg.MessageID)
				b.SaveState(fmt.Sprintf("user:%d", chatID), state)
			}
		} else {
			state.Messages[len(state.Messages)-1] = sentMsg.MessageID
			b.SaveState(fmt.Sprintf("user:%d", chatID), state)
		}
	} else {
		msg := tgbotapi.NewMessage(chatID, msgText)
		msg.ReplyMarkup = keyboard
		msg.ReplyToMessageID = state.LastMsgID // 回复版本输入消息
		var err error
		sentMsg, err = b.bot.Send(msg)
		if err != nil {
			b.logger.Errorf("发送确认消息失败，用户 %d: %v", chatID, err)
		} else {
			state.Messages = append(state.Messages, sentMsg.MessageID)
			b.SaveState(fmt.Sprintf("user:%d", chatID), state)
		}
	}
}

// askContinue 询问是否继续发布
func (b *Bot) askContinue(chatID int64, state *UserState) {
	b.logger.Infof("用户 %d 显示是否继续提交弹窗", chatID)
	keyboard := b.CreateYesNoKeyboard("restart")
	msgText := "是否继续提交数据？"
	var sentMsg tgbotapi.Message
	if len(state.Messages) > 0 {
		lastMsgID := state.Messages[len(state.Messages)-1]
		editMsg := tgbotapi.NewEditMessageText(chatID, lastMsgID, msgText)
		editMsg.ReplyMarkup = &keyboard
		var err error
		sentMsg, err = b.bot.Send(editMsg)
		if err != nil {
			b.logger.Errorf("编辑继续提交消息失败，用户 %d: %v", chatID, err)
		} else {
			state.Messages[len(state.Messages)-1] = sentMsg.MessageID
			b.SaveState(fmt.Sprintf("user:%d", chatID), state)
		}
	} else {
		msg := tgbotapi.NewMessage(chatID, msgText)
		msg.ReplyMarkup = keyboard
		var err error
		sentMsg, err = b.bot.Send(msg)
		if err != nil {
			b.logger.Errorf("发送继续提交消息失败，用户 %d: %v", chatID, err)
		} else {
			state.Messages = append(state.Messages, sentMsg.MessageID)
			b.SaveState(fmt.Sprintf("user:%d", chatID), state)
		}
	}
}

// deleteMessages 删除所有交互消息
func (b *Bot) deleteMessages(chatID int64, state *UserState) {
	for _, msgID := range state.Messages {
		deleteMsg := tgbotapi.NewDeleteMessage(chatID, msgID)
		if _, err := b.bot.Request(deleteMsg); err != nil {
			b.logger.Errorf("删除消息 %d 失败，用户 %d: %v", msgID, chatID, err)
		} else {
			b.logger.Infof("删除消息 %d 成功，用户 %d", msgID, chatID)
		}
	}
	state.Messages = nil
	b.SaveState(fmt.Sprintf("user:%d", chatID), *state)
}

// handleCallback 处理回调查询
func (b *Bot) handleCallback(query *tgbotapi.CallbackQuery) {
	chatID := query.Message.Chat.ID
	data := query.Data
	b.logger.Infof("用户 %d 触发回调: %s", chatID, data)

	// 尝试从交互状态获取
	state, err := b.getState(fmt.Sprintf("user:%d", chatID))
	if err != nil {
		// 尝试从部署状态获取
		deployState, err := b.getState(fmt.Sprintf("deploy:%d", chatID))
		if err == nil {
			b.handleDeployCallback(query, &deployState)
			return
		}
		b.logger.Errorf("获取用户 %d 状态失败: %v", chatID, err)
		return
	}

	switch state.Step {
	case 1: // 服务选择
		if strings.HasPrefix(data, "service:") {
			state.Service = strings.TrimPrefix(data, "service:")
			b.logger.Infof("用户 %d 选择服务: %s", chatID, state.Service)
			b.SaveState(fmt.Sprintf("user:%d", chatID), state)
			b.showServiceSelection(chatID, &state)
		} else if data == "next_service" {
			if state.Service == "" {
				b.logger.Warnf("用户 %d 未选择服务", chatID)
				b.SendMessage(chatID, "请先选择一个服务！", nil)
			} else {
				b.logger.Infof("用户 %d 确认服务选择，继续到环境选择", chatID)
				state.Step = 2
				b.SaveState(fmt.Sprintf("user:%d", chatID), state)
				b.showEnvironmentSelection(chatID, &state)
			}
		} else if data == "cancel" {
			b.logger.Infof("用户 %d 取消服务选择，会话关闭", chatID)
			b.deleteMessages(chatID, &state)
			b.SendMessage(chatID, "会话已关闭", nil)
			b.deleteState(fmt.Sprintf("user:%d", chatID))
		}
	case 2: // 环境选择
		if strings.HasPrefix(data, "env:") {
			env := strings.TrimPrefix(data, "env:")
			if !contains(state.Environments, env) {
				state.Environments = append(state.Environments, env)
				b.logger.Infof("用户 %d 选择环境: %s", chatID, env)
				b.SaveState(fmt.Sprintf("user:%d", chatID), state)
			}
			b.showEnvironmentSelection(chatID, &state)
		} else if data == "next_env" {
			if len(state.Environments) == 0 {
				b.logger.Warnf("用户 %d 未选择任何环境", chatID)
				b.SendMessage(chatID, "请至少选择一个环境！", nil)
			} else {
				b.logger.Infof("用户 %d 完成环境选择: %v", chatID, state.Environments)
				state.Step = 3
				b.SaveState(fmt.Sprintf("user:%d", chatID), state)
				b.SendMessage(chatID, "请输入版本号：", nil)
			}
		} else if data == "cancel" {
			b.logger.Infof("用户 %d 取消环境选择，会话关闭", chatID)
			b.deleteMessages(chatID, &state)
			b.SendMessage(chatID, "会话已关闭", nil)
			b.deleteState(fmt.Sprintf("user:%d", chatID))
		}
	case 4: // 确认提交
		if data == "confirm_yes" {
			b.logger.Infof("用户 %d 确认提交数据: 服务=%s, 环境=%v, 版本=%s", chatID, state.Service, state.Environments, state.Version)
			b.persistData(state)
			b.deleteMessages(chatID, &state)
			b.SendMessage(chatID, "数据提交成功！", nil)
			state.Step = 5
			b.SaveState(fmt.Sprintf("user:%d", chatID), state)
			b.askContinue(chatID, &state)
		} else if data == "confirm_no" {
			b.logger.Infof("用户 %d 取消数据提交，会话关闭", chatID)
			b.deleteMessages(chatID, &state)
			b.SendMessage(chatID, "会话已关闭", nil)
			b.deleteState(fmt.Sprintf("user:%d", chatID))
		}
	case 5: // 是否继续
		if data == "restart_yes" {
			b.logger.Infof("用户 %d 选择继续提交，重新开始交互", chatID)
			b.deleteMessages(chatID, &state)
			b.startInteraction(chatID, state.UserID)
		} else if data == "restart_no" {
			b.logger.Infof("用户 %d 选择结束交互，会话关闭", chatID)
			b.deleteMessages(chatID, &state)
			b.SendMessage(chatID, "会话已关闭", nil)
			b.deleteState(fmt.Sprintf("user:%d", chatID))
		}
	}

	b.bot.Request(tgbotapi.NewCallback(query.ID, ""))
}

// handleDeployCallback 处理 /deploy 接口的确认回调
func (b *Bot) handleDeployCallback(query *tgbotapi.CallbackQuery, state *UserState) {
	chatID := query.Message.Chat.ID
	data := query.Data
	b.logger.Infof("用户 %d 触发部署回调: %s", chatID, data)

	if data == "deploy_confirm_yes" {
		b.logger.Infof("用户 %d 确认部署数据: 服务=%s, 环境=%v, 版本=%s", chatID, state.Service, state.Environments, state.Version)
		b.persistData(*state)
		b.deleteMessages(chatID, state)
		b.SendMessage(chatID, "部署数据提交成功！", nil)
		b.deleteState(fmt.Sprintf("deploy:%d", chatID))
	} else if data == "deploy_confirm_no" {
		b.logger.Infof("用户 %d 取消部署数据提交", chatID)
		b.deleteMessages(chatID, state)
		b.SendMessage(chatID, "部署请求已取消", nil)
		b.deleteState(fmt.Sprintf("deploy:%d", chatID))
	}

	b.bot.Request(tgbotapi.NewCallback(query.ID, ""))
}

// CreateYesNoKeyboard 创建是/否键盘
func (b *Bot) CreateYesNoKeyboard(prefix string) tgbotapi.InlineKeyboardMarkup {
	return tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("是 / Yes", prefix+"_yes"),
			tgbotapi.NewInlineKeyboardButtonData("否 / No", prefix+"_no"),
		),
	)
}

// SendMessage 发送 Telegram 消息
func (b *Bot) SendMessage(chatID int64, text string, keyboard *tgbotapi.InlineKeyboardMarkup) {
	b.logger.Infof("发送消息给用户 %d: %s", chatID, text)
	msg := tgbotapi.NewMessage(chatID, text)
	if keyboard != nil {
		msg.ReplyMarkup = *keyboard
	}
	if _, err := b.bot.Send(msg); err != nil {
		b.logger.Errorf("发送消息给用户 %d 失败: %v", chatID, err)
	}
}

// SaveState 保存用户状态到 Redis
func (b *Bot) SaveState(key string, state UserState) {
	data, _ := json.Marshal(state)
	b.storage.Set(key, string(data))
	b.logger.Infof("保存用户状态 %s: %v", key, state)
}

// getState 从 Redis 获取用户状态
func (b *Bot) getState(key string) (UserState, error) {
	data, err := b.storage.Get(key)
	if err != nil {
		b.logger.Errorf("获取状态 %s 失败: %v", key, err)
		return UserState{}, err
	}
	var state UserState
	if err := json.Unmarshal([]byte(data), &state); err != nil {
		b.logger.Errorf("解析状态 %s 失败: %v", key, err)
		return UserState{}, err
	}
	return state, nil
}

// deleteState 删除用户状态
func (b *Bot) deleteState(key string) {
	b.storage.Delete(key)
	b.logger.Infof("删除状态 %s", key)
}

// persistData 持久化数据到 Redis 队列
func (b *Bot) persistData(state UserState) {
	data, _ := json.Marshal(state)
	b.storage.Push("deploy_queue", string(data))
	b.logger.Infof("持久化用户 %d 数据到队列: 服务=%s, 环境=%v, 版本=%s", state.ChatID, state.Service, state.Environments, state.Version)
}

// isVersionUnique 检查版本号在同一服务下是否唯一
func (b *Bot) isVersionUnique(service, version string) bool {
	items, err := b.storage.List("deploy_queue")
	if err != nil {
		b.logger.Errorf("检查版本号唯一性失败: %v", err)
		return true // 假设错误时允许继续
	}
	for _, item := range items {
		var state UserState
		if err := json.Unmarshal([]byte(item), &state); err != nil {
			b.logger.Errorf("解析队列数据失败: %v", err)
			continue
		}
		if state.Service == service && state.Version == version {
			b.logger.Warnf("服务 %s 下版本号 %s 已存在", service, version)
			return false
		}
	}
	return true
}

// contains 检查字符串是否在切片中
func contains(slice []string, item string) bool {
	for _, s := range slice {
		if s == item {
			return true
		}
	}
	return false
}