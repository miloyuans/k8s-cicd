package agent

import (
	"fmt"
	"strings"
	"time"

	"k8s-cicd/agent/api"
	"k8s-cicd/agent/config"
	"k8s-cicd/agent/kubernetes"
	"k8s-cicd/agent/models"
	"k8s-cicd/agent/task"
	"k8s-cicd/agent/telegram"

	"github.com/fatih/color"
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

// EnvMapper 环境到命名空间映射器
type EnvMapper struct {
	mappings map[string]string
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
	botMgr := telegram.NewBotManager(cfg.Telegram.Bots)
	taskQ := task.NewTaskQueue(cfg.Task.QueueWorkers)
	apiClient := api.NewAPIClient(&cfg.API)
	envMapper := NewEnvMapper(cfg.EnvMapping.Mappings)

	agent := &Agent{
		config:    cfg,
		redis:     redis,
		k8s:       k8s,
		taskQ:     taskQ,
		botMgr:    botMgr,
		apiClient: apiClient,
		envMapper: envMapper,
	}

	// 启动Telegram轮询
	botMgr.StartPolling()

	return agent
}

// Start 启动Agent
func (a *Agent) Start() {
	green := color.New(color.FgGreen).SprintFunc()
	logrus.Infof("%s 🚀 Agent启动成功", green("✅"))

	logrus.Infof("📡 API Base URL: %s", a.config.API.BaseURL)
	logrus.Infof("👤 默认用户: %s", a.config.User.Default)
	logrus.Infof("🔄 推送间隔: %v", a.config.API.PushInterval)
	logrus.Infof("🔍 查询间隔: %v", a.config.API.QueryInterval)
	logrus.Infof("📱 弹窗环境: %v", a.config.Query.ConfirmEnvs)
	logrus.Infof("👥 允许用户: %v", a.config.Telegram.AllowedUsers)

	// 步骤1：启动任务队列Worker
	go a.taskQ.StartWorkers(a.config, a.redis, a.k8s, a.botMgr, a.apiClient)

	// 步骤2：启动周期性推送（需求2：30s轮询）
	go a.periodicPushDiscovery()

	// 步骤3：启动周期性查询+弹窗确认（需求4）
	go a.periodicQueryTasks()
}

// periodicPushDiscovery 周期性K8s发现 + 单个服务推送（需求1、2、3）
func (a *Agent) periodicPushDiscovery() {
	// 立即执行一次
	a.performPushDiscovery()

	// 无限轮询，使用配置间隔（默认30s）
	ticker := time.NewTicker(a.config.API.PushInterval)
	defer ticker.Stop()

	for range ticker.C {
		a.performPushDiscovery()
	}
}

// performPushDiscovery 执行单次推送（按服务拆分）
func (a *Agent) performPushDiscovery() {
	logrus.Info("🌐 === 开始K8s服务发现 ===")

	// 步骤1：构建完整请求（已去重）
	fullReq, err := a.k8s.BuildPushRequest(a.config)
	if err != nil {
		logrus.Errorf("❌ 服务发现失败: %v", err)
		return
	}

	logrus.Infof("📊 发现 %d 个服务，%d 个环境", len(fullReq.Services), len(fullReq.Environments))

	// 步骤2：按服务拆分推送（需求3：单个服务一个请求）
	for _, service := range fullReq.Services {
		// 构建单个服务请求
		var serviceDeployments []models.DeployRequest
		for _, dep := range fullReq.Deployments {
			if dep.Service == service {
				serviceDeployments = append(serviceDeployments, dep)
			}
		}

		pushReq := models.PushRequest{
			Services:     []string{service},                    // 单个服务
			Environments: fullReq.Environments,                 // 所有去重环境
			Deployments:  serviceDeployments,                  // 该服务的部署记录
		}

		// 步骤3：发送单个请求
		err = a.apiClient.PushData(pushReq)
		if err != nil {
			logrus.Errorf("❌ 推送失败 [%s]: %v", service, err)
			continue
		}

		green := color.New(color.FgGreen)
		green.Printf("✅ 推送成功 [%s] -> %d 环境\n", service, len(fullReq.Environments))
		time.Sleep(100 * time.Millisecond) // 避免请求过快
	}
}

// periodicQueryTasks 周期性查询 + 弹窗确认（需求4）
func (a *Agent) periodicQueryTasks() {
	ticker := time.NewTicker(a.config.API.QueryInterval)
	defer ticker.Stop()

	// 步骤1：创建通信通道
	confirmChan := make(chan models.DeployRequest, 100)
	rejectChan := make(chan models.StatusRequest, 100)

	// 步骤2：启动Telegram回调处理
	go a.botMgr.PollUpdates(a.config.Telegram.AllowedUsers, confirmChan, rejectChan)

	// 步骤3：处理确认/拒绝通道
	go a.handleConfirmationChannels(confirmChan, rejectChan)

	for range ticker.C {
		logrus.Info("🔍 === 开始 /query 轮询 ===")

		user := a.config.User.Default
		envs := make([]string, 0, len(a.config.EnvMapping.Mappings))
		for env := range a.config.EnvMapping.Mappings {
			envs = append(envs, env)
		}

		// 步骤4：对每个环境执行查询
		for _, env := range envs {
			queryReq := models.QueryRequest{
				Environment: env,
				User:        user,
			}

			// 执行 /query
			tasks, err := a.apiClient.QueryTasks(queryReq)
			if err != nil {
				logrus.Errorf("❌ /query 失败 [%s]: %v", env, err)
				continue
			}

			logrus.Infof("📋 /query 结果 [%s]: %d 个任务", env, len(tasks))

			// 步骤5：处理每个任务
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

	// 步骤3：环境过滤 - 是否需要弹窗确认
	needConfirm := false
	for _, confirmEnv := range a.config.Query.ConfirmEnvs {
		if queryEnv == confirmEnv {
			needConfirm = true
			break
		}
	}

	if needConfirm {
		// 需要弹窗确认（需求4）
		err := a.botMgr.SendConfirmation(task.Service, queryEnv, task.User, task.Version, a.config.Telegram.AllowedUsers)
		if err != nil {
			logrus.Errorf("❌ 弹窗发送失败 [%s]: %v", task.Service, err)
			return
		}
		logrus.Infof("📱 已发送确认弹窗: %s v%s [%s]", task.Service, task.Version, queryEnv)
	} else {
		// 不需要弹窗，直接入队列
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
			// 处理确认：校验 + 入队
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
			green := color.New(color.FgGreen)
			green.Printf("✅ 确认入队: %s v%s [%s]\n", task.Service, task.Version, task.Environments[0])

		case status := <-rejectChan:
			// 处理拒绝：更新 /status 为 "rejected"
			err := a.apiClient.UpdateStatus(status)
			if err != nil {
				logrus.Errorf("❌ 拒绝状态更新失败: %v", err)
			} else {
				red := color.New(color.FgRed)
				red.Printf("❌ 已拒绝: %s v%s [%s]\n", status.Service, status.Version, status.Environment)
			}
		}
	}
}

// validateAndStoreTask 任务校验 + Redis存储 + 去重
func (a *Agent) validateAndStoreTask(task models.DeployRequest, env string) error {
	// 步骤1：环境映射
	namespace, ok := a.envMapper.GetNamespace(env)
	if !ok {
		return fmt.Errorf("❌ 环境 [%s] 无命名空间配置", env)
	}
	task.Environments = []string{namespace}

	// 步骤2：Redis去重校验
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

	green := color.New(color.FgGreen)
	green.Printf("✅ 校验通过 & 存储成功: %s v%s [%s -> %s]\n", task.Service, task.Version, env, namespace)
	return nil
}

// Stop 优雅停止Agent
func (a *Agent) Stop() {
	blue := color.New(color.FgBlue)
	blue.Println("🛑 停止Agent...")
	
	a.taskQ.Stop()
	a.botMgr.Stop()
	
	time.Sleep(2 * time.Second)
	green := color.New(color.FgGreen)
	green.Println("✅ Agent关闭完成")
}