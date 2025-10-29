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

	// 3. 启动定期清理 delete_pending 任务
	go a.periodicCleanupDeletedTasks()
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
			for _, env := range envs {
				// 仅对配置中需确认的环境处理
				if !contains(a.cfg.Query.ConfirmEnvs, env) {
					continue
				}

				// 修复：使用 = 而不是 :=
				// 修复：传入 []string{service} 和 []string{env}
				tasks, err := a.queryClient.QueryTasks([]string{service}, []string{env})
				if err != nil {
					logrus.Errorf("查询任务失败 [%s@%s]: %v", service, env, err)
					continue
				}

				// 5. 存储到 Mongo（防重）
				for i := range tasks {
					task := &tasks[i]
					task.Namespace = a.cfg.EnvMapping.Mappings[env]
					task.ConfirmationStatus = "pending"
					task.PopupSent = false
					if task.TaskID == "" {
						task.TaskID = fmt.Sprintf("%s-%s-%s", task.Service, task.Version, env)
					}
					if err := a.mongo.StoreTaskIfNotExists(*task); err != nil {
						logrus.Warnf("存储任务失败: %v", err)
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

// periodicCleanupDeletedTasks 定期清理 delete_pending 任务
func (a *Approval) periodicCleanupDeletedTasks() {
	ticker := time.NewTicker(1 * time.Hour)
	defer ticker.Stop()

	for range ticker.C {
		startTime := time.Now()
		total := 0

		for env := range a.cfg.EnvMapping.Mappings {
			coll := a.mongo.GetClient().Database("cicd").Collection(fmt.Sprintf("tasks_%s", env))
			filter := bson.M{
				"confirmation_status": "delete_pending",
				"created_at":          bson.M{"$lt": time.Now().Add(-a.cfg.Mongo.TTL)},
			}
			result, err := coll.DeleteMany(context.Background(), filter)
			if err != nil {
				logrus.Errorf("清理 %s 任务失败: %v", env, err)
				continue
			}
			total += int(result.DeletedCount)
		}

		if total > 0 {
			logrus.WithFields(logrus.Fields{
				"time":   time.Now().Format("2006-01-02 15:04:05"),
				"method": "periodicCleanupDeletedTasks",
				"took":   time.Since(startTime),
			}).Infof("清理了 %d 个过期 delete_pending 任务", total)
		}
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