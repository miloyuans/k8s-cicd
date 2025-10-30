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
// 修改: periodicQueryAndSync 支持多环境拆分存储 + 独立验证
// 完善: periodicQueryAndSync 支持多环境拆分 + 动态状态 (confirm_envs 决定待确认/已确认)
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
					// 注意: TaskID 初始设置，但多环境时会重设
					if task.TaskID == "" {
						task.TaskID = fmt.Sprintf("%s-%s-%s", task.Service, task.Version, env) // 默认单 env TaskID
					}

					logrus.WithFields(logrus.Fields{
						"time":   time.Now().Format("2006-01-02 15:04:05"),
						"method": "periodicQueryAndSync",
						"task_id": task.TaskID,
						"service": task.Service,
						"version": task.Version,
						"env":     env,
						"environments_len": len(task.Environments),
					}).Debugf("准备处理任务 (environments: %v): %+v", task.Environments, task)

					// 完善: 支持多环境拆分存储 + 动态状态
					storedCount := 0
					if len(task.Environments) == 1 {
						// 单环境: 动态设置状态
						subEnv := task.Environments[0]
						isConfirmEnv := contains(a.cfg.Query.ConfirmEnvs, subEnv)
						task.ConfirmationStatus = "待确认"
						task.PopupSent = false
						if !isConfirmEnv {
							task.ConfirmationStatus = "已确认"
							task.PopupSent = true // 跳过弹窗
						}
						task.TaskID = fmt.Sprintf("%s-%s-%s", task.Service, task.Version, subEnv) // 确保 env-specific

						logrus.WithFields(logrus.Fields{
							"time":   time.Now().Format("2006-01-02 15:04:05"),
							"method": "periodicQueryAndSync",
							"task_id": task.TaskID,
							"sub_env": subEnv,
							"is_confirm_env": isConfirmEnv,
							"set_status": task.ConfirmationStatus,
							"set_popup_sent": task.PopupSent,
						}).Debugf("单环境任务状态设置: %s (popup_sent=%t)", task.ConfirmationStatus, task.PopupSent)

						if err := a.mongo.StoreTaskIfNotExistsEnv(*task, subEnv); err != nil {
							logrus.WithFields(logrus.Fields{
								"time":   time.Now().Format("2006-01-02 15:04:05"),
								"method": "periodicQueryAndSync",
								"task_id": task.TaskID,
							}).Debugf("任务已存在，跳过存储: %v", err)
						} else {
							storedCount++
							a.handleStoredTask(task.TaskID, subEnv, isConfirmEnv) // 传 isConfirmEnv 用于验证
						}
					} else {
						// 多环境: 拆分复制，每个 env 独立 TaskID、状态和存储
						logrus.WithFields(logrus.Fields{
							"time":   time.Now().Format("2006-01-02 15:04:05"),
							"method": "periodicQueryAndSync",
							"task_id": task.TaskID,
							"environments": task.Environments,
						}).Infof("检测到多环境任务，开始拆分存储 + 动态状态")

						for _, subEnv := range task.Environments {
							if !contains(a.cfg.EnvMapping.Mappings, subEnv) { // 确保 env 在映射中
								logrus.Debugf("跳过多环境子任务无效环境: %s", subEnv)
								continue
							}

							isConfirmEnv := contains(a.cfg.Query.ConfirmEnvs, subEnv)
							taskCopy := *task // 复制
							taskCopy.Environments = []string{subEnv} // 单 env
							taskCopy.Namespace = a.cfg.EnvMapping.Mappings[subEnv]
							taskCopy.ConfirmationStatus = "待确认"
							taskCopy.PopupSent = false
							if !isConfirmEnv {
								taskCopy.ConfirmationStatus = "已确认"
								taskCopy.PopupSent = true // 跳过弹窗
							}
							taskCopy.TaskID = fmt.Sprintf("%s-%s-%s", taskCopy.Service, taskCopy.Version, subEnv) // env-specific TaskID

							logrus.WithFields(logrus.Fields{
								"time":   time.Now().Format("2006-01-02 15:04:05"),
								"method": "periodicQueryAndSync",
								"sub_task_id": taskCopy.TaskID,
								"sub_env":     subEnv,
								"is_confirm_env": isConfirmEnv,
								"set_status":  taskCopy.ConfirmationStatus,
								"set_popup_sent": taskCopy.PopupSent,
							}).Debugf("多环境子任务状态设置: %s (popup_sent=%t)", taskCopy.ConfirmationStatus, taskCopy.PopupSent)

							if err := a.mongo.StoreTaskIfNotExistsEnv(taskCopy, subEnv); err != nil {
								logrus.WithFields(logrus.Fields{
									"time":   time.Now().Format("2006-01-02 15:04:05"),
									"method": "periodicQueryAndSync",
									"sub_task_id": taskCopy.TaskID,
								}).Debugf("子任务已存在，跳过存储: %v", err)
							} else {
								storedCount++
								a.handleStoredTask(taskCopy.TaskID, subEnv, isConfirmEnv) // 每个子任务独立验证
							}
						}

						logrus.WithFields(logrus.Fields{
							"time":       time.Now().Format("2006-01-02 15:04:05"),
							"method":     "periodicQueryAndSync",
							"task_id":    task.TaskID,
							"stored_sub": storedCount,
							"environments": task.Environments,
						}).Infof("多环境任务拆分完成，存储 %d 个子任务 (动态状态基于 confirm_envs)", storedCount)
					}

					totalNewTasks += storedCount
				}
			}
		}

		logrus.WithFields(logrus.Fields{
			"time":         time.Now().Format("2006-01-02 15:04:05"),
			"method":       "periodicQueryAndSync",
			"new_tasks":    totalNewTasks,
			"took":         time.Since(startTime),
		}).Infof("查询同步完成，本轮新增 %d 个任务 (含多环境拆分 + 动态状态)", totalNewTasks)
	}
}

// 新增: handleStoredTask 统一处理存储后验证 (提取复用)
// 完善: handleStoredTask 支持动态验证 (基于 isConfirmEnv 检查状态正确性)
func (a *Approval) handleStoredTask(taskID, env string, isConfirmEnv bool) {
	logrus.WithFields(logrus.Fields{
		"time":   time.Now().Format("2006-01-02 15:04:05"),
		"method": "handleStoredTask",
		"task_id": taskID,
		"env":     env,
		"is_confirm_env": isConfirmEnv,
	}).Info("=== 开始存储后状态更新和验证 (动态基于 confirm_envs) ===")

	expectedStatus := "待确认"
	expectedPopupSent := false
	if !isConfirmEnv {
		expectedStatus = "已确认"
		expectedPopupSent = true
	}

	// 立即更新状态为预期值（冗余确保）
	updateErr := a.mongo.UpdateTaskStatus(taskID, expectedStatus, "system")
	if updateErr != nil {
		logrus.WithFields(logrus.Fields{
			"time":   time.Now().Format("2006-01-02 15:04:05"),
			"method": "handleStoredTask",
			"task_id": taskID,
			"env":     env,
			"expected_status": expectedStatus,
		}).Errorf("存储后更新状态为 %s 失败: %v", expectedStatus, updateErr)
	} else {
		logrus.WithFields(logrus.Fields{
			"time":   time.Now().Format("2006-01-02 15:04:05"),
			"method": "handleStoredTask",
			"task_id": taskID,
			"env":     env,
			"expected_status": expectedStatus,
		}).Infof("状态更新为 %s 成功", expectedStatus)
	}

	// 查询最新数据验证
	latestTask, queryErr := a.mongo.GetTaskByID(taskID)
	if queryErr != nil {
		logrus.WithFields(logrus.Fields{
			"time":   time.Now().Format("2006-01-02 15:04:05"),
			"method": "handleStoredTask",
			"task_id": taskID,
			"env":     env,
		}).Errorf("存储后查询最新任务失败: %v", queryErr)
		return
	}
	if latestTask.ConfirmationStatus != expectedStatus || latestTask.PopupSent != expectedPopupSent {
		logrus.WithFields(logrus.Fields{
			"time":   time.Now().Format("2006-01-02 15:04:05"),
			"method": "handleStoredTask",
			"task_id": taskID,
			"env":     env,
			"current_status": latestTask.ConfirmationStatus,
			"current_popup_sent": latestTask.PopupSent,
			"expected_status": expectedStatus,
			"expected_popup_sent": expectedPopupSent,
		}).Errorf("最新状态不匹配预期，强制更新")
		// 强制重新更新
		a.mongo.UpdateTaskStatus(taskID, expectedStatus, "system")
		// 更新 PopupSent (需额外 UpdateOne)
		coll := a.mongo.GetClient().Database("cicd").Collection(fmt.Sprintf("tasks_%s", env))
		coll.UpdateOne(context.Background(), bson.M{"task_id": taskID}, bson.M{"$set": bson.M{"popup_sent": expectedPopupSent}})
		latestTask, _ = a.mongo.GetTaskByID(taskID)
	} else {
		logrus.WithFields(logrus.Fields{
			"time":   time.Now().Format("2006-01-02 15:04:05"),
			"method": "handleStoredTask",
			"task_id": taskID,
			"env":     env,
		}).Infof("状态验证成功: %s (popup_sent=%t)", latestTask.ConfirmationStatus, latestTask.PopupSent)
	}

	logrus.WithFields(logrus.Fields{
		"time":   time.Now().Format("2006-01-02 15:04:05"),
		"method": "handleStoredTask",
		"task_id": taskID,
		"env":     env,
		"is_confirm_env": isConfirmEnv,
		"latest_task_status": latestTask.ConfirmationStatus,
		"latest_task_popup_sent": latestTask.PopupSent,
		"latest_task_full": fmt.Sprintf("%+v", latestTask),
	}).Infof("=== 子任务存储成功，状态变更完成 (%s): status=%s, popup_sent=%t ===\nFull Latest Data: %+v",
		map[bool]string{true: "待确认", false: "已确认"}[isConfirmEnv], latestTask.ConfirmationStatus, latestTask.PopupSent, latestTask)
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