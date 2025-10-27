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

	"github.com/fatih/color"
	"github.com/sirupsen/logrus"
	"go.mongodb.org/mongo-driver/bson"
)

// 时间格式常量
const timeFormat = "2006-01-02 15:04:05"

// Agent 主代理结构，协调各组件
type Agent struct {
	config     *config.Config       // 配置
	mongo      *client.MongoClient  // MongoDB客户端
	k8s        *kubernetes.K8sClient // Kubernetes客户端
	taskQ      *task.TaskQueue      // 任务队列
	botMgr     *telegram.BotManager  // Telegram机器人管理器
	apiClient  *api.APIClient       // API客户端
	envMapper  *EnvMapper           // 环境映射器
	ctx        context.Context      // 上下文 for 优雅停止
	cancel     context.CancelFunc   // 取消函数
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
		"time":   time.Now().Format(timeFormat),
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
			"time":   time.Now().Format(timeFormat),
			"method": "GetNamespace",
			"took":   time.Since(startTime),
		}).Errorf(color.RedString("未配置环境 [%s] 的命名空间", env))
		return "", false
	}
	logrus.WithFields(logrus.Fields{
		"time":   time.Now().Format(timeFormat),
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
	ctx, cancel := context.WithCancel(context.Background())
	agent := &Agent{
		config:    cfg,
		mongo:     mongo,
		k8s:       k8s,
		taskQ:     taskQ,
		botMgr:    botMgr,
		apiClient: apiClient,
		envMapper: envMapper,
		ctx:       ctx,
		cancel:    cancel,
	}
	// 步骤6：启动Telegram轮询
	botMgr.StartPolling()
	logrus.WithFields(logrus.Fields{
		"time":   time.Now().Format(timeFormat),
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
		"time":   time.Now().Format(timeFormat),
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
	go a.periodicRecoverPendingOrFailedPopupTasks()
	// 步骤6：处理确认通道
	go a.handleConfirmationChannels()
}

// periodicPushDiscovery 周期推送发现数据
func (a *Agent) periodicPushDiscovery() {
	ticker := time.NewTicker(a.config.API.PushInterval)
	defer ticker.Stop()
	for {
		select {
		case <-a.ctx.Done():
			logrus.WithFields(logrus.Fields{
				"time":   time.Now().Format(timeFormat),
				"method": "periodicPushDiscovery",
			}).Info("周期推送停止")
			return
		case <-ticker.C:
			pushReq, err := a.k8s.BuildPushRequest(a.config)
			if err != nil {
				logrus.WithFields(logrus.Fields{
					"time":   time.Now().Format(timeFormat),
					"method": "periodicPushDiscovery",
				}).Errorf(color.RedString("构建推送请求失败: %v", err))
				continue
			}
			if err := a.apiClient.PushData(pushReq); err != nil {
				logrus.WithFields(logrus.Fields{
					"time":   time.Now().Format(timeFormat),
					"method": "periodicPushDiscovery",
				}).Errorf(color.RedString("推送数据失败: %v", err))
				continue
			}
			if err := a.mongo.StoreLastPushRequest(pushReq); err != nil {
				logrus.WithFields(logrus.Fields{
					"time":   time.Now().Format(timeFormat),
					"method": "periodicPushDiscovery",
				}).Errorf(color.RedString("存储推送数据失败: %v", err))
			}
		}
	}
}

// periodicQueryTasks 周期查询任务
func (a *Agent) periodicQueryTasks() {
	ticker := time.NewTicker(a.config.API.QueryInterval)
	defer ticker.Stop()
	for {
		select {
		case <-a.ctx.Done():
			logrus.WithFields(logrus.Fields{
				"time":   time.Now().Format(timeFormat),
				"method": "periodicQueryTasks",
			}).Info("周期查询任务停止")
			return
		case <-ticker.C:
			for env := range a.config.EnvMapping.Mappings {
				queryReq := models.QueryRequest{
					Environment: env,
					Service:     "all", // 假设查询所有服务
					User:        a.config.User.Default,
				}
				tasks, err := a.apiClient.QueryTasks(queryReq)
				if err != nil {
					logrus.WithFields(logrus.Fields{
						"time":   time.Now().Format(timeFormat),
						"method": "periodicQueryTasks",
					}).Errorf(color.RedString("查询任务失败 [%s]: %v", env, err))
					continue
				}
				for _, task := range tasks {
					if err := a.validateAndStoreTask(task, env); err != nil {
						logrus.WithFields(logrus.Fields{
							"time":   time.Now().Format(timeFormat),
							"method": "periodicQueryTasks",
						}).Errorf(color.RedString("校验并存储任务失败: %v", err))
						continue
					}
					if contains(a.config.Query.ConfirmEnvs, env) {
						if err := a.botMgr.SendPopup(task); err != nil {
							logrus.WithFields(logrus.Fields{
								"time":   time.Now().Format(timeFormat),
								"method": "periodicQueryTasks",
							}).Errorf(color.RedString("发送弹窗失败: %v", err))
							continue
						}
						if err := a.mongo.UpdateConfirmationStatus(task.Service, task.Version, env, task.User, "sent"); err != nil {
							logrus.WithFields(logrus.Fields{
								"time":   time.Now().Format(timeFormat),
								"method": "periodicQueryTasks",
							}).Errorf(color.RedString("更新确认状态失败: %v", err))
						}
					} else {
						taskID := generateTaskID(task)
						a.taskQ.Enqueue(&models.Task{DeployRequest: task, ID: taskID, Retries: 0})
					}
				}
			}
		}
	}
}

// periodicRecoverPendingOrFailedPopupTasks 周期恢复待处理或失败的弹窗任务
func (a *Agent) periodicRecoverPendingOrFailedPopupTasks() {
	ticker := time.NewTicker(time.Duration(a.config.Task.PopupRetryDelay) * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-a.ctx.Done():
			logrus.WithFields(logrus.Fields{
				"time":   time.Now().Format(timeFormat),
				"method": "periodicRecoverPendingOrFailedPopupTasks",
			}).Info("周期恢复弹窗任务停止")
			return
		case <-ticker.C:
			a.recoverPendingOrFailedPopupTasks()
		}
	}
}

// recoverPendingOrFailedPopupTasks 恢复待处理或失败的弹窗任务
func (a *Agent) recoverPendingOrFailedPopupTasks() {
	startTime := time.Now()
	logrus.WithFields(logrus.Fields{
		"time":   time.Now().Format(timeFormat),
		"method": "recoverPendingOrFailedPopupTasks",
		"took":   time.Since(startTime),
	}).Info("开始恢复待处理或失败的弹窗任务")

	for env := range a.config.EnvMapping.Mappings {
		tasks, err := a.mongo.GetPendingOrFailedPopupTasks(env, a.config.Task.PopupMaxRetries)
		if err != nil {
			logrus.WithFields(logrus.Fields{
				"time":   time.Now().Format(timeFormat),
				"method": "recoverPendingOrFailedPopupTasks",
				"took":   time.Since(startTime),
				"data": logrus.Fields{
					"environment": env,
				},
			}).Errorf(color.RedString("恢复失败: %v", err))
			continue
		}
		for _, task := range tasks {
			if err := a.botMgr.SendPopup(task); err != nil {
				logrus.WithFields(logrus.Fields{
					"time":   time.Now().Format(timeFormat),
					"method": "recoverPendingOrFailedPopupTasks",
					"took":   time.Since(startTime),
				}).Errorf(color.RedString("发送弹窗失败: %v", err))
				continue
			}
			if err := a.mongo.UpdateConfirmationStatus(task.Service, task.Version, env, task.User, "sent"); err != nil {
				logrus.WithFields(logrus.Fields{
					"time":   time.Now().Format(timeFormat),
					"method": "recoverPendingOrFailedPopupTasks",
					"took":   time.Since(startTime),
				}).Errorf(color.RedString("更新确认状态失败: %v", err))
			}
			if err := a.mongo.UpdatePopupRetries(task.Service, task.Version, env, task.User, task.PopupRetries+1); err != nil {
				logrus.WithFields(logrus.Fields{
					"time":   time.Now().Format(timeFormat),
					"method": "recoverPendingOrFailedPopupTasks",
					"took":   time.Since(startTime),
				}).Errorf(color.RedString("更新重试次数失败: %v", err))
			}
		}
	}
}

// handleConfirmationChannels 处理确认通道
func (a *Agent) handleConfirmationChannels() {
	for {
		select {
		case <-a.ctx.Done():
			logrus.WithFields(logrus.Fields{
				"time":   time.Now().Format(timeFormat),
				"method": "handleConfirmationChannels",
			}).Info("确认通道处理停止")
			return
		case update := <-a.botMgr.updateChan:
			startTime := time.Now()
			// 处理回调查询
			if callbackQuery, ok := update["callback_query"].(map[string]interface{}); ok {
				data, _ := callbackQuery["data"].(string)
				message, _ := callbackQuery["message"].(map[string]interface{})
				chat, _ := message["chat"].(map[string]interface{})
				chatID, _ := chat["id"].(float64)
				messageID, _ := message["message_id"].(float64)
				user, _ := callbackQuery["from"].(map[string]interface{})
				userID, _ := user["id"].(float64)

				parts := strings.Split(data, ":")
				if len(parts) != 4 {
					logrus.WithFields(logrus.Fields{
						"time":   time.Now().Format(timeFormat),
						"method": "handleConfirmationChannels",
						"took":   time.Since(startTime),
					}).Errorf(color.RedString("无效的回调数据: %s", data))
					continue
				}
				action, service, version, env, user := parts[0], parts[1], parts[2], parts[3]

				// 检查用户权限
				bot, err := a.botMgr.getBotForService(service)
				if err != nil {
					logrus.WithFields(logrus.Fields{
						"time":   time.Now().Format(timeFormat),
						"method": "handleConfirmationChannels",
						"took":   time.Since(startTime),
					}).Errorf(color.RedString("获取机器人失败: %v", err))
					continue
				}
				userIDStr := fmt.Sprintf("%d", int(userID))
				if !contains(bot.AllowedUsers, userIDStr) && !contains(a.botMgr.globalAllowedUsers, userIDStr) {
					logrus.WithFields(logrus.Fields{
						"time":   time.Now().Format(timeFormat),
						"method": "handleConfirmationChannels",
						"took":   time.Since(startTime),
					}).Errorf(color.RedString("用户 %s 无权限", userIDStr))
					continue
				}

				// 处理确认或拒绝
				if action == "confirm" {
					task := models.DeployRequest{
						Service:      service,
						Version:      version,
						Environments: []string{env},
						User:         user,
						CreatedAt:    time.Now(),
					}
					if err := a.validateAndStoreTask(task, env); err != nil {
						logrus.WithFields(logrus.Fields{
							"time":   time.Now().Format(timeFormat),
							"method": "handleConfirmationChannels",
							"took":   time.Since(startTime),
						}).Errorf(color.RedString("存储任务失败: %v", err))
						continue
					}
					taskID := generateTaskID(task)
					a.taskQ.Enqueue(&models.Task{DeployRequest: task, ID: taskID, Retries: 0})
					if err := a.mongo.UpdateConfirmationStatus(service, version, env, user, "confirmed"); err != nil {
						logrus.WithFields(logrus.Fields{
							"time":   time.Now().Format(timeFormat),
							"method": "handleConfirmationChannels",
							"took":   time.Since(startTime),
						}).Errorf(color.RedString("更新确认状态失败: %v", err))
					}
					logrus.WithFields(logrus.Fields{
						"time":   time.Now().Format(timeFormat),
						"method": "handleConfirmationChannels",
						"took":   time.Since(startTime),
						"data": logrus.Fields{
							"service": service,
							"version": version,
							"env":     env,
							"user":    user,
							"status":  "confirmed",
						},
					}).Infof(color.GreenString("用户确认部署，任务已入队: %s v%s [%s]", service, version, env))
				} else if action == "reject" {
					if err := a.mongo.UpdateConfirmationStatus(service, version, env, user, "rejected"); err != nil {
						logrus.WithFields(logrus.Fields{
							"time":   time.Now().Format(timeFormat),
							"method": "handleConfirmationChannels",
							"took":   time.Since(startTime),
						}).Errorf(color.RedString("更新拒绝状态失败: %v", err))
					}
					if err := a.apiClient.UpdateStatus(models.StatusRequest{
						Service:     service,
						Version:     version,
						Environment: env,
						User:        user,
						Status:      "no_action",
					}); err != nil {
						logrus.WithFields(logrus.Fields{
							"time":   time.Now().Format(timeFormat),
							"method": "handleConfirmationChannels",
							"took":   time.Since(startTime),
						}).Errorf(color.RedString("推送no_action状态失败: %v", err))
					}
					if err := a.mongo.DeleteTask(service, version, env, user); err != nil {
						logrus.WithFields(logrus.Fields{
							"time":   time.Now().Format(timeFormat),
							"method": "handleConfirmationChannels",
							"took":   time.Since(startTime),
						}).Errorf(color.RedString("拒绝任务删除失败: %v", err))
					}
					logrus.WithFields(logrus.Fields{
						"time":   time.Now().Format(timeFormat),
						"method": "handleConfirmationChannels",
						"took":   time.Since(startTime),
						"data": logrus.Fields{
							"service": service,
							"version": version,
							"env":     env,
							"user":    user,
							"status":  "rejected",
						},
					}).Infof(color.GreenString("用户拒绝部署，推送no_action成功并更新状态'rejected': %s v%s [%s]", service, version, env))
				}
				// 删除弹窗消息
				if err := a.botMgr.DeleteMessage(bot, int(messageID)); err != nil {
					logrus.WithFields(logrus.Fields{
						"time":   time.Now().Format(timeFormat),
						"method": "handleConfirmationChannels",
						"took":   time.Since(startTime),
					}).Errorf(color.RedString("删除弹窗消息失败: %v", err))
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
			"time":   time.Now().Format(timeFormat),
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
			"time":   time.Now().Format(timeFormat),
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
			"time":   time.Now().Format(timeFormat),
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
			"time":   time.Now().Format(timeFormat),
			"method": "validateAndStoreTask",
			"took":   time.Since(startTime),
			"data": logrus.Fields{
				"task": task,
			},
		}).Errorf(color.RedString("存储任务失败: %v", err))
		return err
	}
	logrus.WithFields(logrus.Fields{
		"time":   time.Now().Format(timeFormat),
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
	if err := a.mongo.Close(); err != nil {
		logrus.WithFields(logrus.Fields{
			"time":   time.Now().Format(timeFormat),
			"method": "Stop",
			"took":   time.Since(startTime),
		}).Errorf(color.RedString("关闭MongoDB失败: %v", err))
	}
	// 步骤4：取消上下文
	a.cancel()
	// 步骤5：等待完成
	time.Sleep(2 * time.Second)
	logrus.WithFields(logrus.Fields{
		"time":   time.Now().Format(timeFormat),
		"method": "Stop",
		"took":   time.Since(startTime),
	}).Infof(color.GreenString("Agent关闭完成"))
}

// generateTaskID 生成任务ID
func generateTaskID(task models.DeployRequest) string {
	return fmt.Sprintf("%s-%s-%s", task.Service, task.Version, task.Environments[0]) // 优化唯一性
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