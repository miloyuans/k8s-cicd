// 文件: agent.go (完整文件，修复: 移除未使用的 "strings" import；更新 getTaskCollection 调用为 GetTaskCollection；其他逻辑保留，确保 import "k8s-cicd/approval/client" 存在以访问 MongoClient 方法)
// 注意: 假设文件顶部 import 已包含 "k8s-cicd/approval/client"，如果未有则添加
package approval

import (
	"context"
	"fmt"
	"time"

	"k8s-cicd/approval/api"
	"k8s-cicd/approval/client" // 确保导入 client 以访问 GetTaskCollection
	"k8s-cicd/approval/config"
	"k8s-cicd/approval/models" // 确保导入 models 以使用 DeployRequest 和 Environment
	"k8s-cicd/approval/telegram"

	"github.com/google/uuid"
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
// 文件: approval/agent.go 中的 periodicQueryAndSync 函数完整代码（变更：每个 service 只查询一次，传所有 confirmEnvs 作为 environments[] 到 QueryTasks，支持 API 多环境查询；移除手动设置 task.Environment = env，因为假设 API 返回的任务已有 Environment 字段；调整日志和 queriedCombinations 为服务数）
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

		// 过滤 envs 仅确认环境（用于查询）
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

		// 2. 组合查询：每个 service 调用一次 QueryTasks，传入所有 confirmEnvs（API 支持多环境）
		var allTasks []models.DeployRequest
		queriedServices := 0
		for _, service := range services {
			if service == "" {
				logrus.Warn("跳过空服务名")
				continue
			}
			logrus.WithFields(logrus.Fields{
				"time":   time.Now().Format("2006-01-02 15:04:05"),
				"method": "periodicQueryAndSync",
				"service": service,
				"envs":    confirmEnvs,
			}).Debugf("调用 /query 接口查询任务 (服务=%s, 多环境: %v)", service, confirmEnvs)

			tasks, err := a.queryClient.QueryTasks(service, confirmEnvs) // 变更: 传入所有 confirmEnvs，支持 API 多环境返回
			if err != nil {
				logrus.WithFields(logrus.Fields{
					"time":   time.Now().Format("2006-01-02 15:04:05"),
					"method": "periodicQueryAndSync",
					"service": service,
					"envs":    confirmEnvs,
				}).Errorf("查询任务失败: %v", err)
				continue
			}

			logrus.WithFields(logrus.Fields{
				"time":   time.Now().Format("2006-01-02 15:04:05"),
				"method": "periodicQueryAndSync",
				"service": service,
				"envs":    confirmEnvs,
				"task_count": len(tasks),
			}).Debugf("查询到 %d 个任务 (多环境合并)", len(tasks))

			// 变更: 移除手动设置 task.Environment = env，因为 API 返回的任务应已有 Environment 字段（基于查询 environments 过滤）

			allTasks = append(allTasks, tasks...)
			queriedServices++
		}

		logrus.WithFields(logrus.Fields{
			"time":             time.Now().Format("2006-01-02 15:04:05"),
			"method":           "periodicQueryAndSync",
			"queried_services": queriedServices,
			"total_tasks":      len(allTasks),
		}).Infof("组合查询完成，收集 %d 个任务 (从 %d 个服务，多环境查询)", len(allTasks), queriedServices)

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
			"deduped_count": len(dedupedTasks),
			"duplicates":   len(allTasks) - len(dedupedTasks),
		}).Infof("去重完成: %d 个唯一任务 (去除 %d 个重复)", len(dedupedTasks), len(allTasks)-len(dedupedTasks))

		// 4. 存储/更新任务（新增生成 TaskID，Upsert）
		totalNewTasks := 0
		for _, task := range dedupedTasks {
			// 生成唯一 TaskID（如果为空）
			if task.TaskID == "" {
				task.TaskID = uuid.New().String()
				logrus.WithFields(logrus.Fields{
					"time":   time.Now().Format("2006-01-02 15:04:05"),
					"method": "periodicQueryAndSync",
					"service": task.Service,
					"env":     task.Environment,
					"version": task.Version,
					"task_id": task.TaskID,
				}).Debugf("生成新 TaskID: %s", task.TaskID)
			}

			// 默认 status 如果为空
			if task.ConfirmationStatus == "" {
				task.ConfirmationStatus = "待确认"
			}

			// 存储主任务（使用第一个环境作为主）
			if len(task.Environments) > 0 && task.Environment == "" {
				task.Environment = task.Environments[0]
			}
			mainEnv := task.Environment
			if err := a.mongo.StoreTask(&task); err != nil {
				logrus.WithFields(logrus.Fields{
					"time":   time.Now().Format("2006-01-02 15:04:05"),
					"method": "periodicQueryAndSync",
					"task_id": task.TaskID,
					"main_env": mainEnv,
				}).Errorf("存储主任务失败: %v", err)
				continue
			}

			// 处理主环境（确认环境）
			a.handleStoredTask(task.TaskID, mainEnv, true)
			totalNewTasks++

			// 5. 多环境拆分：为 Environments 中的其他环境创建子任务（非确认环境设为已确认）
			storedCount := 0
			for _, subEnv := range task.Environments {
				if subEnv == mainEnv {
					continue // 跳过主环境
				}
				taskCopy := task
				taskCopy.Environment = subEnv
				taskCopy.TaskID = uuid.New().String() // 子任务独立 ID

				if err := a.mongo.StoreTask(&taskCopy); err != nil {
					logrus.WithFields(logrus.Fields{
						"time":   time.Now().Format("2006-01-02 15:04:05"),
						"method": "periodicQueryAndSync",
						"task_id": taskCopy.TaskID,
						"sub_env": subEnv,
					}).Errorf("存储子任务失败: %v", err)
				} else {
					storedCount++
					a.handleStoredTask(taskCopy.TaskID, subEnv, false) // 非确认环境
				}
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
		// 更新 PopupSent (使用 GetTaskCollection)
		coll := a.mongo.GetTaskCollection(env)
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