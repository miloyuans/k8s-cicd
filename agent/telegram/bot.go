// telegram/bot.go
package telegram

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"context"
	"time"

	"k8s-cicd/agent/config"
	"k8s-cicd/agent/models"
	"k8s-cicd/agent/client"

	"go.mongodb.org/mongo-driver/bson"
	"github.com/fatih/color"
	"github.com/sirupsen/logrus"
)

// ======================
// 工具函数
// ======================

// escapeMarkdownV2 转义 MarkdownV2 特殊字符
func escapeMarkdownV2(text string) string {
	escapeChars := []rune{'_', '*', '[', ']', '(', ')', '~', '`', '>', '#', '+', '-', '=', '|', '{', '}', '.', '!'}

	var result strings.Builder
	inCode := false
	inLinkURL := false
	i := 0

	for i < len(text) {
		c := rune(text[i])

		if c == '`' && !inCode {
			count := 0
			for j := i; j < len(text) && text[j] == '`'; j++ {
				count++
			}
			if count >= 1 {
				inCode = !inCode
				result.WriteString(text[i : i+count])
				i += count
				continue
			}
		}

		if inCode {
			result.WriteRune(c)
			i++
			continue
		}

		if c == '[' {
			result.WriteRune(c)
			i++
			continue
		}
		if c == ']' && i+1 < len(text) && text[i+1] == '(' {
			result.WriteString("](")
			i += 2
			inLinkURL = true
			continue
		}
		if c == ')' && inLinkURL {
			result.WriteRune(c)
			i++
			inLinkURL = false
			continue
		}

		if containsRune(escapeChars, c) {
			result.WriteString("\\")
		}
		result.WriteRune(c)
		i++
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
// sendMessageWithKeyboard 发送带键盘消息
// ======================
func (bm *BotManager) sendMessageWithKeyboard(bot *TelegramBot, chatID, text string, keyboard map[string]interface{}, parseMode string) (int, error) {
	text = escapeMarkdownV2(text)
	payload := map[string]interface{}{
		"chat_id":      chatID,
		"text":         text,
		"parse_mode":   parseMode,
		"reply_markup": keyboard,
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
// SendNotification 发送通知
// ======================
func (bm *BotManager) SendNotification(service, env, user, oldImage, newVersion string, success bool) error {
	bot, err := bm.getBotForService(service)
	if err != nil {
		return err
	}

	oldTag := extractTag(oldImage)
	newTag := extractTag(newVersion)

	message := bm.generateMarkdownMessage(service, env, user, oldTag, newTag, success)
	_, err = bm.sendMessage(bot, bot.GroupID, message, "MarkdownV2")
	if err != nil {
		logrus.Errorf(color.RedString("发送通知失败: %v", err))
	}
	return err
}

// ======================
// 2. 结构体定义（保留原始设计）
// ======================
type TelegramBot struct {
	Name         string
	Token        string
	GroupID      string
	Services     map[string][]string // prefix -> []patterns
	RegexMatch   bool
	IsEnabled    bool
	AllowedUsers []string
}

type BotManager struct {
	Bots               map[string]*TelegramBot
	offset             int64
	updateChan         chan map[string]interface{}
	stopChan           chan struct{}
	globalAllowedUsers []string
}

// ======================
// 3. 初始化（保留多机器人 + 匹配规则）
// ======================
func NewBotManager(bots []config.TelegramBot) *BotManager {
	m := &BotManager{
		Bots:               make(map[string]*TelegramBot),
		updateChan:         make(chan map[string]interface{}, 100),
		stopChan:           make(chan struct{}),
		globalAllowedUsers: make([]string, 0),
	}

	for i := range bots {
		if bots[i].IsEnabled {
			bot := &TelegramBot{
				Name:         bots[i].Name,
				Token:        bots[i].Token,
				GroupID:      bots[i].GroupID,
				Services:     bots[i].Services,
				RegexMatch:   bots[i].RegexMatch,
				IsEnabled:    true,
				AllowedUsers: bots[i].AllowedUsers,
			}
			m.Bots[bot.Name] = bot
			logrus.Infof(color.GreenString("Telegram机器人 [%s] 已启用", bot.Name))
		}
	}

	if len(m.Bots) == 0 {
		logrus.Warn("未启用任何Telegram机器人")
	}
	return m
}

func (bm *BotManager) SetGlobalAllowedUsers(users []string) {
	bm.globalAllowedUsers = users
}

// ======================
// 4. 轮询（保留默认机器人用于轮询）
// ======================
func (bm *BotManager) StartPolling() {
	go func() {
		for {
			select {
			case <-bm.stopChan:
				return
			default:
				bm.pollUpdates()
			}
		}
	}()
}

func (bm *BotManager) pollUpdates() {
	bot := bm.getDefaultBot()
	if bot == nil {
		time.Sleep(5 * time.Second)
		return
	}

	url := fmt.Sprintf("https://api.telegram.org/bot%s/getUpdates?offset=%d&timeout=10", bot.Token, bm.offset)
	resp, err := http.Get(url)
	if err != nil {
		logrus.Errorf(color.RedString("Telegram轮询网络错误: %v", err))
		time.Sleep(5 * time.Second)
		return
	}
	defer resp.Body.Close()

	var result struct {
		Ok     bool                       `json:"ok"`
		Result []map[string]interface{}   `json:"result"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return
	}

	for _, update := range result.Result {
		bm.offset = int64(update["update_id"].(float64)) + 1
		bm.updateChan <- update
	}
}

func (bm *BotManager) getDefaultBot() *TelegramBot {
	for _, bot := range bm.Bots {
		if bot.IsEnabled {
			return bot
		}
	}
	return nil
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
// 6. 统一发送消息（自动转义 + 支持键盘）
// ======================
func (bm *BotManager) sendTelegramMessage(bot *TelegramBot, chatID, text string, keyboard map[string]interface{}, parseMode string) (int, error) {
	text = escapeMarkdownV2(text)

	payload := map[string]interface{}{
		"chat_id":    chatID,
		"text":       text,
		"parse_mode": parseMode,
	}
	if keyboard != nil {
		payload["reply_markup"] = keyboard
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

// truncateString 截断字符串到指定长度（按字节）
func truncateString(s string, maxBytes int) string {
    if len(s) <= maxBytes {
        return s
    }
    // 按字节截断，避免中文乱码
    for i := maxBytes; i > 0; i-- {
        if (s[i] & 0xC0) != 0x80 {
            return s[:i]
        }
    }
    return s[:maxBytes]
}

// ======================
// 7. 发送确认弹窗（返回 message_id）
// ======================
// SendConfirmation 发送确认弹窗（保留原 UI，优化 callback_data）
func (bm *BotManager) SendConfirmation(task *models.DeployRequest, bot *TelegramBot) (int, error) {
	startTime := time.Now()

	// 1. 生成短唯一 task_id（若未生成）
	if task.TaskID == "" {
		task.TaskID = uuid.New().String()
	}

	env := task.Environments[0]

	// 2. 构建 @用户 列表
	var mentions strings.Builder
	for _, uid := range bm.globalAllowedUsers {
		mentions.WriteString("@")
		mentions.WriteString(uid)
		mentions.WriteString(" ")
	}

	// 3. 构建确认消息（完全保留原模板）
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

	// 4. 短 callback_data：confirm:<task_id>
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

	// 5. 发送弹窗
	messageID, err := bm.sendMessageWithKeyboard(bot, bot.GroupID, message, keyboard, "MarkdownV2")
	if err != nil {
		logrus.WithFields(logrus.Fields{
			"time":   time.Now().Format("2006-01-02 15:04:05"),
			"method": "SendConfirmation",
		}).Errorf(color.RedString("弹窗发送失败: %v"), err)
		return 0, err
	}

	// 6. 记录 message_id（用于删除）
	if bm.mongo != nil {
		if err := bm.mongo.UpdatePopupMessageID(task.Service, task.Version, env, task.User, messageID); err != nil {
			logrus.Warnf("记录弹窗消息ID失败: %v", err)
		}
	}

	logrus.WithFields(logrus.Fields{
		"time":   time.Now().Format("2006-01-02 15:04:05"),
		"method": "SendConfirmation",
	}).Infof(color.GreenString("弹窗发送成功: %s v%s [%s] -> task_id=%s, message_id=%d",
		task.Service, task.Version, env, task.TaskID, messageID))

	return messageID, nil
}

// findTaskByTaskID 通过 task_id 精确查找任务（O(1) 级）
func (bm *BotManager) findTaskByTaskID(taskID string, mongo *client.MongoClient) (*models.DeployRequest, error) {
	ctx := context.Background()
	for env := range mongo.GetEnvMappings() {
		coll := mongo.GetClient().Database("cicd").Collection(fmt.Sprintf("tasks_%s", env))
		filter := bson.M{
			"task_id":             taskID,
			"confirmation_status": bson.M{"$in": []string{"pending", "sent"}},
		}
		var task models.DeployRequest
		if err := coll.FindOne(ctx, filter).Decode(&task); err == nil {
			return &task, nil
		}
	}
	return nil, fmt.Errorf("task not found for task_id: %s", taskID)
}

// ======================
// 8. 发送发布通知（仅显示 tag）
// ======================
func extractTag(image string) string {
	parts := strings.Split(image, ":")
	if len(parts) > 1 {
		return parts[len(parts)-1]
	}
	return image
}

// 通过 task_id 查找任务
func (bm *BotManager) findTaskByID(taskID string, mongo *client.MongoClient) (*models.DeployRequest, error) {
	ctx := context.Background()
	for env := range mongo.GetEnvMappings() {
		coll := mongo.GetClient().Database("cicd").Collection(fmt.Sprintf("tasks_%s", env))
		filter := bson.M{
			"task_id":             taskID,
			"confirmation_status": bson.M{"$in": []string{"pending", "sent"}},
		}
		var task models.DeployRequest
		if err := coll.FindOne(ctx, filter).Decode(&task); err == nil {
			return &task, nil
		}
	}
	return nil, fmt.Errorf("task not found for task_id: %s", taskID)
}

// ======================
// 9. 处理回调（点击后反馈 + 删除原弹窗）
// ======================
func (bm *BotManager) HandleCallback(update map[string]interface{}, confirmChan chan<- models.DeployRequest, rejectChan chan<- models.StatusRequest, mongo *client.MongoClient) {
	startTime := time.Now()

	// 1. 提取 callback_query
	callback, ok := update["callback_query"].(map[string]interface{})
	if !ok {
		return
	}

	// 2. 提取用户 & 数据
	from := callback["from"].(map[string]interface{})
	userName := from["username"].(string)
	data := callback["data"].(string)

	logrus.WithFields(logrus.Fields{
		"time":   time.Now().Format("2006-01-02 15:04:05"),
		"method": "HandleCallback",
	}).Infof("收到回调: username=%s, data=%s", userName, data)

	// 3. 权限检查
	if !contains(bm.globalAllowedUsers, userName) {
		logrus.Warnf("用户无权限: %s", userName)
		return
	}

	// 4. 解析 action:task_id
	parts := strings.SplitN(data, ":", 2)
	if len(parts) != 2 {
		logrus.Warnf("callback_data 格式错误: %s", data)
		return
	}
	action, taskID := parts[0], parts[1]

	// 5. 精确查找任务
	task, err := bm.findTaskByTaskID(taskID, mongo)
	if err != nil {
		logrus.WithFields(logrus.Fields{
			"time":   time.Now().Format("2006-01-02 15:04:05"),
			"method": "HandleCallback",
		}).Warnf("任务未找到 task_id=%s: %v", taskID, err)
		return
	}

	// 6. 删除原弹窗
	message := callback["message"].(map[string]interface{})
	messageID := int(message["message_id"].(float64))
	chatID := fmt.Sprintf("%v", message["chat"].(map[string]interface{})["id"])
	bot, _ := bm.getBotForService(task.Service)
	bm.DeleteMessage(bot, chatID, messageID)

	// 7. 反馈消息（保留原模板）
	actionText := map[string]string{"confirm": "确认", "reject": "拒绝"}[action]
	feedback := fmt.Sprintf("Success 用户 @%s 已 *%s* 部署: `%s` v`%s` 在 `%s`",
		escapeMarkdownV2(userName), actionText,
		escapeMarkdownV2(task.Service), escapeMarkdownV2(task.Version), escapeMarkdownV2(task.Environments[0]),
	)

	feedbackID, _ := bm.sendMessage(bot, chatID, feedback, "MarkdownV2")
	go func() {
		time.Sleep(30 * time.Second)
		bm.DeleteMessage(bot, chatID, feedbackID)
	}()

	// 8. 推送到通道
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

	logrus.WithFields(logrus.Fields{
		"time":   time.Now().Format("2006-01-02 15:04:05"),
		"method": "HandleCallback",
	}).Infof(color.GreenString("回调处理完成: %s task_id=%s", action, taskID))
}

// SetMongoClient 注入 Mongo 客户端（Agent 创建时调用）
func (bm *BotManager) SetMongoClient(mongo *client.MongoClient) {
	bm.mongo = mongo
}

// findTaskByPayload 通过 payload 模糊匹配任务
func (bm *BotManager) findTaskByPayload(payload string, mongo *client.MongoClient) (*models.DeployRequest, error) {
	ctx := context.Background()
	for env := range mongo.GetEnvMappings() {
		collection := mongo.GetClient().Database("cicd").Collection(fmt.Sprintf("tasks_%s", env))
		filter := bson.M{
			"$or": []bson.M{
				{"service": bson.M{"$regex": payload, "$options": "i"}},
				{"version": bson.M{"$regex": payload, "$options": "i"}},
				{"user": bson.M{"$regex": payload, "$options": "i"}},
			},
			"confirmation_status": bson.M{"$in": []string{"pending", "sent"}},
		}
		var task models.DeployRequest
		err := collection.FindOne(ctx, filter).Decode(&task)
		if err == nil {
			return &task, nil
		}
	}
	return nil, fmt.Errorf("task not found for payload: %s", payload)
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

// ======================
// 11. PollUpdates（供 Agent 调用）
// ======================
func (bm *BotManager) PollUpdates(confirmChan chan models.DeployRequest, rejectChan chan models.StatusRequest, mongo *client.MongoClient) {
	for {
		select {
		case update := <-bm.updateChan:
			bm.HandleCallback(update, confirmChan, rejectChan, mongo) // 传入 mongo
		case <-bm.stopChan:
			return
		}
	}
}

func (bm *BotManager) Stop() {
	close(bm.stopChan)
}