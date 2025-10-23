package task

import (
	"container/list"
	"fmt"
	"strings"
	"sync"
	"time"

	"k8s-cicd/agent/config"
	"k8s-cicd/agent/api"
	"k8s-cicd/agent/client"
	"k8s-cicd/agent/kubernetes"
	"k8s-cicd/agent/models"
	"k8s-cicd/agent/telegram"

	"github.com/sirupsen/logrus"
)

// TaskQueue 任务队列结构（FIFO，支持并发）
type TaskQueue struct {
	queue    *list.List    // 任务列表
	mu       sync.Mutex    // 队列锁
	workers  int           // worker数量
	stopCh   chan struct{} // 停止通道
	wg       sync.WaitGroup // 等待组
}

// NewTaskQueue 创建新的任务队列
func NewTaskQueue(workers int) *TaskQueue {
	// 步骤1：初始化队列
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
	// 步骤1：添加等待组
	q.wg.Add(q.workers)
	// 步骤2：启动每个worker
	for i := 0; i < q.workers; i++ {
		go q.worker(cfg, redis, k8s, botMgr, apiClient, i+1)
	}
	// 步骤3：等待所有worker
	q.wg.Wait()
	logrus.Info("所有任务worker已停止")
}

// worker 单个任务worker（无限循环）
func (q *TaskQueue) worker(cfg *config.Config, redis *client.RedisClient, k8s *kubernetes.K8sClient, botMgr *telegram.BotManager, apiClient *api.APIClient, workerID int) {
	// 步骤1：延迟等待组完成
	defer q.wg.Done()

	logrus.Infof("👷 Worker-%d 启动", workerID)

	for {
		select {
		case <-q.stopCh:
			logrus.Infof("🛑 Worker-%d 收到停止信号", workerID)
			return
		default:
			// 出队任务
			task, ok := q.Dequeue()
			if !ok {
				time.Sleep(1 * time.Second)
				continue
			}

			logrus.Infof("🔨 Worker-%d 执行任务: %s", workerID, task.ID)

			// 执行任务
			err := executeTask(cfg, redis, k8s, apiClient, task, botMgr)
			if err != nil {
				logrus.Errorf("💥 Worker-%d 任务执行失败: %v", workerID, err)

				if task.Retries < cfg.Task.MaxRetries {
					task.Retries++
					retryDelay := time.Duration(cfg.Task.RetryDelay*task.Retries) * time.Second
					logrus.Infof("🔄 Worker-%d 任务重试 [%d/%d]，%d秒后重试: %s",
						workerID, task.Retries, cfg.Task.MaxRetries, int(retryDelay.Seconds()), task.ID)
					time.Sleep(retryDelay)
					q.Enqueue(task)
				} else {
					logrus.Errorf("🪦 Worker-%d 任务永久失败: %s", workerID, task.ID)
				}
			} else {
				logrus.Infof("🎉 Worker-%d 任务成功完成: %s", workerID, task.ID)
			}
		}
	}
}

// Enqueue 将任务加入队列尾部
func (q *TaskQueue) Enqueue(task models.Task) {
	q.mu.Lock()
	defer q.mu.Unlock()

	q.queue.PushBack(task)

	logrus.Debugf("📥 任务入队: %s (队列长度: %d)", task.ID, q.queue.Len())
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

	logrus.Debugf("📤 任务出队: %s (剩余: %d)", task.ID, q.queue.Len())

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

	logrus.Infof("🛑 任务队列已停止 (剩余任务: %d)", q.Len())
}

// IsEmpty 检查队列是否为空
func (q *TaskQueue) IsEmpty() bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.queue.Len() == 0
}

// executeTask 执行单个部署任务
func executeTask(cfg *config.Config, redis *client.RedisClient, k8s *kubernetes.K8sClient, apiClient *api.APIClient, task models.Task, botMgr *telegram.BotManager) error {
	// 步骤1：提取命名空间（优化：使用namespace执行k8s操作）
	namespace := task.Environments[0]

	// 步骤2：记录任务开始
	logrus.Infof("🚀 开始执行部署任务: 服务=%s, 环境=%s, 版本=%s, 用户=%s, 超时=%v",
		task.Service, namespace, task.Version, task.User, cfg.Deploy.WaitTimeout)

	// 步骤3：获取旧版本
	oldVersion, err := k8s.GetCurrentImage(namespace, task.Service)
	if err != nil {
		logrus.Warnf("⚠️ 获取当前镜像失败: %v", err)
		oldVersion = "unknown"
	}
	logrus.Infof("📋 当前镜像版本: %s", oldVersion)

	// 步骤4：滚动更新
	newImage := fmt.Sprintf("%s:%s", strings.ToLower(task.Service), task.Version)
	err = k8s.UpdateDeploymentImage(namespace, task.Service, newImage)
	if err != nil {
		// 更新失败处理
		redis.UpdateTaskStatus(task.Service, task.Version, namespace, task.User, "failure")
		botMgr.SendNotification(task.Service, namespace, task.User, oldVersion, task.Version, false)
		return fmt.Errorf("更新失败: %v", err)
	}

	logrus.Infof("✅ Deployment镜像更新成功: %s -> %s", oldVersion, newImage)

	// 步骤5：等待新版本就绪
	logrus.Infof("⏳ 等待新版本就绪（超时: %v）", cfg.Deploy.WaitTimeout)

	startTime := time.Now()
	success := true
	err = k8s.WaitForDeploymentReady(namespace, task.Service, task.Version)
	if err != nil {
		logrus.Errorf("💥 部署超时（%v）", cfg.Deploy.WaitTimeout)
		success = false

		// 执行回滚
		logrus.Infof("🔄 开始回滚（超时: %v）", cfg.Deploy.RollbackTimeout)
		rollbackErr := k8s.RollbackDeployment(namespace, task.Service, oldVersion)
		if rollbackErr != nil {
			logrus.Errorf("🔙 回滚失败: %v", rollbackErr)
		} else {
			logrus.Infof("🔙 回滚操作成功完成")
		}
	} else {
		logrus.Infof("✅ 新版本就绪，耗时: %v", time.Since(startTime))
	}

	// 步骤6：发送通知
	notifyErr := botMgr.SendNotification(task.Service, namespace, task.User, oldVersion, task.Version, success)
	if notifyErr != nil {
		logrus.Errorf("📱 通知发送失败: %v", notifyErr)
	} else {
		logrus.Infof("📱 Telegram通知发送成功")
	}

	// 步骤7：更新Redis状态
	status := "success"
	if !success {
		status = "failure"
	}
	redisErr := redis.UpdateTaskStatus(task.Service, task.Version, namespace, task.User, status)
	if redisErr != nil {
		logrus.Errorf("💾 Redis状态更新失败: %v", redisErr)
	} else {
		logrus.Infof("💾 Redis状态更新成功: %s", status)
	}

	// 步骤8：推送 /status
	statusReq := models.StatusRequest{
		Service:     task.Service,
		Version:     task.Version,
		Environment: namespace,
		User:        task.User,
		Status:      status,
	}
	err = apiClient.UpdateStatus(statusReq)
	if err != nil {
		logrus.Errorf("❌ /status 推送失败: %v", err)
	} else {
		logrus.Infof("✅ /status 推送成功: %s", status)
	}

	// 步骤9：任务总结
	if success {
		logrus.Infof("🎉 部署成功: 服务=%s, 耗时=%v, %s -> %s",
			task.Service, time.Since(startTime), oldVersion, task.Version)
	} else {
		logrus.Infof("💥 部署失败（已回滚）: 服务=%s, 耗时=%v, 回滚至=%s",
			task.Service, time.Since(startTime), oldVersion)
	}

	return nil
}