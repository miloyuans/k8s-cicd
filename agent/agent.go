package agent

import (
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

// Agent 主代理结构
type Agent struct {
	cfg       *config.Config
	mongo     *client.MongoClient
	k8s       *kubernetes.K8sClient
	taskQ     *task.TaskQueue
	botMgr    *telegram.BotManager
	apiClient *api.APIClient
}

// Start 启动 Agent
func (a *Agent) Start() {
	startTime := time.Now()

	// 步骤1：记录启动信息
	logrus.WithFields(logrus.Fields{
		"time":   time.Now().Format("2006-01-02 15:04:05"),
		"method": "Start",
		"took":   time.Since(startTime),
	}).Infof(color.GreenString("k8s-cd Agent 启动成功，API=%s, 推送间隔=%v, 查询间隔=%v",
		a.cfg.API.BaseURL, a.cfg.API.PushInterval, a.cfg.API.QueryInterval))

	// 步骤2：启动任务队列 Worker
	a.taskQ.StartWorkers(a.cfg, a.mongo, a.k8s, a.botMgr, a.apiClient)

	// 步骤3：启动周期性服务发现推送
	go a.periodicPushDiscovery()

	// 步骤4：启动周期性任务轮询（从 Mongo 获取 confirmed 任务）
	go a.periodicPollTasksFromMongo()
}

// periodicPushDiscovery 周期性推送服务发现数据
func (a *Agent) periodicPushDiscovery() {
	ticker := time.NewTicker(a.cfg.API.PushInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			startTime := time.Now()

			// 1. 构建 PushRequest
			pushReq, err := a.k8s.BuildPushRequest(a.cfg)
			if err != nil {
				logrus.WithFields(logrus.Fields{
					"time":   time.Now().Format("2006-01-02 15:04:05"),
					"method": "periodicPushDiscovery",
					"took":   time.Since(startTime),
				}).Errorf(color.RedString("构建 PushRequest 失败: %v"), err)
				continue
			}

			// 2. 调用 /push 接口
			if err := a.apiClient.PushData(pushReq); err != nil {
				logrus.WithFields(logrus.Fields{
					"time":   time.Now().Format("2006-01-02 15:04:05"),
					"method": "periodicPushDiscovery",
					"took":   time.Since(startTime),
				}).Errorf(color.RedString("推送 /push 失败: %v"), err)
			} else {
				logrus.WithFields(logrus.Fields{
					"time":   time.Now().Format("2006-01-02 15:04:05"),
					"method": "periodicPushDiscovery",
					"took":   time.Since(startTime),
					"data": logrus.Fields{
						"service_count": len(pushReq.Services),
						"env_count":     len(pushReq.Environments),
					},
				}).Infof(color.GreenString("推送 /push 成功"))
			}
		}
	}
}

// periodicPollTasksFromMongo 周期性从 Mongo 轮询 confirmed 任务
func (a *Agent) periodicPollTasksFromMongo() {
	ticker := time.NewTicker(a.cfg.API.QueryInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			startTime := time.Now()

			// 1. 遍历所有环境
			for env, namespace := range a.cfg.EnvMapping.Mappings {
				// 2. 查询状态为 confirmed 的任务
				tasks, err := a.mongo.GetTasksByStatus(env, "confirmed")
				if err != nil {
					logrus.WithFields(logrus.Fields{
						"time":   time.Now().Format("2006-01-02 15:04:05"),
						"method": "periodicPollTasksFromMongo",
						"env":    env,
						"took":   time.Since(startTime),
					}).Errorf(color.RedString("查询 %s 任务失败: %v"), env, err)
					continue
				}

				// 3. 提交任务到队列
				for i := range tasks {
					task := tasks[i]
					task.Namespace = namespace // 补全 namespace
					a.taskQ.Enqueue(&models.Task{
						DeployRequest: task,
						ID:            task.Service + "-" + task.Version,
						Retries:       0,
					})
				}

				if len(tasks) > 0 {
					logrus.WithFields(logrus.Fields{
						"time":   time.Now().Format("2006-01-02 15:04:05"),
						"method": "periodicPollTasksFromMongo",
						"env":    env,
						"count":  len(tasks),
						"took":   time.Since(startTime),
					}).Infof(color.GreenString("发现 %d 个待部署任务"), len(tasks))
				}
			}
		}
	}
}

// Stop 优雅停止 Agent
func (a *Agent) Stop() {
	startTime := time.Now()

	// 步骤1：停止任务队列
	a.taskQ.Stop()

	// 步骤2：停止 Telegram 通知（空实现）
	a.botMgr.Stop()

	// 步骤3：等待完成
	time.Sleep(2 * time.Second)

	logrus.WithFields(logrus.Fields{
		"time":   time.Now().Format("2006-01-02 15:04:05"),
		"method": "Stop",
		"took":   time.Since(startTime),
	}).Infof(color.GreenString("k8s-cd Agent 关闭完成"))
}