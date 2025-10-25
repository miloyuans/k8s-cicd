// agent.go
package agent

import (
	"strings"
	"sync"
	"time"

	"k8s-cicd/agent/api"
	"k8s-cicd/agent/client"
	"k8s-cicd/agent/config"
	"k8s-cicd/agent/kubernetes"
	"k8s-cicd/agent/models"
	"k8s-cicd/agent/task"
	"k8s-cicd/agent/telegram"

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
	// 步骤6：启动Telegram轮询
	botMgr.StartPolling()
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
	// 步骤2：推送数据
	err = a.apiClient.PushData(req)
	if err != nil {
		logrus.WithFields(logrus.Fields{
			"time":   time.Now().Format("2006-01-02 15:04:05"),
			"method": "performPushDiscovery",
			"took":   time.Since(startTime),
		}).Errorf(color.RedString("推送数据失败: %v", err))
		return
	}
	logrus.WithFields(logrus.Fields{
		"time":   time.Now().Format("2006-01-02 15:04:05"),
		"method": "performPushDiscovery",
		"took":   time.Since(startTime),
	}).Infof(color.GreenString("服务发现推送成功"))
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
	// 步骤1：构建查询请求
	req := models.QueryRequest{
		Environment: a.config.User.Default, // 假设默认环境，或根据配置调整
		Service:     "",                    // 查询所有服务
	}
	// 步骤2：查询任务
	tasks, err := a.apiClient.QueryTasks(req)
	if err != nil {
		logrus.WithFields(logrus.Fields{
			"time":   time.Now().Format("2006-01-02 15:04:05"),
			"method": "performQueryTasks",
			"took":   time.Since(startTime),
		}).Errorf(color.RedString("查询任务失败: %v", err))
		return
	}
	// 步骤3：处理查询到的任务
	a.processQueryTasks(tasks)
}

// processQueryTasks 处理查询到的任务
func (a *Agent) processQueryTasks(tasks []models.DeployRequest) {
	startTime := time.Now()
	logrus.WithFields(logrus.Fields{
		"time":   time.Now().Format("2006-01-02 15:04:05"),
		"method": "processQueryTasks",
		"took":   time.Since(startTime),
		"data": logrus.Fields{
			"task_count": len(tasks),
		},
	}).Infof(color.GreenString("开始处理 %d 个查询任务", len(tasks)))

	var wg sync.WaitGroup
	for _, task := range tasks {
		wg.Add(1)
		go func(t models.DeployRequest) {
			defer wg.Done()
			for _, env := range t.Environments { // 注意：这里Environments是[]string，但代码中常取[0]，假设单环境
				// 要求1: 弹窗前检查数据库中是否已有相同任务
				exists, err := a.mongo.CheckExistingTask(t.Service, t.Version, env)
				if err != nil {
					logrus.WithFields(logrus.Fields{
						"time":   time.Now().Format("2006-01-02 15:04:05"),
						"method": "processQueryTasks",
						"took":   time.Since(startTime),
						"data": logrus.Fields{
							"service": t.Service, "version": t.Version, "env": env,
						},
					}).Errorf(color.RedString("检查现有任务失败: %v", err))
					continue
				}
				if exists {
					logrus.WithFields(logrus.Fields{
						"time":   time.Now().Format("2006-01-02 15:04:05"),
						"method": "processQueryTasks",
						"took":   time.Since(startTime),
						"data": logrus.Fields{
							"service": t.Service, "version": t.Version, "env": env,
						},
					}).Warnf(color.YellowString("重复任务忽略，不发起弹窗: %s v%s [%s]", t.Service, t.Version, env))
					continue
				}

				// 要求4: 检查状态，确保只弹一次
				if t.ConfirmationStatus != "not_sent" {
					logrus.WithFields(logrus.Fields{
						"time":   time.Now().Format("2006-01-02 15:04:05"),
						"method": "processQueryTasks",
						"took":   time.Since(startTime),
						"data": logrus.Fields{
							"service": t.Service, "version": t.Version, "env": env, "status": t.ConfirmationStatus,
						},
					}).Infof(color.GreenString("弹窗已处理，跳过: %s v%s [%s]", t.Service, t.Version, env))
					continue
				}

				// 存储任务 (已有重复检查)
				err = a.validateAndStoreTask(t, env)
				if err != nil {
					continue
				}

				// 要求2: 如果需要弹窗，发送并设置初始状态"sent"
				if contains(a.config.Query.ConfirmEnvs, env) {
					confirmChan := make(chan models.DeployRequest)
					rejectChan := make(chan models.StatusRequest)
					err := a.botMgr.SendConfirmation(t.Service, env, t.Version, t.User, confirmChan, rejectChan)
					if err != nil {
						// 要求2: 发送失败，设置"failed"
						a.mongo.UpdateConfirmationStatus(t.Service, t.Version, env, t.User, "failed")
						logrus.WithFields(logrus.Fields{
							"time":   time.Now().Format("2006-01-02 15:04:05"),
							"method": "processQueryTasks",
							"took":   time.Since(startTime),
							"data": logrus.Fields{
								"service": t.Service, "version": t.Version, "env": env,
							},
						}).Errorf(color.RedString("弹窗发送失败: %v", err))
						continue
					}
					// 发送成功，更新状态"sent"
					err = a.mongo.UpdateConfirmationStatus(t.Service, t.Version, env, t.User, "sent")
					if err != nil {
						logrus.WithFields(logrus.Fields{
							"time":   time.Now().Format("2006-01-02 15:04:05"),
							"method": "processQueryTasks",
							"took":   time.Since(startTime),
						}).Errorf(color.RedString("更新确认状态失败: %v", err))
					}
					logrus.WithFields(logrus.Fields{
						"time":   time.Now().Format("2006-01-02 15:04:05"),
						"method": "processQueryTasks",
						"took":   time.Since(startTime),
						"data": logrus.Fields{
							"service": t.Service, "version": t.Version, "env": env,
						},
					}).Infof(color.GreenString("弹窗发送成功并设置初始状态'sent': %s v%s [%s]", t.Service, t.Version, env))

					go a.handleConfirmationChannels(confirmChan, rejectChan) // 异步处理
				} else {
					// 无需弹窗，直接入队
					taskID := generateTaskID(t)
					a.taskQ.Enqueue(&models.Task{
						DeployRequest: t,
						ID:            taskID,
						Retries:       0,
					})
					logrus.WithFields(logrus.Fields{
						"time":   time.Now().Format("2006-01-02 15:04:05"),
						"method": "processQueryTasks",
						"took":   time.Since(startTime),
						"data": logrus.Fields{
							"task_id": taskID,
						},
					}).Infof(color.GreenString("无需弹窗，任务直接入队: %s v%s [%s]", t.Service, t.Version, env))
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
		// 要求3: 确认，更新状态"confirmed"，入队
		err := a.mongo.UpdateConfirmationStatus(task.Service, task.Version, task.Environments[0], task.User, "confirmed")
		if err != nil {
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
		// 要求3: 拒绝，更新状态"rejected"，推送no_action，删除任务
		err := a.mongo.UpdateConfirmationStatus(status.Service, status.Version, status.Environment, status.User, "rejected")
		if err != nil {
			logrus.WithFields(logrus.Fields{
				"time":   time.Now().Format("2006-01-02 15:04:05"),
				"method": "handleConfirmationChannels",
				"took":   time.Since(startTime),
			}).Errorf(color.RedString("更新拒绝状态失败: %v", err))
		}
		err = a.apiClient.UpdateStatus(models.StatusRequest{
			Service:     status.Service,
			Version:     status.Version,
			Environment: status.Environment,
			User:        status.User,
			Status:      "no_action",
		})
		if err != nil {
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
		err = a.mongo.DeleteTask(status.Service, status.Version, status.Environment, status.User)
		if err != nil {
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
		return nil
	}
	task.Environments = []string{namespace}
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
		return nil
	}
	if isDuplicate {
		logrus.WithFields(logrus.Fields{
			"time":   time.Now().Format("2006-01-02 15:04:05"),
			"method": "validateAndStoreTask",
			"took":   time.Since(startTime),
			"data": logrus.Fields{
				"task": task,
			},
		}).Warnf(color.GreenString("任务重复，忽略: %s v%s [%s]", task.Service, task.Version, env))
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
		return nil
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

// generateTaskID 生成任务ID（假设实现）
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