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
	"time"

	"k8s-cicd/agent/config"
	"k8s-cicd/agent/models"

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
// SendConfirmation 发送确认弹窗（自动截断 callback_data）
func (bm *BotManager) SendConfirmation(service, env, user, version string, allowedUsers []string) (int, error) {
    bot, err := bm.getBotForService(service)
    if err != nil {
        return 0, err
    }

    // 构建原始 callback_data
    rawConfirm := fmt.Sprintf("confirm:%s:%s:%s:%s", service, env, version, user)
    rawReject := fmt.Sprintf("reject:%s:%s:%s:%s", service, env, version, user)

	// SendConfirmation（关键：截断 + URL 编码）
	callbackDataConfirm := truncateString(fmt.Sprintf("confirm:%s:%s:%s:%s", service, env, version, user), 64)
	callbackDataReject := truncateString(fmt.Sprintf("reject:%s:%s:%s:%s", service, env, version, user), 64)

	callbackDataConfirm = url.QueryEscape(callbackDataConfirm)
	callbackDataReject = url.QueryEscape(callbackDataReject)

    // 自动截断到 64 字节（Telegram 限制）
    const maxLen = 64
    callbackConfirm := truncateString(rawConfirm, maxLen)
    callbackReject := truncateString(rawReject, maxLen)

    // URL 编码（防止特殊字符）
    callbackConfirm = url.QueryEscape(callbackConfirm)
    callbackReject = url.QueryEscape(callbackReject)

    // 构建消息
    var mentions strings.Builder
    for _, u := range allowedUsers {
        mentions.WriteString(fmt.Sprintf("@%s ", escapeMarkdownV2(u)))
    }

    safe := escapeMarkdownV2
    message := fmt.Sprintf("*Deployment Confirmation*\n\n"+
        "**服务**: `%s`\n"+
        "**环境**: `%s`\n"+
        "**版本**: `%s`\n"+
        "**用户**: `%s`\n\n"+
        "*请选择操作*\n\n"+
        "通知: %s", safe(service), safe(env), safe(version), safe(user), mentions.String())

    keyboard := map[string]interface{}{
        "inline_keyboard": [][]map[string]string{
            {{"text": "Confirm 确认部署", "callback_data": callbackConfirm}},
            {{"text": "Reject 拒绝部署", "callback_data": callbackReject}},
        },
    }

    msgID, err := bm.sendMessageWithKeyboard(bot, bot.GroupID, message, keyboard, "MarkdownV2")
    if err != nil {
        logrus.WithFields(logrus.Fields{
            "time":   time.Now().Format("2006-01-02 15:04:05"),
            "method": "SendConfirmation",
            "data":   logrus.Fields{"service": service, "version": version, "env": env},
        }).Errorf(color.RedString("弹窗发送失败: %v", err))
        return 0, err
    }

    logrus.WithFields(logrus.Fields{
        "time":   time.Now().Format("2006-01-02 15:04:05"),
        "method": "SendConfirmation",
    }).Infof(color.GreenString("弹窗发送成功: %s v%s [%s] message_id=%d", service, version, env, msgID))
    return msgID, nil
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



// ======================
// 9. 处理回调（点击后反馈 + 删除原弹窗）
// ======================
// HandleCallback 处理弹窗回调（兼容截断的 callback_data）
func (bm *BotManager) HandleCallback(update map[string]interface{}, confirmChan chan models.DeployRequest, rejectChan chan models.StatusRequest) {
	callback, ok := update["callback_query"].(map[string]interface{})
	if !ok {
		logrus.Warn("无效的 callback_query")
		return
	}

	from, ok := callback["from"].(map[string]interface{})
	if !ok {
		logrus.Warn("无法获取 from 用户信息")
		return
	}
	userName, _ := from["username"].(string)
	if userName == "" {
		userName = from["first_name"].(string)
	}

	data, ok := callback["data"].(string)
	if !ok || data == "" {
		logrus.Warn("callback_data 为空")
		return
	}

	// 步骤1：URL 解码（SendConfirmation 中编码了）
	decodedData, err := url.QueryUnescape(data)
	if err != nil {
		logrus.Warnf("callback_data 解码失败: %v", err)
		decodedData = data // 降级使用原始
	}

	logrus.WithFields(logrus.Fields{
		"time":   time.Now().Format("2006-01-02 15:04:05"),
		"method": "HandleCallback",
		"data":   logrus.Fields{"raw": data, "decoded": decodedData, "user": userName},
	}).Infof("收到回调: %s", decodedData)

	// 步骤2：解析 action
	parts := strings.SplitN(decodedData, ":", 2)
	if len(parts) < 2 {
		logrus.Warnf("callback_data 格式错误: %s", decodedData)
		return
	}
	action := parts[0]
	payload := parts[1]

	// 步骤3：权限检查
	allowed := false
	for _, u := range bm.globalAllowedUsers {
		if u == userName {
			allowed = true
			break
		}
	}
	if !allowed {
		logrus.Warnf("用户无权限: %s", userName)
		return
	}

	// 步骤4：从 Mongo 模糊匹配任务（关键！）
	task, err := bm.findTaskByPayload(payload)
	if err != nil {
		logrus.WithFields(logrus.Fields{
			"time":   time.Now().Format("2006-01-02 15:04:05"),
			"method": "HandleCallback",
		}).Warnf("未找到匹配任务: %s, 错误: %v", payload, err)
		return
	}

	// 步骤5：删除原弹窗
	message, ok := callback["message"].(map[string]interface{})
	if !ok {
		logrus.Warn("无法获取消息信息")
		return
	}
	messageID := int(message["message_id"].(float64))
	chatID := fmt.Sprintf("%v", message["chat"].(map[string]interface{})["id"])
	bot, err := bm.getBotForService(task.Service)
	if err != nil {
		logrus.Warnf("获取机器人失败: %v", err)
		return
	}
	bm.DeleteMessage(bot, chatID, messageID)

	// 步骤6：发送反馈
	actionName := map[string]string{"confirm": "确认", "reject": "拒绝"}[action]
	feedback := fmt.Sprintf("Success 用户 @%s 已 *%s* 部署: `%s` v`%s` 在 `%s`",
		escapeMarkdownV2(userName), actionName, escapeMarkdownV2(task.Service),
		escapeMarkdownV2(task.Version), escapeMarkdownV2(task.Environments[0]))

	feedbackID, _ := bm.sendTelegramMessage(bot, chatID, feedback, nil, "MarkdownV2")

	// 30秒后删除反馈
	go func() {
		time.Sleep(30 * time.Second)
		bm.DeleteMessage(bot, chatID, feedbackID)
	}()

	// 步骤7：推送到通道
	if action == "confirm" {
		confirmChan <- models.DeployRequest{
			Service:      task.Service,
			Environments: task.Environments,
			Version:      task.Version,
			User:         task.User,
		}
	} else {
		rejectChan <- models.StatusRequest{
			Service:     task.Service,
			Version:     task.Version,
			Environment: task.Environments[0],
			User:        task.User,
			Status:      "no_action",
		}
	}

	logrus.WithFields(logrus.Fields{
		"time":   time.Now().Format("2006-01-02 15:04:05"),
		"method": "HandleCallback",
		"data":   logrus.Fields{"action": action, "task_id": task.ID},
	}).Infof("回调处理完成: %s", action)
}

// findTaskByPayload 通过 payload 模糊匹配任务
func (bm *BotManager) findTaskByPayload(payload string) (*models.DeployRequest, error) {
	// 假设 mongo 客户端可通过 bm 获取，或传入
	// 这里简化，使用全局 mongo（实际请注入）
	mongo := client.GetMongoClient() // 你需要实现单例或注入

	ctx := context.Background()
	// 遍历所有环境集合
	for env := range mongo.cfg.EnvMapping.Mappings {
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
func (bm *BotManager) PollUpdates(confirmChan chan models.DeployRequest, rejectChan chan models.StatusRequest) {
	for {
		select {
		case update := <-bm.updateChan:
			bm.HandleCallback(update, confirmChan, rejectChan)
		case <-bm.stopChan:
			return
		}
	}
}

func (bm *BotManager) Stop() {
	close(bm.stopChan)
}