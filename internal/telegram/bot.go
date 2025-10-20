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
			state.Step = 2
			b.SaveState(chatID, state)
			b.logger.Infof("用户 %d 选择服务: %s", chatID, state.Service)
			keyboard := b.CreateYesNoKeyboard("service_confirm")
			b.SendMessage(chatID, fmt.Sprintf("您选择了服务：%s，确认继续？", state.Service), &keyboard)
		}
	case 2: // 服务确认
		if data == "service_confirm_yes" {
			b.logger.Infof("用户 %d 确认服务选择，继续到环境选择", chatID)
			b.showEnvironmentSelection(chatID)
		} else if data == "service_confirm_no" {
			b.logger.Infof("用户 %d 取消服务选择，会话关闭", chatID)
			b.SendMessage(chatID, "会话已关闭", nil)
			b.deleteState(chatID)
		}
	case 3: // 环境选择
		if strings.HasPrefix(data, "env:") {
			env := strings.TrimPrefix(data, "env:")
			if !contains(state.Environments, env) {
				state.Environments = append(state.Environments, env)
				b.SaveState(chatID, state)
				b.logger.Infof("用户 %d 选择环境: %s", chatID, env)
			}
			b.showEnvironmentSelection(chatID)
		} else if data == "env_done" {
			if len(state.Environments) > 0 {
				b.logger.Infof("用户 %d 完成环境选择: %v", chatID, state.Environments)
				keyboard := b.CreateYesNoKeyboard("env_confirm")
				b.SendMessage(chatID, fmt.Sprintf("您选择了环境：%s，确认继续？", strings.Join(state.Environments, ", ")), &keyboard)
				state.Step = 4
				b.SaveState(chatID, state)
			} else {
				b.logger.Warnf("用户 %d 未选择任何环境", chatID)
				b.SendMessage(chatID, "请至少选择一个环境！", nil)
			}
		}
	case 4: // 环境确认
		if data == "env_confirm_yes" {
			b.logger.Infof("用户 %d 确认环境选择，继续到版本输入", chatID)
			state.Step = 5
			b.SaveState(chatID, state)
			b.SendMessage(chatID, "请输入版本号：", nil)
		} else if data == "env_confirm_no" {
			b.logger.Infof("用户 %d 取消环境选择，会话关闭", chatID)
			b.SendMessage(chatID, "会话已关闭", nil)
			b.deleteState(chatID)
		}
	case 5: // 确认提交
		if data == "confirm_yes" {
			b.logger.Infof("用户 %d 确认提交数据: 服务=%s, 环境=%v, 版本=%s", chatID, state.Service, state.Environments, state.Version)
			b.persistData(state)
			b.SendMessage(chatID, "数据提交成功！", nil)
			b.askContinue(chatID)
		} else if data == "confirm_no" {
			b.logger.Infof("用户 %d 取消数据提交，会话关闭", chatID)
			b.SendMessage(chatID, "会话已关闭", nil)
			b.deleteState(chatID)
		}
	case 6: // 是否继续
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

// showEnvironmentSelection 显示环境选择弹窗
func (b *Bot) showEnvironmentSelection(chatID int64) {
	b.logger.Infof("用户 %d 显示环境选择弹窗", chatID)
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
	b.logger.Infof("用户 %d 显示确认弹窗: 服务=%s, 环境=%v, 版本=%s", chatID, state.Service, state.Environments, state.Version)
	msg := fmt.Sprintf("请确认以下信息：\n服务: %s\n环境: %s\n版本: %s",
		state.Service, strings.Join(state.Environments, ", "), state.Version)
	keyboard := b.CreateYesNoKeyboard("confirm")
	b.SendMessage(chatID, msg, &keyboard)
}

// askContinue 询问是否继续发布
func (b *Bot) askContinue(chatID int64) {
	b.logger.Infof("用户 %d 显示是否继续提交弹窗", chatID)
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