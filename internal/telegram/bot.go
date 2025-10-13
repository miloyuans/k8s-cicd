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
			text := update.Message.Text

			if cfg.TelegramChats[service] != chatID {
				log.Printf("Ignoring message from chat %d: not in allowed chats for service %s", chatID, service)
				continue
			}

			// Check for trigger keywords
			for _, trigger := range cfg.TriggerKeywords {
				if text == trigger || strings.Contains(text, trigger) {
					log.Printf("User %d triggered dialog via keyword %s for service %s in chat %d", userID, trigger, service, chatID)
					storage.PersistTelegramMessage(cfg, storage.TelegramMessage{
						UserID:    userID,
						ChatID:    chatID,
						Content:   text,
						Timestamp: time.Now(),
					})
					dialog.StartDialog(userID, chatID, service, cfg)
					continue
				}
			}

			// Check for cancel keywords
			for _, cancel := range cfg.CancelKeywords {
				if text == cancel {
					log.Printf("User %d requested to cancel dialog in chat %d", userID, chatID)
					if dialog.CancelDialog(userID, chatID, cfg) {
						sendMessage(bot, chatID, "对话已取消。\nDialog cancelled.")
					} else {
						log.Printf("No active dialog to cancel for user %d in chat %d", userID, chatID)
					}
					continue
				}
			}

			if dialog.IsDialogActive(userID, chatID) {
				log.Printf("Processing dialog input for user %d in chat %d: %s", userID, chatID, text)
				dialog.ProcessDialog(userID, chatID, text, cfg)
				continue
			}

			response := cfg.InvalidResponses[rand.Intn(len(cfg.InvalidResponses))]
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

			if dialog.IsDialogActive(userID, chatID) {
				log.Printf("Processing callback for user %d in chat %d: %s", userID, chatID, data)
				dialog.ProcessDialog(userID, chatID, data, cfg)
			} else {
				log.Printf("No active dialog for user %d in chat %d, ignoring callback: %s", userID, chatID, data)
			}

			// Answer the callback query to remove the loading state
			callback := tgbotapi.NewCallback(update.CallbackQuery.ID, "")
			if _, err := bot.Request(callback); err != nil {
				log.Printf("Failed to answer callback query for user %d: %v", userID, err)
			}
		}
	}
}

func sendMessage(bot *tgbotapi.BotAPI, chatID int64, text string) {
	msg := tgbotapi.NewMessage(chatID, text)
	msg.ParseMode = "Markdown"
	if _, err := bot.Send(msg); err != nil {
		log.Printf("Failed to send message to chat %d: %v", chatID, err)
	}
}

func SendTelegramNotification(cfg *config.Config, result *storage.DeployResult) {
	token, ok := cfg.TelegramBots[result.Request.Service]
	if !ok {
		log.Printf("No bot configured for service: %s", result.Request.Service)
		return
	}

	chatID, ok := cfg.TelegramChats[result.Request.Service]
	if !ok {
		log.Printf("No chat configured for service: %s", result.Request.Service)
		return
	}

	bot, err := tgbotapi.NewBotAPI(token)
	if err != nil {
		log.Printf("Failed to create bot for service %s: %v", result.Request.Service, err)
		return
	}

	var md strings.Builder
	if result.Success {
		md.WriteString(fmt.Sprintf(`
## 🚀 **部署成功 / Deployment Success**

**服务 / Service**: *%s*  
**环境 / Environment**: *%s*  
**新版本 / New Version**: *%s*  
**旧镜像 / Old Image**: *%s*  

✅ 部署成功完成！  
✅ Deployment completed successfully!

---
**部署时间 / Deployed at**: %s
`, result.Request.Service, result.Request.Env, result.Request.Version, result.OldImage,
			result.Request.Timestamp.Format("2006-01-02 15:04:05")))
	} else {
		md.WriteString(fmt.Sprintf(`
## ❌ **部署失败 / Deployment Failed**

**服务 / Service**: *%s*  
**环境 / Environment**: *%s*  
**版本 / Version**: *%s*  
**错误 / Error**: *%s*  

### 🔍 **诊断信息 / Diagnostics**

**事件 / Events**:  
%s

**环境变量 / Environment Variables**:  
`, result.Request.Service, result.Request.Env, result.Request.Version, result.ErrorMsg, result.Events))

		for k, v := range result.Envs {
			md.WriteString(fmt.Sprintf("• `%s`: %s\n", k, v))
		}

		md.WriteString(fmt.Sprintf(`
**日志 / Logs**: %s  

⚠️ **回滚完成 / Rollback completed**  
---
**失败时间 / Failed at**: %s
`, result.Logs, result.Request.Timestamp.Format("2006-01-02 15:04:05")))
	}

	msg := tgbotapi.NewMessage(chatID, md.String())
	msg.ParseMode = "Markdown"
	if _, err := bot.Send(msg); err != nil {
		log.Printf("Failed to send telegram notification to chat %d: %v", chatID, err)
	}
}