package task

import (
	"fmt"
	"strings"
	"time"

	"k8s-cicd/agent/config"
	"k8s-cicd/agent/client"
	"k8s-cicd/agent/kubernetes"
	"k8s-cicd/agent/models"
	"k8s-cicd/agent/telegram"

	"github.com/fatih/color"
	"github.com/sirupsen/logrus"
)

// executeTask 执行单个部署任务的完整流程
// 功能：
// 1. 更新K8s Deployment镜像
// 2. 等待部署就绪并健康检查
// 3. 失败时自动回滚
// 4. 发送Telegram通知
// 5. 更新Redis任务状态
func executeTask(cfg *config.Config, redis *client.RedisClient, k8s *kubernetes.K8sClient, task models.Task, botMgr *telegram.BotManager) error {
	// ================================
	// 步骤1：初始化彩色日志函数
	// ================================
	green := color.New(color.FgGreen).SprintFunc()   // 绿色：成功信息
	yellow := color.New(color.FgYellow).SprintFunc() // 黄色：警告信息
	red := color.New(color.FgRed).SprintFunc()       // 红色：错误信息
	cyan := color.New(color.FgCyan).SprintFunc()     // 青色：信息输出

	// ================================
	// 步骤2：记录任务开始日志
	// ================================
	logrus.Infof("%s 开始执行部署任务:\n"+
		"  服务: %s\n"+
		"  环境: %s\n"+
		"  版本: %s\n"+
		"  用户: %s\n"+
		"  任务ID: %s",
		green("🚀"), task.Service, strings.Join(task.Environments, ","), task.Version, task.User, task.ID)

	// ================================
	// 步骤3：获取当前Deployment的旧镜像版本
	// ================================
	var oldVersion string
	oldVersion, err := k8s.GetCurrentImage(task.Service)
	if err != nil {
		logrus.Warnf("%s 获取当前镜像失败，使用默认值: %v", yellow("⚠️"), err)
		oldVersion = "unknown"
	}
	logrus.Infof("%s 当前镜像版本: %s", cyan("📋"), oldVersion)

	// ================================
	// 步骤4：构造新的镜像名称
	// ================================
	newImage := fmt.Sprintf("%s:%s", strings.ToLower(task.Service), task.Version)
	logrus.Infof("%s 目标镜像版本: %s", cyan("🎯"), newImage)

	// ================================
	// 步骤5：更新Deployment镜像
	// ================================
	err = k8s.UpdateDeploymentImage(task.Service, newImage)
	if err != nil {
		// 更新失败：记录错误、更新Redis状态、发送失败通知
		logrus.Errorf("%s Deployment镜像更新失败: %v", red("❌"), err)
		
		// 5.1 更新Redis任务状态为failure
		redisErr := redis.UpdateTaskStatus(task.Service, task.Version, task.Environments[0], task.User, "failure")
		if redisErr != nil {
			logrus.Errorf("%s Redis状态更新失败: %v", red("💾"), redisErr)
		}
		
		// 5.2 发送Telegram失败通知
		notifyErr := botMgr.SendNotification(task.Service, task.Environments[0], task.User, oldVersion, task.Version, false)
		if notifyErr != nil {
			logrus.Errorf("%s Telegram通知发送失败: %v", red("📱"), notifyErr)
		}
		
		return fmt.Errorf("deployment更新失败: %v", err)
	}

	logrus.Infof("%s Deployment镜像更新成功: %s -> %s", green("✅"), oldVersion, newImage)

	// ================================
	// 步骤6：等待Deployment就绪（超时5分钟）
	// ================================
	logrus.Infof("%s 等待Deployment就绪（超时: 5分钟）", cyan("⏳"))
	success := true
	deployTimeout := 5 * time.Minute
	
	err = k8s.WaitForDeploymentReady(task.Service, deployTimeout)
	if err != nil {
		// 部署失败：标记失败、执行回滚
		logrus.Errorf("%s Deployment就绪失败: %v", red("💥"), err)
		success = false
		
		// 6.1 执行自动回滚
		logrus.Infof("%s 开始执行回滚操作", yellow("🔄"))
		rollbackErr := k8s.RollbackDeployment(task.Service)
		if rollbackErr != nil {
			logrus.Errorf("%s 回滚操作失败: %v", red("🔙"), rollbackErr)
		} else {
			logrus.Infof("%s 回滚操作成功完成", green("🔙"))
			// 等待回滚完成
			time.Sleep(30 * time.Second)
		}
	} else {
		logrus.Infof("%s Deployment就绪检查通过", green("✅"))
	}

	// ================================
	// 步骤7：发送Telegram通知（成功/失败）
	// ================================
	notifyErr := botMgr.SendNotification(task.Service, task.Environments[0], task.User, oldVersion, task.Version, success)
	if notifyErr != nil {
		logrus.Errorf("%s Telegram通知发送失败: %v", red("📱"), notifyErr)
	} else {
		logrus.Infof("%s Telegram通知发送成功", green("📱"))
	}

	// ================================
	// 步骤8：更新Redis最终任务状态
	// ================================
	status := "success"
	if !success {
		status = "failure"
	}
	
	redisErr := redis.UpdateTaskStatus(task.Service, task.Version, task.Environments[0], task.User, status)
	if redisErr != nil {
		logrus.Errorf("%s Redis最终状态更新失败: %v", red("💾"), redisErr)
	} else {
		logrus.Infof("%s Redis状态更新成功: %s", green("💾"), status)
	}

	// ================================
	// 步骤9：输出任务完成总结
	// ================================
	if success {
		logrus.Infof("%s 🎉 部署任务完成:\n"+
			"  服务: %s\n"+
			"  状态: %s\n"+
			"  耗时: %v\n"+
			"  旧版本: %s -> 新版本: %s",
			green("🎉"), task.Service, status, time.Since(task.CreatedAt), oldVersion, task.Version)
	} else {
		logrus.Infof("%s 💥 部署任务失败（已回滚）:\n"+
			"  服务: %s\n"+
			"  状态: %s\n"+
			"  耗时: %v\n"+
			"  回滚至: %s",
			red("💥"), task.Service, status, time.Since(task.CreatedAt), oldVersion)
	}

	return nil
}