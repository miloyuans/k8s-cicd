// 文件: agent.go (完整文件，移除了 periodicCleanupDeletedTasks 函数及其调用，因为现在拒绝时立即删除)
package approval

import (
	"context"
	"fmt"
	"time"

	"go.mongodb.org/mongo-driver/bson"

	"k8s-cicd/approval/api"
	"k8s-cicd/approval/client"
	"k8s-cicd/approval/config"
	"k8s-cicd/approval/telegram"

	"github.com/fatih/color"
	"github.com/sirupsen/logrus"
)

// Approval 主代理结构
type Approval struct {
	cfg         *config.Config
	mongo       *client.MongoClient
	queryClient *api.QueryClient
	botMgr      *telegram.BotManager
}

// NewApproval 创建 Approval 实例
func NewApproval(cfg *config.Config, mongo *client.MongoClient, queryClient *api.QueryClient, botMgr *telegram.BotManager) *Approval {
	return &Approval{
		cfg:         cfg,
		mongo:       mongo,
		queryClient: queryClient,
		botMgr:      botMgr,
	}
}

// Start 启动 Approval
func (a *Approval) Start() {
	logrus.Info(color.GreenString("k8s-approval Agent 启动"))

	// 1. 启动 Telegram 轮询 + 弹窗
	a.botMgr.Start()

	// 2. 启动周期性 /query 同步
	go a.periodicQueryAndSync()
}

// periodicQueryAndSync 周期性调用 /query 并同步到 Mongo
func (a *Approval) periodicQueryAndSync() {
	ticker := time.NewTicker(a.cfg.API.QueryInterval)
	defer ticker.Stop()

	for range ticker.C {
		startTime := time.Now()

		// 1. 从 Mongo 获取已 push 的服务/环境
		services, envs, err := a.mongo.GetPushedServicesAndEnvs()
		if err != nil {
			logrus.Errorf("获取 push 数据失败: %v", err)
			continue
		}

		if len(services) == 0 || len(envs) == 0 {
			logrus.Info("暂无已 push 的服务/环境")
			continue
		}

		// 2. 遍历调用 /query
		for _, service := range services {
			if service == "" {
				logrus.Warn("跳过空服务名")
				continue
			}

			for _, env := range envs {
				// 仅对配置中需确认的环境处理
				if !contains(a.cfg.Query.ConfirmEnvs, env) {
					continue
				}

				// 调用 /query 接口
				tasks, err := a.queryClient.QueryTasks(service, []string{env})
				if err != nil {
					logrus.Errorf("查询任务失败 [%s@%s]: %v", service, env, err)
					continue
				}

				// 防重存储
				for i := range tasks {
					task := &tasks[i]
					task.Namespace = a.cfg.EnvMapping.Mappings[env]
					task.ConfirmationStatus = "pending"
					task.PopupSent = false
					if task.TaskID == "" {
						task.TaskID = fmt.Sprintf("%s-%s-%s", task.Service, task.Version, env)
					}
					if err := a.mongo.StoreTaskIfNotExists(*task); err != nil {
						logrus.Debugf("任务已存在: %s", task.TaskID)
					}
				}
			}
		}

		logrus.WithFields(logrus.Fields{
			"time":   time.Now().Format("2006-01-02 15:04:05"),
			"method": "periodicQueryAndSync",
			"took":   time.Since(startTime),
		}).Infof("查询同步完成")
	}
}

// Stop 优雅停止
func (a *Approval) Stop() {
	logrus.Info(color.YellowString("k8s-approval Agent 关闭"))
	a.botMgr.Stop()
	time.Sleep(2 * time.Second)
}

// contains 检查切片
func contains(slice []string, item string) bool {
	for _, s := range slice {
		if s == item {
			return true
		}
	}
	return false
}