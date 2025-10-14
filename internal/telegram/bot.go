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
	// Find category for service
	category := ""
	for keyword, cat := range cfg.ServiceKeywords {
		if strings.Contains(result.Request.Service, keyword) {
			category = cat
			break
		}
	}
	if category == "" {
		log.Printf("No category found for service: %s", result.Request.Service)
		return
	}

	token, ok := cfg.TelegramBots[category]
	if !ok {
		log.Printf("No bot configured for category: %s", category)
		return
	}

	chatID, ok := cfg.TelegramChats[category]
	if !ok {
		log.Printf("No chat configured for category: %s", category)
		return
	}

	bot, err := tgbotapi.NewBotAPI(token)
	if err != nil {
		log.Printf("Failed to create bot for category %s: %v", category, err)
		return
	}

	var md strings.Builder
	if result.Success {
		md.WriteString(fmt.Sprintf("**🚀 部署成功 / Deployment Success**\n\n"))
		md.WriteString(fmt.Sprintf("服务 / Service: **%s**\n", result.Request.Service))
		md.WriteString(fmt.Sprintf("环境 / Environment: **%s**\n", result.Request.Env))
		md.WriteString(fmt.Sprintf("新版本 / New Version: **%s**\n", result.Request.Version))
		md.WriteString(fmt.Sprintf("旧镜像 / Old Image: **%s**\n", result.OldImage))
		md.WriteString(fmt.Sprintf("提交用户 / Submitted by: **%s**\n", result.Request.UserName))
		md.WriteString("\n✅ 部署成功完成！\n✅ Deployment completed successfully!\n")
		md.WriteString(fmt.Sprintf("\n**部署时间 / Deployed at**: %s\n", result.Request.Timestamp.Format("2006-01-02 15:04:05")))
	} else {
		md.WriteString(fmt.Sprintf("**❌ 部署失败 / Deployment Failed**\n\n"))
		md.WriteString(fmt.Sprintf("服务 / Service: **%s**\n", result.Request.Service))
		md.WriteString(fmt.Sprintf("环境 / Environment: **%s**\n", result.Request.Env))
		md.WriteString(fmt.Sprintf("版本 / Version: **%s**\n", result.Request.Version))
		md.WriteString(fmt.Sprintf("错误 / Error: **%s**\n", result.ErrorMsg))
		md.WriteString(fmt.Sprintf("提交用户 / Submitted by: **%s**\n", result.Request.UserName))
		md.WriteString("\n**🔍 诊断信息 / Diagnostics**\n\n")
		md.WriteString(fmt.Sprintf("事件 / Events:\n%s\n", result.Events))
		md.WriteString("环境变量 / Environment Variables:\n")
		for k, v := range result.Envs {
			md.WriteString(fmt.Sprintf("- %s: **%s**\n", k, v))
		}
		md.WriteString(fmt.Sprintf("\n日志 / Logs: **%s**\n", result.Logs))
		md.WriteString("\n⚠️ **回滚完成 / Rollback completed**\n")
		md.WriteString(fmt.Sprintf("\n**失败时间 / Failed at**: %s\n", result.Request.Timestamp.Format("2006-01-02 15:04:05")))
	}

	msg := tgbotapi.NewMessage(chatID, md.String())
	msg.ParseMode = "Markdown"
	if _, err := bot.Send(msg); err != nil {
		log.Printf("Failed to send telegram notification to chat %d: %v", chatID, err)
	}
}