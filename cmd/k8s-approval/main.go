package main

import (
	"flag"
	"os"
	"os/signal"
	"syscall"
	"time"

	"k8s-cicd/approval"
	"k8s-cicd/approval/api"        // 修复：导入 api 包
	"k8s-cicd/approval/client"
	"k8s-cicd/approval/config"
	"k8s-cicd/approval/telegram"

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

	// 步骤4：初始化 Telegram BotManager
	botMgr := telegram.NewBotManager(cfg.Telegram.Bots, cfg)
	botMgr.SetGlobalAllowedUsers(cfg.Telegram.AllowedUsers)
	botMgr.SetMongoClient(mongoClient)

	// 步骤5：初始化 Query API 客户端
	queryClient := api.NewQueryClient(cfg.API.BaseURL)

	// 步骤6：创建 Approval 实例
	approvalAgent := approval.NewApproval(cfg, mongoClient, queryClient, botMgr)

	// 步骤7：启动 Approval 服务
	logrus.WithFields(logrus.Fields{
		"time":   time.Now().Format("2006-01-02 15:04:05"),
		"method": "main",
		"took":   time.Since(startTime),
	}).Info(color.GreenString("k8s-approval 启动中..."))

	approvalAgent.Start()

	// 步骤8：等待系统信号
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	logrus.WithFields(logrus.Fields{
		"time":   time.Now().Format("2006-01-02 15:04:05"),
		"method": "main",
	}).Info(color.GreenString("k8s-approval 已启动，等待中断信号..."))

	<-sigChan

	// 步骤9：优雅关闭
	logrus.Info(color.YellowString("收到关闭信号，开始优雅关闭..."))

	stopStart := time.Now()
	approvalAgent.Stop()

	logrus.WithFields(logrus.Fields{
		"time":   time.Now().Format("2006-01-02 15:04:05"),
		"method": "main",
		"took":   time.Since(stopStart),
	}).Info(color.GreenString("k8s-approval 关闭完成"))
}