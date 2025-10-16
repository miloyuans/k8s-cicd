// dialog.go
package dialog

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"sort" // Added import for sort package
	"strings"
	"sync"
	"time"

	"k8s-cicd/internal/config"
	"k8s-cicd/internal/queue"
	"k8s-cicd/internal/storage"
	"k8s-cicd/internal/types"
	"github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

type DialogState struct {
	UserID        int64
	ChatID        int64
	Service       string
	Stage         string // "service", "env", "version", "confirm", "continue"
	Selected      []string
	SelectedEnvs  []string // For multi-select envs
	StartedAt     time.Time
	UserName      string
	Version       string
	timeoutCancel chan bool // Channel to cancel timeout
	Messages      []int     // Store message IDs for cleanup
}

var (
	dialogs             sync.Map // map[int64]*DialogState
	taskQueue           *queue.Queue
	GlobalTaskQueue     *queue.Queue     // Added for API confirmation
	PendingConfirmations sync.Map         // map[string][]types.DeployRequest for confirmationID -> tasks
)

func SetTaskQueue(q *queue.Queue) {
	taskQueue = q
	GlobalTaskQueue = q
}

func StartDialog(userID, chatID int64, service string, cfg *config.Config, userName string) {
	if _, loaded := dialogs.Load(userID); loaded {
		log.Printf("User %d already has an active dialog in chat %d", userID, chatID)
		return
	}

	state := &DialogState{
		UserID:        userID,
		ChatID:        chatID,
		Service:       service,
		Stage:         "service",
		StartedAt:     time.Now(),
		UserName:      userName,
		timeoutCancel: make(chan bool),
		Messages:      []int{},
	}
	dialogs.Store(userID, state)

	log.Printf("Started dialog for user %d in chat %d for service %s", userID, chatID, service)
	sendServiceSelection(userID, chatID, cfg, state)
}

func sendServiceSelection(userID, chatID int64, cfg *config.Config, s *DialogState) {
	serviceLists, err := config.LoadServiceLists(cfg.ServicesDir, cfg.TelegramBots)
	if err != nil {
		log.Printf("Failed to load service lists for user %d: %v", userID, err)
		return
	}
	services, exists := serviceLists[s.Service]
	if !exists {
		log.Printf("No service list found for %s for user %d", s.Service, userID)
		return
	}
	if len(services) == 0 {
		log.Printf("No services available for %s for user %d", s.Service, userID)
		return
	}

	log.Printf("Loaded %d services for %s: %v", len(services), s.Service, services)

	// Calculate columns based on service name lengths and count
	maxLen := 0
	for _, svc := range services {
		if len(svc) > maxLen {
			maxLen = len(svc)
		}
	}
	cols := 2
	if maxLen < 10 {
		cols = 4 // Short names, use 4 columns
	} else if maxLen < 15 {
		cols = 3 // Medium names, use 3 columns
	}
	if len(services) < cols {
		cols = len(services) // Fewer services than columns, use service count
	}

	// Build multi-column button layout
	var buttons [][]tgbotapi.InlineKeyboardButton
	var row []tgbotapi.InlineKeyboardButton
	for i, svc := range services {
		row = append(row, tgbotapi.NewInlineKeyboardButtonData(svc, svc))
		if len(row) == cols || i == len(services)-1 {
			buttons = append(buttons, row)
			row = []tgbotapi.InlineKeyboardButton{}
		}
	}

	log.Printf("Generated %d rows with %d columns for %d services for user %d", len(buttons), cols, len(services), userID)

	keyboard := tgbotapi.NewInlineKeyboardMarkup(buttons...)
	msg := tgbotapi.NewMessage(chatID, "请选择一个服务：\nPlease select a service:")
	msg.ReplyMarkup = keyboard
	if sentMsg, err := sendMessage(cfg, chatID, msg); err == nil {
		s.Messages = append(s.Messages, sentMsg.MessageID)
	}
}

func getEnvironmentsFromDeployFile(cfg *config.Config) []string {
	fileName := storage.GetDailyFileName(time.Now(), "deploy", cfg.StorageDir)
	if err := storage.EnsureDailyFile(fileName, nil, cfg); err != nil {
		log.Printf("Failed to ensure deploy file for environments: %v", err)
		return []string{}
	}

	data, err := os.ReadFile(fileName)
	if err != nil {
		log.Printf("Failed to read deploy file for environments: %v", err)
		return []string{}
	}

	var infos []storage.DeploymentInfo
	if len(data) > 0 {
		if err := json.Unmarshal(data, &infos); err != nil {
			log.Printf("Failed to unmarshal deploy file for environments: %v", err)
			return []string{}
		}
	}

	// Extract unique environments
	envSet := make(map[string]bool)
	for _, info := range infos {
		envSet[info.Env] = true
	}
	var envs []string
	for env := range envSet {
		envs = append(envs, env)
	}
	sort.Strings(envs) // Sort environments for consistent display
	return envs
}

func sendEnvSelection(userID, chatID int64, cfg *config.Config, s *DialogState) {
	envs := getEnvironmentsFromDeployFile(cfg)
	if len(envs) == 0 {
		sendMessage(cfg, chatID, "No environments available.")
		CancelDialog(userID, chatID, cfg)
		return
	}

	// Multi-select for environments
	var buttons [][]tgbotapi.InlineKeyboardButton
	for _, env := range envs {
		buttonText := env
		if contains(s.SelectedEnvs, env) {
			buttonText = "✅ " + env
		}
		buttons = append(buttons, []tgbotapi.InlineKeyboardButton{
			tgbotapi.NewInlineKeyboardButtonData(buttonText, "env:"+env),
		})
	}
	buttons = append(buttons, []tgbotapi.InlineKeyboardButton{
		tgbotapi.NewInlineKeyboardButtonData("下一步 / Next", "next_env"),
		tgbotapi.NewInlineKeyboardButtonData("取消 / Cancel", "cancel"),
	})

	keyboard := tgbotapi.NewInlineKeyboardMarkup(buttons...)
	msg := tgbotapi.NewMessage(chatID, fmt.Sprintf("已选择服务: %s\nSelected services: %s\n\n请选择环境（可多选）:\nPlease select environments (multi-select):", strings.Join(s.Selected, ", "), strings.Join(s.Selected, ", ")))
	msg.ReplyMarkup = keyboard
	if sentMsg, err := sendMessage(cfg, chatID, msg); err == nil {
		s.Messages = append(s.Messages, sentMsg.MessageID)
	}
}

func sendVersionInput(userID, chatID int64, cfg *config.Config, s *DialogState) {
	msg := tgbotapi.NewMessage(chatID, fmt.Sprintf("已选择服务: %s\nSelected services: %s\n已选择环境: %s\nSelected envs: %s\n\n请输入版本号:\nPlease enter version:", strings.Join(s.Selected, ", "), strings.Join(s.Selected, ", "), strings.Join(s.SelectedEnvs, ", "), strings.Join(s.SelectedEnvs, ", ")))
	if sentMsg, err := sendMessage(cfg, chatID, msg); err == nil {
		s.Messages = append(s.Messages, sentMsg.MessageID)
	}
}

func sendConfirmation(userID, chatID int64, cfg *config.Config, s *DialogState) {
	message := fmt.Sprintf("确认部署服务 %s 到环境 %s，版本 %s？\nConfirm deployment for services %s to envs %s, version %s?", strings.Join(s.Selected, ", "), strings.Join(s.SelectedEnvs, ", "), s.Version, strings.Join(s.Selected, ", "), strings.Join(s.SelectedEnvs, ", "), s.Version)
	buttons := [][]tgbotapi.InlineKeyboardButton{
		{
			tgbotapi.NewInlineKeyboardButtonData("确认 / Confirm", "confirm"),
			tgbotapi.NewInlineKeyboardButtonData("取消 / Cancel", "cancel"),
		},
	}
	keyboard := tgbotapi.NewInlineKeyboardMarkup(buttons...)
	msg := tgbotapi.NewMessage(chatID, message)
	msg.ReplyMarkup = keyboard
	if sentMsg, err := sendMessage(cfg, chatID, msg); err == nil {
		s.Messages = append(s.Messages, sentMsg.MessageID)
	}
}

func sendContinuePrompt(userID, chatID int64, cfg *config.Config, s *DialogState) {
	message := "是否继续部署其他服务？\nWould you like to deploy more services?"
	buttons := [][]tgbotapi.InlineKeyboardButton{
		{
			tgbotapi.NewInlineKeyboardButtonData("是 / Yes", "yes"),
			tgbotapi.NewInlineKeyboardButtonData("否 / No", "no"),
		},
	}
	keyboard := tgbotapi.NewInlineKeyboardMarkup(buttons...)
	msg := tgbotapi.NewMessage(chatID, message)
	msg.ReplyMarkup = keyboard
	if sentMsg, err := sendMessage(cfg, chatID, msg); err == nil {
		s.Messages = append(s.Messages, sentMsg.MessageID)
	}
}

func submitTasks(userID, chatID int64, cfg *config.Config, s *DialogState) {
	for _, svc := range s.Selected {
		for _, env := range s.SelectedEnvs {
			deployReq := types.DeployRequest{
				Service:   svc,
				Env:       env,
				Version:   s.Version,
				Timestamp: time.Now(),
				UserName:  s.UserName,
				Status:    "pending",
			}
			taskQueue.Enqueue(queue.Task{DeployRequest: deployReq})
		}
	}
	sendMessage(cfg, chatID, fmt.Sprintf("已提交 %d 个任务。\nSubmitted %d tasks.", len(s.Selected)*len(s.SelectedEnvs), len(s.Selected)*len(s.SelectedEnvs)))
	s.Stage = "continue"
	dialogs.Store(userID, s)
	sendContinuePrompt(userID, chatID, cfg, s)
}

func ProcessDialog(userID, chatID int64, input string, cfg *config.Config) {
	state, loaded := dialogs.Load(userID)
	if !loaded {
		log.Printf("No active dialog for user %d in chat %d, ignoring input: %s", userID, chatID, input)
		return
	}
	s := state.(*DialogState)
	// Cancel timeout on input
	select {
	case s.timeoutCancel <- true:
	default:
	}
	go monitorDialogTimeout(userID, chatID, cfg, s)

	switch s.Stage {
	case "service":
		// Multi-select services
		if contains(s.Selected, input) {
			// Deselect
			var newSelected []string
			for _, sel := range s.Selected {
				if sel != input {
					newSelected = append(newSelected, sel)
				}
			}
			s.Selected = newSelected
		} else {
			s.Selected = append(s.Selected, input)
		}
		dialogs.Store(userID, s)
		// Refresh service selection
		deleteMessages(s, cfg)
		s.Messages = []int{}
		sendServiceSelection(userID, chatID, cfg, s)
	case "env":
		if strings.HasPrefix(input, "env:") {
			env := strings.TrimPrefix(input, "env:")
			if contains(s.SelectedEnvs, env) {
				// Deselect
				var newEnvs []string
				for _, sel := range s.SelectedEnvs {
					if sel != env {
						newEnvs = append(newEnvs, sel)
					}
				}
				s.SelectedEnvs = newEnvs
			} else {
				s.SelectedEnvs = append(s.SelectedEnvs, env)
			}
			dialogs.Store(userID, s)
			// Refresh env selection
			deleteMessages(s, cfg)
			s.Messages = []int{}
			sendEnvSelection(userID, chatID, cfg, s)
		} else if input == "next_env" {
			if len(s.SelectedEnvs) == 0 {
				sendMessage(cfg, chatID, "请至少选择一个环境。\nPlease select at least one environment.")
				return
			}
			s.Stage = "version"
			dialogs.Store(userID, s)
			deleteMessages(s, cfg)
			s.Messages = []int{}
			sendVersionInput(userID, chatID, cfg, s)
		} else if input == "cancel" {
			CancelDialog(userID, chatID, cfg)
		}
	case "version":
		s.Version = input
		s.Stage = "confirm"
		dialogs.Store(userID, s)
		sendConfirmation(userID, chatID, cfg, s)
	case "confirm":
		if input == "confirm" {
			submitTasks(userID, chatID, cfg, s)
		} else if input == "cancel" {
			CancelDialog(userID, chatID, cfg)
		}
	case "continue":
		if input == "yes" {
			// Cancel timeout if continuing
			select {
			case s.timeoutCancel <- true:
			default:
			}
			s.Stage = "service"
			s.Selected = []string{}
			s.SelectedEnvs = []string{}
			s.Version = ""
			s.Messages = []int{} // Reset messages for new dialog
			dialogs.Store(userID, s)
			sendServiceSelection(userID, chatID, cfg, s)
		} else if input == "no" {
			CancelDialog(userID, chatID, cfg)
		}
	}
}

func CancelDialog(userID, chatID int64, cfg *config.Config) bool {
	state, loaded := dialogs.Load(userID)
	if !loaded {
		log.Printf("No active dialog to cancel for user %d in chat %d", userID, chatID)
		return false
	}
	s := state.(*DialogState)
	// Cancel timeout
	select {
	case s.timeoutCancel <- true:
	default:
	}
	// Delete previous messages
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
		log.Printf("No service found for chat %d", chatID)
		return tgbotapi.Message{}, fmt.Errorf("no service found for chat %d", chatID)
	}
	token, ok := cfg.TelegramBots[service]
	if !ok {
		log.Printf("No bot configured for service: %s", service)
		return tgbotapi.Message{}, fmt.Errorf("no bot configured for service %s", service)
	}
	bot, err := tgbotapi.NewBotAPI(token)
	if err != nil {
		log.Printf("Failed to create bot for service %s: %v", service, err)
		return tgbotapi.Message{}, err
	}

	var msg tgbotapi.MessageConfig
	switch t := text.(type) {
	case string:
		msg = tgbotapi.NewMessage(chatID, t)
	case tgbotapi.MessageConfig:
		msg = t
	}
	msg.ParseMode = "Markdown"
	sentMsg, err := bot.Send(msg)
	if err != nil {
		log.Printf("Failed to send message to chat %d: %v", chatID, err)
		return tgbotapi.Message{}, err
	}
	// Log interaction
	storage.PersistInteraction(cfg, map[string]interface{}{
		"chat_id":   chatID,
		"message":   msg.Text,
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
		log.Printf("No service found for chat %d", s.ChatID)
		return
	}
	token, ok := cfg.TelegramBots[service]
	if !ok {
		log.Printf("No bot configured for service: %s", service)
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

func contains(slice []string, item string) bool {
	for _, s := range slice {
		if s == item {
			return true
		}
	}
	return false
}