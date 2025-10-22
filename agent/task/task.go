package task

import (
	"container/list"
	"fmt"
	"strings"
	"sync"
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

// TaskQueue 任务队列结构（FIFO先进先出，支持并发执行）
type TaskQueue struct {
	queue    *list.List        // 任务列表（双向链表实现FIFO）
	mu       sync.Mutex        // 队列锁（保证线程安全）
	workers  int               // 并发worker数量
	stopCh   chan struct{}     // 停止信号通道
	wg       sync.WaitGroup    // 等待组（等待所有worker完成）
}

// NewTaskQueue 创建新的任务队列
func NewTaskQueue(workers int) *TaskQueue {
	q := &TaskQueue{
		queue:   list.New(),
		workers: workers,
		stopCh:  make(chan struct{}),
	}

	logrus.Infof("任务队列初始化完成，worker数量: %d", workers)
	return q
}

// StartWorkers 启动所有任务worker
func (q *TaskQueue) StartWorkers(cfg *config.Config, redis *client.RedisClient, k8s *kubernetes.K8sClient, botMgr *telegram.BotManager, apiClient *api.APIClient) {
	green := color.New(color.FgGreen).SprintFunc()
	logrus.Infof("%s 启动 %d 个任务worker", green("👷"), q.workers)

	q.wg.Add(q.workers)
	for i := 0; i < q.workers; i++ {
		go q.worker(cfg, redis, k8s, botMgr, apiClient, i+1)
	}

	q.wg.Wait()
	logrus.Info("所有任务worker已停止")
}

// worker 单个任务worker（无限循环执行任务）
func (q *TaskQueue) worker(cfg *config.Config, redis *client.RedisClient, k8s *kubernetes.K8sClient, botMgr *telegram.BotManager, apiClient *api.APIClient, workerID int) {
	defer q.wg.Done()

	yellow := color.New(color.FgYellow).SprintFunc()
	logrus.Infof("%s Worker-%d 启动", yellow("👷"), workerID)

	for {
		select {
		case <-q.stopCh:
			green := color.New(color.FgGreen).SprintFunc()
			logrus.Infof("%s Worker-%d 收到停止信号", green("🛑"), workerID)
			return
		default:
		}

		task, ok := q.Dequeue()
		if !ok {
			time.Sleep(1 * time.Second)
			continue
		}

		cyan := color.New(color.FgCyan).SprintFunc()
		logrus.Infof("%s Worker-%d 执行任务: %s", cyan("🔨"), workerID, task.ID)

		err := executeTask(cfg, redis, k8s, apiClient, task, botMgr)
		if err != nil {
			red := color.New(color.FgRed).SprintFunc()
			logrus.Errorf("%s Worker-%d 任务执行失败: %v", red("💥"), workerID, err)

			if task.Retries < cfg.Task.MaxRetries {
				task.Retries++
				retryDelay := time.Duration(cfg.Task.RetryDelay*task.Retries) * time.Second
				green := color.New(color.FgGreen).SprintFunc()
				logrus.Infof("%s Worker-%d 任务重试 [%d/%d]，%d秒后重试: %s",
					green("🔄"), workerID, task.Retries, cfg.Task.MaxRetries, retryDelay.Seconds(), task.ID)
				
				time.Sleep(retryDelay)
				q.Enqueue(task)
			} else {
				red := color.New(color.FgRed).SprintFunc()
				logrus.Errorf("%s Worker-%d 任务永久失败，已达最大重试次数: %s", red("🪦"), workerID, task.ID)
			}
		} else {
			green := color.New(color.FgGreen).SprintFunc()
			logrus.Infof("%s Worker-%d 任务成功完成: %s", green("🎉"), workerID, task.ID)
		}
	}
}

// Enqueue 将任务加入队列尾部（FIFO）
func (q *TaskQueue) Enqueue(task models.Task) {
	q.mu.Lock()
	defer q.mu.Unlock()

	q.queue.PushBack(task)
	
	green := color.New(color.FgGreen).SprintFunc()
	logrus.Debugf("%s 任务入队: %s (队列长度: %d)", green("📥"), task.ID, q.queue.Len())
}

// Dequeue 从队列头部取出任务
func (q *TaskQueue) Dequeue() (models.Task, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()

	if q.queue.Len() == 0 {
		return models.Task{}, false
	}

	element := q.queue.Front()
	q.queue.Remove(element)
	
	task := element.Value.(models.Task)
	
	yellow := color.New(color.FgYellow).SprintFunc()
	logrus.Debugf("%s 任务出队: %s (剩余: %d)", yellow("📤"), task.ID, q.queue.Len())
	
	return task, true
}

// Len 返回当前队列长度
func (q *TaskQueue) Len() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.queue.Len()
}

// Stop 停止所有worker
func (q *TaskQueue) Stop() {
	close(q.stopCh)
	q.wg.Wait()
	
	red := color.New(color.FgRed).SprintFunc()
	logrus.Infof("%s 任务队列已停止 (剩余任务: %d)", red("🛑"), q.Len())
}

// IsEmpty 检查队列是否为空
func (q *TaskQueue) IsEmpty() bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.queue.Len() == 0
}

// executeTask 执行单个部署任务
func executeTask(cfg *config.Config, redis *client.RedisClient, k8s *kubernetes.K8sClient, apiClient *api.APIClient, task models.Task, botMgr *telegram.BotManager) error {
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
		rollbackErr := k8s.RollbackDeployment(task.Service, oldVersion)
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
	// 步骤7：推送 /status 接口
	// ================================
	statusReq := models.StatusRequest{
		Service:     task.Service,
		Version:     task.Version,
		Environment: task.Environments[0],
		User:        task.User,
		Status:      status,
	}
	err = apiClient.UpdateStatus(statusReq)
	if err != nil {
		logrus.Errorf("❌ /status 推送失败: %v", err)
		// 可重试逻辑
	} else {
		logrus.Infof("✅ /status 推送成功: %s", status)
	}

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