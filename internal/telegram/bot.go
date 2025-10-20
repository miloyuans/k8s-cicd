// internal/telegram/bot.go
package telegram

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"math/rand"
	"net/http"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"k8s-cicd/internal/config"
	"k8s-cicd/internal/dialog"
	"k8s-cicd/internal/queue"
	"k8s-cicd/internal/storage"
	"k8s-cicd/internal/types"
)

var (
	bots             map[string]*tgbotapi.BotAPI
	compiledKeywords map[string][]*regexp.Regexp
	keywordsMu       sync.Mutex
)

func StartBot(cfg *config.Config, q *queue.Queue) {
	dialog.SetTaskQueue(q)
	bots = make(map[string]*tgbotapi.BotAPI)
	compiledKeywords = make(map[string][]*regexp.Regexp)

	// Precompile regex patterns
	for category, patterns := range cfg.ServiceKeywords {
		var compiled []*regexp.Regexp
		for _, pattern := range patterns {
			lowerPattern := strings.ToLower(pattern)
			if strings.HasPrefix(lowerPattern, "^") || strings.HasSuffix(lowerPattern, "$") || strings.Contains(lowerPattern, ".*") {
				re, err := regexp.Compile(lowerPattern)
				if err != nil {
					log.Printf("Failed to compile regex pattern %s for category %s: %v", lowerPattern, category, err)
					continue
				}
				compiled = append(compiled, re)
			}
		}
		compiledKeywords[category] = compiled
	}

	// Validate Telegram configuration
	hasErrors := false
	for category := range cfg.TelegramBots {
		if _, ok := cfg.TelegramChats[category]; !ok {
			log.Printf("Error: No chat ID configured for category %s", category)
			hasErrors = true
		}
	}
	if _, ok := cfg.TelegramChats["other"]; !ok {
		log.Printf("Error: No default chat ID configured for category 'other'")
		hasErrors = true
	}
	if _, ok := cfg.TelegramBots["other"]; !ok {
		log.Printf("Error: No bot configured for category 'other'")
		hasErrors = true
	}
	if cfg.DeployCategory != "" && cfg.DeployCategory != "other" {
		if _, ok := cfg.TelegramChats[cfg.DeployCategory]; !ok {
			log.Printf("Error: No chat ID configured for deploy category %s", cfg.DeployCategory)
			hasErrors = true
		}
		if _, ok := cfg.TelegramBots[cfg.DeployCategory]; !ok {
			log.Printf("Error: No bot configured for deploy category %s", cfg.DeployCategory)
			hasErrors = true
		}
	}
	if hasErrors {
		log.Fatal("Critical Telegram configuration errors detected, exiting")
	}

	// Custom HTTP client for Telegram API
	httpClient := &http.Client{
		Timeout: 30 * time.Second,
		Transport: &http.Transport{
			TLSHandshakeTimeout:   10 * time.Second,
			IdleConnTimeout:       30 * time.Second,
			MaxIdleConns:          100,
			MaxIdleConnsPerHost:   10,
			ForceAttemptHTTP2:     true, // Explicitly enable HTTP/2
		},
	}

	for service, token := range cfg.TelegramBots {
		bot, err := tgbotapi.NewBotAPIWithClient(token, tgbotapi.APIEndpoint, httpClient)
		if err != nil {
			log.Printf("Failed to create bot for service %s: %v", service, err)
			continue
		}
		bots[service] = bot
		log.Printf("Started bot for service %s", service)
		go handleBot(bot, cfg, service)
	}
}

func GetBot(service string) (*tgbotapi.BotAPI, error) {
	bot, ok := bots[service]
	if !ok {
		return nil, fmt.Errorf("no bot configured for service %s", service)
	}
	return bot, nil
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

			log.Printf("Received message from user %d in chat %d: %s", userID, chatID, text)

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
			response := fmt.Sprintf("请使用触发关键字（如 %s）开始部署。\nPlease use a trigger keyword (e.g., %s) to start deployment.", triggerList, triggerList)
			sendMessage(bot, chatID, response)
		} else if update.CallbackQuery != nil {
			callbackData := update.CallbackQuery.Data
			chatID := update.CallbackQuery.Message.Chat.ID
			messageID := update.CallbackQuery.Message.MessageID
			userID := update.CallbackQuery.From.ID

			if strings.HasPrefix(callbackData, "confirm_api:") {
				id := strings.TrimPrefix(callbackData, "confirm_api:")
				if tasks, ok := dialog.PendingConfirmations.Load(id); ok {
					for _, t := range tasks.([]types.DeployRequest) {
						queue.GlobalTaskQueue.ConfirmTask(t)
					}
					dialog.PendingConfirmations.Delete(id)
					edit := tgbotapi.NewEditMessageText(chatID, messageID, update.CallbackQuery.Message.Text+"\n\n已确认 / Confirmed")
					bot.Send(edit)
				}
			} else if strings.HasPrefix(callbackData, "cancel_api:") {
				id := strings.TrimPrefix(callbackData, "cancel_api:")
				dialog.PendingConfirmations.Delete(id)
				edit := tgbotapi.NewEditMessageText(chatID, messageID, update.CallbackQuery.Message.Text+"\n\n已取消 / Cancelled")
				bot.Send(edit)
			} else if dialog.IsDialogActive(userID) {
				// 处理对话回调
				if strings.HasPrefix(callbackData, "service:") {
					service := strings.TrimPrefix(callbackData, "service:")
					dialog.ProcessDialog(userID, chatID, service, cfg)
				} else if strings.HasPrefix(callbackData, "env:") {
					env := strings.TrimPrefix(callbackData, "env:")
					dialog.ProcessDialog(userID, chatID, env, cfg)
				} else {
					dialog.ProcessDialog(userID, chatID, callbackData, cfg)
				}
			}
		}
	}
}

func SendTelegramNotification(cfg *config.Config, result *storage.DeployResult) error {
	category := ClassifyService(result.Request.Service, cfg.ServiceKeywords)
	chatID, ok := cfg.TelegramChats[category]
	if !ok {
		log.Printf("No chat configured for category %s, using default 'other'", category)
		if defaultChatID, ok := cfg.TelegramChats["other"]; ok {
			chatID = defaultChatID
		} else {
			log.Printf("No default chat configured for category %s", category)
			return fmt.Errorf("no chat configured for category %s", category)
		}
	}
	token, ok := cfg.TelegramBots[category]
	if !ok {
		log.Printf("No bot configured for category %s, using default 'other'", category)
		if defaultToken, ok := cfg.TelegramBots["other"]; ok {
			token = defaultToken
		} else {
			log.Printf("No default bot configured for category %s", category)
			return fmt.Errorf("no bot configured for category %s", category)
		}
	}
	httpClient := &http.Client{
		Timeout: 30 * time.Second,
		Transport: &http.Transport{
			TLSHandshakeTimeout:   10 * time.Second,
			IdleConnTimeout:       30 * time.Second,
			MaxIdleConns:          100,
			MaxIdleConnsPerHost:   10,
			ForceAttemptHTTP2:     true,
		},
	}
	bot, err := tgbotapi.NewBotAPIWithClient(token, tgbotapi.APIEndpoint, httpClient)
	if err != nil {
		log.Printf("Failed to create bot for category %s: %v", category, err)
		return logToFile(fmt.Sprintf("Failed to create bot for category %s: %v", category, err), result)
	}

	var md strings.Builder
	if result.Success {
		md.WriteString(fmt.Sprintf("<b>✅ 部署成功 / Deployment Succeeded</b>\n\n"))
	} else {
		md.WriteString(fmt.Sprintf("<b>❌ 部署失败 / Deployment Failed</b>\n\n"))
	}
	md.WriteString(fmt.Sprintf("服务 / Service: <b>%s</b>\n", result.Request.Service))
	md.WriteString(fmt.Sprintf("环境 / Environment: <b>%s</b>\n", result.Request.Env))
	md.WriteString(fmt.Sprintf("版本 / Version: <b>%s</b>\n", result.Request.Version))
	if !result.Success {
		md.WriteString(fmt.Sprintf("错误 / Error: <b>%s</b>\n", result.ErrorMsg))
		md.WriteString(fmt.Sprintf("旧版本 / Old Version: <b>%s</b>\n", getVersionFromImage(result.OldImage)))
		md.WriteString(fmt.Sprintf("\n事件 / Events:\n<pre>%s</pre>\n", result.Events))
		md.WriteString(fmt.Sprintf("\n日志 / Logs:\n<pre>%s</pre>\n", result.Logs))
	}
	md.WriteString(fmt.Sprintf("\n<b>由用户 / By user</b>: %s\n", result.Request.UserName))
	md.WriteString(fmt.Sprintf("<b>时间 / Time</b>: %s\n", result.Request.Timestamp.Format("2006-01-02 15:04:05")))

	const maxRetries = 5
	for attempt := 1; attempt <= maxRetries; attempt++ {
		err := sendMessage(bot, chatID, md.String())
		if err == nil {
			log.Printf("Successfully sent notification for deployment of service %s in env %s", result.Request.Service, result.Request.Env)
			return nil
		}
		if apiErr, ok := err.(tgbotapi.APIError); ok && apiErr.ErrorCode == 429 {
			retryAfter := 1 * time.Second
			if apiErr.RetryAfter > 0 {
				retryAfter = time.Duration(apiErr.RetryAfter) * time.Second
			}
			log.Printf("Rate limit hit for notification to chat %d (attempt %d/%d), retrying after %v", chatID, attempt, maxRetries, retryAfter)
			time.Sleep(retryAfter)
			continue
		}
		log.Printf("Failed to send notification to chat %d (attempt %d/%d): %v", chatID, attempt, maxRetries, err)
		if attempt == maxRetries {
			return logToFile(fmt.Sprintf("Failed to send notification after %d attempts: %v", maxRetries, err), result)
		}
		time.Sleep(time.Duration(attempt) * time.Second)
	}
	return fmt.Errorf("failed to send notification after %d attempts", maxRetries)
}

func sendMessage(bot *tgbotapi.BotAPI, chatID int64, text string) error {
	msg := tgbotapi.NewMessage(chatID, text)
	msg.ParseMode = "HTML"
	_, err := bot.Send(msg)
	return err
}

func NotifyDeployTeam(cfg *config.Config, result *storage.DeployResult) error {
	if result.Success {
		return nil // Only notify on failure
	}
	category := cfg.DeployCategory
	chatID, ok := cfg.TelegramChats[category]
	if !ok {
		log.Printf("No chat configured for deploy category %s", category)
		return fmt.Errorf("no chat configured for deploy category %s", category)
	}
	token, ok := cfg.TelegramBots[category]
	if !ok {
		log.Printf("No bot configured for deploy category %s", category)
		return fmt.Errorf("no bot configured for deploy category %s", category)
	}
	httpClient := &http.Client{
		Timeout: 30 * time.Second,
		Transport: &http.Transport{
			TLSHandshakeTimeout:   10 * time.Second,
			IdleConnTimeout:       30 * time.Second,
			MaxIdleConns:          100,
			MaxIdleConnsPerHost:   10,
			ForceAttemptHTTP2:     true,
		},
	}
	bot, err := tgbotapi.NewBotAPIWithClient(token, tgbotapi.APIEndpoint, httpClient)
	if err != nil {
		log.Printf("Failed to create bot for deploy category %s: %v", category, err)
		return logToFile(fmt.Sprintf("Failed to create bot for deploy category %s: %v", category, err), result)
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

	const maxRetries = 5
	for attempt := 1; attempt <= maxRetries; attempt++ {
		err := sendMessage(bot, chatID, md.String())
		if err == nil {
			log.Printf("Successfully sent deploy notification for failed deployment of service %s in env %s", result.Request.Service, result.Request.Env)
			return nil
		}
		if apiErr, ok := err.(tgbotapi.APIError); ok && apiErr.ErrorCode == 429 {
			retryAfter := 1 * time.Second
			if apiErr.RetryAfter > 0 {
				retryAfter = time.Duration(apiErr.RetryAfter) * time.Second
			}
			log.Printf("Rate limit hit for deploy notification to chat %d (attempt %d/%d), retrying after %v", chatID, attempt, maxRetries, retryAfter)
			time.Sleep(retryAfter)
			continue
		}
		log.Printf("Failed to send deploy notification to chat %d (attempt %d/%d): %v", chatID, attempt, maxRetries, err)
		if attempt == maxRetries {
			return logToFile(fmt.Sprintf("Failed to send deploy notification after %d attempts: %v", maxRetries, err), result)
		}
		time.Sleep(time.Duration(attempt) * time.Second)
	}
	return fmt.Errorf("failed to send deploy notification after %d attempts", maxRetries)
}

func logToFile(message string, result *storage.DeployResult) error {
	fileName := fmt.Sprintf("notification_failures_%s.log", time.Now().Format("2006-01-02"))
	f, err := os.OpenFile(fileName, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		log.Printf("Failed to open notification failure log file %s: %v", fileName, err)
		return err
	}
	defer f.Close()

	logEntry := struct {
		Timestamp string                 `json:"timestamp"`
		Message   string                 `json:"message"`
		Service   string                 `json:"service"`
		Env       string                 `json:"env"`
		Version   string                 `json:"version"`
		Success   bool                   `json:"success"`
		ErrorMsg  string                 `json:"error"`
		Events    string                 `json:"events"`
		Logs      string                 `json:"logs"`
		Envs      map[string]string      `json:"environment_variables"`
	}{
		Timestamp: time.Now().Format("2006-01-02 15:04:05"),
		Message:   message,
		Service:   result.Request.Service,
		Env:       result.Request.Env,
		Version:   result.Request.Version,
		Success:   result.Success,
		ErrorMsg:  result.ErrorMsg,
		Events:    result.Events,
		Logs:      result.Logs,
		Envs:      result.Envs,
	}
	data, err := json.Marshal(logEntry)
	if err != nil {
		log.Printf("Failed to marshal notification failure log: %v", err)
		return err
	}
	if _, err := f.WriteString(string(data) + "\n"); err != nil {
		log.Printf("Failed to write to notification failure log file %s: %v", fileName, err)
		return err
	}
	return nil
}

func getVersionFromImage(image string) string {
	parts := strings.Split(image, ":")
	if len(parts) == 2 {
		return parts[1]
	}
	return "unknown"
}

func ClassifyService(service string, keywords map[string][]string) string {
	keywordsMu.Lock()
	defer keywordsMu.Unlock()

	lowerService := strings.ToLower(service)
	for category, patterns := range keywords {
		for _, pattern := range patterns {
			lowerPattern := strings.ToLower(pattern)
			if strings.HasPrefix(lowerPattern, "^") || strings.HasSuffix(lowerPattern, "$") || strings.Contains(lowerPattern, ".*") {
				for _, re := range compiledKeywords[category] {
					if re.MatchString(lowerService) {
						return category
					}
				}
			} else if strings.Contains(lowerService, lowerPattern) {
				return category
			}
		}
	}
	return "other"
}