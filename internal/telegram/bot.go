package telegram

import (
	"encoding/json"
	"fmt"
	"k8s-cicd/internal/storage"
	"strings"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"github.com/sirupsen/logrus"
)

// Bot 封装 Telegram 机器人功能
type Bot struct {
	bot     *tgbotapi.BotAPI
	storage *storage.RedisStorage
	logger  *logrus.Logger
}

// UserState 保存用户交互状态
type UserState struct {
	Step         int      // 当前交互步骤
	Service      string   // 选择的服务
	Environments []string // 选择的环境
	Version      string   // 输入的版本号
	ChatID       int64    // 用户聊天 ID
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
		bot:     bot,
		storage: redisStorage,
		logger:  logger,
	}, nil
}

// Start 启动 Telegram 机器人，处理消息和回调
func (b *Bot) Start() {
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

// handleMessage 处理用户文本输入
func (b *Bot) handleMessage(msg *tgbotapi.Message) {
	chatID := msg.Chat.ID
	state, err := b.getState(chatID)
	if err != nil {
		// 如果没有状态，检查是否输入 "ai" 触发交互
		if strings.ToLower(msg.Text) == "ai" {
			b.startInteraction(chatID)
			return
		}
		b.logger.Errorf("获取用户状态失败: %v", err)
		return
	}

	switch state.Step {
	case 3: // 输入版本号
		if b.isVersionUnique(msg.Text) {
			state.Version = msg.Text
			state.Step = 4
			b.SaveState(chatID, state)
			b.showConfirmation(chatID, state)
		} else {
			b.SendMessage(chatID, "版本号已存在，请输入唯一版本号：", nil)
		}
	}
}

// startInteraction 开始交互流程，显示服务选择弹窗
func (b *Bot) startInteraction(chatID int64) {
	// 初始化用户状态
	state := UserState{
		Step:   1,
		ChatID: chatID,
	}
	b.SaveState(chatID, state)

	// 服务选择弹窗
	services := []string{"ServiceA", "ServiceB", "ServiceC"}
	var buttons []tgbotapi.InlineKeyboardButton
	for _, svc := range services {
		buttons = append(buttons, tgbotapi.NewInlineKeyboardButtonData(svc, fmt.Sprintf("service:%s", svc)))
	}
	keyboard := tgbotapi.NewInlineKeyboardMarkup(tgbotapi.NewInlineKeyboardRow(buttons...))
	b.SendMessage(chatID, "请选择服务（单选）：", &keyboard)
}

// handleCallback 处理回调查询
func (b *Bot) handleCallback(query *tgbotapi.CallbackQuery) {
	chatID := query.Message.Chat.ID
	state, err := b.getState(chatID)
	if err != nil {
		b.logger.Errorf("获取用户状态失败: %v", err)
		return
	}

	data := query.Data
	switch state.Step {
	case 1: // 服务选择
		if strings.HasPrefix(data, "service:") {
			state.Service = strings.TrimPrefix(data, "service:")
			state.Step = 2
			b.SaveState(chatID, state)
			keyboard := b.CreateYesNoKeyboard("service_confirm")
			b.SendMessage(chatID, fmt.Sprintf("您选择了服务：%s，确认继续？", state.Service), &keyboard)
		}
	case 2: // 服务确认
		if data == "service_confirm_yes" {
			b.showEnvironmentSelection(chatID)
		} else if data == "service_confirm_no" {
			b.SendMessage(chatID, "会话已关闭", nil)
			b.deleteState(chatID)
		}
	case 3: // 环境选择
		if strings.HasPrefix(data, "env:") {
			env := strings.TrimPrefix(data, "env:")
			if !contains(state.Environments, env) {
				state.Environments = append(state.Environments, env)
				b.SaveState(chatID, state)
			}
			b.showEnvironmentSelection(chatID)
		} else if data == "env_done" {
			if len(state.Environments) > 0 {
				keyboard := b.CreateYesNoKeyboard("env_confirm")
				b.SendMessage(chatID, fmt.Sprintf("您选择了环境：%s，确认继续？", strings.Join(state.Environments, ", ")), &keyboard)
			} else {
				b.SendMessage(chatID, "请至少选择一个环境！", nil)
			}
		}
	case 4: // 环境确认
		if data == "env_confirm_yes" {
			state.Step = 5
			b.SaveState(chatID, state)
			b.SendMessage(chatID, "请输入版本号：", nil)
		} else if data == "env_confirm_no" {
			b.SendMessage(chatID, "会话已关闭", nil)
			b.deleteState(chatID)
		}
	case 5: // 确认提交
		if data == "confirm_yes" {
			b.persistData(state)
			b.SendMessage(chatID, "数据提交成功！", nil)
			b.askContinue(chatID)
		} else if data == "confirm_no" {
			b.SendMessage(chatID, "会话已关闭", nil)
			b.deleteState(chatID)
		}
	case 6: // 是否继续
		if data == "restart_yes" {
			b.startInteraction(chatID)
		} else if data == "restart_no" {
			b.SendMessage(chatID, "会话已关闭", nil)
			b.deleteState(chatID)
		}
	}

	b.bot.Request(tgbotapi.NewCallback(query.ID, ""))
}

// showEnvironmentSelection 显示环境选择弹窗
func (b *Bot) showEnvironmentSelection(chatID int64) {
	environments := []string{"Dev", "Test", "Prod"}
	var buttons []tgbotapi.InlineKeyboardButton
	for _, env := range environments {
		buttons = append(buttons, tgbotapi.NewInlineKeyboardButtonData(env, fmt.Sprintf("env:%s", env)))
	}
	buttons = append(buttons, tgbotapi.NewInlineKeyboardButtonData("完成", "env_done"))
	keyboard := tgbotapi.NewInlineKeyboardMarkup(tgbotapi.NewInlineKeyboardRow(buttons...))
	b.SendMessage(chatID, "请选择环境（可多选）：", &keyboard)
}

// showConfirmation 显示数据确认弹窗
func (b *Bot) showConfirmation(chatID int64, state UserState) {
	msg := fmt.Sprintf("请确认以下信息：\n服务: %s\n环境: %s\n版本: %s",
		state.Service, strings.Join(state.Environments, ", "), state.Version)
	keyboard := b.CreateYesNoKeyboard("confirm")
	b.SendMessage(chatID, msg, &keyboard)
}

// askContinue 询问是否继续发布
func (b *Bot) askContinue(chatID int64) {
	keyboard := b.CreateYesNoKeyboard("restart")
	b.SendMessage(chatID, "是否继续提交数据？", &keyboard)
}

// CreateYesNoKeyboard 创建是/否键盘
func (b *Bot) CreateYesNoKeyboard(prefix string) tgbotapi.InlineKeyboardMarkup {
	return tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("是", prefix+"_yes"),
			tgbotapi.NewInlineKeyboardButtonData("否", prefix+"_no"),
		),
	)
}

// SendMessage 发送 Telegram 消息
func (b *Bot) SendMessage(chatID int64, text string, keyboard *tgbotapi.InlineKeyboardMarkup) {
	msg := tgbotapi.NewMessage(chatID, text)
	if keyboard != nil {
		msg.ReplyMarkup = *keyboard
	}
	if _, err := b.bot.Send(msg); err != nil {
		b.logger.Errorf("发送消息失败: %v", err)
	}
}

// SaveState 保存用户状态到 Redis
func (b *Bot) SaveState(chatID int64, state UserState) {
	data, _ := json.Marshal(state)
	b.storage.Set(fmt.Sprintf("user:%d", chatID), string(data))
}

// getState 从 Redis 获取用户状态
func (b *Bot) getState(chatID int64) (UserState, error) {
	data, err := b.storage.Get(fmt.Sprintf("user:%d", chatID))
	if err != nil {
		return UserState{}, err
	}
	var state UserState
	if err := json.Unmarshal([]byte(data), &state); err != nil {
		return UserState{}, err
	}
	return state, nil
}

// deleteState 删除用户状态
func (b *Bot) deleteState(chatID int64) {
	b.storage.Delete(fmt.Sprintf("user:%d", chatID))
}

// persistData 持久化数据到 Redis 队列
func (b *Bot) persistData(state UserState) {
	data, _ := json.Marshal(state)
	b.storage.Push("deploy_queue", string(data))
}

// isVersionUnique 检查版本号是否唯一
func (b *Bot) isVersionUnique(version string) bool {
	items, err := b.storage.List("deploy_queue")
	if err != nil {
		b.logger.Errorf("检查版本号唯一性失败: %v", err)
		return true // 假设错误时允许继续
	}
	for _, item := range items {
		var state UserState
		if err := json.Unmarshal([]byte(item), &state); err != nil {
			continue
		}
		if state.Version == version {
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