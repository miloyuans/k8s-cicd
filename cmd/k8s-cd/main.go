// main.go
//k8s-cicd/cmd/k8s-cd/main.go
package main

import (
	"flag"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"k8s-cicd/agent"
	"k8s-cicd/agent/client"
	"k8s-cicd/agent/config"
	"k8s-cicd/agent/kubernetes"

	"github.com/sirupsen/logrus"
)

// AcquireLock ensures only one instance is running
func AcquireLock() (*os.File, error) {
	lockFilePath := "/tmp/k8s-cicd-agent.lock"
	f, err := os.OpenFile(lockFilePath, os.O_WRONLY|os.O_CREATE, 0600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}

// main 程序入口
func main() {
	startTime := time.Now()
	// Acquire lock to ensure single instance
	lockFile, err := AcquireLock()
	if err != nil {
		log.Fatalf("Another instance is already running: %v", err)
	}
	defer lockFile.Close()

	// 步骤1：解析命令行参数
	configFile := flag.String("config", "config.yaml", "配置文件路径")
	flag.Parse()

	logrus.WithFields(logrus.Fields{
		"time":   time.Now().Format("2006-01-02 15:04:05"),
		"method": "main",
		"took":   time.Since(startTime),
	}).Info("K8s-CICD Agent v1.0 启动")

	// 步骤2：加载配置
	cfg, err := config.LoadConfig(*configFile)
	if err != nil {
		logrus.WithFields(logrus.Fields{
			"time":   time.Now().Format("2006-01-02 15:04:05"),
			"method": "main",
			"took":   time.Since(startTime),
		}).Fatalf("配置加载失败: %v", err)
	}

	// 步骤3：创建MongoDB客户端
	mongoClient, err := client.NewMongoClient(&cfg.Mongo)
	if err != nil {
		logrus.WithFields(logrus.Fields{
			"time":   time.Now().Format("2006-01-02 15:04:05"),
			"method": "main",
			"took":   time.Since(startTime),
		}).Fatalf("MongoDB连接失败: %v", err)
	}
	defer mongoClient.Close()

	// 步骤4：创建Kubernetes客户端
	k8sClient, err := kubernetes.NewK8sClient(&cfg.Kubernetes, &cfg.Deploy)
	if err != nil {
		logrus.WithFields(logrus.Fields{
			"time":   time.Now().Format("2006-01-02 15:04:05"),
			"method": "main",
			"took":   time.Since(startTime),
		}).Fatalf("Kubernetes连接失败: %v", err)
	}

	// 步骤5：创建并启动Agent
	ag := agent.NewAgent(cfg, mongoClient, k8sClient)
	ag.Start()

	// 步骤6：等待关闭信号
	c := make(chan os.Signal, 1)
	signal.Notify(c, syscall.SIGINT, syscall.SIGTERM)
	<-c

	logrus.WithFields(logrus.Fields{
		"time":   time.Now().Format("2006-01-02 15:04:05"),
		"method": "main",
		"took":   time.Since(startTime),
	}).Info("收到关闭信号，优雅关闭")
	ag.Stop()

	logrus.WithFields(logrus.Fields{
		"time":   time.Now().Format("2006-01-02 15:04:05"),
		"method": "main",
		"took":   time.Since(startTime),
	}).Info("Agent关闭完成")
}