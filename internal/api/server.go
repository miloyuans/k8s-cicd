// 文件: internal/api/server.go
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

// 全局存储实例，确保并发安全
var globalStorage *storage.MongoStorage

// 全局锁，确保 WorkerPool 安全初始化
var workerPoolOnce sync.Once
var globalWorkerPool *WorkerPool

// Server 封装 HTTP 服务，提供 API 处理
type Server struct {
	Router          *http.ServeMux
	storage         *storage.MongoStorage
	stats           *storage.StatsStorage
	logger          *logrus.Logger
	whitelistIPs    []string
	config          *config.Config
	mu              sync.RWMutex // 读写锁用于并发保护
}

// Task 异步任务接口定义
type Task interface {
	Execute(*storage.MongoStorage) error
	GetID() string
}

// DeployRequestCompat 兼容旧字段的请求结构
type DeployRequestCompat struct {
    Service      string   `json:"service"`
    Envs         []string `json:"envs"`         // 旧
    Environments []string `json:"environments"` // 新
    Version      string   `json:"version"`
    Username     string   `json:"username"`     // 旧
    User         string   `json:"user"`         // 新
}

// DeployTask 部署任务结构
type DeployTask struct {
	Req storage.DeployRequest
	ID  string
}

// WorkerPool 工作池，用于异步任务处理，支持并发
type WorkerPool struct {
	workers int
	jobs    chan Task // 任务通道，支持缓冲避免阻塞
}

// getGlobalWorkerPool 安全获取全局工作池，确保单次初始化
func getGlobalWorkerPool() *WorkerPool {
	workerPoolOnce.Do(func() {
		if globalStorage == nil {
			logrus.Fatal("❌ FATAL: globalStorage 未设置，无法初始化 WorkerPool")
		}
		globalWorkerPool = NewWorkerPool(20) // 默认20个worker，支持并发处理
		logrus.Info("✅ 全局工作池初始化完成 (20 workers)")
	})
	return globalWorkerPool
}

// NewWorkerPool 创建工作池，启动多个goroutine处理任务
func NewWorkerPool(workers int) *WorkerPool {
	pool := &WorkerPool{
		workers: workers,
		jobs:    make(chan Task, 1000), // 缓冲通道，避免任务丢失
	}

	for i := 0; i < workers; i++ {
		go pool.worker() // 启动worker goroutine
	}
	logrus.WithField("workers", workers).Info("WorkerPool 启动完成")
	return pool
}

// worker 工作goroutine，处理通道中的任务
func (wp *WorkerPool) worker() {
	for job := range wp.jobs {
		if job == nil {
			logrus.Warn("收到空任务，跳过")
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
			logrus.WithError(err).Errorf("任务 %s 执行失败", job.GetID())
		}
	}
}

// Submit 提交任务到工作池
func (wp *WorkerPool) Submit(job Task) {
	if wp.jobs == nil {
		logrus.Error("❌ WorkerPool 未初始化")
		return
	}
	wp.jobs <- job // 异步提交，避免阻塞
}

// PushRequest 推送请求结构
type PushRequest struct {
	Services     []string `json:"services"`
	Environments []string `json:"environments"`
}

// QueryRequest 查询请求结构（支持单个environment兼容）
type QueryRequest struct {
	Service      string   `json:"service"`      // 服务名字（必须）
	Environments []string `json:"environments"` // 环境列表（可选，与environment互斥）
	Environment  string   `json:"environment"`  // 单个环境（可选，与environments互斥）
	User         string   `json:"user"`         // 用户名（可选）
}

// PushTask 推送任务结构
type PushTask struct {
	Services     []string
	Environments []string
	ID           string
}

// Execute PushTask 执行方法（必须实现 Task 接口）
func (t *PushTask) Execute(storage *storage.MongoStorage) error {
	if storage == nil {
		return fmt.Errorf("storage 为 nil")
	}

	start := time.Now()
	err := storage.StoreServiceEnvironments(t.Services, t.Environments)
	if err != nil {
		return err
	}

	updatedServices, _ := storage.GetServices()
	logrus.WithFields(logrus.Fields{
		"task_id":          t.ID,
		"updated_services": updatedServices,
	}).Info("更新后服务列表")

	logrus.WithFields(logrus.Fields{
		"task_id":             t.ID,
		"services_count":      len(t.Services),
		"environments_count":  len(t.Environments),
		"total_task_ms":       time.Since(start).Milliseconds(),
	}).Info("推送任务执行完成")

	return nil
}

func (t *PushTask) GetID() string { return t.ID }

// NewServer 创建 Server 实例
func NewServer(mongoStorage *storage.MongoStorage, statsStorage *storage.StatsStorage, cfg *config.Config) *Server {
	startTime := time.Now()
	defer func() {
		logrus.WithField("init_duration_ms", time.Since(startTime).Milliseconds()).Info("Server 初始化完成")
	}()

	globalStorage = mongoStorage
	logrus.Info("✅ globalStorage 设置完成")

	logger := logrus.New()
	logger.SetFormatter(&logrus.JSONFormatter{}) // JSON格式日志，便于解析
	logger.SetLevel(logrus.InfoLevel)

	whitelist := os.Getenv("WHITELIST_IPS")
	whitelistIPs := []string{}
	if whitelist != "" {
		whitelistIPs = strings.Split(whitelist, ",") // 支持IP白名单
	}

	server := &Server{
		Router:       http.NewServeMux(),
		storage:      mongoStorage,
		stats:        statsStorage,
		logger:       logger,
		whitelistIPs: whitelistIPs,
		config:       cfg,
	}

	// 注册路由
	server.Router.HandleFunc("/push", server.ipWhitelistMiddleware(server.handlePush))
	server.Router.HandleFunc("/query", server.ipWhitelistMiddleware(server.handleQuery))
	server.Router.HandleFunc("/status", server.ipWhitelistMiddleware(server.handleStatus))
	server.Router.HandleFunc("/submit-task", server.ipWhitelistMiddleware(server.handleDeploy))

	getGlobalWorkerPool() // 初始化工作池
	return server
}

// ipWhitelistMiddleware IP 白名单中间件，支持并发请求
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

		allowed := false
		s.mu.RLock() // 读锁保护白名单
		for _, ip := range s.whitelistIPs {
			if strings.Contains(ip, "/") {
				_, ipNet, err := net.ParseCIDR(ip)
				if err != nil {
					continue
				}
				if ipNet.Contains(net.ParseIP(clientIP)) {
					allowed = true
					break
				}
			} else if ip == clientIP {
				allowed = true
				break
			}
		}
		s.mu.RUnlock()

		if !allowed {
			s.logger.Warnf("IP %s 不允许访问", clientIP)
			http.Error(w, "IP 不在白名单内", http.StatusForbidden)
			return
		}

		next(w, r)
	}
}

// handlePush 处理推送请求（/push 接口）
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

	reqJSON, _ := json.Marshal(req)
	s.logger.Infof("收到推送请求: %s", string(reqJSON))

	if len(req.Services) == 0 || len(req.Environments) == 0 {
		s.logger.Error("缺少必填字段：服务列表或环境列表")
		http.Error(w, "缺少必填字段：服务列表或环境列表", http.StatusBadRequest)
		return
	}

	// 异步提交推送任务
	wp := getGlobalWorkerPool()
	taskID := fmt.Sprintf("push-%d", time.Now().UnixNano())
	pushTask := &PushTask{
		Services:     req.Services,
		Environments: req.Environments,
		ID:           taskID,
	}

	wp.Submit(pushTask) // 现在可以编译！

	w.WriteHeader(http.StatusOK)
	response := map[string]interface{}{
		"message": "推送请求已入队",
		"task_id": taskID,
	}
	json.NewEncoder(w).Encode(response)
}

// handleDeploy 处理部署请求（/deploy 接口，优化确认：服务单个，环境多个，版本单个，必须检查存在）
// handleDeploy 处理部署请求（兼容旧字段：envs/username）
// handleDeploy 处理部署请求（多环境自动拆分为单环境任务）
// handleDeploy 处理部署请求（多环境拆分 + 查重）
// handleDeploy 处理部署请求（同步查重 + 插入，避免竞态）
func (s *Server) handleDeploy(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	defer func() {
		s.logger.WithField("total_duration_ms", time.Since(start).Milliseconds()).Info("handleDeploy 执行完成")
	}()

	if r.Method != http.MethodPost {
		http.Error(w, "仅支持 POST 方法", http.StatusMethodNotAllowed)
		return
	}

	// 兼容结构体
	type DeployRequestCompat struct {
		Service      string   `json:"service"`
		Envs         []string `json:"envs"`
		Environments []string `json:"environments"`
		Version      string   `json:"version"`
		Username     string   `json:"username"`
		User         string   `json:"user"`
	}

	var compatReq DeployRequestCompat
	if err := json.NewDecoder(r.Body).Decode(&compatReq); err != nil {
		s.logger.WithError(err).Error("解析部署请求失败")
		http.Error(w, "无效的请求数据", http.StatusBadRequest)
		return
	}

	// 字段映射
	req := struct {
		Service      string
		Environments []string
		Version      string
		User         string
	}{}
	req.Service = compatReq.Service
	req.Version = compatReq.Version

	if len(compatReq.Environments) > 0 {
		req.Environments = compatReq.Environments
	} else if len(compatReq.Envs) > 0 {
		req.Environments = compatReq.Envs
	} else {
		req.Environments = []string{}
	}

	if compatReq.User != "" {
		req.User = compatReq.User
	} else if compatReq.Username != "" {
		req.User = compatReq.Username
	} else {
		req.User = "system"
	}

	// 必填校验
	if req.Service == "" || len(req.Environments) == 0 || req.Version == "" {
		s.logger.Error("缺少必填字段")
		http.Error(w, "缺少必填字段", http.StatusBadRequest)
		return
	}

	// 校验服务和环境
	svcEnvs, err := s.storage.GetServiceEnvironments(req.Service)
	if err != nil || svcEnvs == nil {
		s.logger.WithError(err).Error("服务不存在")
		http.Error(w, "服务不存在", http.StatusBadRequest)
		return
	}
	for _, env := range req.Environments {
		if !contains(svcEnvs, env) {
			s.logger.Warnf("环境 %s 不存在", env)
			http.Error(w, fmt.Sprintf("环境 %s 不存在", env), http.StatusBadRequest)
			return
		}
	}

	// 同步处理：查重 + 插入
	taskIDs := []string{}
	successCount := 0
	failedEnvs := []string{}

	for _, env := range req.Environments {
		singleReq := storage.DeployRequest{
			Service:      req.Service,
			Environments: []string{env},
			Version:      req.Version,
			User:         req.User,
			Status:       "pending",
		}

		// 同步插入（含查重）
		err := s.storage.InsertDeployRequest(singleReq)
		if err != nil {
			s.logger.WithError(err).Warnf("环境 %s 任务提交失败", env)
			failedEnvs = append(failedEnvs, env)
			continue
		}

		taskID := fmt.Sprintf("deploy-%s-%s-%s-%d", req.Service, env, req.Version, time.Now().UnixNano())
		taskIDs = append(taskIDs, taskID)
		successCount++

		// 异步提交到 WorkerPool（只做日志，不插入）
		wp := getGlobalWorkerPool()
		wp.Submit(&DeployTask{
			Req: singleReq,
			ID:  taskID,
		})
	}

	// 响应
	response := map[string]interface{}{
		"message":        "部署请求已处理",
		"task_ids":       taskIDs,
		"success_count":  successCount,
		"failed_count":   len(failedEnvs),
		"failed_envs":    failedEnvs,
	}
	if len(compatReq.Envs) > 0 || compatReq.Username != "" {
		response["warning"] = "使用旧字段名，建议升级"
	}

	w.WriteHeader(http.StatusAccepted)
	json.NewEncoder(w).Encode(response)
}

// handleQuery 处理查询请求（支持多环境查询，服务单个，user可选）
// handleQuery 处理查询请求（单环境精确匹配）
// handleQuery 处理查询请求（对外兼容 environments 数组，对内单环境精确匹配）
// handleQuery 处理查询请求（保持原行为）
// handleQuery 处理查询请求（增强日志可读性）
func (s *Server) handleQuery(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	defer func() {
		s.logger.WithField("total_duration_ms", time.Since(start).Milliseconds()).Info("handleQuery 执行完成")
	}()

	if r.Method != http.MethodPost {
		s.logger.WithFields(logrus.Fields{
			"client_ip": r.RemoteAddr,
			"method":    r.Method,
		}).Warn("无效请求方法")
		http.Error(w, "仅支持 POST 方法", http.StatusMethodNotAllowed)
		return
	}

	// 兼容结构体
	type QueryRequestCompat struct {
		Service      string   `json:"service"`
		Environments []string `json:"environments"`
		Environment  string   `json:"environment"`
		User         string   `json:"user,omitempty"`
	}

	var req QueryRequestCompat
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.logger.WithError(err).WithFields(logrus.Fields{
			"client_ip": r.RemoteAddr,
		}).Error("解析查询请求失败")
		http.Error(w, "无效的请求数据", http.StatusBadRequest)
		return
	}

	// 必填校验 + 日志记录请求
	if req.Service == "" {
		s.logger.WithField("client_ip", r.RemoteAddr).Error("缺少必填字段：service")
		http.Error(w, "缺少必填字段：service", http.StatusBadRequest)
		return
	}

	// 合并环境
	envs := req.Environments
	if req.Environment != "" && len(req.Environments) == 0 {
		envs = []string{req.Environment}
	}
	if len(envs) == 0 {
		s.logger.WithFields(logrus.Fields{
			"client_ip": r.RemoteAddr,
			"service":   req.Service,
		}).Error("缺少环境字段")
		http.Error(w, "缺少环境字段：environment 或 environments", http.StatusBadRequest)
		return
	}

	// 关键日志：清晰打印请求内容
	s.logger.WithFields(logrus.Fields{
		"client_ip":     r.RemoteAddr,
		"service":       req.Service,
		"environments":  envs,
		"user":          req.User,
		"env_count":     len(envs),
	}).Info("收到查询请求")

	// 查询 pending 任务
	results, err := s.storage.QueryDeployQueueByServiceEnv(req.Service, envs, req.User)
	if err != nil {
		s.logger.WithError(err).WithFields(logrus.Fields{
			"service":      req.Service,
			"environments": envs,
		}).Error("查询数据库失败")
		http.Error(w, "查询失败", http.StatusInternalServerError)
		return
	}

	if len(results) == 0 {
		s.logger.WithFields(logrus.Fields{
			"service":      req.Service,
			"environments": envs,
			"user":         req.User,
		}).Info("无待处理任务")
		json.NewEncoder(w).Encode(map[string]string{"message": "暂无待处理任务"})
		return
	}

	// 取第一条任务
	task := results[0]
	matchedEnv := ""
	for _, e := range task.Environments {
		for _, q := range envs {
			if e == q {
				matchedEnv = e
				break
			}
		}
		if matchedEnv != "" {
			break
		}
	}

	// 更新为 assigned
	updateReq := storage.StatusRequest{
		Service:     task.Service,
		Version:     task.Version,
		Environment: matchedEnv,
		User:        task.User,
		Status:      "assigned",
	}
	updated, err := s.storage.UpdateStatus(updateReq)
	if err != nil {
		s.logger.WithError(err).WithFields(logrus.Fields{
			"version": task.Version,
			"env":     matchedEnv,
		}).Error("更新状态为 assigned 失败")
	} else if updated {
		s.logger.WithFields(logrus.Fields{
			"task_id":      fmt.Sprintf("deploy-%s-%s-%s", task.Service, matchedEnv, task.Version),
			"service":      task.Service,
			"environment":  matchedEnv,
			"version":      task.Version,
			"from_status":  "pending",
			"to_status":    "assigned",
		}).Info("任务已分配")
	}

	// 响应 + 详细日志
	responseData := map[string]interface{}{
		"task_id":      fmt.Sprintf("deploy-%s-%s-%s", task.Service, matchedEnv, task.Version),
		"service":      task.Service,
		"environment":  matchedEnv,
		"version":      task.Version,
		"user":         task.User,
		"status":       "assigned",
	}
	s.logger.WithFields(logrus.Fields{
		"response":     responseData,
		"client_ip":    r.RemoteAddr,
	}).Info("查询成功并返回任务")

	json.NewEncoder(w).Encode(responseData)
}

// handleStatus 处理状态更新请求
// handleStatus 处理状态更新请求（单环境）
func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	defer func() {
		s.logger.WithField("total_duration_ms", time.Since(start).Milliseconds()).Info("handleStatus 执行完成")
	}()

	if r.Method != http.MethodPost {
		http.Error(w, "仅支持 POST 方法", http.StatusMethodNotAllowed)
		return
	}

	var req storage.StatusRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.logger.WithError(err).Error("解析状态更新请求失败")
		http.Error(w, "无效的请求数据", http.StatusBadRequest)
		return
	}

	if req.Service == "" || req.Version == "" || req.Environment == "" || req.User == "" {
		s.logger.Error("缺少必填字段")
		http.Error(w, "缺少必填字段", http.StatusBadRequest)
		return
	}

	validStatuses := map[string]bool{"success": true, "failure": true, "no_action": true}
	if !validStatuses[req.Status] {
		s.logger.Error("无效的状态值")
		http.Error(w, "无效的状态值", http.StatusBadRequest)
		return
	}

	// 查询原始数据
	originalTasks, err := s.storage.GetDeployByFilter(req.Service, req.Version, req.Environment)
	if err != nil || len(originalTasks) == 0 {
		s.logger.Warnf("未找到任务: %v", req)
		json.NewEncoder(w).Encode(map[string]string{"message": "未找到匹配任务"})
		return
	}

	// 更新状态（仅 assigned 可更新）
	updated, err := s.storage.UpdateStatus(req)
	if err != nil {
		s.logger.WithError(err).Error("更新状态失败")
		http.Error(w, "更新状态失败", http.StatusInternalServerError)
		return
	}

	if updated {
		s.logger.Infof("状态更新成功: %s → %s", originalTasks[0].Status, req.Status)
		if req.Status == "success" {
			if err := s.stats.InsertDeploySuccess(req.Service, req.Environment, req.Version); err != nil {
				s.logger.WithError(err).Error("插入统计记录失败")
			}
		}
		json.NewEncoder(w).Encode(map[string]string{"message": "状态更新成功"})
	} else {
		s.logger.Warn("未找到可更新任务（必须为 assigned 状态）")
		json.NewEncoder(w).Encode(map[string]string{"message": "未找到匹配任务"})
	}
}

// Execute DeployTask 执行方法（只保留一个！）
func (t *DeployTask) Execute(storage *storage.MongoStorage) error {
	// 数据库插入已在前置完成，这里只打印日志
	logrus.WithFields(logrus.Fields{
		"task_id":   t.ID,
		"service":   t.Req.Service,
		"env":       t.Req.Environments[0],
		"version":   t.Req.Version,
		"user":      t.Req.User,
		"status":    t.Req.Status,
	}).Info("部署任务已入队（持久化完成）")

	return nil
}

func (t *DeployTask) GetID() string { return t.ID }

// contains 检查切片是否包含元素
func contains(slice []string, item string) bool {
	for _, s := range slice {
		if s == item {
			return true
		}
	}
	return false
}