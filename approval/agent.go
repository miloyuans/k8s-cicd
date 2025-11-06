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
		start := time.Now()
		services, envs, err := a.mongo.GetPushedServicesAndEnvs()
		if err != nil || len(services) == 0 || len(envs) == 0 {
			logrus.Warn("无 push 数据，跳过本次同步")
			continue
		}

		// 1. 逐 service + 逐 env 调用 QueryTasks（单环境）
		var allTasks []models.DeployRequest
		for _, svc := range services {
			if svc == "" {
				continue
			}
			for _, env := range envs {
				tasks, err := a.queryClient.QueryTasks(svc, []string{env}) // 单环境
				if err != nil {
					logrus.Errorf("查询 %s@%s 失败: %v", svc, env, err)
					continue
				}
				// 为返回的任务手动填充 Environment（防止上游遗漏）
				for i := range tasks {
					tasks[i].Environment = env
				}
				allTasks = append(allTasks, tasks...)
			}
		}

		if len(allTasks) == 0 {
			logrus.Debug("本次无任务")
			continue
		}

		// 2. 去重（service+env+version+status）
		dedup := a.dedupeTasks(allTasks)

		// 3. 存储 + 状态处理（ConfirmEnvs 只决定是否弹窗）
		newCnt := 0
		for _, t := range dedup {
			if t.TaskID == "" {
				t.TaskID = uuid.New().String()
			}
			if t.ConfirmationStatus == "" {
				t.ConfirmationStatus = "待确认"
			}
			// 主环境
			if len(t.Environments) > 0 && t.Environment == "" {
				t.Environment = t.Environments[0]
			}
			mainEnv := t.Environment
			_ = a.mongo.StoreTask(&t)
			isConfirm := contains(a.cfg.Query.ConfirmEnvs, mainEnv)
			a.handleStoredTask(t.TaskID, mainEnv, isConfirm)
			newCnt++

			// 子环境拆分
			for _, sub := range t.Environments {
				if sub == mainEnv {
					continue
				}
				subTask := t
				subTask.Environment = sub
				subTask.TaskID = uuid.New().String()
				_ = a.mongo.StoreTask(&subTask)
				a.handleStoredTask(subTask.TaskID, sub, contains(a.cfg.Query.ConfirmEnvs, sub))
				newCnt++
			}
		}
		logrus.Infof("同步完成，本轮新增 %d 条记录 (took %s)", newCnt, time.Since(start))
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