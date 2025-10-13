package dialog

import (
	"fmt"
	"log"
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
		log.Printf("User %d already has an active dialog in chat %d", userID, chatID)
		sendMessage(cfg, chatID, "您已经有一个活跃的对话。请完成或取消它。\nYou already have an active dialog. Please complete or cancel it.")
		return
	}

	dialogs.Store(userID, &DialogState{
		UserID:    userID,
		ChatID:    chatID,
		Service:   service,
		Stage:     "service",
		StartedAt: time.Now(),
	})

	log.Printf("Started dialog for user %d in chat %d for service %s", userID, chatID, service)
	go monitorDialogTimeout(userID, chatID, cfg)

	serviceLists, err := config.LoadServiceLists(cfg.ServicesDir)
	if err != nil {
		log.Printf("Failed to load service lists for user %d: %v", userID, err)
		sendMessage(cfg, chatID, fmt.Sprintf("无法加载服务列表：%v\nFailed to load service lists: %v", err, err))
		return
	}
	services, exists := serviceLists[service]
	if !exists {
		log.Printf("No service list found for %s for user %d", service, userID)
		sendMessage(cfg, chatID, fmt.Sprintf("未找到 %s 的服务列表。请检查配置。\nNo service list found for %s. Please check configuration.", service, service))
		return
	}
	if len(services) == 0 {
		log.Printf("No services available for %s for user %d", service, userID)
		sendMessage(cfg, chatID, fmt.Sprintf("没有可用的服务 %s。请在 %s.svc.list 中添加服务。\nNo services available for %s. Please add services to %s.svc.list.", service, service, service, service))
		return
	}

	log.Printf("Loaded %d services for %s: %v", len(services), service, services)

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

func ProcessDialog(userID, chatID int64, text string, cfg *config.Config) {
	state, ok := dialogs.Load(userID)
	if !ok {
		log.Printf("No active dialog found for user %d in chat %d", userID, chatID)
		return
	}
	s := state.(*DialogState)

	switch s.Stage {
	case "service":
		serviceLists, err := config.LoadServiceLists(cfg.ServicesDir)
		if err != nil {
			log.Printf("Failed to load service lists for user %d in chat %d: %v", userID, chatID, err)
			sendMessage(cfg, chatID, fmt.Sprintf("无法加载服务列表：%v\nFailed to load service lists: %v", err, err))
			return
		}
		log.Printf("Checking service %s against list for %s: %v", text, s.Service, serviceLists[s.Service])
		if !contains(serviceLists[s.Service], text) {
			log.Printf("Invalid service selected by user %d in chat %d: %s", userID, chatID, text)
			sendMessage(cfg, chatID, "无效的服务。请选择一个有效的服务。\nInvalid service. Please select a valid service.")
			return
		}
		s.Selected = append(s.Selected, text)
		s.Stage = "env"

		if len(cfg.Environments) == 0 {
			log.Printf("No environments configured for user %d in chat %d", userID, chatID)
			sendMessage(cfg, chatID, "未配置任何环境。请检查 config.yaml。\nNo environments configured. Please check config.yaml.")
			return
		}

		log.Printf("Loaded %d environments for user %d: %v", len(cfg.Environments), userID, cfg.Environments)

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
		msg := tgbotapi.NewMessage(chatID, "请选择环境（可多选，按 Done 完成）：\nPlease select environment(s) (multi-select, press Done when finished):")
		msg.ReplyMarkup = keyboard
		sendMessage(cfg, chatID, msg)

	case "env":
		if text == "done" {
			if len(s.Selected) == 0 {
				log.Printf("User %d in chat %d selected Done without environments", userID, chatID)
				sendMessage(cfg, chatID, "请至少选择一个环境。\nPlease select at least one environment.")
				return
			}
			s.Stage = "version"
			log.Printf("User %d in chat %d moved to version stage with environments: %v", userID, chatID, s.Selected[1:])
			msg := tgbotapi.NewMessage(chatID, "请输入版本号：\nPlease enter the version:")
			sendMessage(cfg, chatID, msg)
		} else if contains(cfg.Environments, text) {
			if !contains(s.Selected, text) {
				s.Selected = append(s.Selected, text)
				log.Printf("User %d in chat %d added environment: %s", userID, chatID, text)
			}
			sendMessage(cfg, chatID, "环境已添加。继续选择或按 Done 完成。\nEnvironment added. Select another or press Done.")
		} else {
			log.Printf("Invalid environment selected by user %d in chat %d: %s", userID, chatID, text)
			sendMessage(cfg, chatID, "无效的环境。请选择一个有效的环境。\nInvalid environment. Please select a valid environment.")
		}

	case "version":
		log.Printf("User %d in chat %d submitted version: %s", userID, chatID, text)
		for _, env := range s.Selected[1:] { // First element is service
			task := DeployRequest{
				Service:   s.Selected[0],
				Env:       env,
				Version:   text,
				Timestamp: time.Now(),
			}
			taskList, _ := taskQueue.LoadOrStore(s.Service, []DeployRequest{})
			taskQueue.Store(s.Service, append(taskList.([]DeployRequest), task))
			log.Printf("Stored task for user %d: service=%s, env=%s, version=%s", userID, s.Selected[0], env, text)
			storage.PersistTelegramMessage(cfg, storage.TelegramMessage{
				UserID:    userID,
				ChatID:    chatID,
				Content:   fmt.Sprintf("Deploy: %s, Env: %s, Version: %s", s.Selected[0], env, text),
				Timestamp: time.Now(),
			})
		}
		sendMessage(cfg, chatID, "部署任务已提交。\nDeployment task(s) submitted.")
		dialogs.Delete(userID)
		log.Printf("Dialog completed and removed for user %d in chat %d", userID, chatID)
	}
}

func CancelDialog(userID, chatID int64, cfg *config.Config) bool {
	if _, loaded := dialogs.LoadAndDelete(userID); loaded {
		log.Printf("Dialog cancelled for user %d in chat %d", userID, chatID)
		sendMessage(cfg, chatID, "对话已取消。\nDialog cancelled.")
		return true
	}
	log.Printf("No active dialog to cancel for user %d in chat %d", userID, chatID)
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
			log.Printf("Dialog timed out for user %d in chat %d", userID, chatID)
			sendMessage(cfg, chatID, "对话因 5 分钟未操作而超时。请重新开始对话。\nDialog timed out after 5 minutes. Please start a new dialog if needed.")
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