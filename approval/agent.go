// 文件: agent.go (修改: periodicQueryAndSync 重新设计数据获取：直接使用 cfg.Query.ConfirmEnvs 获取环境列表；移除服务列表获取；
// 对于每个环境，先查询 Mongo GetPendingTasks 检查是否有 pending 任务（confirmation_status="待确认" + popup_sent=false），
// 如果有，则调用 QueryTasks("", []string{env}) 同步所有服务任务（假设 API 支持 service="" 查询所有服务）;
// 其他逻辑保留：存储、拆分、多环境支持、动态状态设置、handleStoredTask 验证)
package approval

import (
	"context"
	"fmt"
	"time"

	"k8s-cicd/approval/api"
	"k8s-cicd/approval/client"
	"k8s-cicd/approval/config"
	"k8s-cicd/approval/telegram"

	"github.com/fatih/color"
	"github.com/sirupsen/logrus"
	"go.mongodb.org/mongo-driver/bson"
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

// 重新设计: periodicQueryAndSync 使用配置文件 ConfirmEnvs 作为环境列表；
// 无服务列表：直接传递空 service 查询所有；
// 先检查 Mongo pending 任务（GetPendingTasks），仅当有 pending 时才查询 API 同步（避免无谓查询）
func (a *Approval) periodicQueryAndSync() {
	ticker := time.NewTicker(a.cfg.API.QueryInterval)
	defer ticker.Stop()

	for range ticker.C {
		startTime := time.Now()
		logrus.WithFields(logrus.Fields{
			"time":   time.Now().Format("2006-01-02 15:04:05"),
			"method": "periodicQueryAndSync",
		}).Debug("=== 开始新一轮查询同步循环 (基于 ConfirmEnvs + pending 检查) ===")

		// 1. 直接使用配置文件环境列表（仅确认环境）
		envs := a.cfg.Query.ConfirmEnvs
		logrus.WithFields(logrus.Fields{
			"time":     time.Now().Format("2006-01-02 15:04:05"),
			"method":   "periodicQueryAndSync",
			"envs":     envs,
			"confirm_envs": a.cfg.Query.ConfirmEnvs,
			"count_env": len(envs),
		}).Infof("使用配置文件环境列表: %v (仅确认环境)", envs)

		if len(envs) == 0 {
			logrus.WithFields(logrus.Fields{
				"time":   time.Now().Format("2006-01-02 15:04:05"),
				"method": "periodicQueryAndSync",
			}).Warn("配置文件无确认环境，跳过本次查询")
			continue
		}

		// 2. 并发查询：针对每个环境，先检查 pending 任务
		totalNewTasks := 0
		for _, env := range envs {
			// 检查当前环境是否有 pending 任务（confirmation_status="待确认" + popup_sent=false）
			pendingTasks, err := a.mongo.GetPendingTasks(env)
			if err != nil {
				logrus.WithFields(logrus.Fields{
					"time":   time.Now().Format("2006-01-02 15:04:05"),
					"method": "periodicQueryAndSync",
					"env":    env,
				}).Errorf("检查 %s pending 任务失败: %v", env, err)
				continue
			}

			pendingCount := len(pendingTasks)
			logrus.WithFields(logrus.Fields{
				"time":   time.Now().Format("2006-01-02 15:04:05"),
				"method": "periodicQueryAndSync",
				"env":    env,
				"pending_count": pendingCount,
			}).Debugf("%s 环境 pending 任务数: %d", env, pendingCount)

			// 仅当有 pending 任务时，才查询 API 同步（判断当前环境是否有服务处于 pending 状态）
			if pendingCount == 0 {
				logrus.WithFields(logrus.Fields{
					"time":   time.Now().Format("2006-01-02 15:04:05"),
					"method": "periodicQueryAndSync",
					"env":    env,
				}).Debugf("无 pending 任务，跳过 %s 环境 API 查询", env)
				continue
			}

			logrus.WithFields(logrus.Fields{
				"time":   time.Now().Format("2006-01-02 15:04:05"),
				"method": "periodicQueryAndSync",
				"env":    env,
			}).Debugf("有 %d pending 任务，调用 /query 接口同步 %s 环境 (service='')", pendingCount, env)

			// 查询 API：传递空 service 查询所有服务，环境作为唯一值
			tasks, err := a.queryClient.QueryTasks("", []string{env})
			if err != nil {
				logrus.WithFields(logrus.Fields{
					"time":   time.Now().Format("2006-01-02 15:04:05"),
					"method": "periodicQueryAndSync",
					"env":    env,
				}).Errorf("查询任务失败: %v", err)
				continue
			}

			logrus.WithFields(logrus.Fields{
				"time":   time.Now().Format("2006-01-02 15:04:05"),
				"method": "periodicQueryAndSync",
				"env":    env,
				"task_count": len(tasks),
			}).Debugf("查询到 %d 个任务 (service='', env=%s)", len(tasks), env)

			storedCount := 0
			for i := range tasks {
				task := &tasks[i]
				task.CreatedAt = time.Now() // 设置 CreatedAt (查询顺序 = 存储顺序)
				task.Namespace = a.cfg.EnvMapping.Mappings[env]
				// TaskID 初始设置，但多环境时会重设
				if task.TaskID == "" {
					task.TaskID = fmt.Sprintf("%s-%s-%s", task.Service, task.Version, env) // 默认 env-specific
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

				// 支持多环境拆分存储 + 动态状态
				if len(task.Environments) == 1 {
					// 单环境: 动态设置状态
					subEnv := task.Environments[0]
					isConfirmEnv := true // 既然在 ConfirmEnvs 中，总是 true
					task.ConfirmationStatus = "待确认"
					task.PopupSent = false
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
						a.handleStoredTask(task.TaskID, subEnv, isConfirmEnv) // 每个任务独立验证
					}
				} else {
					// 多环境: 拆分每个 subEnv
					for _, subEnv := range task.Environments {
						if _, ok := a.cfg.EnvMapping.Mappings[subEnv]; !ok {
							continue // 跳过无效环境
						}
						taskCopy := *task
						taskCopy.Environments = []string{subEnv}
						isConfirmEnv := contains(a.cfg.Query.ConfirmEnvs, subEnv)
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
			}

			totalNewTasks += storedCount
			logrus.WithFields(logrus.Fields{
				"time":   time.Now().Format("2006-01-02 15:04:05"),
				"method": "periodicQueryAndSync",
				"env":    env,
				"stored_count": storedCount,
			}).Infof("%s 环境同步完成，新增 %d 个任务", env, storedCount)
		}

		logrus.WithFields(logrus.Fields{
			"time":         time.Now().Format("2006-01-02 15:04:05"),
			"method":       "periodicQueryAndSync",
			"new_tasks":    totalNewTasks,
			"took":         time.Since(startTime),
		}).Infof("查询同步完成，本轮新增 %d 个任务 (基于 pending 检查 + service='' 查询)", totalNewTasks)
	}
}

// 新增: handleStoredTask 统一处理存储后验证 (提取复用)
// 完善: handleStoredTask 支持动态验证 (基于 isConfirmEnv 检查状态正确性)
// 完善: handleStoredTask 支持动态验证 (基于 isConfirmEnv 检查状态正确性) - line 319 修复: 使用 context.Background() 和 bson.M
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
		// 更新 PopupSent (需额外 UpdateOne) - 修复: 使用 context.Background() 和 bson.M
		coll := a.mongo.GetClient().Database("cicd").Collection(fmt.Sprintf("tasks_%s", env))
		ctx := context.Background() // 修复: context.Background()
		coll.UpdateOne(ctx, bson.M{"task_id": taskID}, bson.M{"$set": bson.M{"popup_sent": expectedPopupSent}}) // 修复: bson.M
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