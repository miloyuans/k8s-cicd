// 文件: main.go (增强日志: 添加启动时配置打印，确保 Debug 级别)
package main

import (
	"flag"
	"os"
	"os/signal"
	"syscall"
	"time"

	"k8s-cicd/approval"
	"k8s-cicd/approval/api"
	"k8s-cicd/approval/client"
	"k8s-cicd/approval/config"
	"k8s-cicd/approval/telegram"

	"github.com/fatih/color"
	"github.com/sirupsen/logrus"
)

func main() {
	startTime := time.Now()

	// 增强: 开启 debug 日志，确保完整输出 + 打印配置摘要
	logrus.SetLevel(logrus.DebugLevel)
	logrus.SetFormatter(&logrus.TextFormatter{
		FullTimestamp:   true,
		TimestampFormat: "2006-01-02 15:04:05",
		ForceColors:     true,
		DisableSorting:  false,
	})

	// 步骤1：解析命令行参数
	configFile := flag.String("config", "config.yaml", "配置文件路径")
	flag.Parse()

	// 步骤2：加载配置 - 添加打印配置摘要
	cfg, err := config.LoadConfig(*configFile)
	if err != nil {
		logrus.Fatalf(color.RedString("加载配置失败: %v"), err)
	}

	logrus.WithFields(logrus.Fields{
		"time":   time.Now().Format("2006-01-02 15:04:05"),
		"method": "main",
		"config_summary": map[string]interface{}{
			"confirm_envs":     cfg.Query.ConfirmEnvs,
			"telegram_bots":    len(cfg.Telegram.Bots),
			"global_allowed":   cfg.Telegram.AllowedUsers,
			"api_base_url":     cfg.API.BaseURL,
			"query_interval":   cfg.API.QueryInterval,
			"mongo_uri":        cfg.Mongo.URI, // 注意: 生产中可掩码
		},
	}).Debug("配置加载摘要")

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

	// 步骤4：初始化 Telegram BotManager - 添加日志
	botMgr := telegram.NewBotManager(cfg.Telegram.Bots, cfg)
	logrus.WithFields(logrus.Fields{
		"time":   time.Now().Format("2006-01-02 15:04:05"),
		"method": "main",
		"bot_count": len(botMgr.Bots),
	}).Infof("初始化 %d 个 Telegram 机器人", len(botMgr.Bots))

	botMgr.SetGlobalAllowedUsers(cfg.Telegram.AllowedUsers)
	botMgr.SetMongoClient(mongoClient)

	// 步骤5：初始化 Query API 客户端
	queryClient := api.NewQueryClient(cfg.API.BaseURL)
	logrus.WithFields(logrus.Fields{
		"time":     time.Now().Format("2006-01-02 15:04:05"),
		"method":   "main",
		"base_url": cfg.API.BaseURL,
	}).Debug("QueryClient 初始化完成")

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