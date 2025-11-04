// 文件: main.go
// 修改: 在 NewTaskQueue 后添加日志打印 workers 和 maxQueueSize 配置，便于调试。
// 保留所有现有功能，包括优雅关闭等。

package main

import (
	"context"
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

	// 步骤4：初始化 Kubernetes 客户端（支持 kubeconfig / serviceaccount）
	k8sClient, err := kubernetes.NewK8sClient(&cfg.Kubernetes, &cfg.Deploy)
	if err != nil {
		logrus.Fatalf(color.RedString("Kubernetes 连接失败: %v"), err)
	}

	// 步骤5：初始化 API 客户端
	apiClient := api.NewAPIClient(&cfg.API)

	// 步骤6：初始化 Telegram BotManager（从配置读取）
	if !cfg.Telegram.Enabled || cfg.Telegram.Token == "" || cfg.Telegram.GroupID == "" {
		logrus.Warn(color.YellowString("Telegram 配置未完整，通知功能将禁用"))
	}
	botMgr := telegram.NewBotManager(&cfg.Telegram) // 传入配置

	// 步骤7：初始化任务队列（资源约束: MaxQueueSize）
	taskQ := task.NewTaskQueue(cfg.Task.QueueWorkers, cfg.Task.MaxQueueSize) // 已通过参数设置，无需重复赋值
	logrus.Infof(color.GreenString("任务队列配置: workers=%d, max_size=%d"), cfg.Task.QueueWorkers, cfg.Task.MaxQueueSize)  // 新增: 配置日志

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
	}).Info(color.YellowString("k8s-cd Agent 已启动，等待中断信号..."))

	<-sigChan

	// 步骤11：优雅关闭，带超时强制
	logrus.Info(color.YellowString("收到关闭信号，开始优雅关闭..."))

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second) // 30s 超时
	defer cancel()

	stopStart := time.Now()
	go func() {
		ag.Stop()
		logrus.WithFields(logrus.Fields{
			"time":   time.Now().Format("2006-01-02 15:04:05"),
			"method": "main",
			"took":   time.Since(stopStart),
		}).Info(color.GreenString("k8s-cd Agent 关闭完成"))
	}()

	select {
	case <-ctx.Done():
		if ctx.Err() == context.DeadlineExceeded {
			logrus.Error(color.RedString("优雅关闭超时，强制终止进程"))
			os.Exit(1) // 强制退出
		}
	}

	// 正常退出
	os.Exit(0)
}