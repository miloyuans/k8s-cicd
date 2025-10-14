package telegram

import (
	"fmt"
	"log"
	"math/rand"
	"strings"

	"k8s-cicd/internal/config"
	"k8s-cicd/internal/dialog"
	"k8s-cicd/internal/queue"
	"k8s-cicd/internal/storage"
	"github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

func StartBot(cfg *config.Config, q *queue.Queue) {
	dialog.SetTaskQueue(q)

	for service, token := range cfg.TelegramBots {
		bot, err := tgbotapi.NewBotAPI(token)
		if err != nil {
			log.Printf("Failed to create bot for service %s: %v", service, err)
			continue
		}
		log.Printf("Started bot for service %s", service)
		go handleBot(bot, cfg, service)
	}
}

func handleBot(bot *tgbotapi.BotAPI, cfg *config.Config, service string) {
	u := tgbotapi.NewUpdate(0)
	u.Timeout = 60
	updates := bot.GetUpdatesChan(u)

	for update := range updates {
		if update.Message != nil {
			chatID := update.Message.Chat.ID
			userID := update.Message.From.ID
			text := strings.TrimSpace(update.Message.Text)
			userName := update.Message.From.UserName

			if cfg.TelegramChats[service] != chatID {
				log.Printf("Ignoring message from chat %d: not in allowed chats for service %s", chatID, service)
				continue
			}

			// Check for trigger keywords
			triggered := false
			for _, trigger := range cfg.TriggerKeywords {
				if text == trigger {
					log.Printf("User %d triggered dialog via keyword %s for service %s in chat %d", userID, trigger, service, chatID)
					dialog.StartDialog(userID, chatID, service, cfg, userName)
					triggered = true
					break
				}
			}
			if triggered {
				continue
			}

			// Check for cancel keywords
			canceled := false
			for _, cancel := range cfg.CancelKeywords {
				if text == cancel {
					log.Printf("User %d requested to cancel dialog in chat %d", userID, chatID)
					dialog.CancelDialog(userID, chatID, cfg)
					canceled = true
					break
				}
			}
			if canceled {
				continue
			}

			if dialog.IsDialogActive(userID) {
				log.Printf("Processing dialog input for user %d in chat %d: %s", userID, chatID, text)
				dialog.ProcessDialog(userID, chatID, text, cfg)
				continue
			}

			// If no trigger or cancel keyword matched, send invalid response
			triggerList := strings.Join(cfg.TriggerKeywords, ", ")
			response := fmt.Sprintf("请使用触发关键字（如 %s）开始部署。\nPlease use a trigger keyword (e.g., %s) to start a deployment.", triggerList, triggerList)
			if len(cfg.InvalidResponses) > 0 {
				response = cfg.InvalidResponses[rand.Intn(len(cfg.InvalidResponses))]
			}
			log.Printf("Invalid input from user %d in chat %d: %s, responding with: %s", userID, chatID, text, response)
			sendMessage(bot, chatID, response)
		} else if update.CallbackQuery != nil {
			// Handle Inline Keyboard button clicks
			chatID := update.CallbackQuery.Message.Chat.ID
			userID := update.CallbackQuery.From.ID
			data := update.CallbackQuery.Data

			log.Printf("Received callback query from user %d in chat %d: %s", userID, chatID, data)

			if cfg.TelegramChats[service] != chatID {
				log.Printf("Ignoring callback from chat %d: not in allowed chats for service %s", chatID, service)
				continue
			}

			if dialog.IsDialogActive(userID) {
				log.Printf("Processing callback for user %d in chat %d: %s", userID, chatID, data)
				dialog.ProcessDialog(userID, chatID, data, cfg)