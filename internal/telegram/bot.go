package telegram

import (
	"context"
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
	bot          *tgbotapi.BotAPI
	storage      *storage.RedisStorage
	logger       *logrus.Logger
	httpClient   *http.Client
	servicesAPI  string // 服务 API 地址
	environsAPI  string // 环境 API 地址
}

// UserState 保存用户交互状态
type UserState struct {
	Step         int      // 当前交互步骤
	Service      string   // 选择的服务
	Environments []string // 选择的环境
	Version      string   // 输入的版本号
	ChatID       int64    // 用户聊天 ID
	Messages     []int    // 交互消息 ID 列表
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
		bot:         bot,
		storage:     redisStorage,
		logger:      logger,
		httpClient:  &http.Client{Timeout: 10 * time.Second},
		servicesAPI: "http://external-api/services",   // 需替换为实际地址
		environsAPI: "http://external-api/environments", // 需替换为实际地址
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

// fetchServices 从外部 API 获取服务列表并去重存储
func (b *Bot) fetchServices() ([]string, error) {
	resp, err := b.httpClient.Get(b.servicesAPI)
	if err != nil {
		b.logger.Errorf("获取服务列表失败: %v", err)
		return nil, err
	}
	defer resp.Body.Close()

	var services []string
	if err := json.NewDecoder(resp.Body).Decode(&services); err != nil {
		b.logger.Errorf("解析服务列表失败: %v", err)
		return nil, err
	}

	// 去重
	uniqueServices := make(map[string]bool)
	for _, svc := range services {
		uniqueServices[strings.ToUpper(svc)] = true
	}
	result := make([]string, 0, len(uniqueServices))
	for svc := range uniqueServices {
		result = append(result, svc)
	}
	sort.Strings(result)

	// 存储到 Redis
	data, _ := json.Marshal(result)
	b.storage.Set("services", string(data))
	b.logger.Infof("存储服务列表: %v", result)
	return result, nil
}

// fetchEnvironments 从外部 API 获取环境列表并去重存储
func (b *Bot) fetchEnvironments() ([]string, error) {
	resp, err := b.httpClient.Get(b.environsAPI)
	if err != nil {
		b.logger.Errorf("获取环境列表失败: %v", err)
		return nil, err
	}
	defer resp.Body.Close()

	var envs []string
	if err := json.NewDecoder(resp.Body).Decode(&envs); err != nil {
		b.logger.Errorf("解析环境列表失败: %v", err)
		return nil, err
	}

	// 去重
	uniqueEnvs := make(map[string]bool)
	for _, env := range envs {
		uniqueEnvs[strings.ToUpper(env)] = true
	}
	result := make([]string, 0, len(uniqueEnvs))
	for env := range uniqueEnvs {
		result = append(result, env)
	}
	sort.Strings(result)

	// 存储到 Redis
	data, _ := json.Marshal(result)
	b.storage.Set("environments", string(data))
	b.logger.Infof("存储环境列表: %v", result)
	return result, nil
}

// getServices 从 Redis 获取服务列表，失败则从 API 刷新
func (b *Bot) getServices() []string {
	data, err := b.storage.Get("services")
	if err != nil || data == "" {
		b.logger.Warn("从 Redis 获取服务列表失败，尝试从 API 刷新")
		services, err := b.fetchServices()
		if err != nil {
			b.logger.Errorf("刷新服务列表失败: %v", err)
			return []string{"ServiceA", "ServiceB", "ServiceC"} // 回退默认值
		}
		return services
	}
	var services []string
	json.Unmarshal([]byte(data), &services)
	return services
}

// getEnvironments 从 Redis 获取环境列表，失败则从 API 刷新
func (b *Bot) getEnvironments() []string {
	data, err := b.storage.Get("environments")
	if err != nil || data == "" {
		b.logger.Warn("从 Redis 获取环境列表失败，尝试从 API 刷新")
		envs, err := b.fetchEnvironments()
		if err != nil {
			b.logger.Errorf("刷新环境列表失败: %v", err)
			return []string{"Dev", "Test", "Prod"} // 回退默认值
		}
		return envs
	}
	var envs []string
	json.Unmarshal([]byte(data), &envs)
	return envs
}

// handleMessage 处理用户文本输入
func (b *Bot) handleMessage(msg *tgbotapi.Message) {
	chatID := msg.Chat.ID
	b.logger.Infof("收到用户 %d 的消息: %s", chatID, msg.Text)

	// 检查是否输入 "ai" 触发交互
	if strings.ToLower(msg.Text) == "ai" {
		b.logger.Infof("用户 %d 触发交互，输入: %s", chatID, msg.Text)
		b.startInteraction(chatID)
		return
	}

	// 获取用户状态，仅处理交互相关消息
	state, err := b.getState(chatID)
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
			b.SaveState(chatID, state)
			b.showConfirmation(chatID, state)
		} else {
			b.logger.Warnf("用户 %d 输入的版本号 %s 在服务 %s 下已存在", chatID, msg.Text, state.Service)
			b.SendMessage(chatID, fmt.Sprintf("此服务 %s 下版本号 %s 已存在，请输入新版本号：", state.Service, msg.Text), nil)
		}
	default:
		b.logger.Warnf("用户 %d 的消息 %s 非交互相关，忽略", chatID, msg.Text)
	}
}

// startInteraction 开始交互流程，显示服务选择弹窗
func (b *Bot) startInteraction(chatID int64) {
	b.logger.Infof("用户 %d 开始交互流程", chatID)
	// 初始化用户状态
	state := UserState{
		Step:   1,
		ChatID: chatID,
	}
	b.SaveState(chatID, state)

	// 服务选择弹窗
	b.showServiceSelection(chatID, &state)
}

// showServiceSelection 显示服务选择弹窗
func (b *Bot) showServiceSelection(chatID int64, state *UserState) {
	services := b.getServices()
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
			b.SaveState(chatID, *state)
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
			b.SaveState(chatID, *state)
		}
	}
	b.logger.Infof("用户 %d 显示服务选择弹窗，消息 ID: %d", chatID, sentMsg.MessageID)
}

// showEnvironmentSelection 显示环境选择弹窗
func (b *Bot) showEnvironmentSelection(chatID int64, state *UserState) {
	environments := b.getEnvironments()
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
			b.SaveState(chatID, *state)
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
			b.SaveState(chatID, *state)
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
		var err error
		sentMsg, err = b.bot.Send(editMsg)
		if err != nil {
			b.logger.Errorf("编辑确认消息失败，用户 %d: %v", chatID, err)
		} else {
			state.Messages[len(state.Messages)-1] = sentMsg.MessageID
			b.SaveState(chatID, *state)
		}
	} else {
		msg := tgbotapi.NewMessage(chatID, msgText)
		msg.ReplyMarkup = keyboard
		var err error
		sentMsg, err = b.bot.Send(msg)
		if err != nil {
			b.logger.Errorf("发送确认消息失败，用户 %d: %v", chatID, err)
		} else {
			state.Messages = append(state.Messages, sentMsg.MessageID)
			b.SaveState(chatID, *state)
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
			b.SaveState(chatID, *state)
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
			b.SaveState(chatID, *state)
		}
	}
}

// handleCallback 处理回调查询
func (b *Bot) handleCallback(query *tgbotapi.CallbackQuery) {
	chatID := query.Message.Chat.ID
	data := query.Data
	b.logger.Infof("用户 %d 触发回调: %s", chatID, data)

	state, err := b.getState(chatID)
	if err != nil {
		b.logger.Errorf("获取用户 %d 状态失败: %v", chatID, err)
		return
	}

	switch state.Step {
	case 1: // 服务选择
		if strings.HasPrefix(data, "service:") {
			state.Service = strings.TrimPrefix(data, "service:")
			b.logger.Infof("用户 %d 选择服务: %s", chatID, state.Service)
			b.SaveState(chatID, state)
			b.showServiceSelection(chatID, &state)
		} else if data == "next_service" {
			if state.Service == "" {
				b.logger.Warnf("用户 %d 未选择服务", chatID)
				b.SendMessage(chatID, "请先选择一个服务！", nil)
			} else {
				b.logger.Infof("用户 %d 确认服务选择，继续到环境选择", chatID)
				state.Step = 2
				b.SaveState(chatID, state)
				b.showEnvironmentSelection(chatID, &state)
			}
		} else if data == "cancel" {
			b.logger.Infof("用户 %d 取消服务选择，会话关闭", chatID)
			b.SendMessage(chatID, "会话已关闭", nil)
			b.deleteState(chatID)
		}
	case 2: // 环境选择
		if strings.HasPrefix(data, "env:") {
			env := strings.TrimPrefix(data, "env:")
			if !contains(state.Environments, env) {
				state.Environments = append(state.Environments, env)
				b.logger.Infof("用户 %d 选择环境: %s", chatID, env)
				b.SaveState(chatID, state)
			}
			b.showEnvironmentSelection(chatID, &state)
		} else if data == "next_env" {
			if len(state.Environments) == 0 {
				b.logger.Warnf("用户 %d 未选择任何环境", chatID)
				b.SendMessage(chatID, "请至少选择一个环境！", nil)
			} else {
				b.logger.Infof("用户 %d 完成环境选择: %v", chatID, state.Environments)
				state.Step = 3
				b.SaveState(chatID, state)
				b.SendMessage(chatID, "请输入版本号：", nil)
			}
		} else if data == "cancel" {
			b.logger.Infof("用户 %d 取消环境选择，会话关闭", chatID)
			b.SendMessage(chatID, "会话已关闭", nil)
			b.deleteState(chatID)
		}
	case 4: // 确认提交
		if data == "confirm_yes" {
			b.logger.Infof("用户 %d 确认提交数据: 服务=%s, 环境=%v, 版本=%s", chatID, state.Service, state.Environments, state.Version)
			b.persistData(state)
			b.SendMessage(chatID, "数据提交成功！", nil)
			state.Step = 5
			b.SaveState(chatID, state)
			b.askContinue(chatID, &state)
		} else if data == "confirm_no" {
			b.logger.Infof("用户 %d 取消数据提交，会话关闭", chatID)
			b.SendMessage(chatID, "会话已关闭", nil)
			b.deleteState(chatID)
		}
	case 5: // 是否继续
		if data == "restart_yes" {
			b.logger.Infof("用户 %d 选择继续提交，重新开始交互", chatID)
			b.startInteraction(chatID)
		} else if data == "restart_no" {
			b.logger.Infof("用户 %d 选择结束交互，会话关闭", chatID)
			b.SendMessage(chatID, "会话已关闭", nil)
			b.deleteState(chatID)
		}
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
func (b *Bot) SaveState(chatID int64, state UserState) {
	data, _ := json.Marshal(state)
	b.storage.Set(fmt.Sprintf("user:%d", chatID), string(data))
	b.logger.Infof("保存用户 %d 状态: %v", chatID, state)
}

// getState 从 Redis 获取用户状态
func (b *Bot) getState(chatID int64) (UserState, error) {
	data, err := b.storage.Get(fmt.Sprintf("user:%d", chatID))
	if err != nil {
		b.logger.Errorf("获取用户 %d 状态失败: %v", chatID, err)
		return UserState{}, err
	}
	var state UserState
	if err := json.Unmarshal([]byte(data), &state); err != nil {
		b.logger.Errorf("解析用户 %d 状态失败: %v", chatID, err)
		return UserState{}, err
	}
	return state, nil
}

// deleteState 删除用户状态
func (b *Bot) deleteState(chatID int64) {
	b.storage.Delete(fmt.Sprintf("user:%d", chatID))
	b.logger.Infof("删除用户 %d 状态", chatID)
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