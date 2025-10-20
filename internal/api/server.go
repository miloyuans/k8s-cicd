// 文件: internal/api/server.go
package api

import (
	"encoding/json"
	"fmt"
	"k8s-cicd/internal/config"
	"k8s-cicd/internal/storage"
	"k8s-cicd/internal/telegram"
	"net"
	"net/http"
	"os"
	"sort"
	"strings"

	"github.com/sirupsen/logrus"
)

// Server 封装 HTTP 服务
type Server struct {
	Router       *http.ServeMux
	storage      *storage.RedisStorage
	logger       *logrus.Logger
	bot          *telegram.Bot
	whitelistIPs []string // IP 白名单
	config       *config.Config
}

// DeployRequest 定义部署请求结构
type DeployRequest struct {
	Service      string   `json:"service"`
	Environments []string `json:"environments"`
	Version      string   `json:"version"`
	User         string   `json:"user"`
	Status       string   `json:"status"` // 任务状态：pending, success, failure, no_action
}

// PushRequest 定义推送请求结构
type PushRequest struct {
	Services     []string        `json:"services"`
	Environments []string        `json:"environments"`
	Deployments  []DeployRequest `json:"deployments"`
}

// QueryRequest 定义查询请求结构
type QueryRequest struct {
	Environment string `json:"environment"`
	User        string `json:"user"`
}

// StatusRequest 定义任务状态更新请求结构
type StatusRequest struct {
	Service     string `json:"service"`
	Version     string `json:"version"`
	Environment string `json:"environment"`
	User        string `json:"user"`
	Status      string `json:"status"` // success, failure, no_action
}

// NewServer 初始化 HTTP 服务
func NewServer(redisAddr string, cfg *config.Config) *Server {
	storage, err := storage.NewRedisStorage(redisAddr)
	if err != nil {
		logrus.Fatalf("初始化 Redis 失败: %v", err)
	}
	logger := logrus.New()
	bot, err := telegram.NewBot(cfg.TelegramToken, cfg.TelegramGroupID)
	if err != nil {
		logrus.Fatalf("初始化 Telegram 机器人失败: %v", err)
	}

	// 解析白名单 IP
	whitelist := os.Getenv("WHITELIST_IPS")
	whitelistIPs := []string{}
	if whitelist != "" {
		whitelistIPs = strings.Split(whitelist, ",")
	}

	server := &Server{
		Router:       http.NewServeMux(),
		storage:      storage,
		logger:       logger,
		bot:          bot,
		whitelistIPs: whitelistIPs,
		config:       cfg,
	}

	// 注册路由，使用 IP 白名单中间件
	server.Router.HandleFunc("/deploy", server.ipWhitelistMiddleware(server.handleDeploy))
	server.Router.HandleFunc("/query", server.ipWhitelistMiddleware(server.handleQuery))
	server.Router.HandleFunc("/push", server.ipWhitelistMiddleware(server.handlePush))
	server.Router.HandleFunc("/status", server.ipWhitelistMiddleware(server.handleStatus))
	return server
}

// ipWhitelistMiddleware IP 白名单中间件
func (s *Server) ipWhitelistMiddleware(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if len(s.whitelistIPs) == 0 {
			next(w, r)
			return
		}

		clientIP := r.RemoteAddr
		if strings.Contains(clientIP, ":") {
			clientIP, _, _ = net.SplitHostPort(clientIP)
		}

		for _, allowed := range s.whitelistIPs {
			if strings.Contains(allowed, "/") {
				_, ipNet, err := net.ParseCIDR(allowed)
				if err != nil {
					s.logger.Errorf("解析 CIDR %s 失败: %v", allowed, err)
					continue
				}
				if ipNet.Contains(net.ParseIP(clientIP)) {
					next(w, r)
					return
				}
			} else if allowed == clientIP {
				next(w, r)
				return
			}
		}

		s.logger.Warnf("IP %s 不允许访问", clientIP)
		http.Error(w, "IP 不在白名单内", http.StatusForbidden)
	}
}

// handleDeploy 处理部署请求
func (s *Server) handleDeploy(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "仅支持 POST 方法", http.StatusMethodNotAllowed)
		return
	}

	var req DeployRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.logger.Errorf("解析部署请求失败: %v", err)
		http.Error(w, "无效的请求数据", http.StatusBadRequest)
		return
	}

	if s.config.TelegramGroupID == 0 {
		s.logger.Errorf("未配置 Telegram 群组 ID")
		http.Error(w, "未配置 Telegram 群组 ID", http.StatusInternalServerError)
		return
	}

	// 构造 Telegram 确认消息
	state := telegram.UserState{
		Service:      req.Service,
		Environments: req.Environments,
		Version:      req.Version,
		ChatID:       s.config.TelegramGroupID,
		UserID:       0, // UserID 由回调处理
		Step:         4,
	}
	s.bot.SaveState(fmt.Sprintf("deploy:%d:0", state.ChatID), state)
	msg := fmt.Sprintf("请确认以下信息：\n服务: %s\n环境: %s\n版本: %s\n用户: %s",
		req.Service, strings.Join(req.Environments, ", "), req.Version, req.User)
	keyboard := s.bot.CreateYesNoKeyboard("deploy_confirm")
	s.bot.SendMessage(state.ChatID, msg, &keyboard)

	s.logger.Infof("收到部署请求: %v", req)
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]string{"message": "请求已提交，等待 Telegram 确认"})
}

// handlePush 处理外部服务推送请求
func (s *Server) handlePush(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "仅支持 POST 方法", http.StatusMethodNotAllowed)
		return
	}

	var req PushRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.logger.Errorf("解析推送请求失败: %v", err)
		http.Error(w, "无效的请求数据", http.StatusBadRequest)
		return
	}

	// 去重并存储服务列表
	if len(req.Services) > 0 {
		uniqueServices := make(map[string]bool)
		for _, svc := range req.Services {
			uniqueServices[strings.ToUpper(svc)] = true
		}
		services := make([]string, 0, len(uniqueServices))
		for svc := range uniqueServices {
			services = append(services, svc)
		}
		sort.Strings(services)
		data, _ := json.Marshal(services)
		s.storage.Set("services", string(data))
		s.logger.Infof("存储服务列表: %v", services)
	}

	// 去重并存储环境列表
	if len(req.Environments) > 0 {
		uniqueEnvs := make(map[string]bool)
		for _, env := range req.Environments {
			uniqueEnvs[strings.ToUpper(env)] = true
		}
		envs := make([]string, 0, len(uniqueEnvs))
		for env := range uniqueEnvs {
			envs = append(envs, env)
		}
		sort.Strings(envs)
		data, _ := json.Marshal(envs)
		s.storage.Set("environments", string(data))
		s.logger.Infof("存储环境列表: %v", envs)
	}

	// 存储部署数据（不进入队列）
	if len(req.Deployments) > 0 {
		data, _ := json.Marshal(req.Deployments)
		s.storage.Set("deployments", string(data))
		s.logger.Infof("存储部署数据: %v", req.Deployments)
	}

	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]string{"message": "数据推送成功"})
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
			if env == req.Environment && deployReq.Status != "success" && deployReq.Status != "failure" {
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

// handleStatus 处理任务状态更新请求
func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "仅支持 POST 方法", http.StatusMethodNotAllowed)
		return
	}

	var req StatusRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.logger.Errorf("解析状态更新请求失败: %v", err)
		http.Error(w, "无效的请求数据", http.StatusBadRequest)
		return
	}

	// 验证状态
	if req.Status != "success" && req.Status != "failure" && req.Status != "no_action" {
		s.logger.Errorf("无效的状态值: %s", req.Status)
		http.Error(w, "无效的状态值", http.StatusBadRequest)
		return
	}

	// 获取队列数据
	items, err := s.storage.List("deploy_queue")
	if err != nil {
		s.logger.Errorf("获取队列失败: %v", err)
		http.Error(w, "获取队列失败", http.StatusInternalServerError)
		return
	}

	// 更新匹配的任务状态
	updated := false
	newItems := []string{}
	for _, item := range items {
		var deployReq DeployRequest
		if err := json.Unmarshal([]byte(item), &deployReq); err != nil {
			s.logger.Errorf("解析队列数据失败: %v", err)
			newItems = append(newItems, item)
			continue
		}
		if deployReq.Service == req.Service && deployReq.Version == req.Version && contains(deployReq.Environments, req.Environment) && deployReq.User == req.User {
			deployReq.Status = req.Status
			updated = true
		}
		data, _ := json.Marshal(deployReq)
		newItems = append(newItems, string(data))
	}

	if updated {
		// 清空并重新写入队列
		s.storage.Delete("deploy_queue")
		for _, item := range newItems {
			s.storage.Push("deploy_queue", item)
		}
		s.logger.Infof("更新任务状态: 服务=%s, 版本=%s, 环境=%s, 用户=%s, 状态=%s", req.Service, req.Version, req.Environment, req.User, req.Status)
	} else {
		s.logger.Warnf("未找到匹配任务: 服务=%s, 版本=%s, 环境=%s, 用户=%s", req.Service, req.Version, req.Environment, req.User)
	}

	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]string{"message": "状态更新成功"})
}

// contains 检查字符串是否在切片中
func contains(slice []string, item string) bool {
	for _, s := range slice {
		if s == item {
			return true
		}
	}
	return false
}