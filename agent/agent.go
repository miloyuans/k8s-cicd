package agent

import (
	"fmt"
	"time"

	"k8s-cicd/agent/api"
	"k8s-cicd/agent/client"
	"k8s-cicd/agent/config"
	"k8s-cicd/agent/kubernetes"
	"k8s-cicd/agent/models"
	"k8s-cicd/agent/task"
	"k8s-cicd/agent/telegram"

	"github.com/sirupsen/logrus"
)

type Agent struct {
	config     *config.Config
	redis      *client.RedisClient
	k8s        *kubernetes.K8sClient
	taskQ      *task.TaskQueue
	botMgr     *telegram.BotManager
	apiClient  *api.APIClient
	envMapper  *EnvMapper
}

// EnvMapper 环境到命名空间的映射器
type EnvMapper struct {
	mappings map[string]string // env -> namespace
}

// NewEnvMapper 创建环境映射器
func NewEnvMapper(mappings map[string]string) *EnvMapper {
	return &EnvMapper{mappings: mappings}
}

// GetNamespace 根据环境获取命名空间
func (m *EnvMapper) GetNamespace(env string) (string, bool) {
	ns, exists := m.mappings[env]
	if !exists {
		logrus.Errorf("❌ 未配置环境 [%s] 的命名空间映射", env)
		return "", false
	}
	logrus.Infof("🔄 环境 [%s] 映射到命名空间 [%s]", env, ns)
	return ns, true
}

// NewAgent 创建Agent实例
func NewAgent(cfg *config.Config, redis *client.RedisClient, k8s *kubernetes.K8sClient) *Agent {
	// 步骤1：创建机器人管理器
	botMgr := telegram.NewBotManager(cfg.Telegram.Bots)
	// 步骤2：创建任务队列
	taskQ := task.NewTaskQueue(cfg.Task.QueueWorkers)
	// 步骤3：创建API客户端
	apiClient := api.NewAPIClient(&cfg.API)
	// 步骤4：创建环境映射器
	envMapper := NewEnvMapper(cfg.EnvMapping.Mappings)

	// 步骤5：组装Agent
	agent := &Agent{
		config:    cfg,
		redis:     redis,
		k8s:       k8s,
		taskQ:     taskQ,
		botMgr:    botMgr,
		apiClient: apiClient,
		envMapper: envMapper,
	}

	// 步骤6：启动Telegram轮询
	botMgr.StartPolling()

	return agent
}

// Start 启动Agent
func (a *Agent) Start() {
	logrus.Infof("🚀 Agent启动成功")

	logrus.Infof("📡 API Base URL: %s", a.config.API.BaseURL)
	logrus.Infof("👤 默认用户: %s", a.config.User.Default)
	logrus.Infof("🔄 推送间隔: %v", a.config.API.PushInterval)
	logrus.Infof("🔍 查询间隔: %v", a.config.API.QueryInterval)
	logrus.Infof("📱 弹窗环境: %v", a.config.Query.ConfirmEnvs)
	logrus.Infof("👥 允许用户: %v", a.config.Telegram.AllowedUsers)

	// 步骤1：启动任务队列Worker
	go a.taskQ.StartWorkers(a.config, a.redis, a.k8s, a.botMgr, a.apiClient)

	// 步骤2：启动周期性推送
	go a.periodicPushDiscovery()

	// 步骤3：启动周期性查询+弹窗确认
	go a.periodicQueryTasks()
}

// periodicPushDiscovery 周期性K8s发现 + 单个服务推送
func (a *Agent) periodicPushDiscovery() {
	// 步骤1：立即执行一次
	a.performPushDiscovery()

	// 步骤2：无限轮询
	ticker := time.NewTicker(a.config.API.PushInterval)
	defer ticker.Stop()

	for range ticker.C {
		a.performPushDiscovery()
	}
}

// performPushDiscovery 执行单次推送（按服务拆分）
func (a *Agent) performPushDiscovery() {
	// 步骤1：记录开始日志
	logrus.Info("🌐 === 开始K8s服务发现 ===")

	// 步骤2：构建完整请求
	fullReq, err := a.k8s.BuildPushRequest(a.config)
	if err != nil {
		logrus.Errorf("❌ 服务发现失败: %v", err)
		return
	}

	logrus.Infof("📊 发现 %d 个服务，%d 个环境", len(fullReq.Services), len(fullReq.Environments))

	// 步骤3：按服务拆分推送
	for _, service := range fullReq.Services {
		// 构建单个服务请求
		var serviceDeployments []models.DeployRequest
		for _, dep := range fullReq.Deployments {
			if dep.Service == service {
				serviceDeployments = append(serviceDeployments, dep)
			}
		}

		pushReq := models.PushRequest{
			Services:     []string{service},
			Environments: fullReq.Environments,
			Deployments:  serviceDeployments,
		}

		// 发送请求
		err = a.apiClient.PushData(pushReq)
		if err != nil {
			logrus.Errorf("❌ 推送失败 [%s]: %v", service, err)
			continue
		}

		logrus.Infof("✅ 推送成功 [%s] -> %d 环境", service, len(fullReq.Environments))
		time.Sleep(100 * time.Millisecond)
	}
}

// periodicQueryTasks 周期性查询 + 弹窗确认
func (a *Agent) periodicQueryTasks() {
	ticker := time.NewTicker(a.config.API.QueryInterval)
	defer ticker.Stop()

	// 步骤1：创建通道
	confirmChan := make(chan models.DeployRequest, 100)
	rejectChan := make(chan models.StatusRequest, 100)

	// 步骤2：启动Telegram回调处理
	go a.botMgr.PollUpdates(a.config.Telegram.AllowedUsers, confirmChan, rejectChan)

	// 步骤3：处理确认/拒绝
	go a.handleConfirmationChannels(confirmChan, rejectChan)

	for range ticker.C {
		// 步骤4：记录轮询日志
		logrus.Info("🔍 === 开始 /query 轮询 ===")

		user := a.config.User.Default
		envs := make([]string, 0, len(a.config.EnvMapping.Mappings))
		for env := range a.config.EnvMapping.Mappings {
			envs = append(envs, env)
		}

		// 步骤5：对每个环境执行查询
		for _, env := range envs {
			queryReq := models.QueryRequest{
				Environment: env,
				User:        user,
			}

			tasks, err := a.apiClient.QueryTasks(queryReq)
			if err != nil {
				logrus.Errorf("❌ /query 失败 [%s]: %v", env, err)
				continue
			}

			logrus.Infof("📋 /query 结果 [%s]: %d 个任务", env, len(tasks))

			for _, task := range tasks {
				a.processTask(task, env)
			}
		}
	}
}

// processTask 处理单个任务（环境过滤 + 弹窗确认）
func (a *Agent) processTask(task models.DeployRequest, queryEnv string) {
	// 步骤1：环境匹配校验
	if task.Environments[0] != queryEnv {
		logrus.Warnf("⚠️ 环境不匹配: 查询[%s] != 任务[%s]", queryEnv, task.Environments[0])
		return
	}

	// 步骤2：状态校验
	if task.Status != "pending" {
		logrus.Warnf("⚠️ 状态非pending: %s", task.Status)
		return
	}

	// 步骤3：检查是否需要弹窗
	needConfirm := false
	for _, confirmEnv := range a.config.Query.ConfirmEnvs {
		if queryEnv == confirmEnv {
			needConfirm = true
			break
		}
	}

	if needConfirm {
		// 发送弹窗
		err := a.botMgr.SendConfirmation(task.Service, queryEnv, task.User, task.Version, a.config.Telegram.AllowedUsers)
		if err != nil {
			logrus.Errorf("❌ 弹窗发送失败 [%s]: %v", task.Service, err)
			return
		}
		logrus.Infof("📱 已发送确认弹窗: %s v%s [%s]", task.Service, task.Version, queryEnv)
	} else {
		// 直接入队
		if err := a.validateAndStoreTask(task, queryEnv); err != nil {
			logrus.Warn(err.Error())
			return
		}
		a.taskQ.Enqueue(models.Task{
			DeployRequest: task,
			ID:            fmt.Sprintf("%s-%s-%d", task.Service, task.Version, time.Now().Unix()),
			CreatedAt:     time.Now(),
			Retries:       0,
		})
		logrus.Infof("📥 直接入队: %s v%s [%s]", task.Service, task.Version, queryEnv)
	}
}

// handleConfirmationChannels 处理确认/拒绝通道
func (a *Agent) handleConfirmationChannels(confirmChan chan models.DeployRequest, rejectChan chan models.StatusRequest) {
	for {
		select {
		case task := <-confirmChan:
			// 处理确认
			if err := a.validateAndStoreTask(task, task.Environments[0]); err != nil {
				logrus.Error(err.Error())
				continue
			}
			a.taskQ.Enqueue(models.Task{
				DeployRequest: task,
				ID:            fmt.Sprintf("%s-%s-%d", task.Service, task.Version, time.Now().Unix()),
				CreatedAt:     time.Now(),
				Retries:       0,
			})
			logrus.Infof("✅ 确认入队: %s v%s [%s]", task.Service, task.Version, task.Environments[0])

		case status := <-rejectChan:
			// 处理拒绝
			err := a.apiClient.UpdateStatus(status)
			if err != nil {
				logrus.Errorf("❌ 拒绝状态更新失败: %v", err)
			} else {
				logrus.Infof("❌ 已拒绝: %s v%s [%s]", status.Service, status.Version, status.Environment)
			}
		}
	}
}

// validateAndStoreTask 任务校验 + Redis存储 + 去重
func (a *Agent) validateAndStoreTask(task models.DeployRequest, env string) error {
	// 步骤1：获取命名空间
	namespace, ok := a.envMapper.GetNamespace(env)
	if !ok {
		return fmt.Errorf("❌ 环境 [%s] 无命名空间配置", env)
	}
	task.Environments = []string{namespace}

	// 步骤2：检查重复
	isDuplicate, err := a.redis.CheckDuplicateTask(task)
	if err != nil {
		return fmt.Errorf("❌ Redis去重失败: %v", err)
	}
	if isDuplicate {
		return fmt.Errorf("⚠️ 任务重复，忽略: %s v%s", task.Service, task.Version)
	}

	// 步骤3：存储到Redis
	err = a.redis.StoreTaskWithDeduplication(task)
	if err != nil {
		return fmt.Errorf("❌ Redis存储失败: %v", err)
	}

	logrus.Infof("✅ 校验通过 & 存储成功: %s v%s [%s -> %s]", task.Service, task.Version, env, namespace)
	return nil
}

// Stop 优雅停止Agent
func (a *Agent) Stop() {
	logrus.Info("🛑 停止Agent...")

	a.taskQ.Stop()
	a.botMgr.Stop()

	time.Sleep(2 * time.Second)
	logrus.Info("✅ Agent关闭完成")
}