package telegram

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"

	"k8s-cicd/agent/client"
	"k8s-cicd/agent/config"
	"k8s-cicd/agent/models"

	"github.com/fatih/color"
	"github.com/google/uuid" // 新增：生成 task_id
	"github.com/sirupsen/logrus"
)

// BotManager 多机器人管理器
type BotManager struct {
	Bots               map[string]*TelegramBot
	globalAllowedUsers []string
	mongo              *client.MongoClient // 新增：Mongo 客户端
	updateChan         chan map[string]interface{}
	stopChan           chan struct{}
}

// NewBotManager 创建管理器
func NewBotManager(bots []config.TelegramBot) *BotManager {
	m := &BotManager{
		Bots:       make(map[string]*TelegramBot),
		updateChan: make(chan map[string]interface{}, 100),
		stopChan:   make(chan struct{}),
	}

	for i := range bots {
		if bots[i].IsEnabled {
			bot := &TelegramBot{
				Name:       bots[i].Name,
				Token:      bots[i].Token,
				GroupID:    bots[i].GroupID,
				Services:   bots[i].Services,
				RegexMatch: bots[i].RegexMatch,
				IsEnabled:  true,
			}
			m.Bots[bot.Name] = bot
			logrus.Infof(color.GreenString("Telegram机器人 [%s] 已启用"), bot.Name)
		}
	}

	if len(m.Bots) == 0 {
		logrus.Warn("未启用任何Telegram机器人")
	}
	logrus.Info(color.GreenString("BotManager创建成功"))
	return m
}

// SetMongoClient 注入 Mongo 客户端（Agent 中调用）
func (bm *BotManager) SetMongoClient(mongo *client.MongoClient) {
	bm.mongo = mongo
}

// SetGlobalAllowedUsers 设置全局允许用户
func (bm *BotManager) SetGlobalAllowedUsers(users []string) {
	bm.globalAllowedUsers = users
}

// StartPolling 启动轮询
func (bm *BotManager) StartPolling() {
	logrus.Info(color.GreenString("启动Telegram Updates轮询"))
	go func() {
		for {
			select {
			case <-bm.stopChan:
				logrus.Info(color.GreenString("Telegram轮询已停止"))
				return
			default:
				bm.pollUpdates()
			}
		}
	}()
}

// pollUpdates 轮询更新
func (bm *BotManager) pollUpdates() {
	bot := bm.getDefaultBot()
	if bot == nil {
		time.Sleep(5 * time.Second)
		return
	}

	url := fmt.Sprintf("https://api.telegram.org/bot%s/getUpdates?offset=%d&timeout=10", bot.Token, bm.offset)
	resp, err := http.Get(url)
	if err != nil {
		logrus.Errorf(color.RedString("Telegram轮询错误: %v"), err)
		time.Sleep(5 * time.Second)
		return
	}
	defer resp.Body.Close()

	var result struct {
		Ok     bool                       `json:"ok"`
		Result []map[string]interface{}   `json:"result"`
	}
	if json.NewDecoder(resp.Body).Decode(&result); err != nil {
		logrus.Errorf(color.RedString("解析响应失败: %v"), err)
		return
	}

	for _, update := range result.Result {
		bm.offset = int64(update["update_id"].(float64)) + 1
		bm.updateChan <- update
	}
}

// PollUpdates 消费 updateChan
func (bm *BotManager) PollUpdates(confirmChan chan<- models.DeployRequest, rejectChan chan<- models.StatusRequest) {
	logrus.Info(color.GreenString("开始处理Telegram回调"))
	for {
		select {
		case update := <-bm.updateChan:
			bm.HandleCallback(update, confirmChan, rejectChan)
		case <-bm.stopChan:
			logrus.Info(color.GreenString("Telegram回调处理停止"))
			return
		}
	}
}

// HandleCallback 处理回调（精确 task_id 匹配）
func (bm *BotManager) HandleCallback(update map[string]interface{}, confirmChan chan<- models.DeployRequest, rejectChan chan<- models.StatusRequest) {
	callback, ok := update["callback_query"].(map[string]interface{})
	if !ok {
		return
	}

	from := callback["from"].(map[string]interface{})
	userName := from["username"].(string)
	data := callback["data"].(string)

	logrus.Infof("收到回调: username=%s, data=%s", userName, data)

	// 权限检查
	if !contains(bm.globalAllowedUsers, userName) {
		logrus.Warnf("用户无权限: %s", userName)
		return
	}

	parts := strings.SplitN(data, ":", 2)
	if len(parts) != 2 {
		logrus.Warnf("callback_data 格式错误: %s", data)
		return
	}
	action, taskID := parts[0], parts[1]

	// 精确查找任务
	task, err := bm.findTaskByTaskID(taskID)
	if err != nil {
		logrus.Warnf("任务未找到 task_id=%s: %v", taskID, err)
		return
	}

	// 删除原弹窗
	message := callback["message"].(map[string]interface{})
	messageID := int(message["message_id"].(float64))
	chatID := fmt.Sprintf("%v", message["chat"].(map[string]interface{})["id"])
	bot, _ := bm.getBotForService(task.Service)
	bm.DeleteMessage(bot, chatID, messageID)

	// 反馈消息
	actionText := map[string]string{"confirm": "确认", "reject": "拒绝"}[action]
	feedback := fmt.Sprintf("Success 用户 @%s 已 *%s* 部署: `%s` v`%s` 在 `%s`",
		escapeMarkdownV2(userName), actionText,
		escapeMarkdownV2(task.Service), escapeMarkdownV2(task.Version), escapeMarkdownV2(task.Environments[0]))

	feedbackID, _ := bm.sendMessage(bot, chatID, feedback, "MarkdownV2")
	go func() {
		time.Sleep(30 * time.Second)
		bm.DeleteMessage(bot, chatID, feedbackID)
	}()

	// 推送到通道
	if action == "confirm" {
		confirmChan <- *task
	} else {
		rejectChan <- models.StatusRequest{
			Service:     task.Service,
			Version:     task.Version,
			Environment: task.Environments[0],
			User:        task.User,
			Status:      "rejected",
		}
	}

	logrus.Infof(color.GreenString("回调处理完成: %s task_id=%s"), action, taskID)
}

// findTaskByTaskID 精确查找任务
func (bm *BotManager) findTaskByTaskID(taskID string) (*models.DeployRequest, error) {
	if bm.mongo == nil {
		return nil, fmt.Errorf("mongo client not set")
	}
	for env := range bm.mongo.GetEnvMappings() {
		coll := bm.mongo.GetClient().Database("cicd").Collection(fmt.Sprintf("tasks_%s", env))
		filter := bson.M{
			"task_id":             taskID,
			"confirmation_status": bson.M{"$in": []string{"pending", "sent"}},
		}
		var task models.DeployRequest
		if err := coll.FindOne(context.Background(), filter).Decode(&task); err == nil {
			return &task, nil
		}
	}
	return nil, fmt.Errorf("task not found for task_id: %s", taskID)
}

// SendConfirmation 发送弹窗（保留原 UI）
func (bm *BotManager) SendConfirmation(task *models.DeployRequest, bot *TelegramBot) (int, error) {
	// 生成 task_id
	if task.TaskID == "" {
		task.TaskID = uuid.New().String()
	}

	env := task.Environments[0]

	// @用户列表
	var mentions strings.Builder
	for _, uid := range bm.globalAllowedUsers {
		mentions.WriteString("@")
		mentions.WriteString(uid)
		mentions.WriteString(" ")
	}

	// 弹窗文本（完全保留）
	message := fmt.Sprintf("*Deployment Confirmation 部署确认*\n\n"+
		"**服务**: `%s`\n"+
		"**环境**: `%s`\n"+
		"**版本**: `%s`\n"+
		"**用户**: `%s`\n\n"+
		"*请选择操作*\n\n"+
		"通知: %s",
		escapeMarkdownV2(task.Service),
		escapeMarkdownV2(env),
		escapeMarkdownV2(task.Version),
		escapeMarkdownV2(task.User),
		mentions.String(),
	)

	// 短 callback_data
	confirmData := fmt.Sprintf("confirm:%s", task.TaskID)
	rejectData := fmt.Sprintf("reject:%s", task.TaskID)

	keyboard := map[string]interface{}{
		"inline_keyboard": [][]map[string]string{
			{
				{"text": "Confirm 确认部署", "callback_data": confirmData},
				{"text": "Reject 拒绝部署", "callback_data": rejectData},
			},
		},
	}

	messageID, err := bm.sendMessageWithKeyboard(bot, bot.GroupID, message, keyboard, "MarkdownV2")
	if err != nil {
		logrus.Errorf(color.RedString("弹窗发送失败: %v"), err)
		return 0, err
	}

	// 记录 message_id
	if bm.mongo != nil {
		bm.mongo.UpdatePopupMessageID(task.Service, task.Version, env, task.User, messageID)
	}

	logrus.Infof(color.GreenString("弹窗发送成功: %s v%s [%s] -> task_id=%s"),
		task.Service, task.Version, env, task.TaskID)
	return messageID, nil
}

// 工具函数
func contains(slice []string, item string) bool {
	for _, s := range slice {
		if s == item {
			return true
		}
	}
	return false
}

func escapeMarkdownV2(text string) string {
	escapeChars := []rune{'_', '*', '[', ']', '(', ')', '~', '`', '>', '#', '+', '-', '=', '|', '{', '}', '.', '!'}
	var result strings.Builder
	for _, c := range text {
		if containsRune(escapeChars, c) {
			result.WriteString("\\")
		}
		result.WriteRune(c)
	}
	return result.String()
}

func containsRune(slice []rune, r rune) bool {
	for _, s := range slice {
		if s == r {
			return true
		}
	}
	return false
}

// ======================
// sendMessage 基础发送（无键盘）
// ======================
func (bm *BotManager) sendMessage(bot *TelegramBot, chatID, text, parseMode string) (int, error) {
	text = escapeMarkdownV2(text)
	payload := map[string]interface{}{
		"chat_id":     chatID,
		"text":        text,
		"parse_mode":  parseMode,
	}
	jsonData, _ := json.Marshal(payload)
	resp, err := http.Post(fmt.Sprintf("https://api.telegram.org/bot%s/sendMessage", bot.Token), "application/json", bytes.NewBuffer(jsonData))
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()

	var result struct {
		Ok          bool                   `json:"ok"`
		Result      map[string]interface{} `json:"result"`
		ErrorCode   int                    `json:"error_code"`
		Description string                 `json:"description"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return 0, err
	}
	if !result.Ok {
		return 0, fmt.Errorf("Telegram API错误: code=%d, description=%s", result.ErrorCode, result.Description)
	}
	return int(result.Result["message_id"].(float64)), nil
}

// ======================
// generateMarkdownMessage 生成通知消息
// ======================
func (bm *BotManager) generateMarkdownMessage(service, env, user, oldVersion, newVersion string, success bool) string {
	safe := escapeMarkdownV2
	status := "Success *部署成功*"
	if !success {
		status = "Failure *部署失败\\-已回滚*"
	}

	return fmt.Sprintf("*Deployment %s %s*\n\n"+
		"**服务**: `%s`\n"+
		"**环境**: `%s`\n"+
		"**操作人**: `%s`\n"+
		"**旧版本**: `%s`\n"+
		"**新版本**: `%s`\n"+
		"**状态**: %s\n"+
		"**时间**: `%s`\n\n"+
		"---\n"+
		"*由 K8s\\-CICD Agent 自动发送*",
		safe(service), map[bool]string{true: "成功", false: "失败"}[success],
		safe(service), safe(env), safe(user), safe(oldVersion), safe(newVersion), status,
		time.Now().Format("2006-01-02 15:04:05"))
}


// ======================
// 5. 核心：服务匹配机器人（保留原始设计）
// ======================
// getBotForService 根据服务名选择机器人
func (bm *BotManager) getBotForService(service string) (*TelegramBot, error) {
	startTime := time.Now()
	// 步骤1：遍历所有机器人
	for _, bot := range bm.Bots {
		// 步骤2：遍历服务的匹配规则
		for _, serviceList := range bot.Services {
			// 步骤3：遍历服务列表中的模式
			for _, pattern := range serviceList {
				if bot.RegexMatch {
					// 使用正则匹配
					matched, err := regexp.MatchString(pattern, service)
					if err == nil && matched {
						logrus.WithFields(logrus.Fields{
							"time":   time.Now().Format("2006-01-02 15:04:05"),
							"method": "getBotForService",
							"took":   time.Since(startTime),
						}).Infof(color.GreenString("服务 %s 匹配机器人 %s", service, bot.Name))
						return bot, nil
					}
				} else {
					// 使用前缀匹配（忽略大小写）
					if strings.HasPrefix(strings.ToUpper(service), strings.ToUpper(pattern)) {
						logrus.WithFields(logrus.Fields{
							"time":   time.Now().Format("2006-01-02 15:04:05"),
							"method": "getBotForService",
							"took":   time.Since(startTime),
						}).Infof(color.GreenString("服务 %s 匹配机器人 %s", service, bot.Name))
						return bot, nil
					}
				}
			}
		}
	}
	// 步骤4：未匹配，返回错误
	logrus.WithFields(logrus.Fields{
		"time":   time.Now().Format("2006-01-02 15:04:05"),
		"method": "getBotForService",
		"took":   time.Since(startTime),
	}).Errorf(color.RedString("服务 %s 未匹配任何机器人", service))
	return nil, fmt.Errorf("服务 %s 未匹配任何机器人", service)
}

// ======================
// 10. 删除消息
// ======================
func (bm *BotManager) DeleteMessage(bot *TelegramBot, chatID string, messageID int) error {
	payload := map[string]interface{}{
		"chat_id":    chatID,
		"message_id": messageID,
	}
	jsonData, _ := json.Marshal(payload)
	resp, err := http.Post(fmt.Sprintf("https://api.telegram.org/bot%s/deleteMessage", bot.Token), "application/json", bytes.NewBuffer(jsonData))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return nil
}