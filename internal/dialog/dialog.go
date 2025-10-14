package dialog

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"strings"
	"sync"
	"time"

	"k8s-cicd/internal/config"
	"k8s-cicd/internal/queue"
	"k8s-cicd/internal/storage"
	"k8s-cicd/internal/types"
	"github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

type DialogState struct {
	UserID    int64
	ChatID    int64
	Service   string
	Stage     string // "service", "env", "version", "confirm", "continue"
	Selected  []string
	StartedAt time.Time
	UserName  string
	Version   string
}

var (
	dialogs   sync.Map // map[int64]*DialogState
	taskQueue *queue.Queue
)

func SetTaskQueue(q *queue.Queue) {
	taskQueue = q
}

func StartDialog(userID, chatID int64, service string, cfg *config.Config, userName string) {
	if _, loaded := dialogs.Load(userID); loaded {
		log.Printf("User %d already has an active dialog in chat %d", userID, chatID)
		return
	}

	state := &DialogState{
		UserID:    userID,
		ChatID:    chatID,
		Service:   service,
		Stage:     "service",
		StartedAt: time.Now(),
		UserName:  userName,
	}
	dialogs.Store(userID, state)

	log.Printf("Started dialog for user %d in chat %d for service %s", userID, chatID, service)
	go monitorDialogTimeout(userID, chatID, cfg)
	sendServiceSelection(userID, chatID, cfg, state)
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

	// Build multi-column button layout
	var buttons [][]tgbotapi.InlineKeyboardButton
	var row []tgbotapi.InlineKeyboardButton
	for i, svc := range services {
		row = append(row, tgbotapi.NewInlineKeyboardButtonData(svc, svc))
		if len(row) == cols || i == len(services)-1 {
			buttons = append(buttons, row)
			row = []tgbotapi.InlineKeyboardButton{}
		}
	}

	log.Printf("Generated %d rows with %d columns for %d services for user %d", len(buttons), cols, len(services), userID)

	keyboard := tgbotapi.NewInlineKeyboardMarkup(buttons...)
	msg := tgbotapi.NewMessage(chatID, "请选择一个服务：\nPlease select a service:")
	msg.ReplyMarkup = keyboard
	sendMessage(cfg, chatID, msg)
}

func getEnvironmentsFromDeployFile(cfg *config.Config) []string {
	fileName := storage.GetDailyFileName(time.Now(), "deploy", cfg.StorageDir)
	if err := storage.EnsureDailyFile(fileName, nil, cfg); err != nil {
		log.Printf("Failed to ensure deploy file for environments: %v", err)
		return []string{}
	}

	data, err := os.ReadFile(fileName)
	if err != nil {
		log.Printf("Failed to read deploy file for environments: %v", err)
		return []string{}
	}

	var infos []storage.DeploymentInfo
	if len(data) > 0 {
		if err := json.Unmarshal(data, &infos); err != nil {
			log.Printf("Failed to unmarshal deploy file for environments: %v", err)
			return []string{}
		}
	}

	// Extract unique environments
	envSet := make(map[string]bool)
	for _, info := range infos {
		envSet[info.Env] = true
	}

	var envs []string
	for env := range envSet {
		envs = append(envs, env)
	}

	// Sort for consistent order
	if len(envs) > 1 {
		for i := 0; i < len(envs)-1; i++ {
			for j := i + 1; j < len(envs); j++ {
				if envs[i] > envs[j] {
					envs[i], envs[j] = envs[j], envs[i]
				}
			}
		}
	}

	return envs
}

func sendEnvSelection(userID, chatID int64, cfg *config.Config, s *DialogState) {
	envs := getEnvironmentsFromDeployFile(cfg)
	if len(envs) == 0 {
		log.Printf("No environments available for user %d", userID)
		return
	}

	var buttons [][]tgbotapi.InlineKeyboardButton
	var row []tgbotapi.InlineKeyboardButton
	cols := 3
	if len(envs) < cols {
		cols = len(envs)
	}
	for i, env := range envs {
		row = append(row, tgbotapi.NewInlineKeyboardButtonData(env, env))
		if len(row) == cols || i == len(envs)-1 {
			buttons = append(buttons, row)
			row = []tgbotapi.InlineKeyboardButton{}
		}
	}

	keyboard := tgbotapi.NewInlineKeyboardMarkup(buttons...)
	msg := tgbotapi.NewMessage(chatID, "请选择一个环境：\nPlease select an environment:")
	msg.ReplyMarkup = keyboard
	sendMessage(cfg, chatID, msg)
}

func sendVersionInput(userID, chatID int64, cfg *config.Config) {
	msg := tgbotapi.NewMessage(chatID, "请输入版本号：\nPlease enter the version:")
	sendMessage(cfg, chatID, msg)
}

func sendConfirmation(userID, chatID int64, cfg *config.Config, s *DialogState) {
	// Get today's submission count and history for the same version
	fileName := storage.GetDailyFileName(time.Now(), "deploy", cfg.StorageDir)
	data, err := os.ReadFile(fileName)
	if err != nil {
		log.Printf("Failed to read deploy file for confirmation: %v", err)
		return
	}
	var infos []storage.DeploymentInfo
	if err := json.Unmarshal(data, &infos); err != nil {
		log.Printf("Failed to unmarshal deploy file for confirmation: %v", err)
		return
	}

	todayCount := 0
	var historyMsg strings.Builder
	for _, info := range infos {
		if info.Service == s.Selected[0] && info.Env == s.Selected[1] && info.Timestamp.Day() == time.Now().Day() {
			todayCount++
		}
		if info.Service == s.Selected[0] && info.Env == s.Selected[1] && strings.Contains(info.Image, ":"+s.Version) {
			historyMsg.WriteString(fmt.Sprintf("- %s: %s\n", info.Env, info.Timestamp.Format("2006-01-02 15:04:05")))
		}
	}

	var md strings.Builder
	md.WriteString(fmt.Sprintf("**提交详情 / Submission Details**\n\n"))
	md.WriteString(fmt.Sprintf("服务 / Service: **%s**\n", s.Selected[0]))
	md.WriteString(fmt.Sprintf("版本 / Version: **%s**\n", s.Version))
	md.WriteString(fmt.Sprintf("环境 / Environments: **%s**\n", strings.Join(s.Selected[1:], ", ")))
	md.WriteString(fmt.Sprintf("当天提交数量 / Today's Submissions: **%d**\n", todayCount))
	md.WriteString(fmt.Sprintf("提交用户 / Submitted by: **%s**\n", s.UserName))

	if historyMsg.Len() > 0 {
		md.WriteString(fmt.Sprintf("\n**历史记录 / History** (same version):\n%s\n", historyMsg.String()))
	}

	md.WriteString("\n确认提交？ / Confirm submission?")

	buttons := [][]tgbotapi.InlineKeyboardButton{
		{
			tgbotapi.NewInlineKeyboardButtonData("确认 / Confirm", "confirm"),
			tgbotapi.NewInlineKeyboardButtonData("取消 / Cancel", "cancel"),
		},
	}
	keyboard := tgbotapi.NewInlineKeyboardMarkup(buttons...)
	msg := tgbotapi.NewMessage(chatID, md.String())
	msg.ReplyMarkup = keyboard
	msg.ParseMode = "Markdown"
	sendMessage(cfg, chatID, msg)
}

func submitTasks(userID, chatID int64, cfg *config.Config, s *DialogState) {
	for _, env := range s.Selected[1:] { // First element is service
		task := types.DeployRequest{
			Service:   s.Selected[0],
			Env:       env,
			Version:   s.Version,
			Timestamp: time.Now(),
			UserName:  s.UserName,
			Status:    "pending",
		}
		taskQueue.Enqueue(queue.Task{DeployRequest: task})
		// Persist deployment info immediately to ensure history is recorded
		storage.PersistDeployment(cfg, task.Service, task.Env, "", "pending", task.UserName)
	}
	sendMessage(cfg, chatID, "部署任务已提交。\nDeployment task(s) submitted.")

	// Prompt for continuing interaction
	s.Stage = "continue"
	dialogs.Store(userID, s)
	buttons := [][]tgbotapi.InlineKeyboardButton{
		{
			tgbotapi.NewInlineKeyboardButtonData("是 / Yes", "yes"),
			tgbotapi.NewInlineKeyboardButtonData("否 / No", "no"),
		},
	}
	keyboard := tgbotapi.NewInlineKeyboardMarkup(buttons...)
	msg := tgbotapi.NewMessage(chatID, "是否继续提交另一个部署任务？\nWould you like to submit another deployment task?")
	msg.ReplyMarkup = keyboard
	msg.ParseMode = "Markdown"
	sendMessage(cfg, chatID, msg)
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
		s.Selected = append(s.Selected, input)
		s.Stage = "env"
		dialogs.Store(userID, s)
		sendEnvSelection(userID, chatID, cfg, s)
	case "env":
		s.Selected = append(s.Selected, input)
		s.Stage = "version"
		dialogs.Store(userID, s)
		sendVersionInput(userID, chatID, cfg)
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
			s.Stage = "service"
			s.Selected = []string{}
			s.Version = ""
			dialogs.Store(userID, s)
			sendServiceSelection(userID, chatID, cfg, s)
		} else if input == "no" {
			CancelDialog(userID, chatID, cfg) // Ensure close on "no"
		}
	}
}

func CancelDialog(userID, chatID int64, cfg *config.Config) bool {
	if _, loaded := dialogs.LoadAndDelete(userID); loaded {
		log.Printf("Dialog cancelled for user %d in chat %d", userID, chatID)
		sendMessage(cfg, chatID, "当前会话已关闭。\nCurrent session has been closed.")
		return true
	}
	log.Printf("No active dialog to cancel for user %d in chat %d", userID, chatID)
	return false
}

func IsDialogActive(userID int64) bool {
	_, ok := dialogs.Load(userID)
	return ok
}

func monitorDialogTimeout(userID, chatID int64, cfg *config.Config) {
	time.Sleep(time.Duration(cfg.DialogTimeout) * time.Second)
	if state, loaded := dialogs.LoadAndDelete(userID); loaded {
		s := state.(*DialogState)
		if s.ChatID == chatID {
			log.Printf("Dialog timed out for user %d in chat %d", userID, chatID)
			sendMessage(cfg, chatID, "对话已超时，请重新触发部署。\nDialog timed out, please trigger deployment again.")
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
		log.Printf("No bot configured for service: %s", service)
		return
	}
	bot, err := tgbotapi.NewBotAPI(token)
	if err != nil {
		log.Printf("Failed to create bot for service %s: %v", service, err)
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
	if _, err := bot.Send(msg); err != nil {
		log.Printf("Failed to send message to chat %d: %v", chatID, err)
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