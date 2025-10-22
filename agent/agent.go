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
	config   *config.Config
	redis    *client.RedisClient
	k8s      *kubernetes.K8sClient
	taskQ    *task.TaskQueue
	botMgr   *telegram.BotManager
	apiPusher *api.APIPusher
}

func NewAgent(cfg *config.Config, redis *client.RedisClient, k8s *kubernetes.K8sClient) *Agent {
	// 创建Telegram机器人管理器
	botMgr := telegram.NewBotManager(cfg.Telegram.Bots)
	
	// 创建任务队列
	taskQ := task.NewTaskQueue(cfg.Task.QueueWorkers)
	
	// 创建API推送器
	apiPusher := api.NewAPIPusher(&cfg.API)
	
	return &Agent{
		config:   cfg,
		redis:    redis,
		k8s:      k8s,
		taskQ:    taskQ,
		botMgr:   botMgr,
		apiPusher: apiPusher,
	}
}

func (a *Agent) Start() {
	green := color.New(color.FgGreen).SprintFunc()
	logrus.Infof("%s Agent启动成功", green("🚀"))
	logrus.Infof("部署等待超时: %v", a.config.Deploy.WaitTimeout)
	logrus.Infof("回滚等待超时: %v", a.config.Deploy.RollbackTimeout)
	logrus.Infof("API推送间隔: %v", a.config.API.PushInterval)

	// 推送初始数据
	a.pushInitialData()

	// 启动API推送循环
	go a.apiPusher.Start([]models.DeploymentStatus{})

	// 轮询任务
	ticker := time.NewTicker(time.Duration(a.config.Task.PollInterval) * time.Second)
	go func() {
		for range ticker.C {
			a.pollTasks()
		}
	}()

	// 启动任务队列worker
	go a.taskQ.StartWorkers(a.config, a.redis, a.k8s, a.botMgr, a.apiPusher)
}

func (a *Agent) pushInitialData() {
	// 步骤1：创建示例部署任务
	deploys := []models.DeployRequest{
		{
			Service:      "API-GATEWAY",
			Environments: []string{"PROD"},
			Version:      "v1.2.3",
			User:         "deployer",
			Status:       "pending",
		},
		{
			Service:      "USER-SERVICE",
			Environments: []string{"STAGING"},
			Version:      "v2.1.0",
			User:         "john.doe",
			Status:       "pending",
		},
	}
	
	// 步骤2：执行推送
	err := a.redis.PushDeployments(deploys)
	if err != nil {
		red := color.New(color.FgRed)
		logrus.Errorf("%s 初始数据推送失败: %v", red("❌"), err)
	} else {
		green := color.New(color.FgGreen)
		logrus.Infof("%s 初始数据推送成功", green("✅"))
	}
}

func (a *Agent) pollTasks() {
	blue := color.New(color.FgBlue).SprintFunc()
	logrus.Infof("%s 开始轮询任务", blue("🔍"))

	users := []string{"deployer", "john.doe", "admin"}
	envs := []string{"PROD", "STAGING"}

	for _, env := range envs {
		for _, user := range users {
			// 查询待处理任务
			tasks, err := a.redis.QueryPendingTasks(env, user)
			if err != nil {
				continue
			}

			// 加入任务队列
			for _, t := range tasks {
				taskModel := models.Task{
					DeployRequest: t,
					ID:            fmt.Sprintf("%s-%s-%s", t.Service, t.Version, time.Now().Unix()),
					CreatedAt:     time.Now(),
					Retries:       0,
				}
				a.taskQ.Enqueue(taskModel)
				
				green := color.New(color.FgGreen)
				logrus.Infof("%s 任务已加入队列: %s", green("📥"), taskModel.ID)
			}
		}
	}
}

func (a *Agent) Stop() {
	blue := color.New(color.FgBlue)
	blue.Println("停止Agent...")
	a.taskQ.Stop()
}