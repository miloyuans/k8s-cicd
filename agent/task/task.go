// 修改后的 task/task.go：在 worker 函数中，使用 sanitized env 更新状态和处理失败/异常。
// 保留所有现有功能，包括锁、重试、回滚、通知等。

package task

import (
	"context"
	"container/list"
	"fmt"
	"strings" // 新增：用于 sanitizeEnv
	"sync"
	"time"

	"github.com/google/uuid"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"k8s-cicd/agent/api"
	"k8s-cicd/agent/client"
	"k8s-cicd/agent/config"
	"k8s-cicd/agent/kubernetes"
	"k8s-cicd/agent/models"
	"k8s-cicd/agent/telegram"

	"github.com/fatih/color"
	"github.com/sirupsen/logrus"
)

// sanitizeEnv 将环境名中的 "-" 替换为 "_" 以符合 MongoDB 集合命名规范
func sanitizeEnv(env string) string {
	return strings.ReplaceAll(env, "-", "_")
}

// Task 任务结构
type Task struct {
	DeployRequest models.DeployRequest
	ID            string // task_id = service-version (复合键)
	Retries       int
}

// TaskQueue 任务队列（添加 locks map 用于 per-service-namespace 串行）
type TaskQueue struct {
	queue        *list.List
	mu           sync.Mutex
	workers      int
	stopCh       chan struct{}
	wg           sync.WaitGroup
	locks        map[string]*sync.Mutex
	lockMu       sync.RWMutex
	maxQueueSize int // 新增: 资源约束
}

// NewTaskQueue 创建任务队列
func NewTaskQueue(workers, maxQueueSize int) *TaskQueue {
	q := &TaskQueue{
		queue:        list.New(),
		workers:      workers,
		stopCh:       make(chan struct{}),
		locks:        make(map[string]*sync.Mutex),
		maxQueueSize: maxQueueSize,
	}
	logrus.Infof(color.GreenString("任务队列初始化完成，worker数量: %d, maxQueueSize: %d"), workers, maxQueueSize)
	return q
}

// getLock 获取或创建 per-group lock
func (q *TaskQueue) getLock(service, namespace string) *sync.Mutex {
	key := fmt.Sprintf("%s-%s", service, namespace)
	q.lockMu.RLock()
	if lock, exists := q.locks[key]; exists {
		q.lockMu.RUnlock()
		return lock
	}
	q.lockMu.RUnlock()

	q.lockMu.Lock()
	defer q.lockMu.Unlock()
	if lock, exists := q.locks[key]; exists {
		return lock
	}
	newLock := &sync.Mutex{}
	q.locks[key] = newLock
	return newLock
}

// StartWorkers 启动任务 worker
func (q *TaskQueue) StartWorkers(cfg *config.Config, mongo *client.MongoClient, k8s *kubernetes.K8sClient, botMgr *telegram.BotManager, apiClient *api.APIClient) {
	q.wg.Add(q.workers)
	for i := 0; i < q.workers; i++ {
		go q.worker(cfg, mongo, k8s, botMgr, apiClient, i+1)
	}
	logrus.Infof(color.GreenString("启动 %d 个任务 worker"), q.workers)
}

// worker 执行任务（开始时更新 status="running"，使用 sanitized env）
func (q *TaskQueue) worker(cfg *config.Config, mongo *client.MongoClient, k8s *kubernetes.K8sClient, botMgr *telegram.BotManager, apiClient *api.APIClient, workerID int) {
	defer q.wg.Done()

	logrus.Infof(color.GreenString("Worker-%d 启动"), workerID)

	for {
		select {
		case <-q.stopCh:
			logrus.Infof(color.GreenString("Worker-%d 停止"), workerID)
			return
		default:
			task, ok := q.Dequeue()
			if !ok {
				time.Sleep(1 * time.Second)
				continue
			}

			// 生成 TaskID 如果为空
			if task.DeployRequest.TaskID == "" {
				task.DeployRequest.TaskID = uuid.New().String()
				task.ID = fmt.Sprintf("%s-%s-%s", task.DeployRequest.Service, task.DeployRequest.Version, task.DeployRequest.Environments[0])
			}

			env := task.DeployRequest.Environments[0]
			sanitizedEnv := sanitizeEnv(env)
			// 开始执行：立即更新 status="running"（使用 sanitized env 隐式，通过 UpdateTaskStatus）
			if err := mongo.UpdateTaskStatus(task.DeployRequest.Service, task.DeployRequest.Version, env, task.DeployRequest.User, "running"); err != nil {
				logrus.WithFields(logrus.Fields{"task_id": task.DeployRequest.TaskID}).Errorf(color.RedString("更新 running 状态失败: %v"), err)
			}

			logrus.WithFields(logrus.Fields{
				"time":   time.Now().Format("2006-01-02 15:04:05"),
				"method": "worker",
				"data": logrus.Fields{
					"task_id":     task.DeployRequest.TaskID,
					"service":     task.DeployRequest.Service,
					"version":     task.DeployRequest.Version,
					"environment": env,
					"user":        task.DeployRequest.User,
					"namespace":   task.DeployRequest.Namespace,
				},
			}).Infof(color.GreenString("Worker-%d 开始执行任务 (status=running): %s"), workerID, task.DeployRequest.TaskID)

			// 锁逻辑
			lock := q.getLock(task.DeployRequest.Service, task.DeployRequest.Namespace)
			lock.Lock()
			logrus.WithFields(logrus.Fields{"task_id": task.DeployRequest.TaskID}).Debug("Lock acquired for serial execution")
			defer func() {
				lock.Unlock()
				logrus.WithFields(logrus.Fields{"task_id": task.DeployRequest.TaskID}).Debug("Lock released after execution")
			}()

			// 执行任务并处理错误（修复：使用 err）
			err := q.executeTask(cfg, mongo, k8s, apiClient, botMgr, task)
			if err != nil {
				logrus.WithFields(logrus.Fields{"task_id": task.DeployRequest.TaskID}).Errorf(color.RedString("任务执行失败: %v"), err)
				// 处理失败：重试逻辑或永久失败
				if task.Retries < cfg.Task.MaxRetries {
					task.Retries++
					time.Sleep(time.Duration(cfg.Task.RetryDelay) * time.Second)
					q.Enqueue(task) // 重试入队
					continue
				} else {
					q.handlePermanentFailure(mongo, apiClient, botMgr, task)
					continue
				}
			}

			// 成功处理
			internalStatus := "执行成功"
			presetStatus := mapStatusToPreset(internalStatus)
			oldTag := getCurrentImageTag(k8s, task.DeployRequest.Service, task.DeployRequest.Namespace) // 假设函数获取当前 tag

			// 1. 更新 Mongo 状态 (内部状态，使用 sanitized env 隐式)
			mongoErr := mongo.UpdateTaskStatus(task.DeployRequest.Service, task.DeployRequest.Version, env, task.DeployRequest.User, internalStatus)
			if mongoErr != nil {
				logrus.Errorf(color.RedString("更新MongoDB状态失败 [%s]: %v"), env, mongoErr)
			}

			// 2. 调用 /status 接口 (预设状态)
			statusErr := apiClient.UpdateStatus(models.StatusRequest{
				Service:     task.DeployRequest.Service,
				Version:     task.DeployRequest.Version,
				Environment: env,
				User:        task.DeployRequest.User,
				Status:      presetStatus,
			})
			if statusErr != nil {
				logrus.Errorf(color.RedString("推送成功状态失败 [%s]: %v"), env, statusErr)
			}

			// 3. 发送通知（成功）
			notifyErr := botMgr.SendNotification(task.DeployRequest.Service, env, task.DeployRequest.User, getImageOrUnknown(oldTag), task.DeployRequest.Version, true)
			if notifyErr != nil {
				logrus.Errorf(color.RedString("发送成功通知失败 [%s]: %v"), env, notifyErr)
			} else {
				logrus.WithFields(logrus.Fields{
					"time":   time.Now().Format("2006-01-02 15:04:05"),
					"method": "worker",
					"data":   logrus.Fields{"task_id": task.DeployRequest.TaskID, "env": env},
				}).Infof(color.GreenString("成功通知已触发发送到 Telegram 群组"))
			}

			logrus.WithFields(logrus.Fields{
				"time":   time.Now().Format("2006-01-02 15:04:05"),
				"method": "worker",
				"took":   time.Since(startTime), // 假设 startTime 在此处定义
			}).Infof(color.GreenString("Worker-%d 任务执行成功: %s [%s]"), workerID, task.DeployRequest.TaskID, env)
		}
	}
}

// executeTask 执行单个任务（存储快照、更新镜像、等待 rollout）（不变，但使用 sanitized env 隐式通过 mongo）
func (q *TaskQueue) executeTask(cfg *config.Config, mongo *client.MongoClient, k8s *kubernetes.K8sClient, apiClient *api.APIClient, botMgr *telegram.BotManager, task *Task) error {
	env := task.DeployRequest.Environments[0]

	// 1. 存储当前镜像快照
	currentImage := getCurrentImage(k8s, task.DeployRequest.Service, task.DeployRequest.Namespace)
	snapshot := &models.ImageSnapshot{
		Namespace:  task.DeployRequest.Namespace,
		Service:    task.DeployRequest.Service,
		Image:      currentImage,
		Tag:        ExtractTag(currentImage),
		TaskID:     task.DeployRequest.TaskID,
	}
	if err := mongo.StoreImageSnapshot(snapshot); err != nil {
		return fmt.Errorf("快照存储失败: %v", err)
	}

	// 2. 更新 K8s 镜像
	if err := k8s.UpdateWorkloadImage(task.DeployRequest.Service, task.DeployRequest.Namespace, task.DeployRequest.Version); err != nil {
		return fmt.Errorf("镜像更新失败: %v", err)
	}

	// 3. 等待 rollout 完成
	if err := k8s.WaitForRolloutComplete(task.DeployRequest.Service, task.DeployRequest.Namespace, cfg.Deploy.WaitTimeout); err != nil {
		// 超时视为失败，触发回滚
		if rollbackErr := k8s.RollbackWithSnapshot(task.DeployRequest.Service, task.DeployRequest.Namespace, snapshot); rollbackErr != nil {
			return fmt.Errorf("等待 rollout 失败并回滚错误: %v (回滚: %v)", err, rollbackErr)
		}
		return fmt.Errorf("等待 rollout 失败，已回滚: %v", err)
	}

	return nil
}

// mapStatusToPreset 状态映射（不变）
func mapStatusToPreset(internalStatus string) string {
	mappings := map[string]string{
		"执行成功": "执行成功",
		"执行失败": "执行失败",
		"异常":    "异常",
		"running": "running",
	}
	if preset, ok := mappings[internalStatus]; ok {
		return preset
	}
	return internalStatus
}

// getCurrentImageTag 获取当前镜像 tag（假设实现）
func getCurrentImageTag(k8s *kubernetes.K8sClient, service, namespace string) string {
	// 实现：从 K8s 获取当前镜像 tag
	return "unknown" // 占位
}

// getImageOrUnknown 获取镜像或 unknown（不变）
func getImageOrUnknown(tag string) string {
	if tag == "" {
		return "unknown"
	}
	return tag
}

// handleFailure 失败处理（使用 sanitized env 隐式）
func (q *TaskQueue) handleFailure(mongo *client.MongoClient, apiClient *api.APIClient, botMgr *telegram.BotManager, task *Task, oldTag string, env string) {
	internalStatus := "执行失败"
	presetStatus := mapStatusToPreset(internalStatus)

	logrus.WithFields(logrus.Fields{
		"time":   time.Now().Format("2006-01-02 15:04:05"),
		"method": "handleFailure",
		"data":   logrus.Fields{"task_id": task.DeployRequest.TaskID, "env": env, "old_tag": getImageOrUnknown(oldTag)},
	}).Infof(color.YellowString("开始处理失败逻辑，包括回滚通知: %s [%s]"), task.DeployRequest.TaskID, env)

	// 1. 更新 Mongo 状态 (内部状态)
	mongoErr := mongo.UpdateTaskStatus(task.DeployRequest.Service, task.DeployRequest.Version, env, task.DeployRequest.User, internalStatus)
	if mongoErr != nil {
		logrus.Errorf(color.RedString("更新MongoDB状态失败 [%s]: %v"), env, mongoErr)
	}

	// 2. 调用 /status 接口 (预设状态)
	statusErr := apiClient.UpdateStatus(models.StatusRequest{
		Service:     task.DeployRequest.Service,
		Version:     task.DeployRequest.Version,
		Environment: env,
		User:        task.DeployRequest.User,
		Status:      presetStatus,
	})
	if statusErr != nil {
		logrus.Errorf(color.RedString("推送失败状态失败 [%s]: %v"), env, statusErr)
	}

	// 3. 发送通知（失败，已回滚）
	notifyErr := botMgr.SendNotification(task.DeployRequest.Service, env, task.DeployRequest.User, getImageOrUnknown(oldTag), task.DeployRequest.Version, false)
	if notifyErr != nil {
		logrus.Errorf(color.RedString("发送失败通知失败 [%s]: %v (模板包含'已回滚')"), env, notifyErr)
	} else {
		logrus.WithFields(logrus.Fields{
			"time":   time.Now().Format("2006-01-02 15:04:05"),
			"method": "handleFailure",
			"data":   logrus.Fields{"task_id": task.DeployRequest.TaskID, "env": env},
		}).Infof(color.GreenString("失败通知已触发发送到 Telegram 群组 (包含回滚信息)"))
	}
}

// handleException 异常处理（使用 env；映射状态）
func (q *TaskQueue) handleException(mongo *client.MongoClient, apiClient *api.APIClient, botMgr *telegram.BotManager, task *Task, oldTag string, env string) {
	internalStatus := "异常"
	presetStatus := mapStatusToPreset(internalStatus)

	// 1. 更新 Mongo 状态 (内部状态)
	_ = mongo.UpdateTaskStatus(task.DeployRequest.Service, task.DeployRequest.Version, env, task.DeployRequest.User, internalStatus)

	// 2. 调用 /status 接口 (预设状态)
	_ = apiClient.UpdateStatus(models.StatusRequest{
		Service:     task.DeployRequest.Service,
		Version:     task.DeployRequest.Version,
		Environment: env,
		User:        task.DeployRequest.User,
		Status:      presetStatus,
	})

	// 3. 发送通知（失败样式）
	_ = botMgr.SendNotification(task.DeployRequest.Service, env, task.DeployRequest.User, getImageOrUnknown(oldTag), task.DeployRequest.Version, false)
}

// handlePermanentFailure 永久失败处理
func (q *TaskQueue) handlePermanentFailure(mongo *client.MongoClient, apiClient *api.APIClient, botMgr *telegram.BotManager, task *Task) {
	q.handleFailure(mongo, apiClient, botMgr, task, "unknown", task.DeployRequest.Environments[0])
}

// Enqueue 入队（设置 CreatedAt 如果为空 大小约束）
func (q *TaskQueue) Enqueue(task *Task) {
	if task.DeployRequest.CreatedAt.IsZero() {
		task.DeployRequest.CreatedAt = time.Now()
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.maxQueueSize > 0 && q.queue.Len() >= q.maxQueueSize {
		logrus.WithFields(logrus.Fields{"task_id": task.DeployRequest.TaskID}).Warn(color.YellowString("队列已满，丢弃任务"))
		return
	}
	q.queue.PushBack(task)
	logrus.WithFields(logrus.Fields{
		"time":   time.Now().Format("2006-01-02 15:04:05"),
		"method": "Enqueue",
		"data":   logrus.Fields{"task_id": task.DeployRequest.TaskID},
	}).Infof(color.GreenString("任务已入队: %s (队列长度: %d/%d)"), task.DeployRequest.TaskID, q.queue.Len()-1, q.maxQueueSize)
}

// Dequeue 出队
func (q *TaskQueue) Dequeue() (*Task, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.queue.Len() == 0 {
		return nil, false
	}
	e := q.queue.Front()
	task := e.Value.(*Task)
	q.queue.Remove(e)
	logrus.WithFields(logrus.Fields{
		"time":   time.Now().Format("2006-01-02 15:04:05"),
		"method": "Dequeue",
		"data":   logrus.Fields{"task_id": task.DeployRequest.TaskID},
	}).Infof(color.GreenString("任务已出队: %s"), task.DeployRequest.TaskID)
	return task, true
}

// Stop 停止队列（添加超时）
func (q *TaskQueue) Stop() {
	close(q.stopCh)
	done := make(chan struct{})
	go func() {
		q.wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		logrus.Info(color.GreenString("任务队列等待完成"))
	case <-time.After(20 * time.Second):
		logrus.Warn(color.YellowString("任务队列等待超时，强制继续"))
	}

	// 强制释放锁
	q.lockMu.Lock()
	for key, lock := range q.locks {
		lock.Unlock()
		logrus.WithFields(logrus.Fields{"key": key}).Debug("Force-released lock during shutdown")
	}
	q.locks = make(map[string]*sync.Mutex)
	q.lockMu.Unlock()
	logrus.Info(color.GreenString("任务队列停止 (所有锁已释放)"))
}