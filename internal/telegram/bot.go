package telegram

import (
	"log"
	"math/rand"
	"time"

	"github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"github.com/yourusername/k8s-cicd/internal/config"
	"github.com/yourusername/k8s-cicd/internal/dialog"
	"github.com/yourusername/k8s-cicd/internal/storage"
)

func StartBot(cfg *config.Config) {
	for service, token := range cfg.TelegramBots {
		bot, err := tgbotapi.NewBotAPI(token)
		if err != nil {
			log.Printf("Failed to create bot for %s: %v", service, err)
			continue
		}
		go handleBot(bot, cfg, service)
	}
}

func handleBot(bot *tgbotapi.BotAPI, cfg *config.Config, service string) {
	u := tgbotapi.NewUpdate(0)
	u.Timeout = 60
	updates, err := bot.GetUpdatesChan(u)
	if err != nil {
		log.Printf("Failed to get updates for %s: %v", service, err)
		return
	}

	for update := range updates {
		if update.Message == nil {
			continue
		}

		chatID := update.Message.Chat.ID
		userID := update.Message.From.ID
		text := update.Message.Text

		if cfg.TelegramChats[service] != chatID {
			continue
		}

		if text == cfg.TriggerKeyword {
			dialog.StartDialog(userID, chatID, service, cfg)
			continue
		}

		if text == cfg.CancelKeyword {
			if dialog.CancelDialog(userID, chatID, cfg) {
				sendMessage(bot, chatID, "Dialog cancelled.")
			}
			continue
		}

		if dialog.IsDialogActive(userID, chatID) {
			dialog.ProcessDialog(userID, chatID, text, cfg)
			continue
		}

		if strings.Contains(text, cfg.TriggerKeyword) {
			storage.PersistTelegramMessage(cfg, storage.TelegramMessage{
				UserID:    userID,
				ChatID:    chatID,
				Content:   text,
				Timestamp: time.Now(),
			})
			dialog.StartDialog(userID, chatID, service, cfg)
		} else {
			response := cfg.InvalidResponses[rand.Intn(len(cfg.InvalidResponses))]
			sendMessage(bot, chatID, response)
		}
	}
}

func sendMessage(bot *tgbotapi.BotAPI, chatID int64, text string) {
	msg := tgbotapi.NewMessage(chatID, text)
	msg.ParseMode = "Markdown"
	bot.Send(msg)
}