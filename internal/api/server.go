package api

import (
	"encoding/json"
	"k8s-cicd/internal/storage"
	"k8s-cicd/internal/telegram"
	"net/http"

	"github.com/sirupsen/logrus"
)

// Server 封装 HTTP 服务
type Server struct {
	Router  *http.ServeMux
	storage *storage.RedisStorage
	logger  *logrus.Logger
	bot     *telegram.Bot
}

// DeployRequest 定义部署请求结构
type DeployRequest struct {
	Service      string   `json:"service"`
	Environments []string `json:"environments"`
	Version      string   `json:"version"`
	User         string   `json:"user"`
}

// QueryRequest 定义查询请求结构
type QueryRequest struct {
	Environment string `json:"environment"`
	User        string `json:"user"`
}

// NewServer 初始化 HTTP 服务
func NewServer(redisAddr string) *Server {
	storage, _ := storage.NewRedisStorage(redisAddr)
	logger := logrus.New()
	bot, _ := telegram.NewBot("your-telegram-bot-token") // 需替换为实际 Token

	server := &Server{
		Router:  http.NewServeMux(),
		storage: storage,
		logger:  logger,
		bot:     bot,
	}

	server.Router.HandleFunc("/deploy", server.handleDeploy)
	server.Router.HandleFunc("/query", server.handleQuery)
	return server
}

// handleDeploy 处理部署请求
func (s *Server) handleDeploy(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "仅支持 POST 方法", http.StatusMethodNotAllowed)
		return
	}

	var req DeployRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.logger.Errorf("解析请求失败: %v", err)
		http.Error(w, "无效的请求数据", http.StatusBadRequest)
		return
	}

	// 构造 Telegram 确认消息
	state := telegram.UserState{
		Service:     req.Service,
		Environments: req.Environments,
		Version:     req.Version,
		ChatID:      123456789, // 假设的 ChatID，需根据实际情况获取
		Step:        4,
	}
	s.bot.SaveState(state.ChatID, state)
	msg := fmt.Sprintf("请确认以下信息：\n服务: %s\n环境: %s\n版本: %s\n用户: %s",
		req.Service, req.Environments, req.Version, req.User)
	keyboard := s.bot.CreateYesNoKeyboard("confirm")
	s.bot.SendMessage(state.ChatID, msg, &keyboard)

	// 等待 Telegram 确认结果（这里简化为直接存储，实际需等待回调）
	data, _ := json.Marshal(req)
	s.storage.Push("deploy_queue", string(data))
	s.logger.Infof("收到部署请求: %v", req)

	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]string{"message": "请求已提交，等待确认"})
}

// handleQuery 处理查询请求
func (s *Server) handleQuery(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "仅支持 POST 方法", http.StatusMethodNotAllowed)
		return
	}

	var req QueryRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.logger.Errorf("解析查询请求失败: %v", err)
		http.Error(w, "无效的请求数据", http.StatusBadRequest)
		return
	}

	// 查询队列中的数据
	items, err := s.storage.List("deploy_queue")
	if err != nil {
		s.logger.Errorf("查询队列失败: %v", err)
		http.Error(w, "查询失败", http.StatusInternalServerError)
		return
	}

	var results []DeployRequest
	for _, item := range items {
		var deployReq DeployRequest
		if err := json.Unmarshal([]byte(item), &deployReq); err != nil {
			s.logger.Errorf("解析队列数据失败: %v", err)
			continue
		}
		for _, env := range deployReq.Environments {
			if env == req.Environment {
				results = append(results, deployReq)
			}
		}
	}

	s.logger.Infof("用户 %s 查询环境 %s 的任务", req.User, req.Environment)
	if len(results) == 0 {
		json.NewEncoder(w).Encode(map[string]string{"message": "暂无任务，请继续等待"})
		return
	}

	json.NewEncoder(w).Encode(results)
}