package telegram

import (
	"encoding/json"
	"fmt"
	"html"
	"log"
	"math/rand"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"github.com/google/uuid"
	"k8s-cicd/internal/config"
	"k8s-cicd/internal/queue"
	"k8s-cicd/internal/storage"
	"k8s-cicd/internal/types"
)

const (
	MaxRetries           = 5                // Maximum retry attempts for Telegram API calls
	RetryBaseDelay       = 1 * time.Second // Base delay for retries
	NotificationLogFile  = "notification_failures_%s.log"
	DialogTimeout        = 300 * time.Second // Default dialog timeout
)

type DialogState struct {
	UserID        int64
	ChatID        int64
	Service       string
	Stage         string // "service", "env", "version", "confirm"
	Selected      string
	SelectedEnvs  []string
	Version       string
	UserName      string
	StartedAt     time.Time
	Messages      []int
	timeoutCancel chan bool
}

var (
	bots           map[string]*tgbotapi.BotAPI
	compiledRegex  map[string][]*regexp.Regexp
	regexMu        sync.Mutex
	dialogs        sync.Map
	taskQueue      *queue.Queue
	PendingConfirmations sync.Map
)

func StartBot(cfg *config.Config, q *queue.Queue) error {
	taskQueue = q
	bots = make(map[string]*tgbotapi.BotAPI)
	compiledRegex = make(map[string][]*regexp.Regexp)

	// Precompile regex patterns
	regexMu.Lock()
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
		compiledRegex[category] = compiled
	}
	regexMu.Unlock()

	// Validate Telegram configuration
	hasErrors := false
	if _, ok := cfg.TelegramChats["other"]; !ok {
		log.Printf("Warning: No default chat ID configured for category 'other'; using first available chat")
		hasErrors = true
	}
	if _, ok := cfg.TelegramBots["other"]; !ok {
		log.Printf("Warning: No default bot configured for category 'other'; using first available bot")
		hasErrors = true
	}
	if cfg.DeployCategory != "" && cfg.DeployCategory != "other" {
		if _, ok := cfg.TelegramChats[cfg.DeployCategory]; !ok {
			log.Printf("Warning: No chat ID configured for deploy category %s; falling back to 'other'", cfg.DeployCategory)
			hasErrors = true
		}
		if _, ok := cfg.TelegramBots[cfg.DeployCategory]; !ok {
			log.Printf("Warning: No bot configured for deploy category %s; falling back to 'other'", cfg.DeployCategory)
			hasErrors = true
		}
	}

	// Initialize bots
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

	for service, token := range cfg.TelegramBots {
		bot, err := tgbotapi.NewBotAPIWithClient(token, tgbotapi.APIEndpoint, httpClient)
		if err != nil {
			log.Printf("Failed to create bot for service %s: %v", service, err)
			continue
		}
		bots[service] = bot
		log.Printf("Started bot for service %s (username: %s)", service, bot.Self.UserName)
		go handleBot(bot, cfg, service)
	}

	if len(bots) == 0 {
		log.Printf("Warning: No Telegram bots initialized; Telegram functionality disabled")
		return fmt.Errorf("no Telegram bots initialized")
	}
	if hasErrors {
		return fmt.Errorf("Telegram configuration incomplete, but proceeding with available bots")
	}
	return nil
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
			userName := html.EscapeString(update.Message.From.UserName)

			log.Printf("Received message from user %d (username: %s) in chat %d for service %s: %s", userID, userName, chatID, service, text)

			if cfg.TelegramChats[service] != chatID && cfg.TelegramChats["other"] != chatID {
				log.Printf("Ignoring message from chat %d: not in allowed chats for service %s or 'other'", chatID, service)
				continue
			}

			// Check for trigger keywords
			for _, trigger := range cfg.TriggerKeywords {
				if text == trigger {
					log.Printf("User %d (username: %s) triggered dialog for service %s in chat %d", userID, userName, service, chatID)
					startDialog(userID, chatID, service, cfg, userName)
					continue
				}
			}

			// Check for cancel keywords
			for _, cancel := range cfg.CancelKeywords {
				if text == cancel {
					log.Printf("User %d (username: %s) requested to cancel dialog in chat %d", userID, userName, chatID)
					cancelDialog(userID, chatID, cfg)
					continue
				}
			}

			// Process dialog input
			if state, ok := dialogs.Load(userID); ok {
				log.Printf("Processing dialog input for user %d (username: %s) in chat %d: %s", userID, userName, chatID, text)
				processDialog(userID, chatID, text, cfg, state.(*DialogState))
				continue
			}

			// Default response for invalid input
			triggerList := strings.Join(cfg.TriggerKeywords, ", ")
			response := fmt.Sprintf("请使用触发关键字（如 %s）开始部署。\nPlease use a trigger keyword (e.g., %s) to start a deployment.", triggerList, triggerList)
			if len(cfg.InvalidResponses) > 0 {
				response = cfg.InvalidResponses[rand.Intn(len(cfg.InvalidResponses))]
			}
			log.Printf("Invalid input from user %d (username: %s) in chat %d: %s, responding with: %s", userID, userName, chatID, text, response)
			sendMessage(bot, chatID, response)
		}
	}
}

func startDialog(userID, chatID int64, service string, cfg *config.Config, userName string) {
	if state, loaded := dialogs.Load(userID); loaded {
		log.Printf("Cancelling existing dialog for user %d in chat %d", userID, chatID)
		cancelDialog(userID, chatID, cfg)
		sendMessage(bots[service], chatID, "Previous dialog cancelled. Starting new deployment dialog.")
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

	log.Printf("Started dialog for user %d (username: %s) in chat %d for service %s", userID, userName, chatID, service)
	sentMsg, err := sendMessage(bots[service], chatID, fmt.Sprintf("开始部署对话 / Starting deployment dialog for service %s", html.EscapeString(service)))
	if err == nil {
		state.Messages = append(state.Messages, sentMsg.MessageID)
	}
	sentMsg, err = sendServiceSelection(userID, chatID, cfg, state)
	if err == nil {
		state.Messages = append(state.Messages, sentMsg.MessageID)
	}
	go monitorDialogTimeout(userID, chatID, cfg, state)
}

func sendServiceSelection(userID, chatID int64, cfg *config.Config, state *DialogState) (tgbotapi.Message, error) {
	serviceLists, err := config.LoadServiceLists(cfg.ServicesDir, cfg.TelegramBots)
	if err != nil {
		log.Printf("Failed to load service lists for user %d: %v", userID, err)
		return sendMessage(bots[state.Service], chatID, "无法加载服务列表 / Failed to load service list.")
	}
	services, exists := serviceLists[state.Service]
	if !exists || len(services) == 0 {
		log.Printf("No services available for %s for user %d", state.Service, userID)
		return sendMessage(bots[state.Service], chatID, "服务列表为空，请稍后重试 / Service list empty, please try again later.")
	}

	message := "请选择服务 / Please select a service:\n"
	for i, svc := range services {
		message += fmt.Sprintf("%d. %s\n", i+1, html.EscapeString(svc))
	}
	buttons := make([][]tgbotapi.InlineKeyboardButton, 0)
	for i, svc := range services {
		buttons = append(buttons, []tgbotapi.InlineKeyboardButton{
			tgbotapi.NewInlineKeyboardButtonData(fmt.Sprintf("%d. %s", i+1, svc), fmt.Sprintf("svc:%s", svc)),
		})
	}
	keyboard := tgbotapi.NewInlineKeyboardMarkup(buttons...)
	msg := tgbotapi.NewMessage(chatID, message)
	msg.ReplyMarkup = keyboard
	msg.ParseMode = "HTML"
	return sendMessage(bots[state.Service], chatID, msg)
}

func processDialog(userID, chatID int64, text string, cfg *config.Config, state *DialogState) {
	switch state.Stage {
	case "service":
		services, err := config.LoadServiceLists(cfg.ServicesDir, cfg.TelegramBots)
		if err != nil {
			log.Printf("Failed to load service lists for user %d: %v", userID, err)
			sendMessage(bots[state.Service], chatID, "无法加载服务列表 / Failed to load service list.")
			return
		}
		if svcList, ok := services[state.Service]; ok && contains(svcList, text) {
			state.Selected = text
			state.Stage = "env"
			sentMsg, _ := sendMessage(bots[state.Service], chatID, fmt.Sprintf("已选择服务: %s\n请选择环境 / Selected service: %s\nPlease select an environment:", html.EscapeString(text), html.EscapeString(text)))
			state.Messages = append(state.Messages, sentMsg.MessageID)
			sendEnvSelection(userID, chatID, cfg, state)
		} else {
			sendMessage(bots[state.Service], chatID, "无效的服务，请从列表中选择 / Invalid service, please select from the list.")
		}
	case "env":
		if validateEnvironment(text, cfg) {
			state.SelectedEnvs = append(state.SelectedEnvs, text)
			state.Stage = "version"
			sentMsg, _ := sendMessage(bots[state.Service], chatID, fmt.Sprintf("已选择环境: %s\n请输入版本号 / Selected environment: %s\nPlease enter version number:", html.EscapeString(text), html.EscapeString(text)))
			state.Messages = append(state.Messages, sentMsg.MessageID)
		} else {
			sendMessage(bots[state.Service], chatID, "无效的环境，请选择有效环境 / Invalid environment, please select a valid environment.")
			sendEnvSelection(userID, chatID, cfg, state)
		}
	case "version":
		if validateVersion(text) {
			state.Version = text
			state.Stage = "confirm"
			message := fmt.Sprintf("确认部署服务 %s 到环境 %s，版本 %s？\nConfirm deployment for service %s to envs %s, version %s?",
				html.EscapeString(state.Selected), strings.Join(state.SelectedEnvs, ","), html.EscapeString(text),
				html.EscapeString(state.Selected), strings.Join(state.SelectedEnvs, ","), html.EscapeString(text))
			buttons := [][]tgbotapi.InlineKeyboardButton{
				{
					tgbotapi.NewInlineKeyboardButtonData("确认 / Confirm", "confirm:"+uuid.New().String()[:8]),
					tgbotapi.NewInlineKeyboardButtonData("取消 / Cancel", "cancel:"+uuid.New().String()[:8]),
				},
			}
			keyboard := tgbotapi.NewInlineKeyboardMarkup(buttons...)
			msg := tgbotapi.NewMessage(chatID, message)
			msg.ReplyMarkup = keyboard
			msg.ParseMode = "HTML"
			sentMsg, _ := sendMessage(bots[state.Service], chatID, msg)
			state.Messages = append(state.Messages, sentMsg.MessageID)
		} else {
			sendMessage(bots[state.Service], chatID, "无效的版本号，请输入有效版本 / Invalid version, please enter a valid version.")
		}
	}
}

func sendEnvSelection(userID, chatID int64, cfg *config.Config, state *DialogState) {
	fileName := filepath.Join(cfg.StorageDir, "environments.json")
	var envs map[string]string
	if data, err := os.ReadFile(fileName); err == nil && len(data) > 0 {
		if err := json.Unmarshal(data, &envs); err == nil {
			var envList []string
			for env := range envs {
				envList = append(envList, env)
			}
			sort.Strings(envList)
			message := "请选择环境 / Please select an environment:\n"
			for i, env := range envList {
				message += fmt.Sprintf("%d. %s\n", i+1, html.EscapeString(env))
			}
			buttons := make([][]tgbotapi.InlineKeyboardButton, 0)
			for i, env := range envList {
				buttons = append(buttons, []tgbotapi.InlineKeyboardButton{
					tgbotapi.NewInlineKeyboardButtonData(fmt.Sprintf("%d. %s", i+1, env), fmt.Sprintf("env:%s", env)),
				})
			}
			keyboard := tgbotapi.NewInlineKeyboardMarkup(buttons...)
			msg := tgbotapi.NewMessage(chatID, message)
			msg.ReplyMarkup = keyboard
			msg.ParseMode = "HTML"
			sentMsg, _ := sendMessage(bots[state.Service], chatID, msg)
			state.Messages = append(state.Messages, sentMsg.MessageID)
		}
	} else {
		sendMessage(bots[state.Service], chatID, "无法加载环境列表，请稍后重试 / Failed to load environment list, please try again later.")
	}
}

func cancelDialog(userID, chatID int64, cfg *config.Config) {
	if state, ok := dialogs.Load(userID); ok {
		s := state.(*DialogState)
		select {
		case s.timeoutCancel <- true:
		default:
		}
		deleteMessages(s, cfg)
		dialogs.Delete(userID)
		log.Printf("Dialog cancelled for user %d in chat %d", userID, chatID)
		if bot, ok := bots[s.Service]; ok {
			sendMessage(bot, chatID, "当前会话已关闭 / Current session has been closed.")
		}
	}
}

func monitorDialogTimeout(userID, chatID int64, cfg *config.Config, state *DialogState) {
	select {
	case <-time.After(DialogTimeout):
		if _, loaded := dialogs.Load(userID); loaded {
			log.Printf("Dialog timed out for user %d in chat %d", userID, chatID)
			deleteMessages(state, cfg)
			sendMessage(bots[state.Service], chatID, "对话已超时，请重新触发部署 / Dialog timed out, please trigger deployment again.")
			dialogs.Delete(userID)
		}
	case <-state.timeoutCancel:
		log.Printf("Timeout cancelled for user %d in chat %d", userID, chatID)
	}
}

func deleteMessages(state *DialogState, cfg *config.Config) {
	bot, ok := bots[state.Service]
	if !ok {
		log.Printf("No bot configured for service %s", state.Service)
		return
	}
	for _, msgID := range state.Messages {
		deleteMsg := tgbotapi.NewDeleteMessage(state.ChatID, msgID)
		if _, err := bot.Request(deleteMsg); err != nil {
			log.Printf("Failed to delete message %d in chat %d: %v", msgID, state.ChatID, err)
		}
	}
}

func sendMessage(bot *tgbotapi.BotAPI, chatID int64, text interface{}) (tgbotapi.Message, error) {
	var msg tgbotapi.MessageConfig
	switch t := text.(type) {
	case string:
		msg = tgbotapi.NewMessage(chatID, html.EscapeString(t))
	case tgbotapi.MessageConfig:
		msg = t
		msg.Text = html.EscapeString(t.Text)
	default:
		return tgbotapi.Message{}, fmt.Errorf("unsupported message type: %T", text)
	}
	msg.ParseMode = "HTML"
	sentMsg, err := bot.Send(msg)
	if err != nil {
		log.Printf("Failed to send message to chat %d: %v", chatID, err)
		return tgbotapi.Message{}, err
	}
	return sentMsg, nil
}

func NotifyCompletion(cfg *config.Config, result *storage.DeployResult) error {
	category := classifyService(result.Request.Service, cfg.ServiceKeywords)
	chatID, ok := cfg.TelegramChats[category]
	if !ok {
		log.Printf("No chat configured for category %s, trying default chat", category)
		category = "other"
		chatID, ok = cfg.TelegramChats[category]
		if !ok {
			log.Printf("No default chat configured for category %s", category)
			return logToFile(fmt.Sprintf("No chat configured for category %s", category), result)
		}
	}
	bot, ok := bots[category]
	if !ok {
		log.Printf("No bot configured for category %s", category)
		return logToFile(fmt.Sprintf("No bot configured for category %s", category), result)
	}

	var md strings.Builder
	if result.Success {
		md.WriteString("<b>✅ 部署完成 / Deployment Completed</b>\n\n")
	} else {
		md.WriteString("<b>❌ 部署失败 / Deployment Failed</b>\n\n")
	}
	md.WriteString(fmt.Sprintf("服务 / Service: <b>%s</b>\n", html.EscapeString(result.Request.Service)))
	md.WriteString(fmt.Sprintf("环境 / Environment: <b>%s</b>\n", html.EscapeString(result.Request.Env)))
	md.WriteString(fmt.Sprintf("版本 / Version: <b>%s</b>\n", html.EscapeString(result.Request.Version)))
	md.WriteString(fmt.Sprintf("用户 / User: <b>%s</b>\n", html.EscapeString(result.Request.UserName)))
	if !result.Success {
		md.WriteString(fmt.Sprintf("错误 / Error: <b>%s</b>\n", html.EscapeString(result.ErrorMsg)))
	}

	for attempt := 1; attempt <= MaxRetries; attempt++ {
		err := sendMessage(bot, chatID, md.String())
		if err == nil {
			log.Printf("Successfully sent completion notification for service %s in env %s (success: %v)", result.Request.Service, result.Request.Env, result.Success)
			return nil
		}
		if apiErr, ok := err.(tgbotapi.Error); ok && apiErr.Code == 429 {
			retryAfter := RetryBaseDelay
			if apiErr.RetryAfter > 0 {
				retryAfter = time.Duration(apiErr.RetryAfter) * time.Second
			}
			log.Printf("Rate limit hit for completion notification to chat %d (attempt %d/%d), retrying after %v", chatID, attempt, MaxRetries, retryAfter)
			time.Sleep(retryAfter)
			continue
		}
		log.Printf("Failed to send completion notification to chat %d (attempt %d/%d): %v", chatID, attempt, MaxRetries, err)
		if attempt == MaxRetries {
			return logToFile(fmt.Sprintf("Failed to send completion notification after %d attempts: %v", MaxRetries, err), result)
		}
		time.Sleep(time.Duration(attempt) * RetryBaseDelay)
	}
	return fmt.Errorf("failed to send completion notification after %d attempts", MaxRetries)
}

func NotifyDeployTeam(cfg *config.Config, result *storage.DeployResult) error {
	category := cfg.DeployCategory
	if category == "" || category == "other" {
		log.Printf("No deploy category configured, using default 'other'")
		category = "other"
	}
	chatID, ok := cfg.TelegramChats[category]
	if !ok {
		log.Printf("No chat configured for category %s", category)
		return logToFile(fmt.Sprintf("No chat configured for deploy category %s", category), result)
	}
	bot, ok := bots[category]
	if !ok {
		log.Printf("No bot configured for category %s", category)
		return logToFile(fmt.Sprintf("No bot configured for deploy category %s", category), result)
	}

	var md strings.Builder
	md.WriteString("<b>⚠️ CI/CD 部署失败，需要人工干预 / CI/CD Deployment Failed, Manual Intervention Needed</b>\n\n")
	md.WriteString(fmt.Sprintf("服务 / Service: <b>%s</b>\n", html.EscapeString(result.Request.Service)))
	md.WriteString(fmt.Sprintf("环境 / Environment: <b>%s</b>\n", html.EscapeString(result.Request.Env)))
	md.WriteString(fmt.Sprintf("尝试版本 / Attempted Version: <b>%s</b>\n", html.EscapeString(result.Request.Version)))
	md.WriteString(fmt.Sprintf("错误 / Error: <b>%s</b>\n", html.EscapeString(result.ErrorMsg)))
	md.WriteString("\n<b>🔍 诊断信息 / Diagnostics</b>\n\n")
	md.WriteString(fmt.Sprintf("事件 / Events:\n%s\n", html.EscapeString(result.Events)))
	md.WriteString("环境变量 / Environment Variables:\n")
	for k, v := range result.Envs {
		md.WriteString(fmt.Sprintf("- %s: <b>%s</b>\n", html.EscapeString(k), html.EscapeString(v)))
	}
	md.WriteString(fmt.Sprintf("\n日志 / Logs: <b>%s</b>\n", html.EscapeString(result.Logs)))
	md.WriteString(fmt.Sprintf("\n<b>失败时间 / Failed at</b>: %s\n", result.Request.Timestamp.Format("2006-01-02 15:04:05")))

	for attempt := 1; attempt <= MaxRetries; attempt++ {
		err := sendMessage(bot, chatID, md.String())
		if err == nil {
			log.Printf("Successfully sent deploy notification for failed deployment of service %s in env %s", result.Request.Service, result.Request.Env)
			return nil
		}
		if apiErr, ok := err.(tgbotapi.Error); ok && apiErr.Code == 429 {
			retryAfter := RetryBaseDelay
			if apiErr.RetryAfter > 0 {
				retryAfter = time.Duration(apiErr.RetryAfter) * time.Second
			}
			log.Printf("Rate limit hit for deploy notification to chat %d (attempt %d/%d), retrying after %v", chatID, attempt, MaxRetries, retryAfter)
			time.Sleep(retryAfter)
			continue
		}
		log.Printf("Failed to send deploy notification to chat %d (attempt %d/%d): %v", chatID, attempt, MaxRetries, err)
		if attempt == MaxRetries {
			return logToFile(fmt.Sprintf("Failed to send deploy notification after %d attempts: %v", MaxRetries, err), result)
		}
		time.Sleep(time.Duration(attempt) * RetryBaseDelay)
	}
	return fmt.Errorf("failed to send deploy notification after %d attempts", MaxRetries)
}

func logToFile(message string, result *storage.DeployResult) error {
	fileName := fmt.Sprintf(NotificationLogFile, time.Now().Format("2006-01-02"))
	f, err := os.OpenFile(fileName, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		log.Printf("Failed to open notification failure log file %s: %v", fileName, err)
		return err
	}
	defer f.Close()

	logEntry := struct {
		Timestamp string            `json:"timestamp"`
		Message   string            `json:"message"`
		Service   string            `json:"service"`
		Env       string            `json:"env"`
		Version   string            `json:"version"`
		Success   bool              `json:"success"`
		ErrorMsg  string            `json:"error"`
		Events    string            `json:"events"`
		Logs      string            `json:"logs"`
		Envs      map[string]string `json:"environment_variables"`
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

func classifyService(service string, keywords map[string][]string) string {
	regexMu.Lock()
	defer regexMu.Unlock()

	lowerService := strings.ToLower(service)
	for category, patterns := range keywords {
		for _, pattern := range patterns {
			lowerPattern := strings.ToLower(pattern)
			if strings.HasPrefix(lowerPattern, "^") || strings.HasSuffix(lowerPattern, "$") || strings.Contains(lowerPattern, ".*") {
				for _, re := range compiledRegex[category] {
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

func validateVersion(version string) bool {
	// Simple version validation (e.g., semantic versioning or custom format)
	return regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+$|^[a-zA-Z0-9_-]+$`).MatchString(version)
}

func contains(slice []string, item string) bool {
	for _, s := range slice {
		if s == item {
			return true
		}
	}
	return false
}