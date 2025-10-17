package telegram

import (
	"fmt"
	"log"
	"math/rand"
	"regexp"
	"strings"
	"time"

	"github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"k8s-cicd/internal/config"
	"k8s-cicd/internal/dialog"
	"k8s-cicd/internal/queue"
	"k8s-cicd/internal/storage"
	"k8s-cicd/internal/types"
)

var bots map[string]*tgbotapi.BotAPI

func StartBot(cfg *config.Config, q *queue.Queue) {
	dialog.SetTaskQueue(q)
	bots = make(map[string]*tgbotapi.BotAPI)

	for service, token := range cfg.TelegramBots {
		bot, err := tgbotapi.NewBotAPI(token)
		if err != nil {
			log.Printf("Failed to create bot for service %s: %v", service, err)
			continue
		}
		bots[service] = bot
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

			triggerList := strings.Join(cfg.TriggerKeywords, ", ")
			response := fmt.Sprintf("请使用触发关键字（如 %s）开始部署。\nPlease use a trigger keyword (e.g., %s) to start a deployment.", triggerList, triggerList)
			if len(cfg.InvalidResponses) > 0 {
				response = cfg.InvalidResponses[rand.Intn(len(cfg.InvalidResponses))]
			}
			log.Printf("Invalid input from user %d in chat %d: %s, responding with: %s", userID, chatID, text, response)
			sendMessage(bot, chatID, response)
		} else if update.CallbackQuery != nil {
			chatID := update.CallbackQuery.Message.Chat.ID
			userID := update.CallbackQuery.From.ID
			data := update.CallbackQuery.Data

			log.Printf("Received callback query from user %d in chat %d: %s", userID, chatID, data)

			if cfg.TelegramChats[service] != chatID {
				log.Printf("Ignoring callback from chat %d: not in allowed chats for service %s", chatID, service)
				continue
			}

			if strings.HasPrefix(data, "confirm_api:") {
				id := strings.TrimPrefix(data, "confirm_api:")
				if tasks, ok := dialog.PendingConfirmations.Load(id); ok {
					taskList := tasks.([]types.DeployRequest)
					for _, task := range taskList {
						task.Status = "pending"
						dialog.GlobalTaskQueue.Enqueue(queue.Task{DeployRequest: task})
					}
					dialog.PendingConfirmations.Delete(id)
					sendMessage(bot, chatID, "Deployment confirmed via API.")
				} else {
					sendMessage(bot, chatID, "Confirmation ID not found or already processed.")
				}
			} else if strings.HasPrefix(data, "cancel_api:") {
				id := strings.TrimPrefix(data, "cancel_api:")
				dialog.PendingConfirmations.Delete(id)
				sendMessage(bot, chatID, "Deployment cancelled via API.")
			} else if dialog.IsDialogActive(userID) {
				log.Printf("Processing callback for user %d in chat %d: %s", userID, chatID, data)
				dialog.ProcessDialog(userID, chatID, data, cfg)
			} else {
				log.Printf("No active dialog for user %d in chat %d, ignoring callback: %s", userID, chatID, data)
			}

			callback := tgbotapi.NewCallback(update.CallbackQuery.ID, "")
			if _, err := bot.Send(callback); err != nil {
				log.Printf("Failed to answer callback query for user %d in chat %d: %v", userID, chatID, err)
			}
		}
	}
}

func sendMessage(bot *tgbotapi.BotAPI, chatID int64, text string) {
	msg := tgbotapi.NewMessage(chatID, text)
	msg.ParseMode = "HTML"
	if _, err := bot.Send(msg); err != nil {
		log.Printf("Failed to send message to chat %d: %v", chatID, err)
	}
}

func SendTelegramNotification(cfg *config.Config, result *storage.DeployResult) {
	category := classifyService(result.Request.Service, cfg.ServiceKeywords)
	chatID, ok := cfg.TelegramChats[category]
	if !ok {
		log.Printf("No chat configured for category %s, skipping notification for service %s", category, result.Request.Service)
		return
	}
	token, ok := cfg.TelegramBots[category]
	if !ok {
		log.Printf("No bot configured for category %s, skipping notification for service %s", category, result.Request.Service)
		return
	}
	bot, err := tgbotapi.NewBotAPI(token)
	if err != nil {
		log.Printf("Failed to create bot for category %s: %v", category, err)
		return
	}

	var md strings.Builder
	if result.Success {
		md.WriteString(fmt.Sprintf("<b>✅ 部署成功 / Deployment Succeeded</b>\n\n"))
		md.WriteString(fmt.Sprintf("服务 / Service: <b>%s</b>\n", result.Request.Service))
		md.WriteString(fmt.Sprintf("环境 / Environment: <b>%s</b>\n", result.Request.Env))
		md.WriteString(fmt.Sprintf("版本 / Version: <b>%s</b>\n", result.Request.Version))
		md.WriteString(fmt.Sprintf("旧版本 / Old Version: <b>%s</b>\n", getVersionFromImage(result.OldImage)))
		md.WriteString(fmt.Sprintf("提交用户 / Submitted by: <b>%s</b>\n", result.Request.UserName))
		md.WriteString("\n<b>✅ 部署成功完成！\n✅ Deployment completed successfully!</b>\n")
		md.WriteString(fmt.Sprintf("\n<b>部署时间 / Deployed at</b>: %s\n", result.Request.Timestamp.Format("2006-01-02 15:04:05")))
	} else {
		md.WriteString(fmt.Sprintf("<b>❌ 部署失败 / Deployment Failed</b>\n\n"))
		md.WriteString(fmt.Sprintf("服务 / Service: <b>%s</b>\n", result.Request.Service))
		md.WriteString(fmt.Sprintf("环境 / Environment: <b>%s</b>\n", result.Request.Env))
		md.WriteString(fmt.Sprintf("尝试版本 / Attempted Version: <b>%s</b>\n", result.Request.Version))
		md.WriteString(fmt.Sprintf("回滚版本 / Rollback Version: <b>%s</b>\n", getVersionFromImage(result.OldImage)))
		md.WriteString(fmt.Sprintf("错误 / Error: <b>%s</b>\n", result.ErrorMsg))
		md.WriteString(fmt.Sprintf("提交用户 / Submitted by: <b>%s</b>\n", result.Request.UserName))
		md.WriteString("\n<b>🔍 诊断信息 / Diagnostics</b>\n\n")
		md.WriteString(fmt.Sprintf("事件 / Events:\n%s\n", result.Events))
		md.WriteString("环境变量 / Environment Variables:\n")
		for k, v := range result.Envs {
			md.WriteString(fmt.Sprintf("- %s: <b>%s</b>\n", k, v))
		}
		md.WriteString(fmt.Sprintf("\n日志 / Logs: <b>%s</b>\n", result.Logs))
		md.WriteString(fmt.Sprintf("\n<b>失败时间 / Failed at</b>: %s\n", result.Request.Timestamp.Format("2006-01-02 15:04:05")))
	}

	msg := tgbotapi.NewMessage(chatID, md.String())
	msg.ParseMode = "HTML"

	const maxRetries = 3
	for attempt := 1; attempt <= maxRetries; attempt++ {
		_, err := bot.Send(msg)
		if err != nil {
			log.Printf("Failed to send notification to chat %d for service %s (attempt %d/%d): %v", chatID, result.Request.Service, attempt, maxRetries, err)
			if attempt == maxRetries {
				return
			}
			time.Sleep(time.Duration(attempt) * time.Second)
			continue
		}
		log.Printf("Successfully sent notification for service %s in env %s with success %t", result.Request.Service, result.Request.Env, result.Success)
		return
	}
}

func NotifyDeployTeam(cfg *config.Config, result *storage.DeployResult) {
	category := cfg.DeployCategory
	token, ok := cfg.TelegramBots[category]
	if !ok {
		log.Printf("Skipping deploy notification: no bot configured for category %s", category)
		return
	}

	chatID, ok := cfg.TelegramChats[category]
	if !ok {
		log.Printf("Skipping deploy notification: no chat configured for category %s", category)
		return
	}

	bot, err := tgbotapi.NewBotAPI(token)
	if err != nil {
		log.Printf("Failed to create bot for deploy category %s: %v", category, err)
		return
	}

	var md strings.Builder
	md.WriteString(fmt.Sprintf("<b>⚠️ CI/CD 部署失败，需要人工干预 / CI/CD Deployment Failed, Manual Intervention Needed</b>\n\n"))
	md.WriteString(fmt.Sprintf("服务 / Service: <b>%s</b>\n", result.Request.Service))
	md.WriteString(fmt.Sprintf("环境 / Environment: <b>%s</b>\n", result.Request.Env))
	md.WriteString(fmt.Sprintf("尝试版本 / Attempted Version: <b>%s</b>\n", result.Request.Version))
	md.WriteString(fmt.Sprintf("错误 / Error: <b>%s</b>\n", result.ErrorMsg))
	md.WriteString("\n<b>🔍 诊断信息 / Diagnostics</b>\n\n")
	md.WriteString(fmt.Sprintf("事件 / Events:\n%s\n", result.Events))
	md.WriteString("环境变量 / Environment Variables:\n")
	for k, v := range result.Envs {
		md.WriteString(fmt.Sprintf("- %s: <b>%s</b>\n", k, v))
	}
	md.WriteString(fmt.Sprintf("\n日志 / Logs: <b>%s</b>\n", result.Logs))
	md.WriteString(fmt.Sprintf("\n<b>失败时间 / Failed at</b>: %s\n", result.Request.Timestamp.Format("2006-01-02 15:04:05")))

	msg := tgbotapi.NewMessage(chatID, md.String())
	msg.ParseMode = "HTML"

	const maxRetries = 3
	for attempt := 1; attempt <= maxRetries; attempt++ {
		_, err := bot.Send(msg)
		if err != nil {
			log.Printf("Failed to send deploy notification to chat %d (attempt %d/%d): %v", chatID, attempt, maxRetries, err)
			if attempt == maxRetries {
				return
			}
			time.Sleep(time.Duration(attempt) * time.Second)
			continue
		}
		log.Printf("Successfully sent deploy notification for failed deployment of service %s in env %s", result.Request.Service, result.Request.Env)
		return
	}
}

func getVersionFromImage(image string) string {
	parts := strings.Split(image, ":")
	if len(parts) == 2 {
		return parts[1]
	}
	return "unknown"
}

func SendConfirmation(category string, chatID int64, message string, callbackData string) error {
	bot, ok := bots[category]
	if !ok {
		return fmt.Errorf("no bot for category %s", category)
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
	_, err := bot.Send(msg)
	return err
}

func classifyService(service string, keywords map[string][]string) string {
	lowerService := strings.ToLower(service)
	for category, patterns := range keywords {
		for _, pattern := range patterns {
			lowerPattern := strings.ToLower(pattern)
			if strings.HasPrefix(lowerPattern, "^") || strings.HasSuffix(lowerPattern, "$") || strings.Contains(lowerPattern, ".*") {
				re, err := regexp.Compile(lowerPattern)
				if err == nil && re.MatchString(lowerService) {
					return category
				}
			} else if strings.Contains(lowerService, lowerPattern) {
				return category
			}
		}
	}
	return ""
}