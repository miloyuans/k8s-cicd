package telegram

import (
	"fmt"
	"log"
	"math/rand"
	"strings"
	"time"

	"k8s-cicd/internal/config"
	"k8s-cicd/internal/dialog"
	"k8s-cicd/internal/storage"
	"github.com/go-telegram-bot-api/telegram-bot-api/v5"
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

func SendTelegramNotification(cfg *config.Config, result *storage.DeployResult) {
	token, ok := cfg.TelegramBots[result.Request.Service]
	if !ok {
		log.Printf("No bot for service: %s", result.Request.Service)
		return
	}

	chatID, ok := cfg.TelegramChats[result.Request.Service]
	if !ok {
		log.Printf("No chat for service: %s", result.Request.Service)
		return
	}

	bot, err := tgbotapi.NewBotAPI(token)
	if err != nil {
		log.Printf("Failed to create bot: %v", err)
		return
	}

	var md strings.Builder
	if result.Success {
		md.WriteString(fmt.Sprintf(`
## 🚀 **Deployment Success**

**Service**: *%s*
**Environment**: *%s*
**New Version**: *%s*
**Old Image**: *%s*

✅ Deployment completed successfully!

---
**Deployed at**: %s
`, result.Request.Service, result.Request.Env, result.Request.Version, result.OldImage,
			result.Request.Timestamp.Format("2006-01-02 15:04:05")))
	} else {
		md.WriteString(fmt.Sprintf(`
## ❌ **Deployment Failed**

**Service**: *%s*
**Environment**: *%s*
**Version**: *%s*
**Error**: *%s*

### 🔍 **Diagnostics**

**Events**:
%s

**Environment Variables**:
`, result.Request.Service, result.Request.Env, result.Request.Version, result.ErrorMsg, result.Events))

		for k, v := range result.Envs {
			md.WriteString(fmt.Sprintf("• `%s`: %s\n", k, v))
		}

		md.WriteString(fmt.Sprintf(`
**Logs**: %s

⚠️ **Rollback completed**
---
**Failed at**: %s
`, result.Logs, result.Request.Timestamp.Format("2006-01-02 15:04:05")))
	}

	msg := tgbotapi.NewMessage(chatID, md.String())
	msg.ParseMode = "Markdown"
	_, err = bot.Send(msg)
	if err != nil {
		log.Printf("Failed to send telegram message: %v", err)
	}
}