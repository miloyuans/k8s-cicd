// agent.go
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

// Agent 主代理结构，协调各组件
type Agent struct {
	config     *config.Config       // 配置
	mongo      *client.MongoClient  // MongoDB客户端
	k8s        *kubernetes.K8sClient // Kubernetes客户端
	taskQ      *task.TaskQueue      // 任务队列
	botMgr     *telegram.BotManager  // Telegram机器人管理器
	apiClient  *api.APIClient       // API客户端
	envMapper  *EnvMapper           // 环境映射器
}

// EnvMapper 环境到命名空间的映射器
type EnvMapper struct {
	mappings map[string]string // env -> namespace
}

// NewEnvMapper 创建环境映射器
func NewEnvMapper(mappings map[string]string) *EnvMapper {
	startTime := time.Now()
	// 步骤1：初始化映射器
	mapper := &EnvMapper{mappings: mappings}
	logrus.WithFields(logrus.Fields{
		"time":   time.Now().Format("2006-01-02 15:04:05"),
		"method": "NewEnvMapper",
		"took":   time.Since(startTime),
	}).Info("环境映射器创建成功")
	return mapper
}

// GetNamespace 根据环境获取命名空间
func (m *EnvMapper) GetNamespace(env string) (string, bool) {
	startTime := time.Now()
	// 步骤1：查找环境对应的命名空间
	ns, exists := m.mappings[env]
	if !exists {
		logrus.WithFields(logrus.Fields{
			"time":   time.Now().Format("2006-01-02 15:04:05"),
			"method": "GetNamespace",
			"took":   time.Since(startTime),
		}).Errorf("未配置环境 [%s] 的命名空间", env)
		return "", false
	}
	logrus.WithFields(logrus.Fields{
		"time":   time.Now().Format("2006-01-02 15:04:05"),
		"method": "GetNamespace",
		"took":   time.Since(startTime),
	}).Infof("环境 [%s] 映射到命名空间 [%s]", env, ns)
	return ns, true
}

// NewAgent 创建Agent实例
func NewAgent(cfg *config.Config, mongo *client.MongoClient, k8s *kubernetes.K8sClient) *Agent {
	startTime := time.Now()
	// 步骤1：创建Telegram机器人管理器
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
		mongo:     mongo,
		k8s:       k8s,
		taskQ:     taskQ,
		botMgr:    botMgr,
		apiClient: apiClient,
		envMapper: envMapper,
	}
	// 步骤6：启动Telegram轮询
	botMgr.StartPolling()
	logrus.WithFields(logrus.Fields{
		"time":   time.Now().Format("2006-01-02 15:04:05"),
		"method": "NewAgent",
		"took":   time.Since(startTime),
	}).Info("Agent创建成功")
	return agent
}

// Start 启动Agent
func (a *Agent) Start() {
	startTime := time.Now()
	// 步骤1：记录启动信息
	logrus.WithFields(logrus.Fields{
		"time":   time.Now().Format("2006-01-02 15:04:05"),
		"method": "Start",
		"took":   time.Since(startTime),
	}).Infof("Agent启动成功，API=%s, 用户=%s, 推送间隔=%v, 查询间隔=%v, 弹窗环境=%v, 允许用户=%v",
		a.config.API.BaseURL, a.config.User.Default, a.config.API.PushInterval, a.config.API.QueryInterval,
		a.config.Query.ConfirmEnvs, a.config.Telegram.AllowedUsers)
	// 步骤2：启动任务队列Worker
	go a.taskQ.StartWorkers(a.config, a.mongo, a.k8s, a.botMgr, a.apiClient)
	// 步骤3：启动周期性推送
	go a.periodicPushDiscovery()
	// 步骤4：启动周期性查询
	go a.periodicQueryTasks()
}

// periodicPushDiscovery 周期性K8s服务发现和推送
func (a *Agent) periodicPushDiscovery() {
	startTime := time.Now()
	// 步骤1：立即执行一次推送
	a.performPushDiscovery()
	// 步骤2：设置定时器进行周期性推送
	ticker := time.NewTicker(a.config.API.PushInterval)
	defer ticker.Stop()
	for range ticker.C {
		a.performPushDiscovery()
	}
	logrus.WithFields(logrus.Fields{
		"time":   time.Now().Format("2006-01-02 15:04:05"),
		"method": "periodicPushDiscovery",
		"took":   time.Since(startTime),
	}).Info("周期性推送任务停止")
}

// performPushDiscovery 执行单次服务发现和推送
func (a *Agent) performPushDiscovery() {
	startTime := time.Now()
	// 步骤1：记录开始日志
	logrus.WithFields(logrus.Fields{
		"time":   time.Now().Format("2006-01-02 15:04:05"),
		"method": "performPushDiscovery",
		"took":   time.Since(startTime),
	}).Info("开始K8s服务发现")
	// 步骤2：构建推送请求
	req, err := a.k8s.BuildPushRequest(a.config)
	if err != nil {
		logrus.WithFields(logrus.Fields{
			"time":   time.Now().Format("2006-01-02 15:04:05"),
			"method": "performPushDiscovery",
			"took":   time.Since(startTime),
		}).Errorf("服务发现失败: %v", err)
		return
	}
	// 步骤3：记录发现的服务和环境
	logrus.WithFields(logrus.Fields{
		"time":   time.Now().Format("2006-01-02 15:04:05"),
		"method": "performPushDiscovery",
		"took":   time.Since(startTime),
	}).Infof("发现 %d 个服务，%d 个环境", len(req.Services), len(req.Environments))
	// 步骤4：按服务逐个推送
	for _, service := range req.Services {
		var serviceDeployments []models.DeployRequest
		for _, dep := range req.Deployments {
			if dep.Service == service {
				serviceDeployments = append(serviceDeployments, dep)
			}
		}
		pushReq := models.PushRequest{
			Services:     []string{service},
			Environments: req.Environments,
			Deployments:  serviceDeployments,
		}
		err = a.apiClient.PushData(pushReq)
		if err != nil {
			logrus.WithFields(logrus.Fields{
				"time":   time.Now().Format("2006-01-02 15:04:05"),
				"method": "performPushDiscovery",
				"took":   time.Since(startTime),
			}).Errorf("推送失败 [%s]: %v", service, err)
			continue
		}
		logrus.WithFields(logrus.Fields{
			"time":   time.Now().Format("2006-01-02 15:04:05"),
			"method": "performPushDiscovery",
			"took":   time.Since(startTime),
		}).Infof("推送成功 [%s] -> %d 环境", service, len(req.Environments))
		time.Sleep(100 * time.Millisecond) // 避免请求过快
	}
}

// periodicQueryTasks 周期性查询任务和弹窗确认
func (a *Agent) periodicQueryTasks() {
	startTime := time.Now()
	// 步骤1：创建定时器
	ticker := time.NewTicker(a.config.API.QueryInterval)
	defer ticker.Stop()
	// 步骤2：创建确认和拒绝通道
	confirmChan := make(chan models.DeployRequest, 100)
	rejectChan := make(chan models.StatusRequest, 100)
	// 步骤3：启动Telegram回调处理
	go a.botMgr.PollUpdates(a.config.Telegram.AllowedUsers, confirmChan, rejectChan)
	// 步骤4：处理确认和拒绝
	go a.handleConfirmationChannels(confirmChan, rejectChan)
	// 步骤5：周期性查询
	for range ticker.C {
		logrus.WithFields(logrus.Fields{
			"time":   time.Now().Format("2006-01-02 15:04:05"),
			"method": "periodicQueryTasks",
			"took":   time.Since(startTime),
		}).Info("开始 /query 轮询")
		user := a.config.User.Default
		for env := range a.config.EnvMapping.Mappings {
			// 步骤6：构建查询请求
			queryReq := models.QueryRequest{Environment: env, User: user}
			tasks, err := a.apiClient.QueryTasks(queryReq)
			if err != nil {
				logrus.WithFields(logrus.Fields{
					"time":   time.Now().Format("2006-01-02 15:04:05"),
					"method": "periodicQueryTasks",
					"took":   time.Since(startTime),
				}).Errorf("/query 失败 [%s]: %v", env, err)
				continue
			}
			logrus.WithFields(logrus.Fields{
				"time":   time.Now().Format("2006-01-02 15:04:05"),
				"method": "periodicQueryTasks",
				"took":   time.Since(startTime),
			}).Infof("/query 结果 [%s]: %d 个任务", env, len(tasks))
			// 步骤7：处理任务
			for _, task := range tasks {
				a.processTask(task, env)
			}
		}
	}
}

// processTask 处理单个任务
func (a *Agent) processTask(task models.DeployRequest, queryEnv string) {
	startTime := time.Now()
	// 步骤1：校验环境
	if task.Environments[0] != queryEnv {
		logrus.WithFields(logrus.Fields{
			"time":   time.Now().Format("2006-01-02 15:04:05"),
			"method": "processTask",
			"took":   time.Since(startTime),
		}).Warnf("环境不匹配: 查询[%s] != 任务[%s]", queryEnv, task.Environments[0])
		return
	}
	// 步骤2：校验状态
	if task.Status != "pending" {
		logrus.WithFields(logrus.Fields{
			"time":   time.Now().Format("2006-01-02 15:04:05"),
			"method": "processTask",
			"took":   time.Since(startTime),
		}).Infof("任务状态非pending，跳过: %s", task.Status)
		return
	}
	// 步骤3：检查是否需要弹窗确认
	needConfirm := false
	for _, confirmEnv := range a.config.Query.ConfirmEnvs {
		if queryEnv == confirmEnv {
			needConfirm = true
			break
		}
	}
	if needConfirm {
		// 步骤4：发送弹窗确认
		err := a.botMgr.SendConfirmation(task.Service, queryEnv, task.User, task.Version, a.config.Telegram.AllowedUsers)
		if err != nil {
			logrus.WithFields(logrus.Fields{
				"time":   time.Now().Format("2006-01-02 15:04:05"),
				"method": "processTask",
				"took":   time.Since(startTime),
			}).Errorf("发送确认弹窗失败 [%s]: %v", task.Service, err)
			return
		}
		logrus.WithFields(logrus.Fields{
			"time":   time.Now().Format("2006-01-02 15:04:05"),
			"method": "processTask",
			"took":   time.Since(startTime),
		}).Infof("已发送确认弹窗: %s v%s [%s]", task.Service, task.Version, queryEnv)
	} else {
		// 步骤5：直接存储并入队
		if err := a.validateAndStoreTask(task, queryEnv); err != nil {
			logrus.WithFields(logrus.Fields{
				"time":   time.Now().Format("2006-01-02 15:04:05"),
				"method": "processTask",
				"took":   time.Since(startTime),
			}).Warn(err.Error())
			return
		}
		taskID := fmt.Sprintf("%s-%s-%d", task.Service, task.Version, time.Now().UnixNano())
		a.taskQ.Enqueue(models.Task{
			DeployRequest: task,
			ID:            taskID,
			CreatedAt:     time.Now(),
			Retries:       0,
		})
		logrus.WithFields(logrus.Fields{
			"time":   time.Now().Format("2006-01-02 15:04:05"),
			"method": "processTask",
			"took":   time.Since(startTime),
		}).Infof("任务直接入队: %s v%s [%s]", task.Service, task.Version, queryEnv)
	}
}

// handleConfirmationChannels 处理确认和拒绝通道
func (a *Agent) handleConfirmationChannels(confirmChan chan models.DeployRequest, rejectChan chan models.StatusRequest) {
	startTime := time.Now()
	for {
		select {
		case task := <-confirmChan:
			// 步骤1：处理确认任务
			if err := a.validateAndStoreTask(task, task.Environments[0]); err != nil {
				logrus.WithFields(logrus.Fields{
					"time":   time.Now().Format("2006-01-02 15:04:05"),
					"method": "handleConfirmationChannels",
					"took":   time.Since(startTime),
				}).Error(err.Error())
				continue
			}
			taskID := fmt.Sprintf("%s-%s-%d", task.Service, task.Version, time.Now().UnixNano())
			a.taskQ.Enqueue(models.Task{
				DeployRequest: task,
				ID:            taskID,
				CreatedAt:     time.Now(),
				Retries:       0,
			})
			logrus.WithFields(logrus.Fields{
				"time":   time.Now().Format("2006-01-02 15:04:05"),
				"method": "handleConfirmationChannels",
				"took":   time.Since(startTime),
			}).Infof("确认任务入队: %s v%s [%s]", task.Service, task.Version, task.Environments[0])
		case status := <-rejectChan:
			// 步骤2：处理拒绝任务
			err := a.apiClient.UpdateStatus(status)
			if err != nil {
				logrus.WithFields(logrus.Fields{
					"time":   time.Now().Format("2006-01-02 15:04:05"),
					"method": "handleConfirmationChannels",
					"took":   time.Since(startTime),
				}).Errorf("拒绝状态更新失败: %v", err)
			} else {
				logrus.WithFields(logrus.Fields{
					"time":   time.Now().Format("2006-01-02 15:04:05"),
					"method": "handleConfirmationChannels",
					"took":   time.Since(startTime),
				}).Infof("任务拒绝: %s v%s [%s]", status.Service, status.Version, status.Environment)
				// 步骤3：更新MongoDB状态
				err = a.mongo.UpdateTaskStatus(status.Service, status.Version, status.Environment, status.User, "rejected")
				if err != nil {
					logrus.WithFields(logrus.Fields{
						"time":   time.Now().Format("2006-01-02 15:04:05"),
						"method": "handleConfirmationChannels",
						"took":   time.Since(startTime),
					}).Errorf("MongoDB状态更新失败: %v", err)
				}
			}
		}
	}
}

// validateAndStoreTask 校验并存储任务
func (a *Agent) validateAndStoreTask(task models.DeployRequest, env string) error {
	startTime := time.Now()
	// 步骤1：获取命名空间
	namespace, ok := a.envMapper.GetNamespace(env)
	if !ok {
		logrus.WithFields(logrus.Fields{
			"time":   time.Now().Format("2006-01-02 15:04:05"),
			"method": "validateAndStoreTask",
			"took":   time.Since(startTime),
		}).Errorf("环境 [%s] 无命名空间配置", env)
		return fmt.Errorf("环境 [%s] 无命名空间配置", env)
	}
	task.Environments = []string{namespace}
	// 步骤2：检查任务重复
	isDuplicate, err := a.mongo.CheckDuplicateTask(task)
	if err != nil {
		logrus.WithFields(logrus.Fields{
			"time":   time.Now().Format("2006-01-02 15:04:05"),
			"method": "validateAndStoreTask",
			"took":   time.Since(startTime),
		}).Errorf("检查任务重复失败: %v", err)
		return fmt.Errorf("检查任务重复失败: %v", err)
	}
	if isDuplicate {
		logrus.WithFields(logrus.Fields{
			"time":   time.Now().Format("2006-01-02 15:04:05"),
			"method": "validateAndStoreTask",
			"took":   time.Since(startTime),
		}).Warnf("任务重复，忽略: %s v%s [%s]", task.Service, task.Version, env)
		return fmt.Errorf("任务重复，忽略: %s v%s", task.Service, task.Version)
	}
	// 步骤3：存储任务到MongoDB
	err = a.mongo.StoreTaskWithDeduplication(task)
	if err != nil {
		logrus.WithFields(logrus.Fields{
			"time":   time.Now().Format("2006-01-02 15:04:05"),
			"method": "validateAndStoreTask",
			"took":   time.Since(startTime),
		}).Errorf("存储任务失败: %v", err)
		return fmt.Errorf("存储任务失败: %v", err)
	}
	logrus.WithFields(logrus.Fields{
		"time":   time.Now().Format("2006-01-02 15:04:05"),
		"method": "validateAndStoreTask",
		"took":   time.Since(startTime),
	}).Infof("任务校验并存储成功: %s v%s [%s -> %s]", task.Service, task.Version, env, namespace)
	return nil
}

// Stop 优雅停止Agent
func (a *Agent) Stop() {
	startTime := time.Now()
	// 步骤1：停止任务队列
	a.taskQ.Stop()
	// 步骤2：停止Telegram轮询
	a.botMgr.Stop()
	// 步骤3：关闭MongoDB连接
	a.mongo.Close()
	// 步骤4：等待完成
	time.Sleep(2 * time.Second)
	logrus.WithFields(logrus.Fields{
		"time":   time.Now().Format("2006-01-02 15:04:05"),
		"method": "Stop",
		"took":   time.Since(startTime),
	}).Info("Agent关闭完成")
}