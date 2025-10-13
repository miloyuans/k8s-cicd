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

	go monitorDialogTimeout(userID, chatID, cfg)

	serviceLists, err := config.LoadServiceLists(cfg.ServicesDir)
	if err != nil {
		sendMessage(cfg, chatID, fmt.Sprintf("无法加载服务列表：%v\nFailed to load service lists: %v", err, err))
		return
	}
	services, exists := serviceLists[service]
	if !exists {
		sendMessage(cfg, chatID, fmt.Sprintf("未找到 %s 的服务列表。请检查配置。\nNo service list found for %s. Please check configuration.", service, service))
		return
	}
	if len(services) == 0 {
		sendMessage(cfg, chatID, fmt.Sprintf("没有可用的服务 %s。请在 %s.svc.list 中添加服务。\nNo services available for %s. Please add services to %s.svc.list.", service, service, service, service))
		return
	}

	// Calculate columns based on service name lengths and count
	maxLen := 0
	for _, svc := range services {
		if len(svc) > maxLen {
			maxLen = len(svc)
		}
	}
	// Estimate columns: assume max 50 chars per row, each button needs ~len+4 chars (for padding)
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

	keyboard := tgbotapi.NewInlineKeyboardMarkup(buttons...)
	msg := tgbotapi.NewMessage(chatID, "请选择一个服务：\nPlease select a service:")
	msg.ReplyMarkup = keyboard
	sendMessage(cfg, chatID, msg)
}

func ProcessDialog(userID, chatID int64, text string, cfg *config.Config) {
	state, ok := dialogs.Load(userID)
	if !ok {
		return
	}
	s := state.(*DialogState)

	switch s.Stage {
	case "service":
		serviceLists, _ := config.LoadServiceLists(cfg.ServicesDir)
		if !contains(serviceLists[s.Service], text) {
			sendMessage(cfg, chatID, "无效的服务。请选择一个有效的服务。\nInvalid service. Please select a valid service.")
			return
		}
		s.Selected = append(s.Selected, text)
		s.Stage = "env"

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
				sendMessage(cfg, chatID, "请至少选择一个环境。\nPlease select at least one environment.")
				return
			}
			s.Stage = "version"
			msg := tgbotapi.NewMessage(chatID, "请输入版本号：\nPlease enter the version:")
			sendMessage(cfg, chatID, msg)
		} else if contains(cfg.Environments, text) {
			if !contains(s.Selected, text) {
				s.Selected = append(s.Selected, text)
			}
			sendMessage(cfg, chatID, "环境已添加。继续选择或按 Done 完成。\nEnvironment added. Select another or press Done.")
		} else {
			sendMessage(cfg, chatID, "无效的环境。请选择一个有效的环境。\nInvalid environment. Please select a valid environment.")
		}

	case "version":
		for _, env := range s.Selected[1:] { // First element is service
			task := DeployRequest{
				Service:   s.Selected[0],
				Env:       env,
				Version:   text,
				Timestamp: time.Now(),
			}
			taskList, _ := taskQueue.LoadOrStore(s.Service, []DeployRequest{})
			taskQueue.Store(s.Service, append(taskList.([]DeployRequest), task))
			storage.PersistTelegramMessage(cfg, storage.TelegramMessage{
				UserID:    userID,
				ChatID:    chatID,
				Content:   fmt.Sprintf("Deploy: %s, Env: %s, Version: %s", s.Selected[0], env, text),
				Timestamp: time.Now(),
			})
		}
		sendMessage(cfg, chatID, "部署任务已提交。\nDeployment task(s) submitted.")
		dialogs.Delete(userID)
	}
}

func CancelDialog(userID, chatID int64, cfg *config.Config) bool {
	if _, loaded := dialogs.LoadAndDelete(userID); loaded {
		sendMessage(cfg, chatID, "对话已取消。\nDialog cancelled.")
		return true
	}
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
		log.Printf("No bot for service: %s", service)
		return
	}
	bot, err := tgbotapi.NewBotAPI(token)
	if err != nil {
		log.Printf("Failed to create bot: %v", err)
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
	_, err = bot.Send(msg)
	if err != nil {
		log.Printf("Failed to send telegram message: %v", err)
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