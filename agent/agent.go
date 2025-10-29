// agent.go
package agent

import (
	"context"
	"fmt"
	"sync"
	"time"

	"k8s-cicd/agent/api"
	"k8s-cicd/agent/client"
	"k8s-cicd/agent/config"
	"k8s-cicd/agent/kubernetes"
	"k8s-cicd/agent/models"
	"k8s-cicd/agent/task"
	"k8s-cicd/agent/telegram"

	"go.mongodb.org/mongo-driver/bson"
	"github.com/fatih/color"
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
	}).Infof(color.GreenString("环境映射器创建成功"))
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
		}).Errorf(color.RedString("未配置环境 [%s] 的命名空间", env))
		return "", false
	}
	logrus.WithFields(logrus.Fields{
		"time":   time.Now().Format("2006-01-02 15:04:05"),
		"method": "GetNamespace",
		"took":   time.Since(startTime),
	}).Infof(color.GreenString("环境 [%s] 映射到命名空间 [%s]", env, ns))
	return ns, true
}

// NewAgent 创建Agent实例
func NewAgent(cfg *config.Config, mongo *client.MongoClient, k8s *kubernetes.K8sClient) *Agent {
	startTime := time.Now()
	// 步骤1：创建Telegram机器人管理器
	botMgr := telegram.NewBotManager(cfg.Telegram.Bots)
	botMgr.SetGlobalAllowedUsers(cfg.Telegram.AllowedUsers)
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

	logrus.WithFields(logrus.Fields{
		"time":   time.Now().Format("2006-01-02 15:04:05"),
		"method": "NewAgent",
		"took":   time.Since(startTime),
	}).Infof(color.GreenString("Agent创建成功"))
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
	}).Infof(color.GreenString("Agent启动成功，API=%s, 用户=%s, 推送间隔=%v, 查询间隔=%v, 弹窗环境=%v, 允许用户=%v",
		a.config.API.BaseURL, a.config.User.Default, a.config.API.PushInterval, a.config.API.QueryInterval,
		a.config.Query.ConfirmEnvs, a.config.Telegram.AllowedUsers))
	// 步骤2：启动任务队列Worker
	go a.taskQ.StartWorkers(a.config, a.mongo, a.k8s, a.botMgr, a.apiClient)
	// 步骤3：启动周期性推送
	go a.periodicPushDiscovery()
	// 步骤4：启动周期性查询
	go a.periodicQueryTasks()
	// 步骤5：恢复待处理或失败的弹窗任务
	go a.recoverPendingOrFailedPopupTasks()

		// 步骤6：启动 Telegram 轮询和回调处理
	a.botMgr.StartPolling()
	confirmChan := make(chan models.DeployRequest, 100)
	rejectChan := make(chan models.StatusRequest, 100)
	go a.botMgr.PollUpdates(confirmChan, rejectChan, a.mongo)
	go a.handleConfirmationChannels(confirmChan, rejectChan)
}

// recoverPendingOrFailedPopupTasks 恢复待处理或失败的弹窗任务
func (a *Agent) recoverPendingOrFailedPopupTasks() {
	startTime := time.Now()
	logrus.WithFields(logrus.Fields{
		"time":   time.Now().Format("2006-01-02 15:04:05"),
		"method": "recoverPendingOrFailedPopupTasks",
		"took":   time.Since(startTime),
	}).Info("开始恢复待处理或失败的弹窗任务")

	for env := range a.config.EnvMapping.Mappings {
		tasks, err := a.mongo.GetPendingOrFailedPopupTasks(env, a.config.Task.PopupMaxRetries)
		if err != nil {
			logrus.WithFields(logrus.Fields{
				"time":   time.Now().Format("2006-01-02 15:04:05"),
				"method": "recoverPendingOrFailedPopupTasks",
				"took":   time.Since(startTime),
				"data": logrus.Fields{
					"environment": env,
				},
			}).Errorf(color.RedString("恢复弹窗任务失败: %v", err))
			continue
		}
		if len(tasks) > 0 {
			logrus.WithFields(logrus.Fields{
				"time":   time.Now().Format("2006-01-02 15:04:05"),
				"method": "recoverPendingOrFailedPopupTasks",
				"took":   time.Since(startTime),
				"data": logrus.Fields{
					"task_count": len(tasks),
					"environment": env,
				},
			}).Infof(color.GreenString("恢复 %d 个待处理或失败的弹窗任务", len(tasks)))
			a.processQueryTasks(tasks)
		}
	}
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
	}).Infof(color.GreenString("周期性推送任务停止"))
}

// performPushDiscovery 执行单次服务发现和推送
func (a *Agent) performPushDiscovery() {
	startTime := time.Now()
	// 步骤1：构建推送请求
	req, err := a.k8s.BuildPushRequest(a.config)
	if err != nil {
		logrus.WithFields(logrus.Fields{
			"time":   time.Now().Format("2006-01-02 15:04:05"),
			"method": "performPushDiscovery",
			"took":   time.Since(startTime),
		}).Errorf(color.RedString("构建推送请求失败: %v", err))
		return
	}

	// 步骤2：获取上次推送的数据
	lastReq, err := a.mongo.GetLastPushRequest()
	if err == nil && equalPushRequests(req, lastReq) {
		logrus.WithFields(logrus.Fields{
			"time":   time.Now().Format("2006-01-02 15:04:05"),
			"method": "performPushDiscovery",
			"took":   time.Since(startTime),
		}).Infof(color.GreenString("数据无更新，忽略推送"))
		time.Sleep(5 * time.Minute) // 静默5分钟
		return
	}

	// 步骤3：推送数据
	err = a.apiClient.PushData(req)
	if err != nil {
		logrus.WithFields(logrus.Fields{
			"time":   time.Now().Format("2006-01-02 15:04:05"),
			"method": "performPushDiscovery",
			"took":   time.Since(startTime),
		}).Errorf(color.RedString("推送数据失败: %v", err))
		return
	}

	// 步骤4：存储本次推送数据
	err = a.mongo.StoreLastPushRequest(req)
	if err != nil {
		logrus.WithFields(logrus.Fields{
			"time":   time.Now().Format("2006-01-02 15:04:05"),
			"method": "performPushDiscovery",
			"took":   time.Since(startTime),
		}).Errorf(color.RedString("存储推送数据失败: %v", err))
		return
	}

	logrus.WithFields(logrus.Fields{
		"time":   time.Now().Format("2006-01-02 15:04:05"),
		"method": "performPushDiscovery",
		"took":   time.Since(startTime),
	}).Infof(color.GreenString("服务发现推送成功"))
}

// equalPushRequests 对比两个PushRequest是否相同
func equalPushRequests(a, b models.PushRequest) bool {
	if len(a.Services) != len(b.Services) || len(a.Environments) != len(b.Environments) {
		return false
	}
	aServices := make(map[string]struct{})
	for _, s := range a.Services {
		aServices[s] = struct{}{}
	}
	for _, s := range b.Services {
		if _, ok := aServices[s]; !ok {
			return false
		}
	}
	aEnvs := make(map[string]struct{})
	for _, e := range a.Environments {
		aEnvs[e] = struct{}{}
	}
	for _, e := range b.Environments {
		if _, ok := aEnvs[e]; !ok {
			return false
		}
	}
	return true
}

// periodicQueryTasks 周期性查询任务
func (a *Agent) periodicQueryTasks() {
	startTime := time.Now()
	// 步骤1：立即执行一次查询
	a.performQueryTasks()
	// 步骤2：设置定时器进行周期性查询
	ticker := time.NewTicker(a.config.API.QueryInterval)
	defer ticker.Stop()
	for range ticker.C {
		a.performQueryTasks()
	}
	logrus.WithFields(logrus.Fields{
		"time":   time.Now().Format("2006-01-02 15:04:05"),
		"method": "periodicQueryTasks",
		"took":   time.Since(startTime),
	}).Infof(color.GreenString("周期性查询任务停止"))
}

// performQueryTasks 执行单次任务查询
func (a *Agent) performQueryTasks() {
	startTime := time.Now()
	// 获取服务列表
	pushReq, err := a.k8s.BuildPushRequest(a.config)
	if err != nil {
		logrus.WithFields(logrus.Fields{
			"time":   time.Now().Format("2006-01-02 15:04:05"),
			"method": "performQueryTasks",
			"took":   time.Since(startTime),
		}).Errorf(color.RedString("获取服务列表失败: %v", err))
		return
	}

	var allTasks []models.DeployRequest
	var mu sync.Mutex
	var wg sync.WaitGroup

	// 对于每个环境和每个服务，并发查询
	for env := range a.config.EnvMapping.Mappings {
		for _, service := range pushReq.Services {
			wg.Add(1)
			go func(e string, s string) {
				defer wg.Done()
				req := models.QueryRequest{
					Environment: e,
					Service:     s,
				}
				tasks, err := a.apiClient.QueryTasks(req)
				if err != nil {
					logrus.WithFields(logrus.Fields{
						"time":   time.Now().Format("2006-01-02 15:04:05"),
						"method": "performQueryTasks",
						"took":   time.Since(startTime),
						"data": logrus.Fields{
							"environment": e,
							"service":     s,
						},
					}).Errorf(color.RedString("查询任务失败: %v", err))
					return
				}
				if len(tasks) == 0 {
					logrus.WithFields(logrus.Fields{
						"time":   time.Now().Format("2006-01-02 15:04:05"),
						"method": "performQueryTasks",
						"took":   time.Since(startTime),
						"data": logrus.Fields{
							"environment": e,
							"service":     s,
						},
					}).Infof(color.GreenString("未查询到pending任务: service=%s, env=%s", s, e))
					return
				}
				mu.Lock()
				allTasks = append(allTasks, tasks...)
				mu.Unlock()
			}(env, service)
		}
	}
	wg.Wait()

	// 处理所有任务
	if len(allTasks) > 0 {
		a.processQueryTasks(allTasks)
	}
	logrus.WithFields(logrus.Fields{
		"time":   time.Now().Format("2006-01-02 15:04:05"),
		"method": "performQueryTasks",
		"took":   time.Since(startTime),
		"data": logrus.Fields{
			"task_count": len(allTasks),
		},
	}).Infof(color.GreenString("查询到 %d 个任务", len(allTasks)))
}

// processQueryTasks 处理查询到的任务
func (a *Agent) processQueryTasks(tasks []models.DeployRequest) {
	startTime := time.Now()
	logrus.WithFields(logrus.Fields{
		"time":   time.Now().Format("2006-01-02 15:04:05"),
		"method": "processQueryTasks",
		"took":   time.Since(startTime),
		"data": logrus.Fields{"task_count": len(tasks)},
	}).Infof(color.GreenString("开始处理 %d 个查询任务", len(tasks)))

	var wg sync.WaitGroup
	for _, task := range tasks {
		wg.Add(1)
		go func(t models.DeployRequest) {
			defer wg.Done()
			for _, env := range t.Environments {
				// 检查数据库中是否已有相同任务
				exists, err := a.mongo.CheckDuplicateTask(t)
				if err != nil {
					logrus.WithFields(logrus.Fields{
						"time":   time.Now().Format("2006-01-02 15:04:05"),
						"method": "processQueryTasks",
						"took":   time.Since(startTime),
						"data": logrus.Fields{"service": t.Service, "version": t.Version, "env": env},
					}).Errorf(color.RedString("检查现有任务失败: %v", err))
					continue
				}
				var confirmationStatus string
				var retries int
				var isNewTask bool = false

				if exists {
					confirmationStatus, err = a.mongo.GetConfirmationStatus(t.Service, t.Version, env, t.User)
					if err != nil {
						logrus.WithFields(logrus.Fields{
							"time":   time.Now().Format("2006-01-02 15:04:05"),
							"method": "processQueryTasks",
							"took":   time.Since(startTime),
							"data": logrus.Fields{"service": t.Service, "version": t.Version, "env": env},
						}).Errorf(color.RedString("获取确认状态失败: %v", err))
						continue
					}
				} else {
					// 新任务：设置命名空间并存储
					namespace, ok := a.envMapper.GetNamespace(env)
					if !ok {
						logrus.WithFields(logrus.Fields{
							"time":   time.Now().Format("2006-01-02 15:04:05"),
							"method": "processQueryTasks",
							"took":   time.Since(startTime),
							"data": logrus.Fields{"service": t.Service, "version": t.Version, "env": env},
						}).Errorf(color.RedString("无法获取环境 [%s] 的命名空间", env))
						continue
					}
					t.Namespace = namespace
					t.ConfirmationStatus = "pending"
					t.PopupRetries = 0
					isNewTask = true

					err = a.validateAndStoreTask(t, env)
					if err != nil {
						logrus.WithFields(logrus.Fields{
							"time":   time.Now().Format("2006-01-02 15:04:05"),
							"method": "processQueryTasks",
							"took":   time.Since(startTime),
							"data": logrus.Fields{"service": t.Service, "version": t.Version, "env": env},
						}).Errorf(color.RedString("存储任务失败: %v", err))
						continue
					}
					confirmationStatus = "pending"
					retries = 0
				}

				// 修复：增强弹窗条件日志
				logrus.WithFields(logrus.Fields{
					"time":   time.Now().Format("2006-01-02 15:04:05"),
					"method": "processQueryTasks",
					"data": logrus.Fields{"env": env, "ConfirmEnvs": a.config.Query.ConfirmEnvs, "status": confirmationStatus, "retries": retries},
				}).Infof("弹窗检查: 环境=%s, ConfirmEnvs=%v, 状态=%s, 重试=%d", env, a.config.Query.ConfirmEnvs, confirmationStatus, retries)

				// 如果需要弹窗
				if contains(a.config.Query.ConfirmEnvs, env) {
					if confirmationStatus == "failed" || confirmationStatus == "pending" {
						if isNewTask {
							retries = 0
							logrus.Infof(color.GreenString("新任务，跳过数据库查询，直接使用 retries=0: %s v%s [%s]", t.Service, t.Version, env))
						} else {
							// 旧任务：从数据库读取 retries（带重试）
							var taskInDB models.DeployRequest
							collection := a.mongo.GetClient().Database("cicd").Collection(fmt.Sprintf("tasks_%s", env))
							const maxQueryRetries = 3
							const queryRetryDelay = 500 * time.Millisecond
							var found bool
							for attempt := 1; attempt <= maxQueryRetries; attempt++ {
								err := collection.FindOne(context.Background(), bson.M{
									"service":     t.Service,
									"version":     t.Version,
									"environment": env,
									"user":        t.User,
								}).Decode(&taskInDB)
								if err == nil {
									retries = taskInDB.PopupRetries
									found = true
									break
								}
								if err.Error() == "mongo: no documents in result" && attempt < maxQueryRetries {
									logrus.Warnf(color.YellowString("获取任务重试次数失败，尝试 %d/%d: %v", attempt, maxQueryRetries, err))
									time.Sleep(queryRetryDelay)
									continue
								}
								logrus.Errorf(color.RedString("获取任务重试次数失败: %v", err))
								break
							}
							if !found {
								retries = 0
							}
						}

						if retries >= a.config.Task.PopupMaxRetries {
							logrus.Warnf(color.YellowString("任务已达最大弹窗重试次数 (%d/%d)，跳过: %s v%s [%s]", retries, a.config.Task.PopupMaxRetries, t.Service, t.Version, env))
							if err := a.mongo.UpdateConfirmationStatus(t.Service, t.Version, env, t.User, "failed"); err != nil {
								logrus.Errorf(color.RedString("更新确认状态失败: %v", err))
							}
							continue
						}

						if confirmationStatus == "failed" {
							time.Sleep(time.Duration(a.config.Task.PopupRetryDelay*retries) * time.Second)
						}
					} else if confirmationStatus == "sent" || confirmationStatus == "confirmed" || confirmationStatus == "rejected" {
						logrus.Infof(color.GreenString("弹窗已处理，跳过: %s v%s [%s]", t.Service, t.Version, env))
						continue
					}

					// 发送弹窗
					confirmChan := make(chan models.DeployRequest)
					rejectChan := make(chan models.StatusRequest)

					msgID, err := a.botMgr.SendConfirmation(t.Service, env, t.User, t.Version, a.config.Telegram.AllowedUsers)
					if err != nil {
						retries++
						if updateErr := a.mongo.UpdatePopupRetries(t.Service, t.Version, env, t.User, retries); updateErr != nil {
							logrus.Errorf(color.RedString("更新弹窗重试次数失败: %v", updateErr))
						}
						if updateErr := a.mongo.UpdateConfirmationStatus(t.Service, t.Version, env, t.User, "failed"); updateErr != nil {
							logrus.Errorf(color.RedString("更新确认状态失败: %v", updateErr))
						}
						logrus.WithFields(logrus.Fields{
							"time":   time.Now().Format("2006-01-02 15:04:05"),
							"method": "processQueryTasks",
							"took":   time.Since(startTime),
							"data": logrus.Fields{"service": t.Service, "version": t.Version, "env": env, "retries": retries},
						}).Errorf(color.RedString("弹窗发送失败，重试次数: %d/%d: %v", retries, a.config.Task.PopupMaxRetries, err))
						continue
					}

					// 记录 message_id
					if storeErr := a.mongo.UpdatePopupMessageID(t.Service, t.Version, env, t.User, msgID); storeErr != nil {
						logrus.Warnf(color.YellowString("记录弹窗 message_id 失败: %v", storeErr))
					}

					// 更新状态为 sent
					if err := a.mongo.UpdateConfirmationStatus(t.Service, t.Version, env, t.User, "sent"); err != nil {
						logrus.Errorf(color.RedString("更新确认状态失败: %v", err))
					}

					logrus.WithFields(logrus.Fields{
						"time":   time.Now().Format("2006-01-02 15:04:05"),
						"method": "processQueryTasks",
						"took":   time.Since(startTime),
						"data": logrus.Fields{"service": t.Service, "version": t.Version, "env": env, "message_id": msgID},
					}).Infof(color.GreenString("弹窗发送成功并设置状态'sent': %s v%s [%s]", t.Service, t.Version, env))

					go a.handleConfirmationChannels(confirmChan, rejectChan)
				} else {
					// 无需弹窗，直接入队
					t.Namespace, _ = a.envMapper.GetNamespace(env)
					taskID := generateTaskID(t)
					a.taskQ.Enqueue(&models.Task{
						DeployRequest: t,
						ID:            taskID,
						Retries:       0,
					})
					logrus.Infof(color.GreenString("无需弹窗，任务直接入队: %s v%s [%s]", t.Service, t.Version, env))
				}
			}
		}(task)
	}
	wg.Wait()
	logrus.WithFields(logrus.Fields{
		"time":   time.Now().Format("2006-01-02 15:04:05"),
		"method": "processQueryTasks",
		"took":   time.Since(startTime),
	}).Infof(color.GreenString("查询任务处理完成"))
}

// handleConfirmationChannels 处理确认通道
func (a *Agent) handleConfirmationChannels(confirmChan <-chan models.DeployRequest, rejectChan <-chan models.StatusRequest) {
	startTime := time.Now()
	select {
	case task := <-confirmChan:
		// 设置命名空间
		namespace, ok := a.envMapper.GetNamespace(task.Environments[0])
		if !ok {
			logrus.WithFields(logrus.Fields{
				"time":   time.Now().Format("2006-01-02 15:04:05"),
				"method": "handleConfirmationChannels",
				"took":   time.Since(startTime),
				"data": logrus.Fields{
					"service": task.Service, "version": task.Version, "env": task.Environments[0],
				},
			}).Errorf(color.RedString("无法获取环境 [%s] 的命名空间", task.Environments[0]))
			return
		}
		task.Namespace = namespace
		// 确认，更新状态"confirmed"，入队
		if err := a.mongo.UpdateConfirmationStatus(task.Service, task.Version, task.Environments[0], task.User, "confirmed"); err != nil {
			logrus.WithFields(logrus.Fields{
				"time":   time.Now().Format("2006-01-02 15:04:05"),
				"method": "handleConfirmationChannels",
				"took":   time.Since(startTime),
			}).Errorf(color.RedString("更新确认状态失败: %v", err))
		}
		// 入队
		taskID := generateTaskID(task)
		a.taskQ.Enqueue(&models.Task{
			DeployRequest: task,
			ID:            taskID,
			Retries:       0,
		})
		logrus.WithFields(logrus.Fields{
			"time":   time.Now().Format("2006-01-02 15:04:05"),
			"method": "handleConfirmationChannels",
			"took":   time.Since(startTime),
			"data": logrus.Fields{
				"task_id": taskID, "service": task.Service, "version": task.Version, "env": task.Environments[0], "user": task.User, "status": "confirmed",
			},
		}).Infof(color.GreenString("用户确认部署，任务入队并更新状态'confirmed': %s v%s [%s]", task.Service, task.Version, task.Environments[0]))

	case status := <-rejectChan:
		// 拒绝，更新状态"rejected"，推送no_action，删除任务
		if err := a.mongo.UpdateConfirmationStatus(status.Service, status.Version, status.Environment, status.User, "rejected"); err != nil {
			logrus.WithFields(logrus.Fields{
				"time":   time.Now().Format("2006-01-02 15:04:05"),
				"method": "handleConfirmationChannels",
				"took":   time.Since(startTime),
			}).Errorf(color.RedString("更新拒绝状态失败: %v", err))
		}
		if err := a.apiClient.UpdateStatus(models.StatusRequest{
			Service:     status.Service,
			Version:     status.Version,
			Environment: status.Environment,
			User:        status.User,
			Status:      "no_action",
		}); err != nil {
			logrus.WithFields(logrus.Fields{
				"time":   time.Now().Format("2006-01-02 15:04:05"),
				"method": "handleConfirmationChannels",
				"took":   time.Since(startTime),
				"data": logrus.Fields{
					"service": status.Service, "version": status.Version, "env": status.Environment, "user": status.User, "status": "rejected",
				},
			}).Errorf(color.RedString("拒绝任务推送no_action失败: %v", err))
		} else {
			logrus.WithFields(logrus.Fields{
				"time":   time.Now().Format("2006-01-02 15:04:05"),
				"method": "handleConfirmationChannels",
				"took":   time.Since(startTime),
				"data": logrus.Fields{
					"service": status.Service, "version": status.Version, "env": status.Environment, "user": status.User, "status": "rejected",
				},
			}).Infof(color.GreenString("用户拒绝部署，推送no_action成功并更新状态'rejected': %s v%s [%s]", status.Service, status.Version, status.Environment))
		}
		if err := a.mongo.DeleteTask(status.Service, status.Version, status.Environment, status.User); err != nil {
			logrus.WithFields(logrus.Fields{
				"time":   time.Now().Format("2006-01-02 15:04:05"),
				"method": "handleConfirmationChannels",
				"took":   time.Since(startTime),
				"data": logrus.Fields{
					"service": status.Service, "version": status.Version, "env": status.Environment, "user": status.User,
				},
			}).Errorf(color.RedString("拒绝任务删除失败: %v", err))
		} else {
			logrus.WithFields(logrus.Fields{
				"time":   time.Now().Format("2006-01-02 15:04:05"),
				"method": "handleConfirmationChannels",
				"took":   time.Since(startTime),
				"data": logrus.Fields{
					"service": status.Service, "version": status.Version, "env": status.Environment, "user": status.User,
				},
			}).Infof(color.GreenString("拒绝任务删除成功: %s v%s [%s]", status.Service, status.Version, status.Environment))
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
			"data": logrus.Fields{
				"task": task,
			},
		}).Errorf(color.RedString("环境 [%s] 无命名空间配置", env))
		return fmt.Errorf("环境 [%s] 无命名空间配置", env)
	}
	task.Environments = []string{env}
	task.Namespace = namespace
	// 步骤2：检查任务重复
	isDuplicate, err := a.mongo.CheckDuplicateTask(task)
	if err != nil {
		logrus.WithFields(logrus.Fields{
			"time":   time.Now().Format("2006-01-02 15:04:05"),
			"method": "validateAndStoreTask",
			"took":   time.Since(startTime),
			"data": logrus.Fields{
				"task": task,
			},
		}).Errorf(color.RedString("检查任务重复失败: %v", err))
		return err
	}
	if isDuplicate {
		logrus.WithFields(logrus.Fields{
			"time":   time.Now().Format("2006-01-02 15:04:05"),
			"method": "validateAndStoreTask",
			"took":   time.Since(startTime),
			"data": logrus.Fields{
				"task": task,
			},
		}).Warnf(color.YellowString("任务重复，忽略: %s v%s [%s]", task.Service, task.Version, env))
		return nil
	}
	// 步骤3：存储任务到MongoDB
	err = a.mongo.StoreTaskWithDeduplication(task)
	if err != nil {
		logrus.WithFields(logrus.Fields{
			"time":   time.Now().Format("2006-01-02 15:04:05"),
			"method": "validateAndStoreTask",
			"took":   time.Since(startTime),
			"data": logrus.Fields{
				"task": task,
			},
		}).Errorf(color.RedString("存储任务失败: %v", err))
		return err
	}
	logrus.WithFields(logrus.Fields{
		"time":   time.Now().Format("2006-01-02 15:04:05"),
		"method": "validateAndStoreTask",
		"took":   time.Since(startTime),
		"data": logrus.Fields{
			"task": task,
		},
	}).Infof(color.GreenString("任务校验并存储成功: %s v%s [%s -> %s]", task.Service, task.Version, env, namespace))
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
	}).Infof(color.GreenString("Agent关闭完成"))
}

// generateTaskID 生成任务ID
func generateTaskID(task models.DeployRequest) string {
	return task.Service + "-" + task.Version // 简单实现，实际可使用UUID
}

// contains 检查切片是否包含元素
func contains(slice []string, item string) bool {
	for _, s := range slice {
		if s == item {
			return true
		}
	}
	return false
}