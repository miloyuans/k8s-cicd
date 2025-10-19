package dialog

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
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

type DialogState struct {
    UserID        int64
    ChatID        int64
    Service       string
    Stage         string // "service", "env", "version", "confirm", "continue"
    Selected      string // Single selection for service
    SelectedEnvs  []string
    StartedAt     time.Time
    UserName      string
    Version       string
    timeoutCancel chan bool
    Messages      []int
    RetryCount    int // New field for tracking service list retries
}

var (
	dialogs             sync.Map
	taskQueue           *queue.Queue
	GlobalTaskQueue     *queue.Queue
	PendingConfirmations sync.Map
)

func SetTaskQueue(q *queue.Queue) {
	taskQueue = q
	GlobalTaskQueue = q
}

func StartDialog(userID, chatID int64, service string, cfg *config.Config, userName string) {
    if _, loaded := dialogs.Load(userID); loaded {
        log.Printf("Cancelling existing dialog for user %d in chat %d", userID, chatID)
        CancelDialog(userID, chatID, cfg)
        sendMessage(cfg, chatID, "Previous dialog was active and has been cancelled. Starting new deployment dialog.")
    }

    newState := &DialogState{
        UserID:        userID,
        ChatID:        chatID,
        Service:       service,
        Stage:         "service",
        StartedAt:     time.Now(),
        UserName:      userName,
        timeoutCancel: make(chan bool),
        Messages:      []int{},
        RetryCount:    0, // Initialize retry counter
    }
    dialogs.Store(userID, newState)

    log.Printf("Started dialog for user %d in chat %d for service %s", userID, chatID, service)
    sentMsg, err := sendMessage(cfg, chatID, fmt.Sprintf("开始部署对话 / Starting deployment dialog for service %s", service))
    if err == nil {
        newState.Messages = append(newState.Messages, sentMsg.MessageID)
    }
    sentMsg, err = sendServiceSelection(userID, chatID, cfg, newState)
    if err == nil {
        newState.Messages = append(newState.Messages, sentMsg.MessageID)
    }
    go monitorDialogTimeout(userID, chatID, cfg, newState)
}

func sendServiceSelection(userID, chatID int64, cfg *config.Config, s *DialogState) (tgbotapi.Message, error) {
    serviceLists, err := config.LoadServiceLists(cfg.ServicesDir, cfg.TelegramBots)
    if err != nil {
        log.Printf("Failed to load service lists for user %d: %v", userID, err)
        sentMsg, sendErr := sendMessage(cfg, chatID, "无法加载服务列表 / Failed to load service list.")
        if sendErr == nil {
            s.Messages = append(s.Messages, sentMsg.MessageID)
        }
        return sentMsg, fmt.Errorf("failed to load service lists: %v", err)
    }
    services, exists := serviceLists[s.Service]
    if !exists {
        log.Printf("No service list found for %s for user %d", s.Service, userID)
        sentMsg, sendErr := sendMessage(cfg, chatID, "未找到服务列表 / No service list found.")
        if sendErr == nil {
            s.Messages = append(s.Messages, sentMsg.MessageID)
        }
        return sentMsg, fmt.Errorf("no service list found for %s", s.Service)
    }
    if len(services) == 0 {
        log.Printf("No services available for %s for user %d", s.Service, userID)
        sentMsg, sendErr := sendMessage(cfg, chatID, "服务列表为空，正在等待k8s-cicd上报数据。请稍后重试 / Service list empty, waiting for k8s-cicd report. Please retry later.")
        if sendErr == nil {
            s.Messages = append(s.Messages, sentMsg.MessageID)
        }
        s.RetryCount++
        if s.RetryCount < 3 {
            time.Sleep(10 * time.Second)
            return sendServiceSelection(userID, chatID, cfg, s)
        }
        sentMsg, sendErr = sendMessage(cfg, chatID, "重试失败，请联系管理员 / Retry failed, contact admin.")
        if sendErr == nil {
            s.Messages = append(s.Messages, sentMsg.MessageID)
        }
        return sentMsg, fmt.Errorf("service list empty for %s after %d retries", s.Service, s.RetryCount)
    }

    log.Printf("Loaded %d services for %s: %v", len(services), s.Service, services)

    // Use editServiceSelection to render the service selection
    if len(s.Messages) > 0 {
        lastMsgID := s.Messages[len(s.Messages)-1]
        editServiceSelection(userID, chatID, lastMsgID, cfg, s)
        // Return the last message ID as a tgbotapi.Message (minimal info, as editServiceSelection handles sending)
        return tgbotapi.Message{MessageID: lastMsgID, Chat: &tgbotapi.Chat{ID: chatID}}, nil
    }
    // Initial send: use editServiceSelection to maintain consistency
    sentMsg, err := editServiceSelection(userID, chatID, 0, cfg, s)
    if err != nil {
        log.Printf("Failed to send initial service selection for user %d in chat %d: %v", userID, chatID, err)
        return tgbotapi.Message{}, err
    }
    return sentMsg, nil
}

func editServiceSelection(userID, chatID int64, msgID int, cfg *config.Config, s *DialogState) (tgbotapi.Message, error) {
    serviceLists, err := config.LoadServiceLists(cfg.ServicesDir, cfg.TelegramBots)
    if err != nil {
        log.Printf("Failed to load service lists for user %d: %v", userID, err)
        sentMsg, sendErr := sendMessage(cfg, chatID, "无法加载服务列表 / Failed to load service list.")
        if sendErr == nil {
            s.Messages = append(s.Messages, sentMsg.MessageID)
        }
        return sentMsg, fmt.Errorf("failed to load service lists: %v", err)
    }
    services, exists := serviceLists[s.Service]
    if !exists {
        log.Printf("No service list found for %s for user %d", s.Service, userID)
        sentMsg, sendErr := sendMessage(cfg, chatID, "未找到服务列表 / No service list found.")
        if sendErr == nil {
            s.Messages = append(s.Messages, sentMsg.MessageID)
        }
        return sentMsg, fmt.Errorf("no service list found for %s", s.Service)
    }
    if len(services) == 0 {
        log.Printf("No services available for %s for user %d", s.Service, userID)
        sentMsg, sendErr := sendMessage(cfg, chatID, "服务列表为空，正在等待k8s-cicd上报数据。请稍后重试 / Service list empty, waiting for k8s-cicd report. Please retry later.")
        if sendErr == nil {
            s.Messages = append(s.Messages, sentMsg.MessageID)
        }
        s.RetryCount++
        if s.RetryCount < 3 {
            time.Sleep(10 * time.Second)
            return sendServiceSelection(userID, chatID, cfg, s)
        }
        sentMsg, sendErr = sendMessage(cfg, chatID, "重试失败，请联系管理员 / Retry failed, contact admin.")
        if sendErr == nil {
            s.Messages = append(s.Messages, sentMsg.MessageID)
        }
        return sentMsg, fmt.Errorf("service list empty for %s after %d retries", s.Service, s.RetryCount)
    }

    maxLen := 0
    for _, svc := range services {
        if len(svc) > maxLen {
            maxLen = len(svc)
        }
    }
    cols := 2
    if maxLen < 10 {
        cols = 4
    } else if maxLen < 15 {
        cols = 3
    }
    if len(services) < cols {
        cols = len(services)
    }

    var buttons [][]tgbotapi.InlineKeyboardButton
    var row []tgbotapi.InlineKeyboardButton
    for i, svc := range services {
        displayText := svc
        if svc == s.Selected {
            displayText = fmt.Sprintf("<b>✅ %s</b>", svc)
        }
        row = append(row, tgbotapi.NewInlineKeyboardButtonData(displayText, svc))
        if len(row) == cols || i == len(services)-1 {
            buttons = append(buttons, row)
            row = []tgbotapi.InlineKeyboardButton{}
        }
    }
    buttons = append(buttons, []tgbotapi.InlineKeyboardButton{
        tgbotapi.NewInlineKeyboardButtonData("下一步 / Next", "next_service"),
        tgbotapi.NewInlineKeyboardButtonData("取消 / Cancel", "cancel"),
    })

    keyboard := tgbotapi.NewInlineKeyboardMarkup(buttons...)
    msgText := fmt.Sprintf("请选择服务（单选） / Please select a service (single selection):\n当前选中 / Currently selected: %s", s.Selected)

    var sentMsg tgbotapi.Message
    if msgID > 0 {
        editMsg := tgbotapi.NewEditMessageText(chatID, msgID, msgText)
        editMsg.ReplyMarkup = &keyboard
        editMsg.ParseMode = "HTML"
        sentMsg, err = sendMessage(cfg, chatID, editMsg)
        if err != nil {
            log.Printf("Failed to edit service selection message for user %d in chat %d: %v", userID, chatID, err)
            // Do not fallback to new message to avoid popups
            return tgbotapi.Message{MessageID: msgID, Chat: &tgbotapi.Chat{ID: chatID}}, err
        }
        // Update last message ID
        if len(s.Messages) > 0 {
            s.Messages[len(s.Messages)-1] = sentMsg.MessageID
        }
    } else {
        msg := tgbotapi.NewMessage(chatID, msgText)
        msg.ReplyMarkup = keyboard
        msg.ParseMode = "HTML"
        sentMsg, err = sendMessage(cfg, chatID, msg)
        if err != nil {
            log.Printf("Failed to send service selection message for user %d in chat %d: %v", userID, chatID, err)
            return tgbotapi.Message{}, err
        }
        s.Messages = append(s.Messages, sentMsg.MessageID)
    }
    return sentMsg, nil
}

func ProcessDialog(userID, chatID int64, text string, cfg *config.Config) {
    s, loaded := dialogs.Load(userID)
    if !loaded {
        log.Printf("No dialog found for user %d in chat %d", userID, chatID)
        sendMessage(cfg, chatID, "No active dialog found. Please start a new deployment.")
        return
    }
    state := s.(*DialogState)

    if time.Now().Sub(state.StartedAt) > time.Duration(cfg.DialogTimeout)*time.Second {
        log.Printf("Dialog timed out for user %d in chat %d", userID, chatID)
        deleteMessages(state, cfg)
        sendMessage(cfg, chatID, "Dialog timed out, please trigger deployment again.")
        dialogs.Delete(userID)
        return
    }

    if state.Stage == "service" {
        if text == "next_service" {
            if state.Selected == "" {
                sendMessage(cfg, chatID, "请先选择一个服务 / Please select a service first.")
                return
            }
            state.Stage = "env"
            deleteMessages(state, cfg)
            state.Messages = []int{}
            sendEnvSelection(userID, chatID, cfg, state)
            return
        } else if text == "cancel" {
            log.Printf("User %d cancelled dialog in chat %d", userID, chatID)
            deleteMessages(state, cfg)
            sendMessage(cfg, chatID, "对话已取消 / Dialog cancelled.")
            dialogs.Delete(userID)
            close(state.timeoutCancel)
            return
        } else {
            serviceLists, err := config.LoadServiceLists(cfg.ServicesDir, cfg.TelegramBots)
            if err != nil {
                log.Printf("Failed to load service lists for user %d: %v", userID, err)
                sendMessage(cfg, chatID, "无法加载服务列表 / Failed to load service list.")
                return
            }
            services, exists := serviceLists[state.Service]
            if !exists || !contains(services, text) {
                sendMessage(cfg, chatID, "无效的服务选择 / Invalid service selection.")
                return
            }
            state.Selected = text
            if len(state.Messages) > 0 {
                lastMsgID := state.Messages[len(state.Messages)-1]
                editServiceSelection(userID, chatID, lastMsgID, cfg, state)
            } else {
                sentMsg, err := sendServiceSelection(userID, chatID, cfg, state)
                if err == nil {
                    state.Messages = append(state.Messages, sentMsg.MessageID)
                }
            }
            return
        }
    }

    if state.Stage == "env" {
        if text == "next_env" {
            if len(state.SelectedEnvs) == 0 {
                sendMessage(cfg, chatID, "请至少选择一个环境 / Please select at least one environment.")
                return
            }
            state.Stage = "version"
            deleteMessages(state, cfg)
            state.Messages = []int{}
            sendVersionPrompt(userID, chatID, cfg, state)
            return
        } else if text == "cancel" {
            log.Printf("User %d cancelled dialog in chat %d", userID, chatID)
            deleteMessages(state, cfg)
            sendMessage(cfg, chatID, "对话已取消 / Dialog cancelled.")
            dialogs.Delete(userID)
            close(state.timeoutCancel)
            return
        } else {
            lowerText := strings.ToLower(text)
            if _, exists := cfg.Environments[lowerText]; !exists {
                sendMessage(cfg, chatID, "无效的环境选择 / Invalid environment selection.")
                return
            }
            if contains(state.SelectedEnvs, lowerText) {
                state.SelectedEnvs = remove(state.SelectedEnvs, lowerText)
            } else {
                state.SelectedEnvs = append(state.SelectedEnvs, lowerText)
            }
            if len(state.Messages) > 0 {
                lastMsgID := state.Messages[len(state.Messages)-1]
                editEnvSelection(userID, chatID, lastMsgID, cfg, state)
            } else {
                sentMsg, err := sendEnvSelection(userID, chatID, cfg, state)
                if err == nil {
                    state.Messages = append(state.Messages, sentMsg.MessageID)
                }
            }
            return
        }
    }

    if state.Stage == "version" {
        if text == "cancel" {
            log.Printf("User %d cancelled dialog in chat %d", userID, chatID)
            deleteMessages(state, cfg)
            sendMessage(cfg, chatID, "对话已取消 / Dialog cancelled.")
            dialogs.Delete(userID)
            close(state.timeoutCancel)
            return
        }
        state.Version = text
        state.Stage = "confirm"
        deleteMessages(state, cfg)
        state.Messages = []int{}
        sendConfirmationPrompt(userID, chatID, cfg, state)
        return
    }

    if state.Stage == "confirm" {
        if text == "confirm" {
            log.Printf("User %d confirmed deployment in chat %d: service=%s, envs=%v, version=%s",
                userID, chatID, state.Selected, state.SelectedEnvs, state.Version)
            deleteMessages(state, cfg)
            state.Messages = []int{}
            var taskKeys []string
            var tasks []types.DeployRequest
            for _, env := range state.SelectedEnvs {
                deployReq := types.DeployRequest{
                    Service:   state.Selected,
                    Env:       env,
                    Version:   state.Version,
                    Timestamp: time.Now(),
                    UserName:  state.UserName,
                    Status:    "pending_confirmation",
                }
                taskKeys = append(taskKeys, queue.ComputeTaskKey(deployReq))
                tasks = append(tasks, deployReq)
            }
            id := uuid.New().String()[:8]
            PendingConfirmations.Store(id, tasks)
            message := fmt.Sprintf("确认部署服务 %s 到环境 %s，版本 %s，由用户 %s 提交？\nConfirm deployment for service %s to envs %s, version %s by %s?",
                state.Selected, strings.Join(state.SelectedEnvs, ","), state.Version, state.UserName,
                state.Selected, strings.Join(state.SelectedEnvs, ","), state.Version, state.UserName)
            sentMsg, err := SendConfirmation(state.Service, chatID, message, id, cfg)
            if err != nil {
                log.Printf("Failed to send confirmation for user %d: %v", userID, err)
                sendMessage(cfg, chatID, "无法发送确认消息，请稍后重试 / Failed to send confirmation, please try again later.")
                dialogs.Delete(userID)
                close(state.timeoutCancel)
                return
            }
            state.Messages = append(state.Messages, sentMsg.MessageID)
            state.Stage = "continue"
            return
        } else if text == "cancel" {
            log.Printf("User %d cancelled dialog in chat %d", userID, chatID)
            deleteMessages(state, cfg)
            sendMessage(cfg, chatID, "对话已取消 / Dialog cancelled.")
            dialogs.Delete(userID)
            close(state.timeoutCancel)
            return
        } else {
            sendMessage(cfg, chatID, "请确认或取消 / Please confirm or cancel.")
            return
        }
    }

    if state.Stage == "continue" {
        sendMessage(cfg, chatID, "请等待管理员确认 / Please wait for admin confirmation.")
        return
    }
}

// Helper function to validate version format (customize as needed)
func isValidVersion(version string) bool {
	return len(version) > 0 // Add specific validation if needed
}

func validateEnvironment(env string, cfg *config.Config) bool {
	fileName := filepath.Join(cfg.StorageDir, "environments.json")
	if data, err := os.ReadFile(fileName); err == nil {
		var envs []string
		if err := json.Unmarshal(data, &envs); err == nil {
			for _, e := range envs {
				if strings.ToLower(e) == strings.ToLower(env) {
					return true
				}
			}
		}
	}
	_, exists := cfg.Environments[strings.ToLower(env)]
	return exists
}

func getEnvironmentsFromDeployFile(cfg *config.Config) []string {
	fileName := storage.GetDailyFileName(time.Now(), "deploy", cfg.StorageDir)
	if err := storage.EnsureDailyFile(fileName, nil, cfg); err != nil {
		log.Printf("Failed to ensure deploy file for environments: %v", err)
		return []string{}
	}

	data, err := os.ReadFile(fileName)
	if err != nil {
		log.Printf("Failed to read deploy file: %v", err)
		return []string{}
	}
	var infos []storage.DeploymentInfo
	if err := json.Unmarshal(data, &infos); err != nil {
		log.Printf("Failed to unmarshal deploy file: %v", err)
		return []string{}
	}

	envSet := make(map[string]bool)
	for _, info := range infos {
		envSet[info.Env] = true
	}
	var envs []string
	for env := range envSet {
		envs = append(envs, env)
	}
	sort.Strings(envs)
	return envs
}

func sendEnvSelection(userID, chatID int64, cfg *config.Config, s *DialogState) {
	// 尝试从deploy文件或cfg.Environments获取环境列表
	envs := getEnvironmentsFromDeployFile(cfg)
	if len(envs) == 0 {
		// Fallback到cfg.Environments
		for env := range cfg.Environments {
			envs = append(envs, env)
		}
		sort.Strings(envs) // 确保顺序一致
	}

	// 如果环境列表仍为空，提示并重试
	if len(envs) == 0 {
		log.Printf("No environments available for user %d in chat %d", userID, chatID)
		sendMessage(cfg, chatID, "无可用环境，正在等待k8s-cicd上报数据。请稍后重试 / No environments available, waiting for k8s-cicd report. Please retry later.")
		
		// 检查重试次数（使用s.Messages长度作为代理，限制3次）
		if len(s.Messages) < 3 {
			time.Sleep(10 * time.Second) // 延迟10秒重试
			sendEnvSelection(userID, chatID, cfg, s) // 递归重试
		} else {
			sendMessage(cfg, chatID, "重试失败，请联系管理员 / Retry failed, contact admin.")
		}
		return
	}

	// 生成按钮，保留选中标记
	var buttons [][]tgbotapi.InlineKeyboardButton
	var row []tgbotapi.InlineKeyboardButton
	cols := 2
	if len(envs) < cols {
		cols = len(envs)
	}
	for i, env := range envs {
		displayText := env
		if contains(s.SelectedEnvs, env) {
			displayText = fmt.Sprintf("<b>✅ %s</b>", env)
		}
		row = append(row, tgbotapi.NewInlineKeyboardButtonData(displayText, env))
		if len(row) == cols || i == len(envs)-1 {
			buttons = append(buttons, row)
			row = []tgbotapi.InlineKeyboardButton{}
		}
	}
	buttons = append(buttons, []tgbotapi.InlineKeyboardButton{
		tgbotapi.NewInlineKeyboardButtonData("下一步 / Next", "next_env"),
		tgbotapi.NewInlineKeyboardButtonData("取消 / Cancel", "cancel"),
	})

	// 创建消息
	keyboard := tgbotapi.NewInlineKeyboardMarkup(buttons...)
	msgText := fmt.Sprintf("请选择环境（可多选） / Please select environments (multiple selection allowed):\n当前选中 / Currently selected: %s", strings.Join(s.SelectedEnvs, ", "))
	msg := tgbotapi.NewMessage(chatID, msgText)
	msg.ReplyMarkup = keyboard
	msg.ParseMode = "HTML"

	// 优先编辑上一条消息
	if len(s.Messages) > 0 {
		edit := tgbotapi.EditMessageTextConfig{
			BaseEdit: tgbotapi.BaseEdit{
				ChatID:    chatID,
				MessageID: s.Messages[len(s.Messages)-1],
			},
			Text:      msgText,
			ParseMode: "HTML",
		}
		edit.ReplyMarkup = &keyboard
		if _, err := sendMessage(cfg, chatID, edit); err != nil {
			log.Printf("Failed to edit message: %v", err)
			// 编辑失败，发送新消息
			if sentMsg, err := sendMessage(cfg, chatID, msg); err == nil {
				s.Messages = append(s.Messages, sentMsg.MessageID)
			}
		}
	} else {
		// 无上一条消息，直接发送新消息
		if sentMsg, err := sendMessage(cfg, chatID, msg); err == nil {
			s.Messages = append(s.Messages, sentMsg.MessageID)
		}
	}
}

func sendConfirmation(userID, chatID int64, cfg *config.Config, s *DialogState) {
	message := fmt.Sprintf(
		"确认部署服务 / Confirm deployment:\n服务 / Service: %s\n环境 / Environments: %s\n版本 / Version: %s\n提交用户 / Submitted by: %s",
		s.Selected, strings.Join(s.SelectedEnvs, ", "), s.Version, s.UserName,
	)
	buttons := [][]tgbotapi.InlineKeyboardButton{
		{
			tgbotapi.NewInlineKeyboardButtonData("确认 / Confirm", "confirm"),
			tgbotapi.NewInlineKeyboardButtonData("取消 / Cancel", "cancel"),
		},
	}
	keyboard := tgbotapi.NewInlineKeyboardMarkup(buttons...)
	msg := tgbotapi.NewMessage(chatID, message)
	msg.ReplyMarkup = keyboard
	msg.ParseMode = "HTML"
	if sentMsg, err := sendMessage(cfg, chatID, msg); err == nil {
		s.Messages = append(s.Messages, sentMsg.MessageID)
	}
}

func submitTasks(userID, chatID int64, cfg *config.Config, s *DialogState) {
	id := uuid.New().String()[:8]
	var tasks []types.DeployRequest
	for _, env := range s.SelectedEnvs {
		tasks = append(tasks, types.DeployRequest{
			Service:   s.Selected,
			Env:       env,
			Version:   s.Version,
			Timestamp: time.Now(),
			UserName:  s.UserName,
			Status:    "pending_confirmation",
		})
	}
	PendingConfirmations.Store(id, tasks)

	message := fmt.Sprintf(
		"确认部署服务 %s 到环境 %s，版本 %s，由用户 %s 提交？\nConfirm deployment for service %s to envs %s, version %s by %s?",
		s.Selected, strings.Join(s.SelectedEnvs, ","), s.Version, s.UserName,
		s.Selected, strings.Join(s.SelectedEnvs, ","), s.Version, s.UserName,
	)
	if err := SendConfirmation(s.Service, chatID, message, id, cfg); err != nil {
		log.Printf("Failed to send confirmation: %v", err)
		sendMessage(cfg, chatID, "Failed to send confirmation to Telegram.")
		return
	}

	sendMessage(cfg, chatID, "Task submitted, awaiting confirmation in Telegram.")
}

func sendVersionPrompt(userID, chatID int64, cfg *config.Config, s *DialogState) {
	msg := tgbotapi.NewMessage(chatID, "请输入版本号 / Please enter the version number:")
	msg.ParseMode = "HTML"
	if sentMsg, err := sendMessage(cfg, chatID, msg); err == nil {
		s.Messages = append(s.Messages, sentMsg.MessageID)
	}
}

func sendContinuePrompt(userID, chatID int64, cfg *config.Config, s *DialogState) {
	message := "是否继续部署其他服务？\nWould you like to continue deploying another service?"
	buttons := [][]tgbotapi.InlineKeyboardButton{
		{
			tgbotapi.NewInlineKeyboardButtonData("是 / Yes", "yes"),
			tgbotapi.NewInlineKeyboardButtonData("否 / No", "no"),
		},
	}
	keyboard := tgbotapi.NewInlineKeyboardMarkup(buttons...)
	msg := tgbotapi.NewMessage(chatID, message)
	msg.ReplyMarkup = keyboard
	msg.ParseMode = "HTML"
	if sentMsg, err := sendMessage(cfg, chatID, msg); err == nil {
		s.Messages = append(s.Messages, sentMsg.MessageID)
	}
}

func CancelDialog(userID, chatID int64, cfg *config.Config) bool {
	state, loaded := dialogs.Load(userID)
	if !loaded {
		log.Printf("No active dialog to cancel for user %d in chat %d", userID, chatID)
		return false
	}
	s := state.(*DialogState)
	select {
	case s.timeoutCancel <- true:
	default:
	}
	deleteMessages(s, cfg)
	dialogs.Delete(userID)
	log.Printf("Dialog cancelled for user %d in chat %d", userID, chatID)
	sendMessage(cfg, chatID, "当前会话已关闭。\nCurrent session has been closed.")
	return true
}

func IsDialogActive(userID int64) bool {
	_, ok := dialogs.Load(userID)
	return ok
}

func monitorDialogTimeout(userID, chatID int64, cfg *config.Config, s *DialogState) {
	select {
	case <-time.After(time.Duration(cfg.DialogTimeout) * time.Second):
		if _, loaded := dialogs.Load(userID); loaded {
			log.Printf("Dialog timed out for user %d in chat %d", userID, chatID)
			deleteMessages(s, cfg)
			sendMessage(cfg, chatID, "对话已超时，请重新触发部署。\nDialog timed out, please trigger deployment again.")
			dialogs.Delete(userID)
		}
	case <-s.timeoutCancel:
		log.Printf("Timeout cancelled for user %d in chat %d", userID, chatID)
	}
}

func sendMessage(cfg *config.Config, chatID int64, text interface{}) (tgbotapi.Message, error) {
	service := ""
	for svc, id := range cfg.TelegramChats {
		if id == chatID {
			service = svc
			break
		}
	}
	if service == "" {
		log.Printf("No service found for chat %d, trying default chat", chatID)
		if defaultChatID, ok := cfg.TelegramChats["other"]; ok {
			chatID = defaultChatID
			service = "other"
		} else {
			log.Printf("No default chat configured for chat %d", chatID)
			return tgbotapi.Message{}, fmt.Errorf("no service or default chat found for chat %d", chatID)
		}
	}
	token, ok := cfg.TelegramBots[service]
	if !ok {
		log.Printf("No bot configured for service %s", service)
		return tgbotapi.Message{}, fmt.Errorf("no bot configured for service %s", service)
	}
	bot, err := tgbotapi.NewBotAPI(token)
	if err != nil {
		log.Printf("Failed to create bot for service %s: %v", service, err)
		return tgbotapi.Message{}, err
	}

	var sentMsg tgbotapi.Message
	switch t := text.(type) {
	case string:
		msg := tgbotapi.NewMessage(chatID, t)
		msg.ParseMode = "HTML"
		sentMsg, err = bot.Send(msg)
	case tgbotapi.MessageConfig:
		t.ParseMode = "HTML"
		sentMsg, err = bot.Send(t)
	case tgbotapi.EditMessageTextConfig:
		sentMsg, err = bot.Send(t)
	}
	if err != nil {
		log.Printf("Failed to send/edit message to chat %d: %v", chatID, err)
		return tgbotapi.Message{}, err
	}
	storage.PersistInteraction(cfg, map[string]interface{}{
		"chat_id":   chatID,
		"message":   fmt.Sprint(text),
		"timestamp": time.Now().Format(time.RFC3339),
	})
	return sentMsg, nil
}

func deleteMessages(s *DialogState, cfg *config.Config) {
	service := ""
	for svc, id := range cfg.TelegramChats {
		if id == s.ChatID {
			service = svc
			break
		}
	}
	if service == "" {
		log.Printf("No service found for chat %d, trying default chat", s.ChatID)
		if defaultChatID, ok := cfg.TelegramChats["other"]; ok {
			s.ChatID = defaultChatID
			service = "other"
		} else {
			log.Printf("No default chat configured for chat %d", s.ChatID)
			return
		}
	}
	token, ok := cfg.TelegramBots[service]
	if !ok {
		log.Printf("No bot configured for service %s", service)
		return
	}
	bot, err := tgbotapi.NewBotAPI(token)
	if err != nil {
		log.Printf("Failed to create bot for service %s: %v", service, err)
		return
	}

	for _, msgID := range s.Messages {
		deleteMsg := tgbotapi.NewDeleteMessage(s.ChatID, msgID)
		if _, err := bot.Request(deleteMsg); err != nil {
			log.Printf("Failed to delete message %d in chat %d: %v", msgID, s.ChatID, err)
		} else {
			log.Printf("Deleted message %d in chat %d", msgID, s.ChatID)
		}
	}
}

func SendConfirmation(category string, chatID int64, message string, callbackData string, cfg *config.Config) error {
	token, ok := cfg.TelegramBots[category]
	if !ok {
		log.Printf("No bot configured for category %s", category)
		return fmt.Errorf("no bot configured for category %s", category)
	}
	bot, err := tgbotapi.NewBotAPI(token)
	if err != nil {
		log.Printf("Failed to create bot for category %s: %v", category, err)
		return err
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
	_, err = bot.Send(msg)
	return err
}

func remove(slice []string, item string) []string {
	for i, s := range slice {
		if s == item {
			return append(slice[:i], slice[i+1:]...)
		}
	}
	return slice
}

func contains(slice []string, item string) bool {
	for _, s := range slice {
		if s == item {
			return true
		}
	}
	return false
}