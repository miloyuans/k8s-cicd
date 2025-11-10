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
// 文件: approval/agent.go 中的 periodicQueryAndSync 函数完整代码（变更：移除 confirmEnvs 过滤，使用所有 envs 从 push_data 进行查询，传入 QueryTasks(service, envs) 支持所有环境查询；在 handleStoredTask 调用时，动态检查 env 是否在 ConfirmEnvs 来决定 isConfirmEnv=true/false，确保非白名单环境直接设为已确认而不漏查/漏存；调整日志以反映所有环境查询）
// 文件: approval/agent.go 中的 periodicQueryAndSync 函数完整代码（最新优化: 查询所有 envs，白名单只用于 isConfirmEnv 判断）
func (a *Approval) periodicQueryAndSync() {
	ticker := time.NewTicker(a.cfg.API.QueryInterval)
	defer ticker.Stop()

	for range ticker.C {
		startTime := time.Now()
		logrus.WithFields(logrus.Fields{
			"time":   time.Now().Format("2006-01-02 15:04:05"),
			"method": "periodicQueryAndSync",
		}).Debug("=== 开始新一轮查询同步循环 (pushlist 组合查询 + 多环境拆分) ===")

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
		}).Infof("已获取服务列表: %v, 环境列表: %v (确认环境用于弹窗: %v)", services, envs, a.cfg.Query.ConfirmEnvs)

		if len(services) == 0 || len(envs) == 0 {
			logrus.WithFields(logrus.Fields{
				"time":   time.Now().Format("2006-01-02 15:04:05"),
				"method": "periodicQueryAndSync",
			}).Warn("暂无已 push 的服务/环境，跳过本次查询")
			continue
		}

		logrus.WithFields(logrus.Fields{
			"time":       time.Now().Format("2006-01-02 15:04:05"),
			"method":     "periodicQueryAndSync",
			"queried_envs": envs,
			"count_env": len(envs),
		}).Infof("查询所有环境: %v (ConfirmEnvs 只用于弹窗判断)", envs)

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
				"envs":    envs,
			}).Debugf("调用 /query 接口查询任务 (服务=%s, 多环境: %v)", service, envs)

			tasks, err := a.queryClient.QueryTasks(service, envs)
			if err != nil {
				logrus.WithFields(logrus.Fields{
					"time":   time.Now().Format("2006-01-02 15:04:05"),
					"method": "periodicQueryAndSync",
					"service": service,
					"envs":    envs,
				}).Errorf("查询任务失败: %v", err)
				continue
			}

			logrus.WithFields(logrus.Fields{
				"time":   time.Now().Format("2006-01-02 15:04:05"),
				"method": "periodicQueryAndSync",
				"service": service,
				"envs":    envs,
				"task_count": len(tasks),
			}).Debugf("查询到 %d 个任务 (多环境合并)", len(tasks))

			allTasks = append(allTasks, tasks...)
			queriedServices++
		}

		logrus.WithFields(logrus.Fields{
			"time":             time.Now().Format("2006-01-02 15:04:05"),
			"method":           "periodicQueryAndSync",
			"queried_services": queriedServices,
			"total_tasks":      len(allTasks),
		}).Infof("组合查询完成，收集 %d 个任务 (从 %d 个服务，所有环境查询)", len(allTasks), queriedServices)

		if len(allTasks) == 0 {
			logrus.WithFields(logrus.Fields{
				"time":   time.Now().Format("2006-01-02 15:04:05"),
				"method": "periodicQueryAndSync",
			}).Debug("无任务数据，跳过去重和存储")
			continue
		}

		dedupedTasks := a.dedupeTasks(allTasks)
		logrus.WithFields(logrus.Fields{
			"time":         time.Now().Format("2006-01-02 15:04:05"),
			"method":       "periodicQueryAndSync",
			"deduped_count": len(dedupedTasks),
			"duplicates":   len(allTasks) - len(dedupedTasks),
		}).Infof("去重完成: %d 个唯一任务 (去除 %d 个重复)", len(dedupedTasks), len(allTasks)-len(dedupedTasks))

		totalNewTasks := 0
		for _, task := range dedupedTasks {
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

			if task.ConfirmationStatus == "" {
				task.ConfirmationStatus = "待确认"
			}

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

			isMainConfirm := contains(a.cfg.Query.ConfirmEnvs, mainEnv)
			a.handleStoredTask(task.TaskID, mainEnv, isMainConfirm)
			totalNewTasks++

			storedCount := 0
			for _, subEnv := range task.Environments {
				if subEnv == mainEnv {
					continue
				}
				taskCopy := task
				taskCopy.Environment = subEnv
				taskCopy.TaskID = uuid.New().String()

				if err := a.mongo.StoreTask(&taskCopy, isSubConfirm); err != nil {
					logrus.WithFields(logrus.Fields{
						"time":   time.Now().Format("2006-01-02 15:04:05"),
						"method": "periodicQueryAndSync",
						"task_id": taskCopy.TaskID,
						"sub_env": subEnv,
					}).Errorf("存储子任务失败: %v", err)
				} else {
					isSubConfirm := contains(a.cfg.Query.ConfirmEnvs, subEnv)
					storedCount++
					a.handleStoredTask(taskCopy.TaskID, subEnv, isSubConfirm)
				}
			}

			totalNewTasks += storedCount
		}

		logrus.WithFields(logrus.Fields{
			"time":         time.Now().Format("2006-01-02 15:04:05"),
			"method":       "periodicQueryAndSync",
			"new_tasks":    totalNewTasks,
			"took":         time.Since(startTime),
		}).Infof("查询同步完成，本轮新增 %d 个任务 (pushlist 组合查询 + 去重 + 多环境拆分，所有环境覆盖)", totalNewTasks)
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
        "time":            time.Now().Format("2006-01-02 15:04:05"),
        "method":          "handleStoredTask",
        "task_id":         taskID,
        "env":             env,
        "is_confirm_env":  isConfirmEnv,
    }).Info("=== 开始存储后状态更新和验证 (动态基于 confirm_envs) ===")

    expectedStatus := "待确认"
    expectedPopupSent := false
    if !isConfirmEnv {
        expectedStatus = "已确认"
        expectedPopupSent = true
    }

    // ---------- 1. 直接使用新方法更新 ----------
    popupSent := expectedPopupSent
    if err := a.mongo.UpdateTaskStatus(taskID, expectedStatus, "system", env, &popupSent); err != nil {
        logrus.WithFields(logrus.Fields{
            "time":            time.Now().Format("2006-01-02 15:04:05"),
            "method":          "handleStoredTask",
            "task_id":         taskID,
            "env":             env,
            "expected_status": expectedStatus,
        }).Errorf("存储后更新状态为 %s 失败: %v", expectedStatus, err)
    } else {
        logrus.WithFields(logrus.Fields{
            "time":            time.Now().Format("2006-01-02 15:04:05"),
            "method":          "handleStoredTask",
            "task_id":         taskID,
            "env":             env,
            "expected_status": expectedStatus,
        }).Infof("状态更新为 %s 成功", expectedStatus)
    }

    // ---------- 2. 查询最新任务验证 ----------
    latestTask, queryErr := a.mongo.GetTaskByID(taskID, env)
    if queryErr != nil {
        logrus.WithFields(logrus.Fields{
            "time":    time.Now().Format("2006-01-02 15:04:05"),
            "method":  "handleStoredTask",
            "task_id": taskID,
            "env":     env,
        }).Errorf("存储后查询最新任务失败: %v", queryErr)
        return
    }

    if latestTask.ConfirmationStatus != expectedStatus || latestTask.PopupSent != expectedPopupSent {
        // 记录异常但不再强制更新（防止循环）
        logrus.WithFields(logrus.Fields{
            "time":               time.Now().Format("2006-01-02 15:04:05"),
            "method":             "handleStoredTask",
            "task_id":            taskID,
            "env":                env,
            "current_status":     latestTask.ConfirmationStatus,
            "current_popup_sent": latestTask.PopupSent,
            "expected_status":    expectedStatus,
            "expected_popup_sent": expectedPopupSent,
        }).Warn("最新状态不匹配预期，已记录但不再强制更新")
    } else {
        logrus.WithFields(logrus.Fields{
            "time":    time.Now().Format("2006-01-02 15:04:05"),
            "method":  "handleStoredTask",
            "task_id": taskID,
            "env":     env,
        }).Infof("状态验证成功: %s (popup_sent=%t)", latestTask.ConfirmationStatus, latestTask.PopupSent)
    }

    // ---------- 3. 最终日志 ----------
    logrus.WithFields(logrus.Fields{
        "time":               time.Now().Format("2006-01-02 15:04:05"),
        "method":             "handleStoredTask",
        "task_id":            taskID,
        "env":                env,
        "is_confirm_env":     isConfirmEnv,
        "latest_task_status": latestTask.ConfirmationStatus,
        "latest_task_popup_sent": latestTask.PopupSent,
        "latest_task_full":   fmt.Sprintf("%+v", latestTask),
    }).Infof("=== 子任务存储成功，状态变更完成 (%s): status=%s, popup_sent=%t ===\nFull Latest Data: %+v",
        map[bool]string{true: "待确认", false: "已确认"}[isConfirmEnv],
        latestTask.ConfirmationStatus, latestTask.PopupSent, latestTask)
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