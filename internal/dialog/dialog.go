package dialog

import (
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"k8s-cicd/internal/config"
	"k8s-cicd/internal/storage"
	"github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

type DeployRequest struct {
	Service   string    `json:"service"`
	Env       string    `json:"env"`
	Version   string    `json:"version"`
	Timestamp time.Time `json:"timestamp"`
}

type DialogState struct {
	UserID    int64
	ChatID    int64
	Service   string
	Stage     string // "service", "env", "version"
	Selected  []string
	StartedAt time.Time
}

var (
	dialogs   sync.Map // map[int64]*DialogState
	taskQueue sync.Map // map[string][]DeployRequest
)

func StartDialog(userID, chatID int64, service string, cfg *config.Config) {
	if _, loaded := dialogs.Load(userID); loaded {
		sendMessage(cfg, chatID, "You already have an active dialog. Please complete or cancel it.")
		return
	}

	dialogs.Store(userID, &DialogState{
		UserID:    userID,
		ChatID:    chatID,
		Service:   service,
		Stage:     "service",
		StartedAt: time.Now(),
	})

	go monitorDialogTimeout(userID, chatID, cfg)

	serviceLists, err := config.LoadServiceLists(cfg.ServicesDir)
	if err != nil {
		sendMessage(cfg, chatID, "Failed to load service lists.")
		return
	}
	services := serviceLists[service]
	if len(services) == 0 {
		sendMessage(cfg, chatID, "No services available.")
		return
	}

	var buttons [][]tgbotapi.InlineKeyboardButton
	for _, svc := range services {
		buttons = append(buttons, []tgbotapi.InlineKeyboardButton{
			tgbotapi.NewInlineKeyboardButtonData(svc, svc),
		})
	}
	keyboard := tgbotapi.NewInlineKeyboardMarkup(buttons...)
	msg := tgbotapi.NewMessage(chatID, "Please select a service:")
	msg.ReplyMarkup = keyboard
	sendMessage(cfg, chatID, msg)
}

func ProcessDialog(userID, chatID int64, text string, cfg *config.Config) {
	state, ok := dialogs.Load(userID)
	if !ok {
		return
	}
	s := state.(*DialogState)

	switch s.Stage {
	case "service":
		serviceLists, _ := config.LoadServiceLists(cfg.ServicesDir)
		if !contains(serviceLists[s.Service], text) {
			sendMessage(cfg, chatID, "Invalid service. Please select a valid service.")
			return
		}
		s.Selected = append(s.Selected, text)
		s.Stage = "env"

		var buttons [][]tgbotapi.InlineKeyboardButton
		for _, env := range cfg.Environments {
			buttons = append(buttons, []tgbotapi.InlineKeyboardButton{
				tgbotapi.NewInlineKeyboardButtonData(env, env),
			})
		}
		buttons = append(buttons, []tgbotapi.InlineKeyboardButton{
			tgbotapi.NewInlineKeyboardButtonData("Done", "done"),
		})
		keyboard := tgbotapi.NewInlineKeyboardMarkup(buttons...)
		msg := tgbotapi.NewMessage(chatID, "Please select environment(s) (multi-select, press Done when finished):")
		msg.ReplyMarkup = keyboard
		sendMessage(cfg, chatID, msg)

	case "env":
		if text == "done" {
			if len(s.Selected) == 0 {
				sendMessage(cfg, chatID, "Please select at least one environment.")
				return
			}
			s.Stage = "version"
			msg := tgbotapi.NewMessage(chatID, "Please enter the version:")
			sendMessage(cfg, chatID, msg)
		} else if contains(cfg.Environments, text) {
			if !contains(s.Selected, text) {
				s.Selected = append(s.Selected, text)
			}
			sendMessage(cfg, chatID, "Environment added. Select another or press Done.")
		} else {
			sendMessage(cfg, chatID, "Invalid environment. Please select a valid environment.")
		}

	case "version":
		for _, env := range s.Selected[1:] { // First element is service
			task := DeployRequest{
				Service:   s.Selected[0],
				Env:       env,
				Version:   text,
				Timestamp: time.Now(),
			}
			taskList, _ := taskQueue.LoadOrStore(s.Service, []DeployRequest{})
			taskQueue.Store(s.Service, append(taskList.([]DeployRequest), task))
			storage.PersistTelegramMessage(cfg, storage.TelegramMessage{
				UserID:    userID,
				ChatID:    chatID,
				Content:   fmt.Sprintf("Deploy: %s, Env: %s, Version: %s", s.Selected[0], env, text),
				Timestamp: time.Now(),
			})
		}
		sendMessage(cfg, chatID, "Deployment task(s) submitted.")
		dialogs.Delete(userID)
	}
}

func CancelDialog(userID, chatID int64, cfg *config.Config) bool {
	if _, loaded := dialogs.LoadAndDelete(userID); loaded {
		sendMessage(cfg, chatID, "Dialog cancelled.")
		return true
	}
	return false
}

func IsDialogActive(userID, chatID int64) bool {
	_, ok := dialogs.Load(userID)
	return ok
}

func monitorDialogTimeout(userID, chatID int64, cfg *config.Config) {
	time.Sleep(time.Duration(cfg.DialogTimeout) * time.Second)
	if state, loaded := dialogs.LoadAndDelete(userID); loaded {
		s := state.(*DialogState)
		if s.ChatID == chatID {
			sendMessage(cfg, chatID, "Dialog timed out after 5 minutes. Please start a new dialog if needed.")
		}
	}
}

func sendMessage(cfg *config.Config, chatID int64, text interface{}) {
	service := ""
	for svc, id := range cfg.TelegramChats {
		if id == chatID {
			service = svc
			break
		}
	}
	if service == "" {
		log.Printf("No service found for chat %d", chatID)
		return
	}
	token, ok := cfg.TelegramBots[service]
	if !ok {
		log.Printf("No bot for service: %s", service)
		return
	}
	bot, err := tgbotapi.NewBotAPI(token)
	if err != nil {
		log.Printf("Failed to create bot: %v", err)
		return
	}

	var msg tgbotapi.MessageConfig
	switch t := text.(type) {
	case string:
		msg = tgbotapi.NewMessage(chatID, t)
	case tgbotapi.MessageConfig:
		msg = t
	}
	msg.ParseMode = "Markdown"
	_, err = bot.Send(msg)
	if err != nil {
		log.Printf("Failed to send telegram message: %v", err)
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