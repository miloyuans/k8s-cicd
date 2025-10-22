package task

import (
	"fmt"
	"strings"
	"time"

	"k8s-cicd/agent/api"
	"k8s-cicd/agent/config"
	"k8s-cicd/agent/client"
	"k8s-cicd/agent/kubernetes"
	"k8s-cicd/agent/models"
	"k8s-cicd/agent/telegram"

	"github.com/fatih/color"
	"github.com/sirupsen/logrus"
)

// executeTask 执行单个部署任务
func executeTask(cfg *config.Config, redis *client.RedisClient, k8s *kubernetes.K8sClient, apiPusher *api.APIPusher, task models.Task, botMgr *telegram.BotManager) error {
	green := color.New(color.FgGreen).SprintFunc()
	yellow := color.New(color.FgYellow).SprintFunc()
	red := color.New(color.FgRed).SprintFunc()
	cyan := color.New(color.FgCyan).SprintFunc()

	// ================================
	// 步骤1：记录任务开始
	// ================================
	logrus.Infof("%s 开始执行部署任务:\n"+
		"  服务: %s\n"+
		"  环境: %s\n"+
		"  版本: %s\n"+
		"  用户: %s\n"+
		"  等待超时: %v",
		green("🚀"), task.Service, task.Environments[0], task.Version, task.User, cfg.Deploy.WaitTimeout)

	// ================================
	// 步骤2：获取旧版本
	// ================================
	oldVersion, err := k8s.GetCurrentImage(task.Service)
	if err != nil {
		logrus.Warnf("%s 获取当前镜像失败，使用默认值: %v", yellow("⚠️"), err)
		oldVersion = "unknown"
	}
	logrus.Infof("%s 当前镜像版本: %s", cyan("📋"), oldVersion)

	// ================================
	// 步骤3：滚动更新
	// ================================
	newImage := fmt.Sprintf("%s:%s", strings.ToLower(task.Service), task.Version)
	err = k8s.UpdateDeploymentImage(task.Service, newImage)
	if err != nil {
		// 更新失败
		redis.UpdateTaskStatus(task.Service, task.Version, task.Environments[0], task.User, "failure")
		botMgr.SendNotification(task.Service, task.Environments[0], task.User, oldVersion, task.Version, false)
		return fmt.Errorf("更新失败: %v", err)
	}

	logrus.Infof("%s Deployment镜像更新成功: %s -> %s", green("✅"), oldVersion, newImage)

	// ================================
	// 步骤4：等待新版本就绪（使用配置超时）
	// ================================
	logrus.Infof("%s 等待新版本就绪（超时: %v）", cyan("⏳"), cfg.Deploy.WaitTimeout)
	
	startTime := time.Now()
	success := true
	err = k8s.WaitForDeploymentReady(task.Service, task.Version)
	if err != nil {
		logrus.Errorf("%s 部署超时（%v）", red("💥"), cfg.Deploy.WaitTimeout)
		success = false
		
		// 执行回滚
		logrus.Infof("%s 开始回滚（超时: %v）", yellow("🔄"), cfg.Deploy.RollbackTimeout)
		rollbackErr := k8s.RollbackDeployment(task.Service)
		if rollbackErr != nil {
			logrus.Errorf("%s 回滚失败: %v", red("🔙"), rollbackErr)
		} else {
			logrus.Infof("%s 回滚操作成功完成", green("🔙"))
		}
	} else {
		logrus.Infof("%s 新版本就绪，耗时: %v", green("✅"), time.Since(startTime))
	}

	// ================================
	// 步骤5：发送Telegram通知
	// ================================
	notifyErr := botMgr.SendNotification(task.Service, task.Environments[0], task.User, oldVersion, task.Version, success)
	if notifyErr != nil {
		logrus.Errorf("%s 通知发送失败: %v", red("📱"), notifyErr)
	} else {
		logrus.Infof("%s Telegram通知发送成功", green("📱"))
	}

	// ================================
	// 步骤6：更新Redis状态
	// ================================
	status := "success"
	if !success {
		status = "failure"
	}
	redisErr := redis.UpdateTaskStatus(task.Service, task.Version, task.Environments[0], task.User, status)
	if redisErr != nil {
		logrus.Errorf("%s Redis状态更新失败: %v", red("💾"), redisErr)
	} else {
		logrus.Infof("%s Redis状态更新成功: %s", green("💾"), status)
	}

	// ================================
	// 步骤7：推送API
	// ================================
	deployStatus := models.DeploymentStatus{
		OldVersion: oldVersion,
		NewVersion: task.Version,
		IsSuccess:  success,
		Message:    fmt.Sprintf("部署%s，耗时%v", map[bool]string{true: "成功", false: "失败"}[success], time.Since(startTime)),
	}
	go apiPusher.pushDeployments([]models.DeploymentStatus{deployStatus})

	// ================================
	// 步骤8：任务总结
	// ================================
	if success {
		logrus.Infof("%s 🎉 部署成功:\n"+
			"  服务: %s\n"+
			"  耗时: %v\n"+
			"  旧版本: %s -> 新版本: %s",
			green("🎉"), task.Service, time.Since(startTime), oldVersion, task.Version)
	} else {
		logrus.Infof("%s 💥 部署失败（已回滚）:\n"+
			"  服务: %s\n"+
			"  耗时: %v\n"+
			"  回滚至: %s",
			red("💥"), task.Service, time.Since(startTime), oldVersion)
	}

	return nil
}