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

// 确认实现: periodicQueryAndSync 在存储后再次查询最新数据并打印 - 增强日志突出
func (a *Approval) periodicQueryAndSync() {
	ticker := time.NewTicker(a.cfg.API.QueryInterval)
	defer ticker.Stop()

	for range ticker.C {
		startTime := time.Now()
		logrus.WithFields(logrus.Fields{
			"time":   time.Now().Format("2006-01-02 15:04:05"),
			"method": "periodicQueryAndSync",
		}).Debug("=== 开始新一轮查询同步循环 ===")

		// 1. 获取已 push 的服务和环境 - 添加日志
		services, envs, err := a.mongo.GetPushedServicesAndEnvs()
		if err != nil {
			logrus.WithFields(logrus.Fields{
				"time":   time.Now().Format("2006-01-02 15:04:05"),
				"method": "periodicQueryAndSync",
			}).Errorf("获取 push 数据失败: %v", err)
			continue
		}

		logrus.WithFields(logrus.Fields{
			"time":     time.Now().Format("2006-01-02 15:04:05"),
			"method":   "periodicQueryAndSync",
			"services": services,
			"envs":     envs,
			"confirm_envs": a.cfg.Query.ConfirmEnvs,
		}).Infof("已获取服务列表: %v, 环境列表: %v (确认环境: %v)", services, envs, a.cfg.Query.ConfirmEnvs)

		if len(services) == 0 || len(envs) == 0 {
			logrus.WithFields(logrus.Fields{
				"time":   time.Now().Format("2006-01-02 15:04:05"),
				"method": "periodicQueryAndSync",
			}).Warn("暂无已 push 的服务/环境，跳过本次查询")
			continue
		}

		// 2. 打印完整服务和环境列表 - 已存在，增强
		logrus.WithFields(logrus.Fields{
			"time":     time.Now().Format("2006-01-02 15:04:05"),
			"method":   "periodicQueryAndSync",
			"services": services,
			"envs":     envs,
			"count_svc": len(services),
			"count_env": len(envs),
		}).Infof("开始查询 /query 接口，待查服务: %v，环境: %v", services, envs)

		// 3. 并发查询 - 添加每个 service/env 的日志
		totalNewTasks := 0
		for _, service := range services {
			if service == "" {
				logrus.Warn("跳过空服务名")
				continue
			}

			for _, env := range envs {
				if !contains(a.cfg.Query.ConfirmEnvs, env) {
					logrus.Debugf("跳过非确认环境: %s", env)
					continue
				}

				logrus.WithFields(logrus.Fields{
					"time":   time.Now().Format("2006-01-02 15:04:05"),
					"method": "periodicQueryAndSync",
					"service": service,
					"env":     env,
				}).Debugf("调用 /query 接口查询任务")

				tasks, err := a.queryClient.QueryTasks(service, []string{env})
				if err != nil {
					logrus.WithFields(logrus.Fields{
						"time":   time.Now().Format("2006-01-02 15:04:05"),
						"method": "periodicQueryAndSync",
						"service": service,
						"env":     env,
					}).Errorf("查询任务失败: %v", err)
					continue
				}

				logrus.WithFields(logrus.Fields{
					"time":   time.Now().Format("2006-01-02 15:04:05"),
					"method": "periodicQueryAndSync",
					"service": service,
					"env":     env,
					"task_count": len(tasks),
				}).Debugf("查询到 %d 个任务", len(tasks))

				for i := range tasks {
					task := &tasks[i]
					task.Namespace = a.cfg.EnvMapping.Mappings[env]
					task.ConfirmationStatus = "pending"
					task.PopupSent = false
					if task.TaskID == "" {
						task.TaskID = fmt.Sprintf("%s-%s-%s", task.Service, task.Version, env)
					}

					logrus.WithFields(logrus.Fields{
						"time":   time.Now().Format("2006-01-02 15:04:05"),
						"method": "periodicQueryAndSync",
						"task_id": task.TaskID,
						"service": task.Service,
						"version": task.Version,
						"env":     env,
					}).Debugf("准备存储任务: %+v", task) // 打印完整任务

					if err := a.mongo.StoreTaskIfNotExists(*task); err != nil {
						logrus.WithFields(logrus.Fields{
							"time":   time.Now().Format("2006-01-02 15:04:05"),
							"method": "periodicQueryAndSync",
							"task_id": task.TaskID,
						}).Debugf("任务已存在，跳过存储: %v", err)
					} else {
						totalNewTasks++

						// 确认实现: 存储成功后，立即再次查询该任务的最新数据并打印
						logrus.WithFields(logrus.Fields{
							"time":   time.Now().Format("2006-01-02 15:04:05"),
							"method": "periodicQueryAndSync",
							"task_id": task.TaskID,
						}).Info("=== 开始查询最新存储任务数据 ===")

						latestTask, err := a.mongo.GetTaskByID(task.TaskID)
						if err != nil {
							logrus.WithFields(logrus.Fields{
								"time":   time.Now().Format("2006-01-02 15:04:05"),
								"method": "periodicQueryAndSync",
								"task_id": task.TaskID,
							}).Errorf("存储后查询最新任务失败: %v", err)
						} else {
							logrus.WithFields(logrus.Fields{
								"time":   time.Now().Format("2006-01-02 15:04:05"),
								"method": "periodicQueryAndSync",
								"task_id": task.TaskID,
								"latest_task_status": latestTask.ConfirmationStatus,
								"latest_task_popup_sent": latestTask.PopupSent,
								"latest_task_full": fmt.Sprintf("%+v", latestTask), // 完整最新数据
							}).Infof("=== 任务存储成功，最新数据查询完成: status=%s, popup_sent=%t ===\nFull Data: %+v", 
								latestTask.ConfirmationStatus, latestTask.PopupSent, latestTask)
						}

						logrus.WithFields(logrus.Fields{
							"time":   time.Now().Format("2006-01-02 15:04:05"),
							"method": "periodicQueryAndSync",
							"task_id": task.TaskID,
						}).Infof("新任务存储成功")
					}
				}
			}
		}

		logrus.WithFields(logrus.Fields{
			"time":         time.Now().Format("2006-01-02 15:04:05"),
			"method":       "periodicQueryAndSync",
			"new_tasks":    totalNewTasks,
			"took":         time.Since(startTime),
		}).Infof("查询同步完成，本轮新增 %d 个任务", totalNewTasks)
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