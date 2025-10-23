package agent

import (
	"fmt"
	"time"

	"k8s-cicd/agent/api"
	"k8s-cicd/agent/config"
	"k8s-cicd/agent/task"
	"k8s-cicd/agent/client"
	"k8s-cicd/agent/kubernetes"
	"k8s-cicd/agent/models"
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

func NewAgent(cfg *config.Config, redis *client.RedisClient, k8s *kubernetes.K8sClient) *Agent {
	botMgr := telegram.NewBotManager(cfg.Telegram.Bots)
	taskQ := task.NewTaskQueue(cfg.Task.QueueWorkers)
	apiClient := api.NewAPIClient(&cfg.API)
	envMapper := NewEnvMapper(cfg.EnvMapping.Mappings)
	
	return &Agent{
		config:    cfg,
		redis:     redis,
		k8s:       k8s,
		taskQ:     taskQ,
		botMgr:    botMgr,
		apiClient: apiClient,
		envMapper: envMapper,
	}
}

func (a *Agent) Start() {
	green := color.New(color.FgGreen).SprintFunc()
	logrus.Infof("%s Agent启动成功", green("🚀"))
	logrus.Infof("API Base URL: %s", a.config.API.BaseURL)
	logrus.Infof("默认用户: %s", a.config.User.Default)
	logrus.Infof("环境映射: %+v", a.config.EnvMapping.Mappings)
	logrus.Infof("部署等待超时: %v", a.config.Deploy.WaitTimeout)
	logrus.Infof("回滚等待超时: %v", a.config.Deploy.RollbackTimeout)
	logrus.Infof("API推送间隔: %v", a.config.API.PushInterval)

	// 周期性从K8s发现并推送 /push
	go a.periodicPushDiscovery()

	// 周期性 /query 轮询
	go a.periodicQueryTasks()

	// 启动任务队列worker
	go a.taskQ.StartWorkers(a.config, a.redis, a.k8s, a.botMgr, a.apiClient)
}

// periodicPushDiscovery 周期性K8s发现 + /push
func (a *Agent) periodicPushDiscovery() {
    // 立即执行一次
    a.performPushDiscovery()

    ticker := time.NewTicker(a.config.API.PushInterval)  // 使用配置间隔，默认30s
    defer ticker.Stop()

    for range ticker.C {
        a.performPushDiscovery()
    }
}

// performPushDiscovery 执行单次 /push
func (a *Agent) performPushDiscovery() {
    logrus.Info("🌐 开始K8s服务发现")
    
    fullReq, err := a.k8s.BuildPushRequest(a.config)
    if err != nil {
        logrus.Error("服务发现失败:", err)
        return
    }
    
    // 按服务拆分推送
    for _, service := range fullReq.Services {
        var serviceDeployments []models.DeployRequest
        for _, dep := range fullReq.Deployments {
            if dep.Service == service {
                serviceDeployments = append(serviceDeployments, dep)
            }
        }
        
        pushReq := models.PushRequest{
            Services:     []string{service},  // 只当前服务
            Environments: fullReq.Environments,  // 保持所有环境（去重后）
            Deployments:  serviceDeployments,
        }
        
        err = a.apiClient.PushData(pushReq)
        if err != nil {
            logrus.Errorf("推送 /push 失败 [%s]: %v", service, err)
            continue
        }
        logrus.Infof("✅ 推送 /push 成功 [%s]", service)
    }
}

// periodicQueryTasks 周期性 /query + 校验 + Redis存储
func (a *Agent) periodicQueryTasks() {
    ticker := time.NewTicker(time.Duration(a.config.Task.PollInterval) * time.Second)
    defer ticker.Stop()

    confirmChan := make(chan models.DeployRequest)
    rejectChan := make(chan models.StatusRequest)
    
    // 启动 Telegram updates polling（简化，使用 goroutine）
    go a.botMgr.PollUpdates(a.config.Telegram.AllowedUsers, confirmChan, rejectChan)  // 新增 PollUpdates 函数实现 getUpdates API 循环

    for range ticker.C {
        // ... 现有 /query 逻辑
        
        for _, task := range tasks {
            // 环境过滤：如果在 ConfirmEnvs 中，才弹窗
            needConfirm := false
            for _, confirmEnv := range a.config.Query.ConfirmEnvs {
                if task.Environments[0] == confirmEnv {
                    needConfirm = true
                    break
                }
            }
            
            if !needConfirm {
                // 不弹窗，直接校验存储入队列
                if err := a.validateAndStoreTask(task, env); err != nil {
                    continue
                }
                a.taskQ.Enqueue(models.Task{...})  // 现有
                continue
            }
            
            // 选择机器人并发送弹窗
            bot, _ := a.botMgr.getBotForService(task.Service)
            _, err := a.botMgr.SendConfirmation(bot, task.Service, env, task.User, task.Version, a.config.Telegram.AllowedUsers)
            if err != nil {
                logrus.Error("弹窗发送失败:", err)
                continue
            }
        }
    }
    
    // 处理渠道
    go func() {
        for {
            select {
            case confirmed := <-confirmChan:
                // 确认：校验存储入队列
                a.validateAndStoreTask(confirmed, confirmed.Environments[0])
                a.taskQ.Enqueue(models.Task{DeployRequest: confirmed, ...})
            case rejected := <-rejectChan:
                // 拒绝：丢弃并更新 /status
                a.apiClient.UpdateStatus(rejected)
            }
        }
    }()
}

// validateAndStoreTask 严格校验 + Redis存储
func (a *Agent) validateAndStoreTask(task models.DeployRequest, queryEnv string) error {
	logrus.Infof("🔍 校验任务: %s v%s [%s/%s/%s]", task.Service, task.Version, queryEnv, task.User, task.Status)
	
	// 校验1: 环境匹配
	if task.Environments[0] != queryEnv {
		return fmt.Errorf("❌ 环境不匹配: 查询[%s] != 任务[%s]", queryEnv, task.Environments[0])
	}
	
	// 校验2: 状态pending
	if task.Status != "pending" {
		return fmt.Errorf("❌ 状态非pending: %s", task.Status)
	}
	
	// 校验3: Redis去重
	isDuplicate, err := a.redis.CheckDuplicateTask(task)
	if err != nil {
		return err
	}
	if isDuplicate {
		return fmt.Errorf("❌ 任务重复，忽略: %s v%s", task.Service, task.Version)
	}
	
	// 环境映射
	namespace, ok := a.envMapper.GetNamespace(queryEnv)
	if !ok {
		return fmt.Errorf("❌ 环境 [%s] 无命名空间配置", queryEnv)
	}
	
	task.Environments = []string{namespace}
	
	// 存储到Redis
	err = a.redis.StoreTaskWithDeduplication(task)
	if err != nil {
		return fmt.Errorf("❌ Redis存储失败: %v", err)
	}
	
	green := color.New(color.FgGreen)
	green.Printf("✅ 校验通过 & 存储Redis: %s v%s [%s → %s]\n", task.Service, task.Version, queryEnv, namespace)
	
	return nil
}

// notifyStatus 部署完成后推送 /status
func (a *Agent) notifyStatus(task models.Task, status string) {
	statusReq := models.StatusRequest{
		Service:     task.Service,
		Version:     task.Version,
		Environment: task.Environments[0], // 命名空间
		User:        task.User,
		Status:      status,
	}
	
	err := a.apiClient.UpdateStatus(statusReq)
	if err != nil {
		logrus.Error(" /status 推送失败: ", err)
	}
}

func (a *Agent) Stop() {
	blue := color.New(color.FgBlue)
	blue.Println("停止Agent...")
	a.taskQ.Stop()
}