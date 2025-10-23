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

// 全局存储实例
var globalStorage *storage.MongoStorage

// 全局锁，确保 WorkerPool 安全初始化
var workerPoolOnce sync.Once
var globalWorkerPool *WorkerPool

// Server 封装 HTTP 服务
type Server struct {
	Router          *http.ServeMux
	storage         *storage.MongoStorage
	stats           *storage.StatsStorage
	logger          *logrus.Logger
	whitelistIPs    []string
	config          *config.Config
	mu              sync.RWMutex
}

// Task 异步任务接口
type Task interface {
	Execute(*storage.MongoStorage) error
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
	Req storage.DeployRequest
	ID  string
}

// WorkerPool 工作池
type WorkerPool struct {
	workers int
	jobs    chan Task
}

// getGlobalWorkerPool 安全获取全局工作池
func getGlobalWorkerPool() *WorkerPool {
	workerPoolOnce.Do(func() {
		if globalStorage == nil {
			logrus.Fatal("❌ FATAL: globalStorage 未设置，无法初始化 WorkerPool")
		}
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

	for i := 0; i < workers; i++ {
		go pool.worker()
	}
	logrus.WithField("workers", workers).Info("WorkerPool 启动完成")
	return pool
}

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

func (wp *WorkerPool) Submit(job Task) {
	if wp.jobs == nil {
		logrus.Error("❌ WorkerPool 未初始化")
		return
	}
	wp.jobs <- job
}

// PushRequest 推送请求
type PushRequest struct {
	Services     []string `json:"services"`
	Environments []string `json:"environments"`
}

// QueryRequest 查询请求
type QueryRequest struct {
	Environment string `json:"environment"`
	User        string `json:"user"`
}

// NewServer 创建 Server
func NewServer(mongoStorage *storage.MongoStorage, statsStorage *storage.StatsStorage, cfg *config.Config) *Server {
	startTime := time.Now()
	defer func() {
		logrus.WithField("init_duration_ms", time.Since(startTime).Milliseconds()).Info("Server 初始化完成")
	}()

	globalStorage = mongoStorage
	logrus.Info("✅ globalStorage 设置完成")

	logger := logrus.New()
	logger.SetFormatter(&logrus.JSONFormatter{})
	logger.SetLevel(logrus.InfoLevel)

	whitelist := os.Getenv("WHITELIST_IPS")
	whitelistIPs := []string{}
	if whitelist != "" {
		whitelistIPs = strings.Split(whitelist, ",")
	}

	server := &Server{
		Router:       http.NewServeMux(),
		storage:      mongoStorage,
		stats:        statsStorage,
		logger:       logger,
		whitelistIPs: whitelistIPs,
		config:       cfg,
	}

	server.Router.HandleFunc("/push", server.ipWhitelistMiddleware(server.handlePush))
	server.Router.HandleFunc("/query", server.ipWhitelistMiddleware(server.handleQuery))
	server.Router.HandleFunc("/status", server.ipWhitelistMiddleware(server.handleStatus))
	server.Router.HandleFunc("/deploy", server.ipWhitelistMiddleware(server.handleDeploy))

	getGlobalWorkerPool()
	return server
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

		allowed := false
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

		if !allowed {
			s.logger.Warnf("IP %s 不允许访问", clientIP)
			http.Error(w, "IP 不在白名单内", http.StatusForbidden)
			return
		}

		next(w, r)
	}
}

// handlePush 处理推送请求
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

	if len(req.Services) == 0 || len(req.Environments) == 0 {
		http.Error(w, "服务名和环境必须存在", http.StatusBadRequest)
		return
	}

	s.logger.WithFields(logrus.Fields{
		"services_count":     len(req.Services),
		"environments_count": len(req.Environments),
	}).Info("收到推送请求")

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

// handleDeploy 处理部署请求
func (s *Server) handleDeploy(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	defer func() {
		s.logger.WithField("total_duration_ms", time.Since(start).Milliseconds()).Info("handleDeploy 执行完成")
	}()

	if r.Method != http.MethodPost {
		http.Error(w, "仅支持 POST 方法", http.StatusMethodNotAllowed)
		return
	}

	var req storage.DeployRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.logger.WithError(err).Error("解析部署请求失败")
		http.Error(w, "无效的请求数据", http.StatusBadRequest)
		return
	}

	if req.Service == "" || len(req.Environments) == 0 || req.Version == "" {
		http.Error(w, "缺少必填字段：服务名、环境、版本", http.StatusBadRequest)
		return
	}

	if req.User == "" {
		req.User = "system"
	}
	if req.Status == "" {
		req.Status = "pending"
	}

	svcEnvs, err := s.storage.GetServiceEnvironments(req.Service)
	if err != nil {
		http.Error(w, "查询服务失败", http.StatusInternalServerError)
		return
	}
	if svcEnvs == nil {
		http.Error(w, fmt.Sprintf("服务 %s 不存在", req.Service), http.StatusBadRequest)
		return
	}

	for _, env := range req.Environments {
		if !contains(svcEnvs, env) {
			http.Error(w, fmt.Sprintf("环境 %s 不存在于服务 %s", env, req.Service), http.StatusBadRequest)
			return
		}
	}

	wp := getGlobalWorkerPool()
	taskID := fmt.Sprintf("deploy-%s-%s-%d", req.Service, req.Version, time.Now().UnixNano())
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
		http.Error(w, "无效的请求数据", http.StatusBadRequest)
		return
	}

	if req.Environment == "" || req.User == "" {
		http.Error(w, "缺少必填字段", http.StatusBadRequest)
		return
	}

	results, err := s.storage.QueryDeployQueue(req.Environment, req.User)
	if err != nil {
		http.Error(w, "查询失败", http.StatusInternalServerError)
		return
	}

	if len(results) == 0 {
		json.NewEncoder(w).Encode(map[string]string{"message": "暂无待处理任务"})
		return
	}

	json.NewEncoder(w).Encode(results)
}

// handleStatus 处理状态更新请求
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
		http.Error(w, "无效的请求数据", http.StatusBadRequest)
		return
	}

	if req.Service == "" || req.Version == "" || req.Environment == "" || req.User == "" {
		http.Error(w, "缺少必填字段", http.StatusBadRequest)
		return
	}

	validStatuses := map[string]bool{"success": true, "failure": true, "no_action": true}
	if !validStatuses[req.Status] {
		http.Error(w, "无效的状态值", http.StatusBadRequest)
		return
	}

	updated, err := s.storage.UpdateStatus(req)
	if err != nil {
		http.Error(w, "更新状态失败", http.StatusInternalServerError)
		return
	}

	if updated && req.Status == "success" {
		if err := s.stats.InsertDeploySuccess(req.Service, req.Environment, req.Version); err != nil {
			s.logger.WithError(err).Error("插入统计记录失败")
		}
	}

	if updated {
		json.NewEncoder(w).Encode(map[string]string{"message": "状态更新成功"})
	} else {
		json.NewEncoder(w).Encode(map[string]string{"message": "未找到匹配任务"})
	}
}

// PushTask Execute
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

// DeployTask Execute
func (t *DeployTask) Execute(storage *storage.MongoStorage) error {
	if storage == nil {
		return fmt.Errorf("storage 为 nil")
	}

	start := time.Now()
	err := storage.InsertDeployRequest(t.Req)

	logrus.WithFields(logrus.Fields{
		"task_id":   t.ID,
		"service":   t.Req.Service,
		"version":   t.Req.Version,
		"total_ms":  time.Since(start).Milliseconds(),
	}).Info("部署任务入队完成")

	return err
}

func (t *DeployTask) GetID() string { return t.ID }

// contains 检查是否包含
func contains(slice []string, item string) bool {
	for _, s := range slice {
		if s == item {
			return true
		}
	}
	return false
}