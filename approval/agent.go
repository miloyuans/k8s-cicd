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

// 修复: periodicQueryAndSync 使用 "待确认" 状态设置、更新和验证
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
					task.ConfirmationStatus = "待确认" // 修复: 设置为 "待确认"
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
						"set_status": task.ConfirmationStatus, // 日志记录设置的状态
					}).Debugf("准备存储任务 (设置 status=待确认): %+v", task) // 打印完整任务

					if err := a.mongo.StoreTaskIfNotExists(*task); err != nil {
						logrus.WithFields(logrus.Fields{
							"time":   time.Now().Format("2006-01-02 15:04:05"),
							"method": "periodicQueryAndSync",
							"task_id": task.TaskID,
						}).Debugf("任务已存在，跳过存储: %v", err)
					} else {
						totalNewTasks++

						// 修复: 存储成功后，立即更新状态为 "待确认"（如果未设置）并验证
						logrus.WithFields(logrus.Fields{
							"time":   time.Now().Format("2006-01-02 15:04:05"),
							"method": "periodicQueryAndSync",
							"task_id": task.TaskID,
						}).Info("=== 开始存储后状态更新和验证 ===")

						// 立即更新状态为 待确认（冗余确保）
						updateErr := a.mongo.UpdateTaskStatus(task.TaskID, "待确认", "system") // system 表示系统自动设置
						if updateErr != nil {
							logrus.WithFields(logrus.Fields{
								"time":   time.Now().Format("2006-01-02 15:04:05"),
								"method": "periodicQueryAndSync",
								"task_id": task.TaskID,
							}).Errorf("存储后更新状态为 待确认 失败: %v", updateErr)
						} else {
							logrus.WithFields(logrus.Fields{
								"time":   time.Now().Format("2006-01-02 15:04:05"),
								"method": "periodicQueryAndSync",
								"task_id": task.TaskID,
							}).Infof("状态更新为 待确认 成功")
						}

						// 查询最新数据验证
						latestTask, queryErr := a.mongo.GetTaskByID(task.TaskID)
						if queryErr != nil {
							logrus.WithFields(logrus.Fields{
								"time":   time.Now().Format("2006-01-02 15:04:05"),
								"method": "periodicQueryAndSync",
								"task_id": task.TaskID,
							}).Errorf("存储后查询最新任务失败: %v", queryErr)
						} else if latestTask.ConfirmationStatus != "待确认" {
							logrus.WithFields(logrus.Fields{
								"time":   time.Now().Format("2006-01-02 15:04:05"),
								"method": "periodicQueryAndSync",
								"task_id": task.TaskID,
								"current_status": latestTask.ConfirmationStatus,
							}).Errorf("最新状态非 待确认，重新更新")
							// 强制重新更新
							a.mongo.UpdateTaskStatus(task.TaskID, "待确认", "system")
							latestTask, _ = a.mongo.GetTaskByID(task.TaskID)
						} else {
							logrus.WithFields(logrus.Fields{
								"time":   time.Now().Format("2006-01-02 15:04:05"),
								"method": "periodicQueryAndSync",
								"task_id": task.TaskID,
							}).Infof("状态验证成功: %s", latestTask.ConfirmationStatus)
						}

						logrus.WithFields(logrus.Fields{
							"time":   time.Now().Format("2006-01-02 15:04:05"),
							"method": "periodicQueryAndSync",
							"task_id": task.TaskID,
							"latest_task_status": latestTask.ConfirmationStatus,
							"latest_task_popup_sent": latestTask.PopupSent,
							"latest_task_full": fmt.Sprintf("%+v", latestTask), // 完整最新数据
						}).Infof("=== 任务存储成功，状态变更完成 (待确认): status=%s, popup_sent=%t ===\nFull Latest Data: %+v", 
							latestTask.ConfirmationStatus, latestTask.PopupSent, latestTask)

						logrus.WithFields(logrus.Fields{
							"time":   time.Now().Format("2006-01-02 15:04:05"),
							"method": "periodicQueryAndSync",
							"task_id": task.TaskID,
						}).Infof("新任务存储成功 (状态已更新为 待确认)")
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