// 文件: agent.go (修复: 添加 import "k8s-cicd/approval/models" 以解决 undefined: models；
// 假设 models.DeployRequest 已添加 Environment 字段；其他逻辑保留)
package approval

import (
	"context"
	"fmt"
	"time"

	"k8s-cicd/approval/api"
	"k8s-cicd/approval/client"
	"k8s-cicd/approval/config"
	"k8s-cicd/approval/models" // 新增: 导入 models 以使用 DeployRequest
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

// 修改: periodicQueryAndSync 恢复 pushlist 方案：获取 services/envs，组合查询 (per service+env)，收集 allTasks，去重，存储+拆分
func (a *Approval) periodicQueryAndSync() {
	ticker := time.NewTicker(a.cfg.API.QueryInterval)
	defer ticker.Stop()

	for range ticker.C {
		startTime := time.Now()
		logrus.WithFields(logrus.Fields{
			"time":   time.Now().Format("2006-01-02 15:04:05"),
			"method": "periodicQueryAndSync",
		}).Debug("=== 开始新一轮查询同步循环 (pushlist 组合查询 + 多环境拆分) ===")

		// 1. 从 pushlist 获取已 push 服务和环境列表
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
			"count_svc": len(services),
			"count_env": len(envs),
		}).Infof("已获取服务列表: %v, 环境列表: %v (确认环境: %v)", services, envs, a.cfg.Query.ConfirmEnvs)

		if len(services) == 0 || len(envs) == 0 {
			logrus.WithFields(logrus.Fields{
				"time":   time.Now().Format("2006-01-02 15:04:05"),
				"method": "periodicQueryAndSync",
			}).Warn("暂无已 push 的服务/环境，跳过本次查询")
			continue
		}

		// 过滤 envs 仅确认环境
		confirmEnvs := make([]string, 0, len(a.cfg.Query.ConfirmEnvs))
		for _, env := range envs {
			if contains(a.cfg.Query.ConfirmEnvs, env) {
				confirmEnvs = append(confirmEnvs, env)
			}
		}
		if len(confirmEnvs) == 0 {
			logrus.WithFields(logrus.Fields{
				"time":   time.Now().Format("2006-01-02 15:04:05"),
				"method": "periodicQueryAndSync",
			}).Warn("无确认环境，跳过查询")
			continue
		}

		logrus.WithFields(logrus.Fields{
			"time":       time.Now().Format("2006-01-02 15:04:05"),
			"method":     "periodicQueryAndSync",
			"confirm_envs_filtered": confirmEnvs,
			"count_confirm_env": len(confirmEnvs),
		}).Infof("过滤后确认环境: %v", confirmEnvs)

		// 2. 组合查询：每个 service + 每个 confirm_env 调用 QueryTasks
		var allTasks []models.DeployRequest
		queriedCombinations := 0
		for _, service := range services {
			if service == "" {
				logrus.Warn("跳过空服务名")
				continue
			}
			for _, env := range confirmEnvs {
				logrus.WithFields(logrus.Fields{
					"time":   time.Now().Format("2006-01-02 15:04:05"),
					"method": "periodicQueryAndSync",
					"service": service,
					"env":     env,
				}).Debugf("调用 /query 接口查询任务 (组合: service=%s, env=%s)", service, env)

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

				// 设置临时 environment 字段 (用于去重)
				for i := range tasks {
					tasks[i].Environment = env
				}

				allTasks = append(allTasks, tasks...)
				queriedCombinations++
			}
		}

		logrus.WithFields(logrus.Fields{
			"time":              time.Now().Format("2006-01-02 15:04:05"),
			"method":            "periodicQueryAndSync",
			"queried_combos":    queriedCombinations,
			"total_tasks":       len(allTasks),
		}).Infof("组合查询完成，收集 %d 个任务 (从 %d 个 service-env 组合)", len(allTasks), queriedCombinations)

		if len(allTasks) == 0 {
			logrus.WithFields(logrus.Fields{
				"time":   time.Now().Format("2006-01-02 15:04:05"),
				"method": "periodicQueryAndSync",
			}).Debug("无任务数据，跳过去重和存储")
			continue
		}

		// 3. 全局去重：基于 service + environment + version + confirmation_status
		dedupedTasks := a.dedupeTasks(allTasks)
		logrus.WithFields(logrus.Fields{
			"time":         time.Now().Format("2006-01-02 15:04:05"),
			"method":       "periodicQueryAndSync",
			"before_dedup": len(allTasks),
			"after_dedup":  len(dedupedTasks),
		}).Infof("去重完成: %d -> %d (标准: service+environment+version+status)", len(allTasks), len(dedupedTasks))

		// 4. 处理去重后的任务：存储 + 验证 (自动拆分多环境)
		totalNewTasks := 0
		for i := range dedupedTasks {
			task := &dedupedTasks[i]
			task.CreatedAt = time.Now() // 设置 CreatedAt
			task.Namespace = a.cfg.EnvMapping.Mappings[task.Environment] // 使用 environment
			if task.TaskID == "" {
				task.TaskID = fmt.Sprintf("%s-%s-%s", task.Service, task.Version, task.Environment)
			}

			logrus.WithFields(logrus.Fields{
				"time":   time.Now().Format("2006-01-02 15:04:05"),
				"method": "periodicQueryAndSync",
				"task_id": task.TaskID,
				"service": task.Service,
				"version": task.Version,
				"env":     task.Environment,
				"environments_len": len(task.Environments),
			}).Debugf("准备处理去重任务 (environments: %v): %+v", task.Environments, task)

			// 支持多环境拆分存储 + 动态状态
			storedCount := 0
			if len(task.Environments) == 1 {
				subEnv := task.Environments[0]
				isConfirmEnv := contains(a.cfg.Query.ConfirmEnvs, subEnv)
				task.ConfirmationStatus = "待确认"
				task.PopupSent = false
				if !isConfirmEnv {
					task.ConfirmationStatus = "已确认"
					task.PopupSent = true
				}
				task.TaskID = fmt.Sprintf("%s-%s-%s", task.Service, task.Version, subEnv)

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
					a.handleStoredTask(task.TaskID, subEnv, isConfirmEnv)
				}
			} else {
				// 多环境拆分 (自动拆解)
				for _, subEnv := range task.Environments {
					if _, ok := a.cfg.EnvMapping.Mappings[subEnv]; !ok {
						continue
					}
					taskCopy := *task
					taskCopy.Environments = []string{subEnv}
					taskCopy.Environment = subEnv // 同步设置
					isConfirmEnv := contains(a.cfg.Query.ConfirmEnvs, subEnv)
					taskCopy.ConfirmationStatus = "待确认"
					taskCopy.PopupSent = false
					if !isConfirmEnv {
						taskCopy.ConfirmationStatus = "已确认"
						taskCopy.PopupSent = true
					}
					taskCopy.TaskID = fmt.Sprintf("%s-%s-%s", taskCopy.Service, taskCopy.Version, subEnv)

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
						a.handleStoredTask(taskCopy.TaskID, subEnv, isConfirmEnv)
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

		logrus.WithFields(logrus.Fields{
			"time":         time.Now().Format("2006-01-02 15:04:05"),
			"method":       "periodicQueryAndSync",
			"new_tasks":    totalNewTasks,
			"took":         time.Since(startTime),
		}).Infof("查询同步完成，本轮新增 %d 个任务 (pushlist 组合查询 + 去重 + 多环境拆分)", totalNewTasks)
	}
}

// 新增: dedupeTasks 全局去重任务列表 (基于 service + environment + version + confirmation_status)
func (a *Approval) dedupeTasks(tasks []models.DeployRequest) []models.DeployRequest {
	dedupMap := make(map[string]models.DeployRequest)
	for _, task := range tasks {
		// 默认 status 如果为空
		status := task.ConfirmationStatus
		if status == "" {
			status = "待确认" // 假设默认
		}
		key := fmt.Sprintf("%s-%s-%s-%s", task.Service, task.Environment, task.Version, status)
		if _, exists := dedupMap[key]; !exists {
			dedupMap[key] = task
		} else {
			logrus.WithFields(logrus.Fields{
				"time":   time.Now().Format("2006-01-02 15:04:05"),
				"method": "dedupeTasks",
				"key":    key,
				"service": task.Service,
				"env":     task.Environment,
				"version": task.Version,
				"status":  status,
			}).Debugf("检测到重复任务，跳过 (标准: service+env+version+status)")
		}
	}

	deduped := make([]models.DeployRequest, 0, len(dedupMap))
	for _, task := range dedupMap {
		deduped = append(deduped, task)
	}

	return deduped
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
		// 更新 PopupSent
		coll := a.mongo.GetClient().Database("cicd").Collection(fmt.Sprintf("tasks_%s", env))
		ctx := context.Background()
		coll.UpdateOne(ctx, bson.M{"task_id": taskID}, bson.M{"$set": bson.M{"popup_sent": expectedPopupSent}})
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