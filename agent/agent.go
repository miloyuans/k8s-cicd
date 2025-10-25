//agent.go
package agent

import (
	"context"
	"fmt"
	"reflect"
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
	"go.mongodb.org/mongo-driver/mongo/options"
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
		}).Errorf(color.RedString("服务发现失败: %v", err))
		return
	}

	// 步骤2：检查MongoDB中数据是否存在且是否相同
	ctx := context.Background()
	versionsColl := a.mongo.GetClient().Database("cicd").Collection("versions")
	currentData := make(map[string]string) // 服务 -> 版本
	for _, service := range req.Services {
		// 获取每个服务的当前版本
		for env := range a.config.EnvMapping.Mappings {
			namespace, ok := a.envMapper.GetNamespace(env)
			if !ok {
				continue
			}
			version := a.k8s.GetCurrentImage(namespace, service)
			if version != "unknown" {
				currentData[service] = version
			}
		}
	}
	dbServices, err := a.getServicesFromMongo(ctx)
	if err != nil {
		logrus.WithFields(logrus.Fields{
			"time":   time.Now().Format("2006-01-02 15:04:05"),
			"method": "performPushDiscovery",
			"took":   time.Since(startTime),
		}).Errorf(color.RedString("查询MongoDB版本数据失败: %v", err))
		return
	}
	dbData := make(map[string]string)
	for _, svc := range dbServices {
		var version struct {
			Service string `bson:"service"`
			Version string `bson:"version"`
		}
		err := versionsColl.FindOne(ctx, bson.M{"service": svc}).Decode(&version)
		if err == nil {
			dbData[version.Service] = version.Version
		}
	}

	// 步骤3：比较数据是否相同
	uniqueEnvs := make(map[string]string)
	for env, ns := range a.config.EnvMapping.Mappings {
		uniqueEnvs[env] = ns
	}
	if reflect.DeepEqual(req.Services, dbServices) && reflect.DeepEqual(req.Environments, a.config.EnvMapping.Mappings) {
		logrus.WithFields(logrus.Fields{
			"time":   time.Now().Format("2006-01-02 15:04:05"),
			"method": "performPushDiscovery",
			"took":   time.Since(startTime),
			"data": logrus.Fields{
				"services":      req.Services,
				"service_count": len(req.Services),
				"env_mappings":  uniqueEnvs,
				"env_count":     len(uniqueEnvs),
			},
		}).Infof(color.GreenString("数据无变化，无需推送 /push 接口"))
		return
	}

	// 步骤4：更新MongoDB版本数据
	for service, version := range currentData {
		_, err = versionsColl.UpdateOne(ctx,
			bson.M{"service": service},
			bson.M{
				"$set": bson.M{
					"service":    service,
					"version":    version,
					"updated_at": time.Now(),
				},
			},
			options.Update().SetUpsert(true),
		)
		if err != nil {
			logrus.WithFields(logrus.Fields{
				"time":   time.Now().Format("2006-01-02 15:04:05"),
				"method": "performPushDiscovery",
				"took":   time.Since(startTime),
			}).Errorf(color.RedString("更新MongoDB版本数据失败: %v", err))
			continue
		}
	}

	// 步骤5：按服务逐个推送
	for _, service := range req.Services {
		pushReq := models.PushRequest{
			Services:     []string{service},
			Environments: req.Environments,
		}
		err = a.apiClient.PushData(pushReq)
		if err != nil {
			logrus.WithFields(logrus.Fields{
				"time":   time.Now().Format("2006-01-02 15:04:05"),
				"method": "performPushDiscovery",
				"took":   time.Since(startTime),
			}).Errorf(color.RedString("推送失败 [%s]: %v", service, err))
			continue
		}
	}

	// 步骤6：总结推送结果
	uniqueServices := make(map[string]struct{})
	for _, s := range req.Services {
		uniqueServices[s] = struct{}{}
	}
	logrus.WithFields(logrus.Fields{
		"time":   time.Now().Format("2006-01-02 15:04:05"),
		"method": "performPushDiscovery",
		"took":   time.Since(startTime),
		"data": logrus.Fields{
			"services":      uniqueServices,
			"service_count": len(uniqueServices),
			"env_mappings":  uniqueEnvs,
			"env_count":     len(uniqueEnvs),
		},
	}).Infof(color.GreenString("服务发现和推送完成，服务数: %d, 环境数: %d", len(uniqueServices), len(uniqueEnvs)))
}

// getServicesFromMongo 从MongoDB获取服务列表
func (a *Agent) getServicesFromMongo(ctx context.Context) ([]string, error) {
	startTime := time.Now()
	// 步骤1：查询版本集合
	versionsColl := a.mongo.GetClient().Database("cicd").Collection("versions")
	cursor, err := versionsColl.Find(ctx, bson.M{})
	if err != nil {
		logrus.WithFields(logrus.Fields{
			"time":   time.Now().Format("2006-01-02 15:04:05"),
			"method": "getServicesFromMongo",
			"took":   time.Since(startTime),
		}).Errorf(color.RedString("查询版本集合失败: %v", err))
		return nil, err
	}
	defer cursor.Close(ctx)

	// 步骤2：收集服务名
	var services []string
	seen := make(map[string]struct{})
	for cursor.Next(ctx) {
		var version struct {
			Service string `bson:"service"`
		}
		if err := cursor.Decode(&version); err != nil {
			continue
		}
		if _, exists := seen[version.Service]; !exists {
			seen[version.Service] = struct{}{}
			services = append(services, version.Service)
		}
	}

	logrus.WithFields(logrus.Fields{
		"time":   time.Now().Format("2006-01-02 15:04:05"),
		"method": "getServicesFromMongo",
		"took":   time.Since(startTime),
	}).Infof(color.GreenString("从MongoDB获取 %d 个服务", len(services)))
	return services, nil
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
		user := a.config.User.Default
		ctx := context.Background()
		// 步骤6：从MongoDB获取服务列表
		services, err := a.getServicesFromMongo(ctx)
		if err != nil {
			logrus.WithFields(logrus.Fields{
				"time":   time.Now().Format("2006-01-02 15:04:05"),
				"method": "periodicQueryTasks",
				"took":   time.Since(startTime),
			}).Errorf(color.RedString("获取服务列表失败: %v", err))
			continue
		}

		// 步骤7：对每个环境和服务执行查询
		for env := range a.config.EnvMapping.Mappings {
			for _, service := range services {
				queryReq := models.QueryRequest{
					Environment: env,
					User:        user,
					Service:     service,
				}
				tasks, err := a.apiClient.QueryTasks(queryReq)
				if err != nil {
					logrus.WithFields(logrus.Fields{
						"time":   time.Now().Format("2006-01-02 15:04:05"),
						"method": "periodicQueryTasks",
						"took":   time.Since(startTime),
						"data": logrus.Fields{
							"service":     service,
							"environment": env,
						},
					}).Errorf(color.RedString("查询失败: %v", err))
					continue
				}

				// 步骤8：处理任务并总结
				for _, task := range tasks {
					a.processTask(task, env)
				}
			}
		}
		// 步骤9：总结查询周期
		logrus.WithFields(logrus.Fields{
			"time":   time.Now().Format("2006-01-02 15:04:05"),
			"method": "periodicQueryTasks",
			"took":   time.Since(startTime),
		}).Infof(color.GreenString("完成一轮 /query 轮询"))
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
			"data": logrus.Fields{
				"task": task,
			},
		}).Warnf("环境不匹配: 查询[%s] != 任务[%s]", queryEnv, task.Environments[0])
		return
	}
	// 步骤2：校验状态
	if task.Status != "pending" {
		logrus.WithFields(logrus.Fields{
			"time":   time.Now().Format("2006-01-02 15:04:05"),
			"method": "processTask",
			"took":   time.Since(startTime),
			"data": logrus.Fields{
				"task": task,
			},
		}).Infof(color.GreenString("任务状态非pending，跳过: %s", task.Status))
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
				"data": logrus.Fields{
					"task": task,
				},
			}).Errorf(color.RedString("发送确认弹窗失败: %v", err))
			return
		}
		logrus.WithFields(logrus.Fields{
			"time":   time.Now().Format("2006-01-02 15:04:05"),
			"method": "processTask",
			"took":   time.Since(startTime),
			"data": logrus.Fields{
				"task": task,
			},
		}).Infof(color.GreenString("已发送确认弹窗: %s v%s [%s]", task.Service, task.Version, queryEnv))
	} else {
		// 步骤5：直接存储并入队
		if err := a.validateAndStoreTask(task, queryEnv); err != nil {
			logrus.WithFields(logrus.Fields{
				"time":   time.Now().Format("2006-01-02 15:04:05"),
				"method": "processTask",
				"took":   time.Since(startTime),
				"data": logrus.Fields{
					"task": task,
				},
			}).Errorf(color.RedString("校验和存储任务失败: %v", err))
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
			"data": logrus.Fields{
				"task_id": taskID,
				"task":    task,
			},
		}).Infof(color.GreenString("任务直接入队: %s v%s [%s]", task.Service, task.Version, queryEnv))
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
					"data": logrus.Fields{
						"task": task,
					},
				}).Errorf(color.RedString("校验任务失败: %v", err))
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
				"data": logrus.Fields{
					"task_id": taskID,
					"task":    task,
				},
			}).Infof(color.GreenString("确认任务入队: %s v%s [%s]", task.Service, task.Version, task.Environments[0]))
		case status := <-rejectChan:
			// 步骤2：处理拒绝任务
			err := a.apiClient.UpdateStatus(status)
			if err != nil {
				logrus.WithFields(logrus.Fields{
					"time":   time.Now().Format("2006-01-02 15:04:05"),
					"method": "handleConfirmationChannels",
					"took":   time.Since(startTime),
					"data": logrus.Fields{
						"status_request": status,
					},
				}).Errorf(color.RedString("拒绝状态更新失败: %v", err))
			} else {
				logrus.WithFields(logrus.Fields{
					"time":   time.Now().Format("2006-01-02 15:04:05"),
					"method": "handleConfirmationChannels",
					"took":   time.Since(startTime),
					"data": logrus.Fields{
						"status_request": status,
					},
				}).Infof(color.GreenString("任务拒绝: %s v%s [%s]", status.Service, status.Version, status.Environment))
				// 步骤3：更新MongoDB状态
				err = a.mongo.UpdateTaskStatus(status.Service, status.Version, status.Environment, status.User, "rejected")
				if err != nil {
					logrus.WithFields(logrus.Fields{
						"time":   time.Now().Format("2006-01-02 15:04:05"),
						"method": "handleConfirmationChannels",
						"took":   time.Since(startTime),
						"data": logrus.Fields{
							"status_request": status,
						},
					}).Errorf(color.RedString("MongoDB状态更新失败: %v", err))
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
			"data": logrus.Fields{
				"task": task,
			},
		}).Errorf(color.RedString("环境 [%s] 无命名空间配置", env))
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
			"data": logrus.Fields{
				"task": task,
			},
		}).Errorf(color.RedString("检查任务重复失败: %v", err))
		return fmt.Errorf("检查任务重复失败: %v", err)
	}
	if isDuplicate {
		logrus.WithFields(logrus.Fields{
			"time":   time.Now().Format("2006-01-02 15:04:05"),
			"method": "validateAndStoreTask",
			"took":   time.Since(startTime),
			"data": logrus.Fields{
				"task": task,
			},
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
			"data": logrus.Fields{
				"task": task,
			},
		}).Errorf(color.RedString("存储任务失败: %v", err))
		return fmt.Errorf("存储任务失败: %v", err)
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