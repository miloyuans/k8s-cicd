// 文件: internal/telegram/bot.go
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
	"strings"
	"sync"
	"time"

	"github.com/sirupsen/logrus"
)

// Server 封装 HTTP 服务（支持并发处理）
type Server struct {
	Router          *http.ServeMux
	storage         *storage.RedisStorage
	logger          *logrus.Logger
	bot             *telegram.Bot
	whitelistIPs    []string // IP 白名单
	config          *config.Config
	asyncQueue      chan interface{} // 异步处理队列
	workerPool      *WorkerPool      // 工作池
	mu              sync.RWMutex     // 并发锁
}

// PushRequest 定义推送请求结构（优化：只保留 services 和 environments）
type PushRequest struct {
	Services     []string `json:"services"`     // 服务列表（保留原始大小写）
	Environments []string `json:"environments"` // 环境列表（保留原始大小写）
}

// DeployRequest 定义部署请求结构（优化：status 可选，默认 pending）
type DeployRequest struct {
	Service      string   `json:"service"`
	Environments []string `json:"environments"`
	Version      string   `json:"version"`
	User         string   `json:"user"`
	Status       string   `json:"status,omitempty"` // 可选，默认为 "pending"
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
	Status      string `json:"status"` // 必填：success/failure/no_action
}

// WorkerPool 工作池结构
type WorkerPool struct {
	workers int
	jobs    chan interface{}
}

// NewWorkerPool 创建工作池
func NewWorkerPool(workers int) *WorkerPool {
	pool := &WorkerPool{
		workers: workers,
		jobs:    make(chan interface{}, 100),
	}
	for i := 0; i < workers; i++ {
		go pool.worker()
	}
	return pool
}

func (wp *WorkerPool) worker() {
	for job := range wp.jobs {
		// 处理异步任务
		time.Sleep(10 * time.Millisecond) // 模拟处理
	}
}

func (wp *WorkerPool) Submit(job interface{}) {
	wp.jobs <- job
}

// NewServer 初始化 HTTP 服务（优化：支持异步并发）
func NewServer(redisAddr string, cfg *config.Config) *Server {
	// 初始化 Redis
	storage, err := storage.NewRedisStorage(redisAddr)
	if err != nil {
		logrus.Fatalf("初始化 Redis 失败: %v", err)
	}

	// 初始化日志
	logger := logrus.New()
	logger.SetFormatter(&logrus.JSONFormatter{}) // 结构化日志
	logger.SetLevel(logrus.InfoLevel)

	// 初始化 Telegram 机器人
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

	// 初始化工作池（10 个 worker）
	workerPool := NewWorkerPool(10)

	server := &Server{
		Router:       http.NewServeMux(),
		storage:      storage,
		logger:       logger,
		bot:          bot,
		whitelistIPs: whitelistIPs,
		config:       cfg,
		workerPool:   workerPool,
	}

	// 注册路由，使用 IP 白名单中间件
	server.Router.HandleFunc("/push", server.ipWhitelistMiddleware(server.handlePush))
	server.Router.HandleFunc("/query", server.ipWhitelistMiddleware(server.handleQuery))
	server.Router.HandleFunc("/status", server.ipWhitelistMiddleware(server.handleStatus))
	server.Router.HandleFunc("/deploy", server.ipWhitelistMiddleware(server.handleDeploy))

	// 启动异步处理协程
	go server.processAsyncQueue()

	return server
}

// ipWhitelistMiddleware IP 白名单中间件（线程安全）
func (s *Server) ipWhitelistMiddleware(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		s.logger.WithFields(logrus.Fields{
			"client_ip": r.RemoteAddr,
			"path":      r.URL.Path,
		}).Info("收到 HTTP 请求")

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

		s.logger.WithFields(logrus.Fields{
			"client_ip": clientIP,
		}).Warn("IP 不允许访问")
		http.Error(w, "IP 不在白名单内", http.StatusForbidden)
	}
}

// handlePush 处理外部服务推送请求（优化：只存 services/environments，保留大小写）
func (s *Server) handlePush(w http.ResponseWriter, r *http.Request) {
	startTime := time.Now()
	defer func() {
		s.logger.WithFields(logrus.Fields{
			"duration_ms": time.Since(startTime).Milliseconds(),
		}).Info("handlePush 执行完成")
	}()

	if r.Method != http.MethodPost {
		s.logger.Warn("仅支持 POST 方法")
		http.Error(w, "仅支持 POST 方法", http.StatusMethodNotAllowed)
		return
	}

	var req PushRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.logger.WithError(err).Error("解析推送请求失败")
		http.Error(w, "无效的请求数据", http.StatusBadRequest)
		return
	}

	s.logger.WithFields(logrus.Fields{
		"services":     req.Services,
		"environments": req.Environments,
	}).Info("收到推送请求")

	// 异步存储（保留原始大小写，不转换）
	go func() {
		if err := s.asyncStoreServices(req.Services); err != nil {
			s.logger.WithError(err).Error("异步存储服务列表失败")
		}
		if err := s.asyncStoreEnvironments(req.Environments); err != nil {
			s.logger.WithError(err).Error("异步存储环境列表失败")
		}
	}()

	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]string{"message": "数据推送成功"})
}

// asyncStoreServices 异步存储服务列表（保留原始大小写）
func (s *Server) asyncStoreServices(services []string) error {
	if len(services) == 0 {
		return nil
	}
	s.logger.WithFields(logrus.Fields{"services": services}).Info("异步存储服务列表")
	data, err := json.Marshal(services)
	if err != nil {
		return err
	}
	return s.storage.Set("services", string(data))
}

// asyncStoreEnvironments 异步存储环境列表（保留原始大小写）
func (s *Server) asyncStoreEnvironments(environments []string) error {
	if len(environments) == 0 {
		return nil
	}
	s.logger.WithFields(logrus.Fields{"environments": environments}).Info("异步存储环境列表")
	data, err := json.Marshal(environments)
	if err != nil {
		return err
	}
	return s.storage.Set("environments", string(data))
}

// handleDeploy 处理部署请求（优化：验证服务/环境存在，保留大小写）
func (s *Server) handleDeploy(w http.ResponseWriter, r *http.Request) {
	startTime := time.Now()
	defer func() {
		s.logger.WithFields(logrus.Fields{
			"duration_ms": time.Since(startTime).Milliseconds(),
		}).Info("handleDeploy 执行完成")
	}()

	if r.Method != http.MethodPost {
		s.logger.Warn("仅支持 POST 方法")
		http.Error(w, "仅支持 POST 方法", http.StatusMethodNotAllowed)
		return
	}

	var req DeployRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.logger.WithError(err).Error("解析部署请求失败")
		http.Error(w, "无效的请求数据", http.StatusBadRequest)
		return
	}

	// 必填字段验证
	if req.Service == "" || len(req.Environments) == 0 || req.Version == "" || req.User == "" {
		s.logger.Warn("部署请求缺少必填字段")
		http.Error(w, "缺少必填字段：service, environments, version, user", http.StatusBadRequest)
		return
	}

	// 默认状态为 pending
	if req.Status == "" {
		req.Status = "pending"
	}

	s.logger.WithFields(logrus.Fields{
		"service":      req.Service,
		"environments": req.Environments,
		"version":      req.Version,
		"user":         req.User,
		"status":       req.Status,
	}).Info("收到部署请求")

	// 验证服务和环境是否存在（线程安全）
	services, _ := s.storage.GetServices()
	environments, _ := s.storage.GetEnvironments()

	if !contains(services, req.Service) {
		s.logger.WithField("service", req.Service).Warn("服务不存在")
		http.Error(w, fmt.Sprintf("服务 %s 不存在", req.Service), http.StatusBadRequest)
		return
	}

	for _, env := range req.Environments {
		if !contains(environments, env) {
			s.logger.WithField("environment", env).Warn("环境不存在")
			http.Error(w, fmt.Sprintf("环境 %s 不存在", env), http.StatusBadRequest)
			return
		}
	}

	// 异步入队（保留原始数据）
	go func() {
		s.logger.Info("异步将部署请求入队")
		if err := s.asyncEnqueueDeploy(req); err != nil {
			s.logger.WithError(err).Error("部署请求入队失败")
		}
	}()

	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]string{"message": "部署请求已提交"})
}

// asyncEnqueueDeploy 异步入队部署请求
func (s *Server) asyncEnqueueDeploy(req DeployRequest) error {
	data, err := json.Marshal(req)
	if err != nil {
		return err
	}
	return s.storage.Push("deploy_queue", string(data))
}

// handleQuery 处理查询请求（优化：并发安全）
func (s *Server) handleQuery(w http.ResponseWriter, r *http.Request) {
	startTime := time.Now()
	defer func() {
		s.logger.WithFields(logrus.Fields{
			"duration_ms": time.Since(startTime).Milliseconds(),
		}).Info("handleQuery 执行完成")
	}()

	if r.Method != http.MethodPost {
		http.Error(w, "仅支持 POST 方法", http.StatusMethodNotAllowed)
		return
	}

	var req QueryRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.logger.WithError(err).Error("解析查询请求失败")
		http.Error(w, "无效的请求数据", http.StatusBadRequest)
		return
	}

	s.logger.WithFields(logrus.Fields{
		"environment": req.Environment,
		"user":        req.User,
	}).Info("收到查询请求")

	// 线程安全查询
	items, err := s.storage.List("deploy_queue")
	if err != nil {
		s.logger.WithError(err).Error("查询队列失败")
		http.Error(w, "查询失败", http.StatusInternalServerError)
		return
	}

	var results []DeployRequest
	for _, item := range items {
		var deployReq DeployRequest
		if err := json.Unmarshal([]byte(item), &deployReq); err != nil {
			continue
		}
		if contains(deployReq.Environments, req.Environment) &&
			deployReq.User == req.User &&
			deployReq.Status != "success" && deployReq.Status != "failure" {
			results = append(results, deployReq)
		}
	}

	s.logger.WithFields(logrus.Fields{
		"user":        req.User,
		"environment": req.Environment,
		"task_count":  len(results),
	}).Info("查询任务结果")

	if len(results) == 0 {
		json.NewEncoder(w).Encode(map[string]string{"message": "暂无任务，请继续等待"})
		return
	}

	json.NewEncoder(w).Encode(results)
}

// handleStatus 处理任务状态更新请求（优化：精确匹配更新）
func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	startTime := time.Now()
	defer func() {
		s.logger.WithFields(logrus.Fields{
			"duration_ms": time.Since(startTime).Milliseconds(),
		}).Info("handleStatus 执行完成")
	}()

	if r.Method != http.MethodPost {
		http.Error(w, "仅支持 POST 方法", http.StatusMethodNotAllowed)
		return
	}

	var req StatusRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.logger.WithError(err).Error("解析状态更新请求失败")
		http.Error(w, "无效的请求数据", http.StatusBadRequest)
		return
	}

	// 必填字段和状态验证
	if req.Service == "" || req.Version == "" || req.Environment == "" || req.User == "" {
		s.logger.Warn("状态更新请求缺少必填字段")
		http.Error(w, "缺少必填字段", http.StatusBadRequest)
		return
	}

	validStatuses := map[string]bool{"success": true, "failure": true, "no_action": true}
	if !validStatuses[req.Status] {
		s.logger.WithField("status", req.Status).Warn("无效的状态值")
		http.Error(w, "无效的状态值，仅支持：success, failure, no_action", http.StatusBadRequest)
		return
	}

	s.logger.WithFields(logrus.Fields{
		"service":     req.Service,
		"version":     req.Version,
		"environment": req.Environment,
		"user":        req.User,
		"status":      req.Status,
	}).Info("收到状态更新请求")

	// 线程安全更新
	updated, err := s.updateStatus(req)
	if err != nil {
		s.logger.WithError(err).Error("更新状态失败")
		http.Error(w, "更新状态失败", http.StatusInternalServerError)
		return
	}

	if updated {
		s.logger.Info("任务状态更新成功")
	} else {
		s.logger.Warn("未找到匹配的任务")
	}

	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]string{"message": "状态更新成功"})
}

// updateStatus 线程安全更新状态
func (s *Server) updateStatus(req StatusRequest) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	items, err := s.storage.List("deploy_queue")
	if err != nil {
		return false, err
	}

	updated := false
	newItems := []string{}
	for _, item := range items {
		var deployReq DeployRequest
		if err := json.Unmarshal([]byte(item), &deployReq); err != nil {
			newItems = append(newItems, item)
			continue
		}

		// 精确匹配（区分大小写）
		if deployReq.Service == req.Service &&
			deployReq.Version == req.Version &&
			deployReq.User == req.User &&
			contains(deployReq.Environments, req.Environment) {
			deployReq.Status = req.Status
			updated = true
		}

		data, _ := json.Marshal(deployReq)
		newItems = append(newItems, string(data))
	}

	if updated {
		// 重写队列
		s.storage.Delete("deploy_queue")
		for _, item := range newItems {
			s.storage.Push("deploy_queue", item)
		}
	}

	return updated, nil
}

// processAsyncQueue 处理异步队列
func (s *Server) processAsyncQueue() {
	for {
		s.workerPool.Submit(nil) // 保持工作池活跃
		time.Sleep(100 * time.Millisecond)
	}
}

// contains 检查字符串是否在切片中（区分大小写）
func contains(slice []string, item string) bool {
	for _, s := range slice {
		if s == item {
			return true
		}
	}
	return false
}