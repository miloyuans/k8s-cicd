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

// *** 修复：api 包统一管理 globalStorage ***
// 全局存储实例，使用 MongoDB 存储
var globalStorage *storage.MongoStorage

// *** 全局锁，确保 WorkerPool 安全初始化 ***
var workerPoolOnce sync.Once
var globalWorkerPool *WorkerPool

// Server 封装 HTTP 服务
type Server struct {
	Router          *http.ServeMux
	storage         *storage.MongoStorage
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

// PushTask 推送任务，用于更新 services 和 environments
type PushTask struct {
	Services     []string
	Environments []string
	ID           string
}

// DeployTask 部署任务，用于插入部署请求到 deploy_queue
type DeployTask struct {
	Req DeployRequest
	ID  string
}

// WorkerPool 工作池，用于处理异步任务
type WorkerPool struct {
	workers int
	jobs    chan Task
}

// *** 安全获取全局工作池 ***
func getGlobalWorkerPool() *WorkerPool {
	workerPoolOnce.Do(func() {
		// *** 修复：确保 globalStorage 已设置 ***
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

// 数据结构定义（保持不变）
type PushRequest struct {
	Services     []string `json:"services"`
	Environments []string `json:"environments"`
}

type DeployRequest struct {
	Service      string   `json:"service"`
	Environments []string `json:"environments"`
	Version      string   `json:"version"`
	User         string   `json:"user"`
	Status       string   `json:"status,omitempty"`
}

type QueryRequest struct {
	Environment string `json:"environment"`
	User        string `json:"user"`
}

type StatusRequest struct {
	Service     string `json:"service"`
	Version     string `json:"version"`
	Environment string `json:"environment"`
	User        string `json:"user"`
	Status      string `json:"status"`
}

// *** 修复：NewServer 接收 Mongo 实例 ***
func NewServer(mongoStorage *storage.MongoStorage, cfg *config.Config) *Server {
	startTime := time.Now()
	defer func() {
		logrus.WithField("init_duration_ms", time.Since(startTime).Milliseconds()).Info("Server 初始化完成")
	}()

	// *** 修复：设置全局存储 ***
	globalStorage = mongoStorage
	logrus.Info("✅ globalStorage 设置完成")

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
		storage:      mongoStorage,
		logger:       logger,
		whitelistIPs: whitelistIPs,
		config:       cfg,
	}

	// 注册路由
	server.Router.HandleFunc("/push", server.ipWhitelistMiddleware(server.handlePush))
	server.Router.HandleFunc("/query", server.ipWhitelistMiddleware(server.handleQuery))
	server.Router.HandleFunc("/status", server.ipWhitelistMiddleware(server.handleStatus))
	server.Router.HandleFunc("/deploy", server.ipWhitelistMiddleware(server.handleDeploy))

	// *** 初始化全局工作池 ***
	getGlobalWorkerPool()

	return server
}

// ipWhitelistMiddleware（完整版本）
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

// handlePush 处理推送请求，异步更新 services 和 environments 集合
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

// handleDeploy 处理部署请求，异步插入到 deploy_queue 集合
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

	// 验证必填字段
	if req.Service == "" || len(req.Environments) == 0 || req.Version == "" || req.User == "" {
		http.Error(w, "缺少必填字段", http.StatusBadRequest)
		return
	}

	if req.Status == "" {
		req.Status = "pending"
	}

	// 验证服务和环境（从独立集合查询）
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

// handleQuery 查询 deploy_queue 集合中的待处理任务
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

// handleStatus 更新 deploy_queue 集合中的任务状态
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

	if updated {
		json.NewEncoder(w).Encode(map[string]string{"message": "状态更新成功"})
	} else {
		json.NewEncoder(w).Encode(map[string]string{"message": "未找到匹配任务"})
	}
}

// PushTask Execute 执行推送任务，更新 services 和 environments 集合
func (t *PushTask) Execute(storage *storage.MongoStorage) error {
	if storage == nil {
		return fmt.Errorf("storage 为 nil")
	}
	
	start := time.Now()
	
	if err := storage.StoreServices(t.Services); err != nil {
		return err
	}
	if err := storage.StoreEnvironments(t.Environments); err != nil {
		return err
	}

	// 新增: 查询更新后的服务列表和环境列表，并日志输出
	updatedServices, err := storage.GetServices()
	if err != nil {
		logrus.WithError(err).Error("查询更新后服务列表失败")
	} else {
		logrus.WithFields(logrus.Fields{
			"task_id":          t.ID,
			"updated_services": updatedServices,
		}).Info("更新后服务列表")
	}

	updatedEnvs, err := storage.GetEnvironments()
	if err != nil {
		logrus.WithError(err).Error("查询更新后环境列表失败")
	} else {
		logrus.WithFields(logrus.Fields{
			"task_id":             t.ID,
			"updated_environments": updatedEnvs,
		}).Info("更新后环境列表")
	}

	logrus.WithFields(logrus.Fields{
		"task_id":             t.ID,
		"services_count":      len(t.Services),
		"environments_count":  len(t.Environments),
		"total_task_ms":       time.Since(start).Milliseconds(),
	}).Info("推送任务执行完成")
	
	return nil
}

func (t *PushTask) GetID() string { return t.ID }

// DeployTask Execute 执行部署任务，插入到 deploy_queue 集合
func (t *DeployTask) Execute(storage *storage.MongoStorage) error {
	if storage == nil {
		return fmt.Errorf("storage 为 nil")
	}
	
	start := time.Now()
	
	err := storage.InsertDeployRequest(t.Req)
	
	logrus.WithFields(logrus.Fields{
		"task_id":     t.ID,
		"service":     t.Req.Service,
		"version":     t.Req.Version,
		"total_ms":    time.Since(start).Milliseconds(),
	}).Info("部署任务入队完成")
	
	return err
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