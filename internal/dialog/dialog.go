// internal/dialog/dialog.go
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
	Selected      string
	SelectedEnvs  []string
	StartedAt     time.Time
	UserName      string
	Version       string
	timeoutCancel chan bool
	Messages      []int
	RetryCount    int
}

var (
	dialogs                   sync.Map
	taskQueue                 *queue.Queue
	GlobalTaskQueue           *queue.Queue
	PendingConfirmations      sync.Map // For API-driven confirmations
	DialogPendingConfirmations sync.Map // For dialog-driven confirmations
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
		RetryCount:    0,
	}
	dialogs.Store(userID, newState)

	log.Printf("Started dialog for user %d in chat %d for service %s", userID, chatID, service)
	sentMsg, err := sendMessage(cfg, chatID, fmt.Sprintf("开始部署对话 / Starting deployment dialog for service %s", service))
	if err == nil {
		newState.Messages = append(newState.Messages, sentMsg.MessageID)
	}
	sentMsg, err = sendServiceSelection(userID, chatID, cfg, newState)
	if err == nil {
		newState.Messages = append(newState.Messages, sentMsg.MessageID)
	}
	go monitorDialogTimeout(userID, chatID, cfg, newState)
}

func sendServiceSelection(userID, chatID int64, cfg *config.Config, s *DialogState) (tgbotapi.Message, error) {
	serviceLists, err := config.LoadServiceLists(cfg.ServicesDir, cfg.TelegramBots)
	if err != nil {
		log.Printf("Failed to load service lists for user %d: %v", userID, err)
		sentMsg, sendErr := sendMessage(cfg, chatID, "无法加载服务列表 / Failed to load service list.")
		if sendErr == nil {
			s.Messages = append(s.Messages, sentMsg.MessageID)
		}
		return sentMsg, fmt.Errorf("failed to load service lists: %v", err)
	}
	services, exists := serviceLists[s.Service]
	if !exists {
		log.Printf("No service list found for %s for user %d", s.Service, userID)
		sentMsg, sendErr := sendMessage(cfg, chatID, "未找到服务列表 / No service list found.")
		if sendErr == nil {
			s.Messages = append(s.Messages, sentMsg.MessageID)
		}
		return sentMsg, fmt.Errorf("no service list found for %s", s.Service)
	}
	if len(services) == 0 {
		log.Printf("No services available for %s for user %d", s.Service, userID)
		sentMsg, sendErr := sendMessage(cfg, chatID, "服务列表为空，正在等待k8s-cicd上报数据。请稍后重试 / Service list empty, waiting for k8s-cicd report. Please retry later.")
		if sendErr == nil {
			s.Messages = append(s.Messages, sentMsg.MessageID)
		}
		s.RetryCount++
		if s.RetryCount < 3 {
			time.Sleep(10 * time.Second)
			return sendServiceSelection(userID, chatID, cfg, s)
		}
		sentMsg, sendErr = sendMessage(cfg, chatID, "重试失败，请联系管理员 / Retry failed, contact admin.")
		if sendErr == nil {
			s.Messages = append(s.Messages, sentMsg.MessageID)
		}
		return sentMsg, fmt.Errorf("service list empty for %s after %d retries", s.Service, s.RetryCount)
	}

	log.Printf("Loaded %d services for %s: %v", len(services), s.Service, services)
	if len(s.Messages) > 0 {
		lastMsgID := s.Messages[len(s.Messages)-1]
		return editServiceSelection(userID, chatID, lastMsgID, cfg, s)
	}
	return editServiceSelection(userID, chatID, 0, cfg, s)
}

func editServiceSelection(userID, chatID int64, msgID int, cfg *config.Config, s *DialogState) (tgbotapi.Message, error) {
	serviceLists, err := config.LoadServiceLists(cfg.ServicesDir, cfg.TelegramBots)
	if err != nil {
		return tgbotapi.Message{}, err
	}
	services := serviceLists[s.Service]

	sort.Strings(services)
	var rows [][]tgbotapi.InlineKeyboardButton
	var row []tgbotapi.InlineKeyboardButton
	for i, svc := range services {
		button := tgbotapi.NewInlineKeyboardButtonData(svc, "service:"+svc)
		row = append(row, button)
		if (i+1)%3 == 0 || i == len(services)-1 {
			rows = append(rows, row)
			row = nil
		}
	}
	rows = append(rows, []tgbotapi.InlineKeyboardButton{tgbotapi.NewInlineKeyboardButtonData("取消 / Cancel", "cancel")})
	keyboard := tgbotapi.NewInlineKeyboardMarkup(rows...)

	msgText := "请选择服务 / Select a service:"
	if msgID == 0 {
		msg := tgbotapi.NewMessage(chatID, msgText)
		msg.ReplyMarkup = keyboard
		return sendMessage(cfg, chatID, msg)
	}
	edit := tgbotapi.NewEditMessageText(chatID, msgID, msgText)
	edit.ReplyMarkup = &keyboard
	return sendMessage(cfg, chatID, edit)
}

func ProcessDialog(userID, chatID int64, text string, cfg *config.Config) {
	state, loaded := dialogs.Load(userID)
	if !loaded {
		log.Printf("No active dialog for user %d in chat %d", userID, chatID)
		return
	}
	s := state.(*DialogState)
	select {
	case s.timeoutCancel <- true:
	default:
	}

	switch s.Stage {
	case "service":
		if strings.HasPrefix(text, "service:") {
			text = strings.TrimPrefix(text, "service:")
		}
		processServiceSelection(userID, chatID, text, cfg, s)
	case "env":
		if strings.HasPrefix(text, "env:") {
			text = strings.TrimPrefix(text, "env:")
		}
		processEnvSelection(userID, chatID, text, cfg, s)
	case "version":
		processVersionInput(userID, chatID, text, cfg, s)
	case "confirm":
		if strings.HasPrefix(text, "confirm_dialog:") || strings.HasPrefix(text, "cancel_dialog:") {
			processConfirmation(userID, chatID, text, cfg, s)
		}
	case "continue":
		if text == "continue_yes" || text == "continue_no" {
			processContinue(userID, chatID, text, cfg, s)
		}
	}
}

func processServiceSelection(userID, chatID int64, text string, cfg *config.Config, s *DialogState) {
	if text == "cancel" {
		CancelDialog(userID, chatID, cfg)
		return
	}
	s.Selected = text
	s.Stage = "env"
	sentMsg, _ := sendEnvSelection(userID, chatID, cfg, s)
	s.Messages = append(s.Messages, sentMsg.MessageID)
}

func sendEnvSelection(userID, chatID int64, cfg *config.Config, s *DialogState) (tgbotapi.Message, error) {
	envFile := filepath.Join(cfg.StorageDir, "environments.json")
	var envs map[string]string
	if data, err := os.ReadFile(envFile); err == nil && len(data) > 0 {
		if err := json.Unmarshal(data, &envs); err != nil {
			log.Printf("Failed to unmarshal environments.json: %v", err)
		}
	} else {
		envs = cfg.Environments
	}
	envList := make([]string, 0, len(envs))
	for env := range envs {
		envList = append(envList, env)
	}
	sort.Strings(envList)
	var rows [][]tgbotapi.InlineKeyboardButton
	var row []tgbotapi.InlineKeyboardButton
	for i, env := range envList {
		label := env
		if contains(s.SelectedEnvs, env) {
			label = "✅ " + env
		}
		button := tgbotapi.NewInlineKeyboardButtonData(label, "env:"+env)
		row = append(row, button)
		if (i+1)%3 == 0 || i == len(envList)-1 {
			rows = append(rows, row)
			row = nil
		}
	}
	rows = append(rows, []tgbotapi.InlineKeyboardButton{
		tgbotapi.NewInlineKeyboardButtonData("完成 / Done", "env_done"),
		tgbotapi.NewInlineKeyboardButtonData("取消 / Cancel", "cancel"),
	})
	keyboard := tgbotapi.NewInlineKeyboardMarkup(rows...)

	msgText := fmt.Sprintf("已选择服务: %s\nSelected service: %s\n请选择环境 / Select environments (multi-select):", s.Selected, s.Selected)
	msg := tgbotapi.NewMessage(chatID, msgText)
	msg.ReplyMarkup = keyboard
	return sendMessage(cfg, chatID, msg)
}

func processEnvSelection(userID, chatID int64, text string, cfg *config.Config, s *DialogState) {
	if text == "cancel" {
		CancelDialog(userID, chatID, cfg)
		return
	}
	if text == "env_done" {
		if len(s.SelectedEnvs) == 0 {
			sendMessage(cfg, chatID, "请至少选择一个环境 / Please select at least one environment.")
			return
		}
		s.Stage = "version"
		sentMsg, _ := sendVersionInput(userID, chatID, cfg, s)
		s.Messages = append(s.Messages, sentMsg.MessageID)
		return
	}
	if contains(s.SelectedEnvs, text) {
		s.SelectedEnvs = remove(s.SelectedEnvs, text)
	} else {
		s.SelectedEnvs = append(s.SelectedEnvs, text)
	}
	if len(s.Messages) > 0 {
		editEnvSelection(userID, chatID, s.Messages[len(s.Messages)-1], cfg, s)
	}
}

func editEnvSelection(userID, chatID int64, msgID int, cfg *config.Config, s *DialogState) {
	envFile := filepath.Join(cfg.StorageDir, "environments.json")
	var envs map[string]string
	if data, err := os.ReadFile(envFile); err == nil && len(data) > 0 {
		if err := json.Unmarshal(data, &envs); err != nil {
			log.Printf("Failed to unmarshal environments.json: %v", err)
		}
	} else {
		envs = cfg.Environments
	}
	envList := make([]string, 0, len(envs))
	for env := range envs {
		envList = append(envList, env)
	}
	sort.Strings(envList)
	var rows [][]tgbotapi.InlineKeyboardButton
	var row []tgbotapi.InlineKeyboardButton
	for i, env := range envList {
		label := env
		if contains(s.SelectedEnvs, env) {
			label = "✅ " + env
		}
		button := tgbotapi.NewInlineKeyboardButtonData(label, "env:"+env)
		row = append(row, button)
		if (i+1)%3 == 0 || i == len(envList)-1 {
			rows = append(rows, row)
			row = nil
		}
	}
	rows = append(rows, []tgbotapi.InlineKeyboardButton{
		tgbotapi.NewInlineKeyboardButtonData("完成 / Done", "env_done"),
		tgbotapi.NewInlineKeyboardButtonData("取消 / Cancel", "cancel"),
	})
	keyboard := tgbotapi.NewInlineKeyboardMarkup(rows...)

	msgText := fmt.Sprintf("已选择服务: %s\nSelected service: %s\n请选择环境 / Select environments (multi-select):", s.Selected, s.Selected)
	edit := tgbotapi.NewEditMessageText(chatID, msgID, msgText)
	edit.ReplyMarkup = &keyboard
	sendMessage(cfg, chatID, edit)
}

func sendVersionInput(userID, chatID int64, cfg *config.Config, s *DialogState) (tgbotapi.Message, error) {
	msgText := fmt.Sprintf("已选择服务: %s\nSelected service: %s\n已选择环境: %s\nSelected envs: %s\n请输入版本号 / Enter version:",
		s.Selected, s.Selected, strings.Join(s.SelectedEnvs, ","), strings.Join(s.SelectedEnvs, ","))
	msg := tgbotapi.NewMessage(chatID, msgText)
	return sendMessage(cfg, chatID, msg)
}

func processVersionInput(userID, chatID int64, text string, cfg *config.Config, s *DialogState) {
	s.Version = text
	s.Stage = "confirm"
	sentMsg, _ := sendConfirmation(userID, chatID, cfg, s)
	s.Messages = append(s.Messages, sentMsg.MessageID)
}

func sendConfirmation(userID, chatID int64, cfg *config.Config, s *DialogState) (tgbotapi.Message, error) {
	id := uuid.New().String()[:8]
	message := fmt.Sprintf("确认部署服务 %s 到环境 %s，版本 %s？\nConfirm deployment for service %s to envs %s, version %s?",
		s.Selected, strings.Join(s.SelectedEnvs, ","), s.Version, s.Selected, strings.Join(s.SelectedEnvs, ","), s.Version)
	buttons := [][]tgbotapi.InlineKeyboardButton{
		{
			tgbotapi.NewInlineKeyboardButtonData("确认 / Confirm", "confirm_dialog:"+id),
			tgbotapi.NewInlineKeyboardButtonData("取消 / Cancel", "cancel_dialog:"+id),
		},
	}
	keyboard := tgbotapi.NewInlineKeyboardMarkup(buttons...)
	msg := tgbotapi.NewMessage(chatID, message)
	msg.ReplyMarkup = keyboard
	msg.ParseMode = "HTML"

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
	DialogPendingConfirmations.Store(id, tasks)

	return sendMessage(cfg, chatID, msg)
}

func processConfirmation(userID, chatID int64, text string, cfg *config.Config, s *DialogState) {
	if strings.HasPrefix(text, "cancel_dialog:") {
		id := strings.TrimPrefix(text, "cancel_dialog:")
		DialogPendingConfirmations.Delete(id)
		CancelDialog(userID, chatID, cfg)
		return
	}
	if strings.HasPrefix(text, "confirm_dialog:") {
		id := strings.TrimPrefix(text, "confirm_dialog:")
		if tasks, ok := DialogPendingConfirmations.Load(id); ok {
			for _, t := range tasks.([]types.DeployRequest) {
				taskQueue.Enqueue(queue.Task{DeployRequest: t})
			}
			DialogPendingConfirmations.Delete(id)
		}
		s.Stage = "continue"
		sentMsg, _ := sendContinuePrompt(userID, chatID, cfg, s)
		s.Messages = append(s.Messages, sentMsg.MessageID)
	}
}

func sendContinuePrompt(userID, chatID int64, cfg *config.Config, s *DialogState) (tgbotapi.Message, error) {
	message := "是否继续另一个部署？ / Do you want to continue with another deployment?"
	buttons := [][]tgbotapi.InlineKeyboardButton{
		{
			tgbotapi.NewInlineKeyboardButtonData("是 / Yes", "continue_yes"),
			tgbotapi.NewInlineKeyboardButtonData("否 / No", "continue_no"),
		},
	}
	keyboard := tgbotapi.NewInlineKeyboardMarkup(buttons...)
	msg := tgbotapi.NewMessage(chatID, message)
	msg.ReplyMarkup = keyboard
	return sendMessage(cfg, chatID, msg)
}

func processContinue(userID, chatID int64, text string, cfg *config.Config, s *DialogState) {
	if text == "continue_no" {
		CancelDialog(userID, chatID, cfg)
	} else if text == "continue_yes" {
		s.Stage = "service"
		s.Selected = ""
		s.SelectedEnvs = nil
		s.Version = ""
		s.RetryCount = 0
		sendMessage(cfg, chatID, "开始新部署 / Starting new deployment")
		sentMsg, _ := sendServiceSelection(userID, chatID, cfg, s)
		s.Messages = append(s.Messages, sentMsg.MessageID)
	}
}

func SendConfirmation(category string, chatID int64, message string, callbackData string, cfg *config.Config) error {
	bot, err := GetBot(category)
	if err != nil {
		log.Printf("No bot configured for category %s: %v", category, err)
		return fmt.Errorf("no bot configured for category %s: %v", category, err)
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
		go monitorDialogTimeout(userID, chatID, cfg, s) // Restart timeout
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
	bot, err := GetBot(service)
	if err != nil {
		log.Printf("Failed to get bot for service %s: %v", service, err)
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
	bot, err := GetBot(service)
	if err != nil {
		log.Printf("Failed to get bot for service %s: %v", service, err)
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