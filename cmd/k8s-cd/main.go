package main

import (
	"flag"
	"os"
	"os/signal"
	"syscall"
	"time"

	"k8s-cicd/agent"
	"k8s-cicd/agent/config"
	"k8s-cicd/client"

	"github.com/sirupsen/logrus"
)

func main() {
	// 命令行参数解析
	configFile := flag.String("config", "config.yaml", "配置文件路径")
	flag.Parse()

	// 步骤1：加载配置
	cfg, err := config.LoadConfig(*configFile)
	if err != nil {
		logrus.Fatalf("配置加载失败: %v", err)
	}

	// 步骤2：创建Redis客户端
	redisClient, err := client.NewRedisClient(&cfg.Redis)
	if err != nil {
		logrus.Fatalf("Redis连接失败: %v", err)
	}
	defer redisClient.Close()

	// 步骤3：创建K8s客户端
	k8sClient, err := client.NewK8sClient(&cfg.Kubernetes)
	if err != nil {
		logrus.Fatalf("K8s连接失败: %v", err)
	}

	// 步骤4：启动Agent
	ag := agent.NewAgent(cfg, redisClient, k8sClient)
	ag.Start()

	// 步骤5：优雅关闭
	c := make(chan os.Signal, 1)
	signal.Notify(c, syscall.SIGINT, syscall.SIGTERM)
	
	logrus.Info("Agent启动成功，等待信号...")
	<-c
	
	logrus.Info("开始关闭Agent...")
	ag.Stop()
	time.Sleep(2 * time.Second)
	logrus.Info("Agent关闭完成")
}