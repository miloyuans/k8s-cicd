// 文件: agent.go
package approval

import (
	//"context"
	"fmt"
	"time"

	"k8s-cicd/approval/api"
	"k8s-cicd/approval/client"
	"k8s-cicd/approval/config"
	"k8s-cicd/approval/telegram"

	"github.com/fatih/color"
	"github.com/sirupsen/logrus"
)

type Approval struct {
	cfg         *config.Config
	mongo       *client.MongoClient
	queryClient *api.QueryClient
	botMgr      *telegram.BotManager
}

func NewApproval(cfg *config.Config, mongo *client.MongoClient, queryClient *api.QueryClient, botMgr *telegram.BotManager) *Approval {
	return &Approval{
		cfg:         cfg,
		mongo:       mongo,
		queryClient: queryClient,
		botMgr:      botMgr,
	}
}

func (a *Approval) Start() {
	logrus.Info(color.GreenString("k8s-approval Agent 启动"))

	a.botMgr.Start()
	go a.periodicQueryAndSync()
}

func (a *Approval) periodicQueryAndSync() {
	ticker := time.NewTicker(a.cfg.API.QueryInterval)
	defer ticker.Stop()

	for range ticker.C {
		startTime := time.Now()

		// 1. 获取已 push 的服务和环境
		services, envs, err := a.mongo.GetPushedServicesAndEnvs()
		if err != nil {
			logrus.Errorf("获取 push 数据失败: %v", err)
			continue
		}

		if len(services) == 0 || len(envs) == 0 {
			logrus.Info("暂无已 push 的服务/环境，跳过本次查询")
			continue
		}

		// 2. 打印完整服务和环境列表（需求4）
		logrus.WithFields(logrus.Fields{
			"time":     time.Now().Format("2006-01-02 15:04:05"),
			"method":   "periodicQueryAndSync",
			"services": services,
			"envs":     envs,
		}).Infof("开始查询 /query 接口，待查服务数=%d，环境数=%d", len(services), len(envs))

		// 3. 并发查询
		for _, service := range services {
			if service == "" {
				logrus.Warn("跳过空服务名")
				continue
			}

			for _, env := range envs {
				if !contains(a.cfg.Query.ConfirmEnvs, env) {
					continue
				}

				tasks, err := a.queryClient.QueryTasks(service, []string{env})
				if err != nil {
					logrus.Errorf("查询任务失败 [%s@%s]: %v", service, env, err)
					continue
				}

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

func (a *Approval) Stop() {
	logrus.Info(color.YellowString("k8s-approval Agent 关闭"))
	a.botMgr.Stop()
	time.Sleep(2 * time.Second)
}

func contains(slice []string, item string) bool {
	for _, s := range slice {
		if s == item {
			return true
		}
	}
	return false
}