package main

import (
	"flag"
	"os"
	"os/signal"
	"syscall"

	"k8s-cicd/agent"
	"k8s-cicd/agent/client"
	"k8s-cicd/agent/config"
	"k8s-cicd/agent/kubernetes"
	"k8s-cicd/agent/task"
	"k8s-cicd/agent/telegram"

	"github.com/sirupsen/logrus"
)

func main() {
	configFile := flag.String("config", "config.yaml", "config file")
	flag.Parse()

	cfg, err := config.LoadConfig(*configFile)
	if err != nil {
		logrus.Fatalf("加载配置失败: %v", err)
	}

	mongoClient, err := client.NewMongoClient(&cfg.Mongo)
	if err != nil {
		logrus.Fatalf("MongoDB 连接失败: %v", err)
	}
	defer mongoClient.Close()

	k8sClient, err := kubernetes.NewK8sClient(&cfg.Kubernetes, &cfg.Deploy)
	if err != nil {
		logrus.Fatalf("Kubernetes 连接失败: %v", err)
	}

	apiClient := agent.NewAPIClient(&cfg.API)
	botMgr := telegram.NewBotManager(cfg.Telegram.Bots)
	botMgr.SetGlobalAllowedUsers(cfg.Telegram.AllowedUsers)

	taskQ := task.NewTaskQueue(cfg.Task.QueueWorkers)

	ag := &agent.Agent{
		cfg:       cfg,
		mongo:     mongoClient,
		k8s:       k8sClient,
		taskQ:     taskQ,
		botMgr:    botMgr,
		apiClient: apiClient,
	}

	ag.Start()

	c := make(chan os.Signal, 1)
	signal.Notify(c, syscall.SIGINT, syscall.SIGTERM)
	<-c

	ag.Stop()
}