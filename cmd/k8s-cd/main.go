package main

import (
	"flag"
	"os"
	"os/signal"
	"syscall"
	"time"

	"k8s-cicd/agent"
	"k8s-cicd/agent/api"
	"k8s-cicd/agent/client"
	"k8s-cicd/agent/config"
	"k8s-cicd/agent/kubernetes"
	"k8s-cicd/agent/task"
	"k8s-cicd/agent/telegram"

	"github.com/fatih/color"
	"github.com/sirupsen/logrus"
)

func main() {
	startTime := time.Now()

	// 步骤1：解析命令行参数
	configFile := flag.String("config", "config.yaml", "配置文件路径")
	flag.Parse()

	// 步骤2：加载配置
	cfg, err := config.LoadConfig(*configFile)
	if err != nil {
		logrus.Fatalf(color.RedString("加载配置失败: %v"), err)
	}

	// 步骤3：初始化 MongoDB 客户端
	mongoClient, err := client.NewMongoClient(&cfg.Mongo)
	if err != nil {
		logrus.Fatalf(color.RedString("MongoDB 连接失败: %v"), err)
	}
	defer func() {
		if err := mongoClient.Close(); err != nil {
			logrus.Errorf(color.RedString("关闭 MongoDB 连接失败: %v"), err)
		}
	}()

	// 步骤4：初始化 Kubernetes 客户端
	k8sClient, err := kubernetes.NewK8sClient(&cfg.Kubernetes, &cfg.Deploy)
	if err != nil {
		logrus.Fatalf(color.RedString("Kubernetes 连接失败: %v"), err)
	}

	// 步骤5：初始化 API 客户端
	apiClient := api.NewAPIClient(&cfg.API)

	// 步骤6：初始化 Telegram BotManager（从环境变量读取）
	telegramToken := os.Getenv("TELEGRAM_TOKEN")
	telegramGroupID := os.Getenv("TELEGRAM_GROUP_ID")
	if telegramToken == "" || telegramGroupID == "" {
		logrus.Warn(color.YellowString("TELEGRAM_TOKEN 或 TELEGRAM_GROUP_ID 未设置，通知功能将禁用"))
	}
	botMgr := telegram.NewBotManager()

	// 步骤7：初始化任务队列
	taskQ := task.NewTaskQueue(cfg.Task.QueueWorkers)

	// 步骤8：组装 Agent（字段名大写）
	ag := &agent.Agent{
		Cfg:       cfg,
		Mongo:     mongoClient,
		K8s:       k8sClient,
		TaskQ:     taskQ,
		BotMgr:    botMgr,
		ApiClient: apiClient,
	}

	// 步骤9：启动 Agent
	logrus.WithFields(logrus.Fields{
		"time":   time.Now().Format("2006-01-02 15:04:05"),
		"method": "main",
		"took":   time.Since(startTime),
	}).Info(color.GreenString("k8s-cd Agent 启动中..."))

	ag.Start()

	// 步骤10：等待系统信号
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	logrus.WithFields(logrus.Fields{
		"time":   time.Now().Format("2006-01-02 15:04:05"),
		"method": "main",
	}).Info(color.GreenString("k8s-cd Agent 已启动，等待中断信号..."))

	<-sigChan

	// 步骤11：优雅关闭
	logrus.Info(color.YellowString("收到关闭信号，开始优雅关闭..."))

	stopStart := time.Now()
	ag.Stop()

	logrus.WithFields(logrus.Fields{
		"time":   time.Now().Format("2006-01-02 15:04:05"),
		"method": "main",
		"took":   time.Since(stopStart),
	}).Info(color.GreenString("k8s-cd Agent 关闭完成"))
}