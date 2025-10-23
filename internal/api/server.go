package api

import (
	"encoding/json"
	"fmt"
	"k8s-cicd/internal/config"
	"k8s-cicd/internal/storage"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/sirupsen/logrus"
)

// *** 修复：添加全局锁，确保 WorkerPool 安全初始化 ***
var workerPoolOnce sync.Once
var globalWorkerPool *WorkerPool

// Server 封装 HTTP 服务（支持高并发多任务异步处理）
type Server struct {
	Router          *http.ServeMux
	storage         *storage.RedisStorage
	logger          *logrus.Logger
	whitelistIPs    []string // IP 白名单
	config          *config.Config
	workerPool      *WorkerPool   // 工作池
	mu              sync.RWMutex  // 并发锁
	wg              sync.WaitGroup // 等待组
}

// Task 异步任务接口
type Task interface {
	Execute(*storage.RedisStorage) error
	GetID() string
}

// PushTask 推送任务
type PushTask struct {
	Services     []string
	Environments []string
	ID           string
}

// DeployTask 部署任务
type DeployTask struct {
	Req DeployRequest
	ID  string
}

// WorkerPool 工作池
type WorkerPool struct {
	workers int
	jobs    chan Task
}

// *** 修复：添加 Nil 检查 ***
func getGlobalWorkerPool() *WorkerPool {
	workerPoolOnce.Do(func() {
		globalWorkerPool = NewWorkerPool(20)
		logrus.Info("✅ 全局工作池初始化完成 (20 workers)")
	})
	return globalWorkerPool
}

// NewWorkerPool 创建工作池
func NewWorkerPool(workers int) *WorkerPool {
	pool := &WorkerPool{
		workers: workers,
		jobs:    make(chan Task, 1000),
	}
	
	// *** 修复：确保 globalStorage 已初始化 ***
	if globalStorage == nil {
		logrus.Fatal("❌ FATAL: globalStorage 未初始化，无法启动 WorkerPool")
	}
	
	for i := 0; i < workers; i++ {
		go pool.worker()
	}
	logrus.WithField("workers", workers).Info("WorkerPool 启动完成")
	return pool
}

// *** 修复：添加 Nil 检查 ***
func (wp *WorkerPool) worker() {
	for job := range wp.jobs {
		if job == nil {
			logrus.Warn("收到空任务，跳过")
			continue
		}
		
		// *** 修复：双重 Nil 检查 ***
		if globalStorage == nil {
			logrus.Error("❌ globalStorage 为 nil，任务执行失败")
			continue
		}
		
		start := time.Now()
		err := job.Execute(globalStorage)
		duration := time.Since(start).Milliseconds()
		
		logrus.WithFields(logrus.Fields{
			"task_id":     job.GetID(),
			"duration_ms": duration,
			"success":     err == nil,
		}).Info("异步任务执行完成")
		
		if err != nil {
			logrus.WithFields(logrus.Fields{
				"task_id": job.GetID(),
				"error":   err.Error(),
			}).Error("异步任务执行失败")
		}
	}
}

// Submit 提交任务
func (wp *WorkerPool) Submit(job Task) {
	if wp.jobs == nil {
		logrus.Error("❌ WorkerPool 未初始化")
		return
	}
	wp.jobs <- job
}

// PushRequest 定义推送请求结构
type PushRequest struct {
	Services     []string `json:"services"`     // 服务列表（保留原始大小写）
	Environments []string `json:"environments"` // 环境列表（保留原始大小写）
}

// DeployRequest 定义部署请求结构
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

// NewServer 初始化 HTTP 服务
func NewServer(redisAddr string, cfg *config.Config) *Server {
	startTime := time.Now()
	defer func() {
		logrus.WithField("init_duration_ms", time.Since(startTime).Milliseconds()).Info("Server 初始化完成")
	}()

	// *** 修复：验证 globalStorage 已初始化 ***
	if globalStorage == nil {
		logrus.Fatal("❌ FATAL: globalStorage 未初始化")
	}

	// 初始化本地 Redis（用于同步操作）
	storage, err := storage.NewRedisStorage(redisAddr)
	if err != nil {
		logrus.Fatalf("初始化 Redis 失败: %v", err)
	}

	// 初始化日志
	logger := logrus.New()
	logger.SetFormatter(&logrus.JSONFormatter{})
	logger.SetLevel(logrus.InfoLevel)

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
		whitelistIPs: whitelistIPs,
		config:       cfg,
	}

	// 注册路由
	server.Router.HandleFunc("/push", server.ipWhitelistMiddleware(server.handlePush))
	server.Router.HandleFunc("/query", server.ipWhitelistMiddleware(server.handleQuery))
	server.Router.HandleFunc("/status", server.ipWhitelistMiddleware(server.handleStatus))
	server.Router.HandleFunc("/deploy", server.ipWhitelistMiddleware(server.handleDeploy))

	logrus.WithField("total_init_ms", time.Since(startTime).Milliseconds()).Info("HTTP 服务初始化完成")
	return server
}

// *** 修复：handlePush 使用全局工作池 ***
func (s *Server) handlePush(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	defer func() {
		s.logger.WithField("total_duration_ms", time.Since(start).Milliseconds()).Info("handlePush 执行完成")
	}()

	if r.Method != http.MethodPost {
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
		"services_count":     len(req.Services),
		"environments_count": len(req.Environments),
	}).Info("收到推送请求")

	// *** 修复：使用全局工作池 ***
	wp := getGlobalWorkerPool()
	
	taskID := fmt.Sprintf("push-%d", time.Now().UnixNano())
	pushTask := &PushTask{
		Services:     req.Services,
		Environments: req.Environments,
		ID:           taskID,
	}

	wp.Submit(pushTask)

	w.WriteHeader(http.StatusOK)
	response := map[string]interface{}{
		"message": "数据推送成功",
		"task_id": taskID,
	}
	json.NewEncoder(w).Encode(response)
}

// ipWhitelistMiddleware IP 白名单中间件
func (s *Server) ipWhitelistMiddleware(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		defer func() {
			s.logger.WithFields(logrus.Fields{
				"path":        r.URL.Path,
				"method":      r.Method,
				"duration_ms": time.Since(start).Milliseconds(),
			}).Info("HTTP 请求处理完成")
		}()

		s.logger.WithFields(logrus.Fields{
			"client_ip": r.RemoteAddr,
			"path":      r.URL.Path,
			"method":    r.Method,
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

// handlePush 处理推送请求（多任务异步）
func (s *Server) handlePush(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	defer func() {
		s.logger.WithField("total_duration_ms", time.Since(start).Milliseconds()).Info("handlePush 执行完成")
	}()

	if r.Method != http.MethodPost {
		s.logger.Warn("仅支持 POST 方法")
		http.Error(w, "仅支持 POST 方法", http.StatusMethodNotAllowed)
		return
	}

	decodeStart := time.Now()
	var req PushRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.logger.WithError(err).Error("解析推送请求失败")
		http.Error(w, "无效的请求数据", http.StatusBadRequest)
		return
	}
	s.logger.WithField("decode_ms", time.Since(decodeStart).Milliseconds()).Debug("JSON 解析完成")

	s.logger.WithFields(logrus.Fields{
		"services_count":     len(req.Services),
		"environments_count": len(req.Environments),
	}).Info("收到推送请求")

	// 创建异步任务
	taskID := fmt.Sprintf("push-%d", time.Now().UnixNano())
	pushTask := &PushTask{
		Services:     req.Services,
		Environments: req.Environments,
		ID:           taskID,
	}

	// 提交到工作池（立即返回）
	s.workerPool.Submit(pushTask)

	w.WriteHeader(http.StatusOK)
	response := map[string]interface{}{
		"message": "数据推送成功",
		"task_id": taskID,
	}
	json.NewEncoder(w).Encode(response)
}

// *** 修复：调用导出的方法 AsyncStoreServices ***
func (t *PushTask) Execute(storage *storage.RedisStorage) error {
	if storage == nil {
		return fmt.Errorf("storage 为 nil")
	}
	
	start := time.Now()
	
	storeServicesStart := time.Now()
	if err := storage.AsyncStoreServices(t.Services); err != nil {
		return fmt.Errorf("存储服务列表失败: %v", err)
	}
	storeServicesDuration := time.Since(storeServicesStart)

	storeEnvsStart := time.Now()
	if err := storage.AsyncStoreEnvironments(t.Environments); err != nil {
		return fmt.Errorf("存储环境列表失败: %v", err)
	}
	storeEnvsDuration := time.Since(storeEnvsStart)

	logrus.WithFields(logrus.Fields{
		"task_id":             t.ID,
		"services_count":      len(t.Services),
		"environments_count":  len(t.Environments),
		"store_services_ms":   storeServicesDuration.Milliseconds(),
		"store_envs_ms":       storeEnvsDuration.Milliseconds(),
		"total_task_ms":       time.Since(start).Milliseconds(),
	}).Info("推送任务执行完成")
	
	return nil
}

func (t *PushTask) GetID() string { return t.ID }

// handleDeploy 处理部署请求（多任务异步）
// *** 修复：handleDeploy 使用全局工作池 ***
func (s *Server) handleDeploy(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	defer func() {
		s.logger.WithField("total_duration_ms", time.Since(start).Milliseconds()).Info("handleDeploy 执行完成")
	}()

	if r.Method != http.MethodPost {
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
		http.Error(w, "缺少必填字段", http.StatusBadRequest)
		return
	}

	// 默认状态
	if req.Status == "" {
		req.Status = "pending"
	}

	// 验证服务和环境
	services, _ := s.storage.GetServices()
	environments, _ := s.storage.GetEnvironments()

	if !contains(services, req.Service) {
		http.Error(w, fmt.Sprintf("服务 %s 不存在", req.Service), http.StatusBadRequest)
		return
	}

	for _, env := range req.Environments {
		if !contains(environments, env) {
			http.Error(w, fmt.Sprintf("环境 %s 不存在", env), http.StatusBadRequest)
			return
		}
	}

	// *** 修复：使用全局工作池 ***
	wp := getGlobalWorkerPool()
	
	taskID := fmt.Sprintf("deploy-%s-%s", req.Service, req.Version)
	deployTask := &DeployTask{
		Req: req,
		ID:  taskID,
	}

	wp.Submit(deployTask)

	w.WriteHeader(http.StatusOK)
	response := map[string]interface{}{
		"message": "部署请求已入队",
		"task_id": taskID,
	}
	json.NewEncoder(w).Encode(response)
}

func (t *DeployTask) Execute(storage *storage.RedisStorage) error {
	if storage == nil {
		return fmt.Errorf("storage 为 nil")
	}
	
	start := time.Now()
	
	enqueueStart := time.Now()
	data, _ := json.Marshal(t.Req)
	err := storage.Push("deploy_queue", string(data))
	enqueueDuration := time.Since(enqueueStart)
	
	logrus.WithFields(logrus.Fields{
		"task_id":     t.ID,
		"service":     t.Req.Service,
		"version":     t.Req.Version,
		"enqueue_ms":  enqueueDuration.Milliseconds(),
		"total_ms":    time.Since(start).Milliseconds(),
	}).Info("部署任务入队完成")
	
	return err
}

func (t *DeployTask) GetID() string { return t.ID }

// handleQuery 处理查询请求
func (s *Server) handleQuery(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	defer func() {
		s.logger.WithField("total_duration_ms", time.Since(start).Milliseconds()).Info("handleQuery 执行完成")
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

	if req.Environment == "" || req.User == "" {
		http.Error(w, "缺少必填字段", http.StatusBadRequest)
		return
	}

	s.logger.WithFields(logrus.Fields{
		"environment": req.Environment,
		"user":        req.User,
	}).Info("收到查询请求")

	queryStart := time.Now()
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
		"query_ms":       time.Since(queryStart).Milliseconds(),
		"pending_count":  len(results),
	}).Debug("查询完成")

	if len(results) == 0 {
		json.NewEncoder(w).Encode(map[string]string{"message": "暂无待处理任务"})
		return
	}

	json.NewEncoder(w).Encode(results)
}

// handleStatus 处理状态更新
func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	defer func() {
		s.logger.WithField("total_duration_ms", time.Since(start).Milliseconds()).Info("handleStatus 执行完成")
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

	// 验证
	if req.Service == "" || req.Version == "" || req.Environment == "" || req.User == "" {
		http.Error(w, "缺少必填字段", http.StatusBadRequest)
		return
	}

	validStatuses := map[string]bool{"success": true, "failure": true, "no_action": true}
	if !validStatuses[req.Status] {
		http.Error(w, "无效的状态值，仅支持：success, failure, no_action", http.StatusBadRequest)
		return
	}

	s.logger.WithFields(logrus.Fields{
		"service":     req.Service,
		"version":     req.Version,
		"status":      req.Status,
	}).Info("收到状态更新请求")

	updateStart := time.Now()
	updated, err := s.updateStatus(req)
	updateDuration := time.Since(updateStart)

	s.logger.WithFields(logrus.Fields{
		"update_ms": updateDuration.Milliseconds(),
		"updated":   updated,
	}).Debug("状态更新完成")

	if err != nil {
		http.Error(w, "更新状态失败", http.StatusInternalServerError)
		return
	}

	if updated {
		json.NewEncoder(w).Encode(map[string]string{"message": "状态更新成功"})
	} else {
		json.NewEncoder(w).Encode(map[string]string{"message": "未找到匹配任务"})
	}
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
		s.storage.Delete("deploy_queue")
		for _, item := range newItems {
			s.storage.Push("deploy_queue", item)
		}
	}

	return updated, nil
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

// 全局存储实例
var globalStorage *storage.RedisStorage