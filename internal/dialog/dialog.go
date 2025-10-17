package dialog

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"github.com/google/uuid"
	"k8s-cicd/internal/config"
	"k8s-cicd/internal/queue"
	"k8s-cicd/internal/storage"
	"k8s-cicd/internal/types"
)

type DialogState struct {
	UserID        int64
	ChatID        int64
	Service       string
	Stage         string // "service", "env", "version", "confirm", "continue"
	Selected      string // Single selection for service
	SelectedEnvs  []string
	StartedAt     time.Time
	UserName      string
	Version       string
	timeoutCancel chan bool
	Messages      []int
}

var (
	dialogs             sync.Map
	taskQueue           *queue.Queue
	GlobalTaskQueue     *queue.Queue
	PendingConfirmations sync.Map
)

func SetTaskQueue(q *queue.Queue) {
	taskQueue = q
	GlobalTaskQueue = q
}

func StartDialog(userID, chatID int64, service string, cfg *config.Config, userName string) {
	if _, loaded := dialogs.Load(userID); loaded {
		log.Printf("Cancelling existing dialog for user %d in chat %d", userID, chatID)
		CancelDialog(userID, chatID, cfg)
		sendMessage(cfg, chatID, "Previous dialog was active and has been cancelled. Starting new deployment dialog.")
	}

	newState := &DialogState{
		UserID:        userID,
		ChatID:        chatID,
		Service:       service,
		Stage:         "service",
		StartedAt:     time.Now(),
		UserName:      userName,
		timeoutCancel: make(chan bool),
		Messages:      []int{},
	}
	dialogs.Store(userID, newState)

	log.Printf("Started dialog for user %d in chat %d for service %s", userID, chatID, service)
	sendMessage(cfg, chatID, fmt.Sprintf("开始部署对话 / Starting deployment dialog for service %s", service))
	sendServiceSelection(userID, chatID, cfg, newState)
	go monitorDialogTimeout(userID, chatID, cfg, newState)
}

func sendServiceSelection(userID, chatID int64, cfg *config.Config, s *DialogState) {
	serviceLists, err := config.LoadServiceLists(cfg.ServicesDir, cfg.TelegramBots)
	if err != nil {
		log.Printf("Failed to load service lists for user %d: %v", userID, err)
		sendMessage(cfg, chatID, "无法加载服务列表 / Failed to load service list.")
		return
	}
	services, exists := serviceLists[s.Service]
	if !exists {
		log.Printf("No service list found for %s for user %d", s.Service, userID)
		sendMessage(cfg, chatID, "未找到服务列表 / No service list found.")
		return
	}
	if len(services) == 0 {
		log.Printf("No services available for %s for user %d", s.Service, userID)
		sendMessage(cfg, chatID, "无可用服务 / No services available.")
		return
	}

	log.Printf("Loaded %d services for %s: %v", len(services), s.Service, services)

	maxLen := 0
	for _, svc := range services {
		if len(svc) > maxLen {
			maxLen = len(svc)
		}
	}
	cols := 2
	if maxLen < 10 {
		cols = 4
	} else if maxLen < 15 {
		cols = 3
	}
	if len(services) < cols {
		cols = len(services)
	}

	var buttons [][]tgbotapi.InlineKeyboardButton
	var row []tgbotapi.InlineKeyboardButton
	for i, svc := range services {
		displayText := svc
		if svc == s.Selected {
			displayText = fmt.Sprintf("<b>✅ %s</b>", svc)
		}
		row = append(row, tgbotapi.NewInlineKeyboardButtonData(displayText, svc))
		if len(row) == cols || i == len(services)-1 {
			buttons = append(buttons, row)
			row = []tgbotapi.InlineKeyboardButton{}
		}
	}

	buttons = append(buttons, []tgbotapi.InlineKeyboardButton{
		tgbotapi.NewInlineKeyboardButtonData("下一步 / Next", "next_service"),
		tgbotapi.NewInlineKeyboardButtonData("取消 / Cancel", "cancel"),
	})

	log.Printf("Generated %d rows with %d columns for %d services for user %d", len(buttons), cols, len(services), userID)

	keyboard := tgbotapi.NewInlineKeyboardMarkup(buttons...)
	msgText := fmt.Sprintf("请选择服务 / Please select a service:\n当前选中 / Currently selected: %s", s.Selected)
	msg := tgbotapi.NewMessage(chatID, msgText)
	msg.ReplyMarkup = keyboard
	msg.ParseMode = "HTML"
	if sentMsg, err := sendMessage(cfg, chatID, msg); err == nil {
		s.Messages = append(s.Messages, sentMsg.MessageID)
	}
}

func ProcessDialog(userID, chatID int64, input string, cfg *config.Config) {
	state, loaded := dialogs.Load(userID)
	if !loaded {
		log.Printf("No active dialog for user %d in chat %d", userID, chatID)
		return
	}
	s := state.(*DialogState)

	switch s.Stage {
	case "service":
		if input == "next_service" {
			if s.Selected == "" {
				sendMessage(cfg, chatID, "请先选择一个服务。\nPlease select a service first.")
				return
			}
			s.Stage = "env"
			dialogs.Store(userID, s)
			sendEnvSelection(userID, chatID, cfg, s)
		} else if input == "cancel" {
			CancelDialog(userID, chatID, cfg)
		} else {
			s.Selected = input
			dialogs.Store(userID, s)
			serviceLists, err := config.LoadServiceLists(cfg.ServicesDir, cfg.TelegramBots)
			if err != nil {
				log.Printf("Failed to load service lists: %v", err)
				sendMessage(cfg, chatID, "无法加载服务列表 / Failed to load service list.")
				return
			}
			services, exists := serviceLists[s.Service]
			if !exists {
				log.Printf("No service list found for %s", s.Service)
				sendMessage(cfg, chatID, "未找到服务列表 / No service list found.")
				return
			}
			var buttons [][]tgbotapi.InlineKeyboardButton
			var row []tgbotapi.InlineKeyboardButton
			cols := 2
			if len(services) < cols {
				cols = len(services)
			}
			for i, svc := range services {
				displayText := svc
				if svc == s.Selected {
					displayText = fmt.Sprintf("<b>✅ %s</b>", svc)
				}
				row = append(row, tgbotapi.NewInlineKeyboardButtonData(displayText, svc))
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
			msgText := fmt.Sprintf("请选择服务 / Please select a service:\n当前选中 / Currently selected: %s", s.Selected)
			edit := tgbotapi.EditMessageTextConfig{
				BaseEdit: tgbotapi.BaseEdit{
					ChatID:    chatID,
					MessageID: s.Messages[len(s.Messages)-1],
				},
				Text:      msgText,
				ParseMode: "HTML",
			}
			edit.ReplyMarkup = &keyboard
			if _, err := sendMessage(cfg, chatID, edit); err != nil {
				log.Printf("Failed to edit message: %v", err)
			}
		}
	case "env":
		if input == "next_env" {
			if len(s.SelectedEnvs) == 0 {
				sendMessage(cfg, chatID, "请至少选择一个环境。\nPlease select at least one environment.")
				return
			}
			s.Stage = "version"
			dialogs.Store(userID, s)
			sendVersionPrompt(userID, chatID, cfg, s)
		} else if input == "cancel" {
			CancelDialog(userID, chatID, cfg)
		} else if validateEnvironment(input, cfg) {
			if contains(s.SelectedEnvs, input) {
				s.SelectedEnvs = remove(s.SelectedEnvs, input)
			} else {
				s.SelectedEnvs = append(s.SelectedEnvs, input)
			}
			dialogs.Store(userID, s)
			sendEnvSelection(userID, chatID, cfg, s)
		} else {
			sendMessage(cfg, chatID, fmt.Sprintf("无效环境 %s / Invalid environment %s. Please select a valid environment.", input, input))
		}
	case "version":
		s.Version = input
		s.Stage = "confirm"
		dialogs.Store(userID, s)
		sendConfirmation(userID, chatID, cfg, s)
	case "confirm":
		if input == "confirm" {
			submitTasks(userID, chatID, cfg, s)
			s.Stage = "continue"
			dialogs.Store(userID, s)
			sendContinuePrompt(userID, chatID, cfg, s)
		} else if input == "cancel" {
			CancelDialog(userID, chatID, cfg)
		}
	case "continue":
		if input == "yes" {
			select {
			case s.timeoutCancel <- true:
			default:
			}
			s.Stage = "service"
			s.Selected = ""
			s.SelectedEnvs = []string{}
			s.Version = ""
			s.Messages = []int{}
			dialogs.Store(userID, s)
			sendServiceSelection(userID, chatID, cfg, s)
		} else if input == "no" {
			CancelDialog(userID, chatID, cfg)
		}
	}
}

func validateEnvironment(env string, cfg *config.Config) bool {
	fileName := filepath.Join(cfg.StorageDir, "environments.json")
	if data, err := os.ReadFile(fileName); err == nil {
		var envs []string
		if err := json.Unmarshal(data, &envs); err == nil {
			for _, e := range envs {
				if strings.ToLower(e) == strings.ToLower(env) {
					return true
				}
			}
		}
	}
	_, exists := cfg.Environments[strings.ToLower(env)]
	return exists
}

func getEnvironmentsFromDeployFile(cfg *config.Config) []string {
	fileName := storage.GetDailyFileName(time.Now(), "deploy", cfg.StorageDir)
	if err := storage.EnsureDailyFile(fileName, nil, cfg); err != nil {
		log.Printf("Failed to ensure deploy file for environments: %v", err)
		return []string{}
	}

	data, err := os.ReadFile(fileName)
	if err != nil {
		log.Printf("Failed to read deploy file: %v", err)
		return []string{}
	}
	var infos []storage.DeploymentInfo
	if err := json.Unmarshal(data, &infos); err != nil {
		log.Printf("Failed to unmarshal deploy file: %v", err)
		return []string{}
	}

	envSet := make(map[string]bool)
	for _, info := range infos {
		envSet[info.Env] = true
	}
	var envs []string
	for env := range envSet {
		envs = append(envs, env)
	}
	sort.Strings(envs)
	return envs
}

func sendEnvSelection(userID, chatID int64, cfg *config.Config, s *DialogState) {
	envs := getEnvironmentsFromDeployFile(cfg)
	if len(envs) == 0 {
		log.Printf("No environments available for user %d in chat %d", userID, chatID)
		sendMessage(cfg, chatID, "无可用环境 / No environments available.")
		return
	}

	var buttons [][]tgbotapi.InlineKeyboardButton
	var row []tgbotapi.InlineKeyboardButton
	cols := 2
	if len(envs) < cols {
		cols = len(envs)
	}
	for i, env := range envs {
		displayText := env
		if contains(s.SelectedEnvs, env) {
			displayText = fmt.Sprintf("<b>✅ %s</b>", env)
		}
		row = append(row, tgbotapi.NewInlineKeyboardButtonData(displayText, env))
		if len(row) == cols || i == len(envs)-1 {
			buttons = append(buttons, row)
			row = []tgbotapi.InlineKeyboardButton{}
		}
	}
	buttons = append(buttons, []tgbotapi.InlineKeyboardButton{
		tgbotapi.NewInlineKeyboardButtonData("下一步 / Next", "next_env"),
		tgbotapi.NewInlineKeyboardButtonData("取消 / Cancel", "cancel"),
	})

	keyboard := tgbotapi.NewInlineKeyboardMarkup(buttons...)
	msgText := fmt.Sprintf("请选择环境（可多选） / Please select environments (multiple selection allowed):\n当前选中 / Currently selected: %s", strings.Join(s.SelectedEnvs, ", "))
	msg := tgbotapi.NewMessage(chatID, msgText)
	msg.ReplyMarkup = keyboard
	msg.ParseMode = "HTML"
	if len(s.Messages) > 0 {
		edit := tgbotapi.EditMessageTextConfig{
			BaseEdit: tgbotapi.BaseEdit{
				ChatID:    chatID,
				MessageID: s.Messages[len(s.Messages)-1],
			},
			Text:      msgText,
			ParseMode: "HTML",
		}
		edit.ReplyMarkup = &keyboard
		if _, err := sendMessage(cfg, chatID, edit); err != nil {
			log.Printf("Failed to edit message: %v", err)
			if sentMsg, err := sendMessage(cfg, chatID, msg); err == nil {
				s.Messages = append(s.Messages, sentMsg.MessageID)
			}
		}
	} else {
		if sentMsg, err := sendMessage(cfg, chatID, msg); err == nil {
			s.Messages = append(s.Messages, sentMsg.MessageID)
		}
	}
}

func sendConfirmation(userID, chatID int64, cfg *config.Config, s *DialogState) {
	message := fmt.Sprintf(
		"确认部署服务 / Confirm deployment:\n服务 / Service: %s\n环境 / Environments: %s\n版本 / Version: %s\n提交用户 / Submitted by: %s",
		s.Selected, strings.Join(s.SelectedEnvs, ", "), s.Version, s.UserName,
	)
	buttons := [][]tgbotapi.InlineKeyboardButton{
		{
			tgbotapi.NewInlineKeyboardButtonData("确认 / Confirm", "confirm"),
			tgbotapi.NewInlineKeyboardButtonData("取消 / Cancel", "cancel"),
		},
	}
	keyboard := tgbotapi.NewInlineKeyboardMarkup(buttons...)
	msg := tgbotapi.NewMessage(chatID, message)
	msg.ReplyMarkup = keyboard
	msg.ParseMode = "HTML"
	if sentMsg, err := sendMessage(cfg, chatID, msg); err == nil {
		s.Messages = append(s.Messages, sentMsg.MessageID)
	}
}

func submitTasks(userID, chatID int64, cfg *config.Config, s *DialogState) {
	id := uuid.New().String()[:8]
	var tasks []types.DeployRequest
	for _, env := range s.SelectedEnvs {
		tasks = append(tasks, types.DeployRequest{
			Service:   s.Selected,
			Env:       env,
			Version:   s.Version,
			Timestamp: time.Now(),
			UserName:  s.UserName,
			Status:    "pending_confirmation",
		})
	}
	PendingConfirmations.Store(id, tasks)

	message := fmt.Sprintf(
		"确认部署服务 %s 到环境 %s，版本 %s，由用户 %s 提交？\nConfirm deployment for service %s to envs %s, version %s by %s?",
		s.Selected, strings.Join(s.SelectedEnvs, ","), s.Version, s.UserName,
		s.Selected, strings.Join(s.SelectedEnvs, ","), s.Version, s.UserName,
	)
	if err := SendConfirmation(s.Service, chatID, message, id, cfg); err != nil {
		log.Printf("Failed to send confirmation: %v", err)
		sendMessage(cfg, chatID, "Failed to send confirmation to Telegram.")
		return
	}

	sendMessage(cfg, chatID, "Task submitted, awaiting confirmation in Telegram.")
}

func sendVersionPrompt(userID, chatID int64, cfg *config.Config, s *DialogState) {
	msg := tgbotapi.NewMessage(chatID, "请输入版本号 / Please enter the version number:")
	msg.ParseMode = "HTML"
	if sentMsg, err := sendMessage(cfg, chatID, msg); err == nil {
		s.Messages = append(s.Messages, sentMsg.MessageID)
	}
}

func sendContinuePrompt(userID, chatID int64, cfg *config.Config, s *DialogState) {
	message := "是否继续部署其他服务？\nWould you like to continue deploying another service?"
	buttons := [][]tgbotapi.InlineKeyboardButton{
		{
			tgbotapi.NewInlineKeyboardButtonData("是 / Yes", "yes"),
			tgbotapi.NewInlineKeyboardButtonData("否 / No", "no"),
		},
	}
	keyboard := tgbotapi.NewInlineKeyboardMarkup(buttons...)
	msg := tgbotapi.NewMessage(chatID, message)
	msg.ReplyMarkup = keyboard
	msg.ParseMode = "HTML"
	if sentMsg, err := sendMessage(cfg, chatID, msg); err == nil {
		s.Messages = append(s.Messages, sentMsg.MessageID)
	}
}

func CancelDialog(userID, chatID int64, cfg *config.Config) bool {
	state, loaded := dialogs.Load(userID)
	if !loaded {
		log.Printf("No active dialog to cancel for user %d in chat %d", userID, chatID)
		return false
	}
	s := state.(*DialogState)
	select {
	case s.timeoutCancel <- true:
	default:
	}
	deleteMessages(s, cfg)
	dialogs.Delete(userID)
	log.Printf("Dialog cancelled for user %d in chat %d", userID, chatID)
	sendMessage(cfg, chatID, "当前会话已关闭。\nCurrent session has been closed.")
	return true
}

func IsDialogActive(userID int64) bool {
	_, ok := dialogs.Load(userID)
	return ok
}

func monitorDialogTimeout(userID, chatID int64, cfg *config.Config, s *DialogState) {
	select {
	case <-time.After(time.Duration(cfg.DialogTimeout) * time.Second):
		if _, loaded := dialogs.Load(userID); loaded {
			log.Printf("Dialog timed out for user %d in chat %d", userID, chatID)
			deleteMessages(s, cfg)
			sendMessage(cfg, chatID, "对话已超时，请重新触发部署。\nDialog timed out, please trigger deployment again.")
			dialogs.Delete(userID)
		}
	case <-s.timeoutCancel:
		log.Printf("Timeout cancelled for user %d in chat %d", userID, chatID)
	}
}

func sendMessage(cfg *config.Config, chatID int64, text interface{}) (tgbotapi.Message, error) {
	service := ""
	for svc, id := range cfg.TelegramChats {
		if id == chatID {
			service = svc
			break
		}
	}
	if service == "" {
		log.Printf("No service found for chat %d, trying default chat", chatID)
		if defaultChatID, ok := cfg.TelegramChats["other"]; ok {
			chatID = defaultChatID
			service = "other"
		} else {
			log.Printf("No default chat configured for chat %d", chatID)
			return tgbotapi.Message{}, fmt.Errorf("no service or default chat found for chat %d", chatID)
		}
	}
	token, ok := cfg.TelegramBots[service]
	if !ok {
		log.Printf("No bot configured for service %s", service)
		return tgbotapi.Message{}, fmt.Errorf("no bot configured for service %s", service)
	}
	bot, err := tgbotapi.NewBotAPI(token)
	if err != nil {
		log.Printf("Failed to create bot for service %s: %v", service, err)
		return tgbotapi.Message{}, err
	}

	var sentMsg tgbotapi.Message
	switch t := text.(type) {
	case string:
		msg := tgbotapi.NewMessage(chatID, t)
		msg.ParseMode = "HTML"
		sentMsg, err = bot.Send(msg)
	case tgbotapi.MessageConfig:
		t.ParseMode = "HTML"
		sentMsg, err = bot.Send(t)
	case tgbotapi.EditMessageTextConfig:
		sentMsg, err = bot.Send(t)
	}
	if err != nil {
		log.Printf("Failed to send/edit message to chat %d: %v", chatID, err)
		return tgbotapi.Message{}, err
	}
	storage.PersistInteraction(cfg, map[string]interface{}{
		"chat_id":   chatID,
		"message":   fmt.Sprint(text),
		"timestamp": time.Now().Format(time.RFC3339),
	})
	return sentMsg, nil
}

func deleteMessages(s *DialogState, cfg *config.Config) {
	service := ""
	for svc, id := range cfg.TelegramChats {
		if id == s.ChatID {
			service = svc
			break
		}
	}
	if service == "" {
		log.Printf("No service found for chat %d, trying default chat", s.ChatID)
		if defaultChatID, ok := cfg.TelegramChats["other"]; ok {
			s.ChatID = defaultChatID
			service = "other"
		} else {
			log.Printf("No default chat configured for chat %d", s.ChatID)
			return
		}
	}
	token, ok := cfg.TelegramBots[service]
	if !ok {
		log.Printf("No bot configured for service %s", service)
		return
	}
	bot, err := tgbotapi.NewBotAPI(token)
	if err != nil {
		log.Printf("Failed to create bot for service %s: %v", service, err)
		return
	}

	for _, msgID := range s.Messages {
		deleteMsg := tgbotapi.NewDeleteMessage(s.ChatID, msgID)
		if _, err := bot.Request(deleteMsg); err != nil {
			log.Printf("Failed to delete message %d in chat %d: %v", msgID, s.ChatID, err)
		} else {
			log.Printf("Deleted message %d in chat %d", msgID, s.ChatID)
		}
	}
}

func SendConfirmation(category string, chatID int64, message string, callbackData string, cfg *config.Config) error {
	token, ok := cfg.TelegramBots[category]
	if !ok {
		log.Printf("No bot configured for category %s", category)
		return fmt.Errorf("no bot configured for category %s", category)
	}
	bot, err := tgbotapi.NewBotAPI(token)
	if err != nil {
		log.Printf("Failed to create bot for category %s: %v", category, err)
		return err
	}

	buttons := [][]tgbotapi.InlineKeyboardButton{
		{
			tgbotapi.NewInlineKeyboardButtonData("确认 / Confirm", "confirm_api:"+callbackData),
			tgbotapi.NewInlineKeyboardButtonData("取消 / Cancel", "cancel_api:"+callbackData),
		},
	}
	keyboard := tgbotapi.NewInlineKeyboardMarkup(buttons...)
	msg := tgbotapi.NewMessage(chatID, message)
	msg.ReplyMarkup = keyboard
	msg.ParseMode = "HTML"
	_, err = bot.Send(msg)
	return err
}

func remove(slice []string, item string) []string {
	for i, s := range slice {
		if s == item {
			return append(slice[:i], slice[i+1:]...)
		}
	}
	return slice
}

func contains(slice []string, item string) bool {
	for _, s := range slice {
		if s == item {
			return true
		}
	}
	return false
}