// 修改后的 agent/agent.go：在 Enqueue 时确保 CreatedAt 和 TaskID（task.go 已处理）；查询状态 "已确认"。

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

	"github.com/fatih/color"
	"github.com/sirupsen/logrus"
)

// Agent 主代理结构（不变）
type Agent struct {
	Cfg       *config.Config
	Mongo     *client.MongoClient
	K8s       *kubernetes.K8sClient
	TaskQ     *task.TaskQueue
	BotMgr    *telegram.BotManager
	ApiClient *api.APIClient
}

// Start 启动 Agent（不变）
func (a *Agent) Start() {
	startTime := time.Now()

	// 步骤1：记录启动信息
	logrus.WithFields(logrus.Fields{
		"time":   time.Now().Format("2006-01-02 15:04:05"),
		"method": "Start",
		"took":   time.Since(startTime),
	}).Infof(color.GreenString("k8s-cd Agent 启动成功，API=%s, 推送间隔=%v, 查询间隔=%v",
		a.Cfg.API.BaseURL, a.Cfg.API.PushInterval, a.Cfg.API.QueryInterval))

	// 步骤2：启动任务队列 Worker
	a.TaskQ.StartWorkers(a.Cfg, a.Mongo, a.K8s, a.BotMgr, a.ApiClient)

	// 步骤3：启动周期性任务轮询（从 Mongo 获取 "已确认" 任务，按 created_at 排序）
	go a.periodicPollTasksFromMongo()

	// 步骤4：全量推送一次发现数据（启动时）
	a.initialFullPush()

	// 步骤5：启动周期性推送（优化后逻辑）
	go a.periodicPushDiscovery()
}

// initialFullPush 启动时全量推送（不变）
func (a *Agent) initialFullPush() {
	startTime := time.Now()
	logrus.WithFields(logrus.Fields{
		"time":   time.Now().Format("2006-01-02 15:04:05"),
		"method": "initialFullPush",
	}).Info(color.GreenString("执行启动时全量推送..."))

	// 1. 构建 PushRequest
	pushReq, err := a.K8s.BuildPushRequest(a.Cfg)
	if err != nil {
		logrus.WithFields(logrus.Fields{
			"time":   time.Now().Format("2006-01-02 15:04:05"),
			"method": "initialFullPush",
			"took":   time.Since(startTime),
		}).Errorf(color.RedString("构建 PushRequest 失败: %v"), err)
		return
	}

	// 2. 调用 /push 接口
	if err := a.ApiClient.PushData(pushReq); err != nil {
		logrus.WithFields(logrus.Fields{
			"time":   time.Now().Format("2006-01-02 15:04:05"),
			"method": "initialFullPush",
			"took":   time.Since(startTime),
		}).Errorf(color.RedString("全量推送 /push 失败: %v"), err)
		return
	}

	// 3. 存储到 Mongo pushlist
	pushData := &models.PushData{
		Services:     pushReq.Services,
		Environments: pushReq.Environments,
	}
	if err := a.Mongo.StorePushData(pushData); err != nil {
		logrus.WithFields(logrus.Fields{
			"time":   time.Now().Format("2006-01-02 15:04:05"),
			"method": "initialFullPush",
			"took":   time.Since(startTime),
		}).Errorf(color.RedString("存储全量推送数据失败: %v"), err)
		return
	}

	logrus.WithFields(logrus.Fields{
		"time":   time.Now().Format("2006-01-02 15:04:05"),
		"method": "initialFullPush",
		"took":   time.Since(startTime),
		"data": logrus.Fields{
			"service_count": len(pushReq.Services),
			"env_count":     len(pushReq.Environments),
		},
	}).Infof(color.GreenString("启动时全量推送成功"))
}

// periodicPushDiscovery 周期性推送服务发现数据（不变）
func (a *Agent) periodicPushDiscovery() {
	ticker := time.NewTicker(a.Cfg.API.PushInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			startTime := time.Now()

			// 1. 构建当前 PushRequest
			pushReq, err := a.K8s.BuildPushRequest(a.Cfg)
			if err != nil {
				logrus.WithFields(logrus.Fields{
					"time":   time.Now().Format("2006-01-02 15:04:05"),
					"method": "periodicPushDiscovery",
					"took":   time.Since(startTime),
				}).Errorf(color.RedString("构建 PushRequest 失败: %v"), err)
				continue
			}

			// 2. 获取存储的推送数据
			storedData, err := a.Mongo.GetPushData()
			if err != nil {
				logrus.WithFields(logrus.Fields{
					"time":   time.Now().Format("2006-01-02 15:04:05"),
					"method": "periodicPushDiscovery",
					"took":   time.Since(startTime),
				}).Errorf(color.RedString("获取存储推送数据失败: %v"), err)
				continue
			}

			// 3. 检查是否有变化
			if !a.Mongo.HasChanges(pushReq.Services, pushReq.Environments, storedData) {
				logrus.WithFields(logrus.Fields{
					"time":   time.Now().Format("2006-01-02 15:04:05"),
					"method": "periodicPushDiscovery",
					"took":   time.Since(startTime),
				}).Info(color.GreenString("推送数据无变化，跳过执行"))
				continue
			}

			// 4. 有变化：推送全量数据
			if err := a.ApiClient.PushData(pushReq); err != nil {
				logrus.WithFields(logrus.Fields{
					"time":   time.Now().Format("2006-01-02 15:04:05"),
					"method": "periodicPushDiscovery",
					"took":   time.Since(startTime),
				}).Errorf(color.RedString("推送 /push 失败: %v"), err)
				continue
			}

			// 5. 更新存储数据
			pushData := &models.PushData{
				Services:     pushReq.Services,
				Environments: pushReq.Environments,
			}
			if err := a.Mongo.StorePushData(pushData); err != nil {
				logrus.WithFields(logrus.Fields{
					"time":   time.Now().Format("2006-01-02 15:04:05"),
					"method": "periodicPushDiscovery",
					"took":   time.Since(startTime),
				}).Errorf(color.RedString("更新推送数据失败: %v"), err)
				continue
			}

			logrus.WithFields(logrus.Fields{
				"time":   time.Now().Format("2006-01-02 15:04:05"),
				"method": "periodicPushDiscovery",
				"took":   time.Since(startTime),
				"data": logrus.Fields{
					"service_count": len(pushReq.Services),
					"env_count":     len(pushReq.Environments),
				},
			}).Infof(color.GreenString("周期推送 /push 成功（数据已更新）"))
		}
	}
}

// periodicPollTasksFromMongo 周期性从 Mongo 轮询 "已确认" 任务（Sort 已处理，确保按顺序 Enqueue）
func (a *Agent) periodicPollTasksFromMongo() {
	ticker := time.NewTicker(a.Cfg.API.QueryInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			startTime := time.Now()

			// 1. 遍历所有环境
			for env, namespace := range a.Cfg.EnvMapping.Mappings {
				// 2. 查询状态为 "已确认" 的任务 (已按 created_at 排序)
				deployReqs, err := a.Mongo.GetTasksByStatus(env, "已确认")
				if err != nil {
					logrus.WithFields(logrus.Fields{
						"time":   time.Now().Format("2006-01-02 15:04:05"),
						"method": "periodicPollTasksFromMongo",
						"env":    env,
						"took":   time.Since(startTime),
					}).Errorf(color.RedString("查询 %s 任务失败: %v"), env, err)
					continue
				}

				// 3. 提交任务到队列（按顺序 Enqueue，确保 FIFO + lock 串行）
				for _, dr := range deployReqs {
					// 补全 namespace（根据环境映射匹配）
					dr.Namespace = namespace
					// 创建 task.Task (TaskID/CreatedAt 已存在)
					t := &task.Task{
						DeployRequest: dr,
						ID:            fmt.Sprintf("%s-%s", dr.Service, dr.Version),
						Retries:       0,
					}
					a.TaskQ.Enqueue(t)
				}

				if len(deployReqs) > 0 {
					logrus.WithFields(logrus.Fields{
						"time":   time.Now().Format("2006-01-02 15:04:05"),
						"method": "periodicPollTasksFromMongo",
						"env":    env,
						"count":  len(deployReqs),
						"took":   time.Since(startTime),
					}).Infof(color.GreenString("发现 %d 个待部署任务 (按 created_at 排序)"), len(deployReqs))
				}
			}
		}
	}
}

// Stop 优雅停止 Agent（不变）
func (a *Agent) Stop() {
	startTime := time.Now()

	// 步骤1：停止任务队列
	a.TaskQ.Stop()

	// 步骤2：停止 Telegram 通知
	a.BotMgr.Stop()

	// 步骤3：等待完成
	time.Sleep(2 * time.Second)

	logrus.WithFields(logrus.Fields{
		"time":   time.Now().Format("2006-01-02 15:04:05"),
		"method": "Stop",
		"took":   time.Since(startTime),
	}).Infof(color.GreenString("k8s-cd Agent 关闭完成"))
}