// dialog.go
package dialog

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/go-telegram-bot-api/telegram-bot-api/v5"
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
	if state, loaded := dialogs.Load(userID); loaded {
		log.Printf("Cancelling existing dialog for user %d in chat %d", userID, chatID)
		CancelDialog(userID, chatID, cfg) // Reset stuck dialog
		sendMessage(cfg, chatID, "Previous dialog was active and has been cancelled. Starting new deployment dialog.")
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
	go monitorDialogTimeout(userID, chatID, cfg, state) // Ensure timeout monitor starts
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

	// Build multi-column button layout with visual marks
	var buttons [][]tgbotapi.InlineKeyboardButton
	var row []tgbotapi.InlineKeyboardButton
	for i, svc := range services {
		prefix := ""
		if contains(s.Selected, svc) {
			prefix = "✓ " // Visual mark for selected
		}
		row = append(row, tgbotapi.NewInlineKeyboardButtonData(prefix+svc, svc))
		if len(row) == cols || i == len(services)-1 {
			buttons = append(buttons, row)
			row = []tgbotapi.InlineKeyboardButton{}
		}
	}

	// Add Next and Cancel buttons
	buttons = append(buttons, []tgbotapi.InlineKeyboardButton{
		tgbotapi.NewInlineKeyboardButtonData("下一步 / Next", "next_service"),
		tgbotapi.NewInlineKeyboardButtonData("取消 / Cancel", "cancel"),
	})

	log.Printf("Generated %d rows with %d columns for %d services for user %d", len(buttons), cols, len(services), userID)

	keyboard := tgbotapi.NewInlineKeyboardMarkup(buttons...)
	msgText := "请选择一个或多个服务（已选将标记 ✓）：\nPlease select one or more services (selected will be marked ✓):"
	if len(s.Selected) > 0 {
		msgText += fmt.Sprintf("\n已选 / Selected: %s", strings.Join(s.Selected, ", "))
	}
	msg := tgbotapi.NewMessage(chatID, msgText)
	msg.ReplyMarkup = keyboard
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
			if len(s.Selected) == 0 {
				sendMessage(cfg, chatID, "请至少选择一个服务。\nPlease select at least one service.")
				return
			}
			s.Stage = "env"
			dialogs.Store(userID, s)
			sendEnvSelection(userID, chatID, cfg, s)
		} else if input == "cancel" {
			CancelDialog(userID, chatID, cfg)
		} else {
			// Toggle selection
			if contains(s.Selected, input) {
				s.Selected = remove(s.Selected, input)
			} else {
				s.Selected = append(s.Selected, input)
			}
			dialogs.Store(userID, s)
			// Refresh keyboard by editing the message
			serviceLists, err := config.LoadServiceLists(cfg.ServicesDir, cfg.TelegramBots)
			if err != nil {
				log.Printf("Failed to load service lists: %v", err)
				return
			}
			services, exists := serviceLists[s.Service]
			if !exists {
				log.Printf("No service list found for %s", s.Service)
				return
			}
			var buttons [][]tgbotapi.InlineKeyboardButton
			var row []tgbotapi.InlineKeyboardButton
			cols := 2
			if len(services) < cols {
				cols = len(services)
			}
			for i, svc := range services {
				prefix := ""
				if contains(s.Selected, svc) {
					prefix = "✓ "
				}
				row = append(row, tgbotapi.NewInlineKeyboardButtonData(prefix+svc, svc))
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
			msgText := "请选择一个或多个服务（已选将标记 ✓）：\nPlease select one or more services (selected will be marked ✓):"
			if len(s.Selected) > 0 {
				msgText += fmt.Sprintf("\n已选 / Selected: %s", strings.Join(s.Selected, ", "))
			}
			edit := tgbotapi.EditMessageTextConfig{
				BaseEdit: tgbotapi.BaseEdit{
					ChatID:    chatID,
					MessageID: s.Messages[len(s.Messages)-1],
				},
				Text:      msgText,
				ParseMode: "Markdown",
			}
			edit.ReplyMarkup = &keyboard
			if _, err := sendMessage(cfg, chatID, edit); err != nil {
				log.Printf("Failed to edit message: %v", err)
			}
		}
	case "env":
		// Existing env handling (assumed unchanged)
		if contains(getEnvironmentsFromDeployFile(cfg), input) {
			s.SelectedEnvs = append(s.SelectedEnvs, input)
			s.Stage = "version"
			dialogs.Store(userID, s)
			sendVersionPrompt(userID, chatID, cfg, s)
		} else if input == "cancel" {
			CancelDialog(userID, chatID, cfg)
		} else {
			sendMessage(cfg, chatID, "Invalid environment. Please select a valid environment.")
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
			select {
			case s.timeoutCancel <- true:
			default:
			}
			s.Stage = "service"
			s.Selected = []string{}
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
	// Placeholder for env selection (assumed unchanged)
}

func sendConfirmation(userID, chatID int64, cfg *config.Config, s *DialogState) {
	// Placeholder for confirmation (assumed unchanged)
}

func submitTasks(userID, chatID int64, cfg *config.Config, s *DialogState) {
	// Placeholder for task submission (assumed unchanged)
}

func sendVersionPrompt(userID, chatID int64, cfg *config.Config, s *DialogState) {
	// Placeholder for version prompt (assumed unchanged)
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

	var sentMsg tgbotapi.Message
	switch t := text.(type) {
	case string:
		msg := tgbotapi.NewMessage(chatID, t)
		msg.ParseMode = "Markdown"
		sentMsg, err = bot.Send(msg)
	case tgbotapi.MessageConfig:
		t.ParseMode = "Markdown"
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