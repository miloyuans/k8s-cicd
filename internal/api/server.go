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

// PushTask 推送任务结构
type PushTask struct {
	Services     []string
	Environments []string
	ID           string
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

// DeployRequestCompat 兼容旧版字段的部署请求结构
type DeployRequestCompat struct {
	Service      string   `json:"service"`
	Envs         []string `json:"envs"`         // 旧字段
	Environments []string `json:"environments"` // 新字段
	Version      string   `json:"version"`
	Username     string   `json:"username"`     // 旧字段
	User         string   `json:"user"`         // 新字段
}

// QueryRequest 查询请求结构（支持单个environment兼容）
type QueryRequest struct {
	Service      string   `json:"service"`      // 服务名字（必须）
	Environments []string `json:"environments"` // 环境列表（可选，与environment互斥）
	Environment  string   `json:"environment"`  // 单个环境（可选，与environments互斥）
	User         string   `json:"user"`         // 用户名（可选）
}

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

	wp.Submit(pushTask)

	w.WriteHeader(http.StatusOK) // 200 OK
	response := map[string]interface{}{
		"message": "推送请求已入队",
		"task_id": taskID,
	}
	respJSON, _ := json.Marshal(response)
	s.logger.Infof("推送响应: %s", string(respJSON))
	json.NewEncoder(w).Encode(response)
}

// handleDeploy 处理部署请求（/deploy 接口，优化确认：服务单个，环境多个，版本单个，必须检查存在）
// handleDeploy 处理部署请求（兼容旧字段：envs/username）
func (s *Server) handleDeploy(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	defer func() {
		s.logger.WithField("total_duration_ms", time.Since(start).Milliseconds()).Info("handleDeploy 执行完成")
	}()

	if r.Method != http.MethodPost {
		http.Error(w, "仅支持 POST 方法", http.StatusMethodNotAllowed)
		return
	}

	// 兼容结构体：支持新旧字段
	type DeployRequestCompat struct {
		Service      string   `json:"service"`
		Envs         []string `json:"envs"`         // 旧字段
		Environments []string `json:"environments"` // 新字段
		Version      string   `json:"version"`
		Username     string   `json:"username"`     // 旧字段
		User         string   `json:"user"`         // 新字段
	}

	var compatReq DeployRequestCompat
	if err := json.NewDecoder(r.Body).Decode(&compatReq); err != nil {
		s.logger.WithError(err).Error("解析部署请求失败")
		http.Error(w, "无效的请求数据", http.StatusBadRequest)
		return
	}

	// 转换为标准结构体
	req := storage.DeployRequest{
		Service: compatReq.Service,
		Version: compatReq.Version,
		Status:  "pending",
	}

	// 环境字段映射：优先新字段
	envSource := "environments"
	if len(compatReq.Environments) > 0 {
		req.Environments = compatReq.Environments
	} else if len(compatReq.Envs) > 0 {
		req.Environments = compatReq.Envs
		envSource = "envs"
	} else {
		req.Environments = []string{}
	}

	// 用户字段映射：优先新字段
	userSource := "user"
	if compatReq.User != "" {
		req.User = compatReq.User
	} else if compatReq.Username != "" {
		req.User = compatReq.Username
		userSource = "username"
	} else {
		req.User = "system"
		userSource = "default(system)"
	}

	// 必填字段校验
	if req.Service == "" || len(req.Environments) == 0 || req.Version == "" {
		missing := []string{}
		if req.Service == "" {
			missing = append(missing, "service")
		}
		if len(req.Environments) == 0 {
			missing = append(missing, "environments/envs")
		}
		if req.Version == "" {
			missing = append(missing, "version")
		}
		s.logger.WithFields(logrus.Fields{
			"missing_fields": missing,
			"raw_request":    compatReq,
		}).Error("缺少必填字段")
		http.Error(w, fmt.Sprintf("缺少必填字段: %s", strings.Join(missing, ", ")), http.StatusBadRequest)
		return
	}

	// 记录字段来源日志
	s.logger.WithFields(logrus.Fields{
		"service":        req.Service,
		"version":        req.Version,
		"environments":   req.Environments,
		"user":           req.User,
		"env_source":     envSource,
		"user_source":    userSource,
		"using_deprecated": len(compatReq.Envs) > 0 || compatReq.Username != "",
	}).Info("收到部署请求（字段来源已记录）")

	// 检查服务是否存在
	svcEnvs, err := s.storage.GetServiceEnvironments(req.Service)
	if err != nil {
		s.logger.WithError(err).Error("查询服务环境失败")
		http.Error(w, "查询服务失败", http.StatusInternalServerError)
		return
	}
	if svcEnvs == nil {
		s.logger.Warnf("服务 %s 不存在", req.Service)
		http.Error(w, fmt.Sprintf("服务 %s 不存在", req.Service), http.StatusBadRequest)
		return
	}

	// 检查每个环境是否属于该服务
	for _, env := range req.Environments {
		if !contains(svcEnvs, env) {
			s.logger.Warnf("环境 %s 不存在于服务 %s", env, req.Service)
			http.Error(w, fmt.Sprintf("环境 %s 不存在于服务 %s", env, req.Service), http.StatusBadRequest)
			return
		}
	}

	// 异步提交部署任务
	wp := getGlobalWorkerPool()
	taskID := fmt.Sprintf("deploy-%s-%s-%d", req.Service, req.Version, time.Now().UnixNano())
	deployTask := &DeployTask{
		Req: req,
		ID:  taskID,
	}

	wp.Submit(deployTask)

	// 响应构建
	response := map[string]interface{}{
		"message": "部署请求已入队",
		"task_id": taskID,
	}

	// 如果使用了旧字段，返回警告
	if len(compatReq.Envs) > 0 || compatReq.Username != "" {
		response["warning"] = "检测到使用旧字段名 (envs/username)，建议升级为 environments/user 以获得更好兼容性"
		s.logger.Warn("客户端使用旧字段名，建议升级接口")
	}

	// 使用标准 HTTP 状态码 202 Accepted
	w.WriteHeader(http.StatusAccepted)
	if err := json.NewEncoder(w).Encode(response); err != nil {
		s.logger.WithError(err).Error("响应编码失败")
	}
}

// handleQuery 处理查询请求（支持多环境查询，服务单个，user可选）
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
		fmt.Printf("\033[31m[错误] 解析查询请求失败: %v\033[0m\n", err)
		http.Error(w, "无效的请求数据", http.StatusBadRequest)
		return
	}

	// 兼容单个环境
	if len(req.Environments) == 0 && req.Environment != "" {
		req.Environments = []string{req.Environment}
	}

	reqJSON, _ := json.Marshal(req)
	s.logger.Infof("收到查询请求: %s", string(reqJSON))
	fmt.Printf("\033[32m[成功] 收到查询请求: %s\033[0m\n", string(reqJSON))

	if req.Service == "" || len(req.Environments) == 0 {
		s.logger.Error("缺少必填字段：服务名或环境列表")
		fmt.Printf("\033[31m[错误] 缺少必填字段：service=%s, environments=%v\033[0m\n", req.Service, req.Environments)
		http.Error(w, "缺少必填字段：服务名或环境列表", http.StatusBadRequest)
		return
	}

	// 查询pending状态的任务
	fmt.Printf("\033[32m[成功] 开始查询 MongoDB: service=%s, environments=%v, user=%s\033[0m\n",
		req.Service, req.Environments, req.User)
	s.logger.Infof("开始查询: service=%s, environments=%v, user=%s", req.Service, req.Environments, req.User)

	results, err := s.storage.QueryDeployQueueByServiceEnv(req.Service, req.Environments, req.User)
	if err != nil {
		s.logger.WithError(err).Error("查询数据库失败")
		fmt.Printf("\033[31m[错误] 查询数据库失败: %v\033[0m\n", err)
		http.Error(w, "查询失败", http.StatusInternalServerError)
		return
	}

	if len(results) > 0 {
		dataJSON, _ := json.Marshal(results)
		s.logger.Infof("查询反馈数据: %s", string(dataJSON))
		fmt.Printf("\033[32m[成功] 查询到 %d 条 pending 任务:\n%s\033[0m\n", len(results), string(dataJSON))

		// 先反馈数据给外部服务
		err = json.NewEncoder(w).Encode(results)
		if err != nil {
			s.logger.WithError(err).Error("反馈数据失败")
			fmt.Printf("\033[31m[错误] 反馈数据失败: %v\033[0m\n", err)
			return
		}

		// 反馈成功后，更新状态为 assigned
		fmt.Printf("\033[32m[成功] 开始更新任务状态为 assigned\033[0m\n")
		s.logger.Info("开始更新任务状态为 assigned")

		for _, task := range results {
			// 确定任务匹配的环境
			var matchedEnv string
			for _, env := range task.Environments {
				for _, reqEnv := range req.Environments {
					if env == reqEnv {
						matchedEnv = env
						break
					}
				}
				if matchedEnv != "" {
					break
				}
			}
			if matchedEnv == "" {
				s.logger.Warnf("任务 %s 未找到匹配环境", task.Version)
				fmt.Printf("\033[33m[异常] 任务 %s 未找到匹配环境\033[0m\n", task.Version)
				continue
			}

			updateReq := storage.StatusRequest{
				Service:     task.Service,
				Version:     task.Version,
				Environment: matchedEnv,
				User:        task.User,
				Status:      "assigned",
			}

			updated, err := s.storage.UpdateStatus(updateReq)
			if err != nil {
				s.logger.WithError(err).Errorf("更新任务 %s 到 assigned 失败", task.Version)
				fmt.Printf("\033[31m[错误] 更新任务 %s 到 assigned 失败: %v\033[0m\n", task.Version, err)
			} else if !updated {
				s.logger.Warnf("未更新任务 %s 到 assigned (可能状态不匹配)", task.Version)
				fmt.Printf("\033[33m[异常] 未更新任务 %s 到 assigned (可能状态不匹配)\033[0m\n", task.Version)
			} else {
				// 更新成功，重新查询验证
				recheckedTasks, recheckErr := s.storage.GetDeployByFilter(task.Service, task.Version, matchedEnv)
				if recheckErr != nil {
					s.logger.WithError(recheckErr).Errorf("重新查询更新后任务 %s 失败", task.Version)
					fmt.Printf("\033[31m[错误] 重新查询更新后任务 %s 失败: %v\033[0m\n", task.Version, recheckErr)
				} else {
					recheckedJSON, _ := json.Marshal(recheckedTasks)
					s.logger.Infof("任务 %s 更新为 assigned, 更新后数据: %s", task.Version, string(recheckedJSON))
					fmt.Printf("\033[32m[成功] 任务 %s 更新为 assigned, 更新后数据:\n%s\033[0m\n", task.Version, string(recheckedJSON))
				}
			}
		}
	} else {
		s.logger.Info("查询成功但无待处理任务")
		fmt.Printf("\033[33m[信息] 查询成功但无pending任务: service=%s, environments=%v, user=%s\033[0m\n",
			req.Service, req.Environments, req.User)
		json.NewEncoder(w).Encode(map[string]string{"message": "暂无待处理任务"})
	}
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

	// 先查询原始数据（用于日志）
	originalTasks, err := s.storage.GetDeployByFilter(req.Service, req.Version, req.Environment)
	if err != nil {
		s.logger.WithError(err).Error("预查询任务失败")
		http.Error(w, "预查询失败", http.StatusInternalServerError)
		return
	}

	// 尝试更新（UpdateStatus已内置检查status="assigned"）
	updated, err := s.storage.UpdateStatus(req)
	if err != nil {
		s.logger.WithError(err).Error("更新状态失败")
		http.Error(w, "更新状态失败", http.StatusInternalServerError)
		return
	}

	if updated {
		// 更新成功，查询更新后数据用于日志
		updatedTasks, _ := s.storage.GetDeployByFilter(req.Service, req.Version, req.Environment)
		origJSON, _ := json.Marshal(originalTasks)
		updJSON, _ := json.Marshal(updatedTasks)
		s.logger.Infof("状态更新成功: 原始数据=%s, 更新后数据=%s", string(origJSON), string(updJSON))

		if req.Status == "success" {
			if err := s.stats.InsertDeploySuccess(req.Service, req.Environment, req.Version); err != nil {
				s.logger.WithError(err).Error("插入统计记录失败")
			}
		}
		json.NewEncoder(w).Encode(map[string]string{"message": "状态更新成功"})
	} else {
		// 不匹配: 打印查询到的数据
		dataJSON, _ := json.Marshal(originalTasks)
		s.logger.Warnf("未找到匹配任务 (必须是assigned状态): 查询条件=service=%s, version=%s, env=%s, user=%s, 找到的数据=%s",
			req.Service, req.Version, req.Environment, req.User, string(dataJSON))
		json.NewEncoder(w).Encode(map[string]string{"message": "未找到匹配任务"})
	}
}

// Execute PushTask 执行方法
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

// Execute DeployTask 执行方法
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

// contains 检查切片是否包含元素
func contains(slice []string, item string) bool {
	for _, s := range slice {
		if s == item {
			return true
		}
	}
	return false
}