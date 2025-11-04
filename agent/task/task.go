// 文件: task/task.go
// 修改: 修复 handleFailure 添加 k8s 参数并传入；Dequeue 日志使用 q.queue.Len() 后 remove 的剩余；Enqueue 日志使用 q.queue.Len() 当前长度。
// 保留所有现有功能，包括锁、执行、重试等。

package task

import (
	"container/list"
	"context"
	"fmt"
	"strings"
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
	if q.workers == 0 {
		logrus.Warn(color.YellowString("警告: QueueWorkers=0，无worker启动，任务将积压!"))
	}
}

// worker 执行任务（Dequeue后立即更新confirmation_status="已执行"；增强日志）
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
				logrus.Debugf("Worker-%d: 队列为空，sleep 1s", workerID)  // 新增: 空队列日志 (Debug避免刷屏)
				time.Sleep(1 * time.Second)
				continue
			}

			// Dequeue后 log 剩余长度 (remove后)
			logrus.WithFields(logrus.Fields{
				"time":   time.Now().Format("2006-01-02 15:04:05"),
				"method": "worker",
				"data":   logrus.Fields{"task_id": task.DeployRequest.TaskID, "worker_id": workerID},
			}).Infof(color.GreenString("Worker-%d 出队任务: %s (队列剩余: %d)"), workerID, task.DeployRequest.TaskID, q.queue.Len())

			taskStartTime := time.Now() // 定义任务开始时间

			// 生成 TaskID 如果为空
			if task.DeployRequest.TaskID == "" {
				task.DeployRequest.TaskID = uuid.New().String()
				task.ID = fmt.Sprintf("%s-%s-%s", task.DeployRequest.Service, task.DeployRequest.Version, task.DeployRequest.Environments[0])
			}

			env := task.DeployRequest.Environments[0]

			// 新增: Dequeue后立即更新 confirmation_status="已执行"，防重复入队
			if err := mongo.UpdateConfirmationStatus(task.DeployRequest.TaskID, "已执行"); err != nil {
				logrus.WithFields(logrus.Fields{"task_id": task.DeployRequest.TaskID}).Errorf(color.RedString("更新 confirmation_status 失败: %v (继续执行)"), err)
				// 继续执行，不阻塞
			}

			// 开始执行：立即更新 status="running"
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

			// 执行任务并处理错误
			err := q.executeTask(cfg, mongo, k8s, apiClient, botMgr, task)
			if err != nil {
				logrus.WithFields(logrus.Fields{"task_id": task.DeployRequest.TaskID}).Errorf(color.RedString("任务执行失败: %v"), err)
				task.Retries++
				if task.Retries < cfg.Task.MaxRetries {
					logrus.WithFields(logrus.Fields{"task_id": task.DeployRequest.TaskID, "retries": task.Retries}).Warnf(color.YellowString("任务重试 %d/%d 次"), task.Retries, cfg.Task.MaxRetries)
					time.Sleep(time.Duration(cfg.Task.RetryDelay) * time.Second)
					q.Enqueue(task)  // 重入队
					continue
				} else {
					q.handlePermanentFailure(mongo, apiClient, botMgr, task)
				}
			}
		}
	}
}

// executeTask 执行核心任务逻辑
func (q *TaskQueue) executeTask(cfg *config.Config, mongo *client.MongoClient, k8s *kubernetes.K8sClient, apiClient *api.APIClient, botMgr *telegram.BotManager, task *Task) error {
	env := task.DeployRequest.Environments[0]
	var oldTag string
	var snapshotErr error
	oldSnapshot, snapshotErr := k8s.SnapshotImage(task.DeployRequest.Service, task.DeployRequest.Namespace, task.DeployRequest.TaskID)
	if oldSnapshot != nil {
		oldTag = oldSnapshot.Tag
	}
	if snapshotErr != nil {
		q.handleException(mongo, apiClient, botMgr, task, oldTag, env)
		return snapshotErr
	}

	if err := k8s.UpdateWorkloadImage(task.DeployRequest.Service, task.DeployRequest.Namespace, task.DeployRequest.Version); err != nil {
		q.handleFailure(k8s, mongo, apiClient, botMgr, task, oldTag, env)
		return err
	}

	if err := k8s.WaitForRolloutComplete(task.DeployRequest.Service, task.DeployRequest.Namespace, cfg.Deploy.WaitTimeout); err != nil {
		q.handleFailure(k8s, mongo, apiClient, botMgr, task, oldTag, env)
		return err
	}

	q.handleSuccess(mongo, apiClient, botMgr, task, oldTag, env)
	return nil
}

// handleSuccess 成功处理（使用 env）
func (q *TaskQueue) handleSuccess(mongo *client.MongoClient, apiClient *api.APIClient, botMgr *telegram.BotManager, task *Task, oldTag string, env string) {
	internalStatus := "执行成功"
	presetStatus := mapStatusToPreset(internalStatus)

	// 1. 更新 Mongo 状态
	_ = mongo.UpdateTaskStatus(task.DeployRequest.Service, task.DeployRequest.Version, env, task.DeployRequest.User, internalStatus)

	// 2. 调用 /status 接口
	_ = apiClient.UpdateStatus(models.StatusRequest{
		Service:     task.DeployRequest.Service,
		Version:     task.DeployRequest.Version,
		Environment: env,
		User:        task.DeployRequest.User,
		Status:      presetStatus,
	})

	// 3. 发送成功通知
	_ = botMgr.SendNotification(task.DeployRequest.Service, env, task.DeployRequest.User, getImageOrUnknown(oldTag), task.DeployRequest.Version, true)
}

// mapStatusToPreset 状态映射（内部 -> 预设中文）
func mapStatusToPreset(internalStatus string) string {
	mapping := map[string]string{
		"执行成功": "执行成功",
		"执行失败": "执行失败",
		"异常":    "异常",
	}
	if preset, ok := mapping[internalStatus]; ok {
		return preset
	}
	return "未知"
}

// getImageOrUnknown 获取镜像或默认
func getImageOrUnknown(tag string) string {
	if tag != "" {
		return tag
	}
	return "unknown"
}

// handleFailure 失败处理（回滚，使用 env；映射状态；添加 k8s 参数）
func (q *TaskQueue) handleFailure(k8s *kubernetes.K8sClient, mongo *client.MongoClient, apiClient *api.APIClient, botMgr *telegram.BotManager, task *Task, oldTag string, env string) {
	internalStatus := "执行失败"
	presetStatus := mapStatusToPreset(internalStatus)

	logrus.WithFields(logrus.Fields{
		"time":   time.Now().Format("2006-01-02 15:04:05"),
		"method": "handleFailure",
		"data":   logrus.Fields{"task_id": task.DeployRequest.TaskID, "env": env, "old_tag": getImageOrUnknown(oldTag)},
	}).Infof(color.YellowString("开始处理失败逻辑，包括回滚通知: %s [%s]"), task.DeployRequest.TaskID, env)

	// 1. 回滚（如果有旧Tag）
	if oldTag != "" {
		snapshot := &models.ImageSnapshot{
			Namespace: task.DeployRequest.Namespace,
			Service:   task.DeployRequest.Service,
			Tag:       oldTag,
		}
		if err := k8s.RollbackWithSnapshot(task.DeployRequest.Service, task.DeployRequest.Namespace, snapshot); err != nil {
			logrus.WithFields(logrus.Fields{"task_id": task.DeployRequest.TaskID}).Errorf(color.RedString("回滚失败: %v"), err)
		} else {
			logrus.WithFields(logrus.Fields{"task_id": task.DeployRequest.TaskID}).Info(color.GreenString("回滚成功到旧版本: %s"), oldTag)
		}
	}

	// 2. 更新 Mongo 状态 (内部状态)
	mongoErr := mongo.UpdateTaskStatus(task.DeployRequest.Service, task.DeployRequest.Version, env, task.DeployRequest.User, internalStatus)
	if mongoErr != nil {
		logrus.Errorf(color.RedString("更新MongoDB状态失败 [%s]: %v"), env, mongoErr)
	}

	// 3. 调用 /status 接口 (预设状态)
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

	// 4. 发送通知（失败，已回滚）
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
	q.handleFailure(nil, mongo, apiClient, botMgr, task, "unknown", task.DeployRequest.Environments[0])  // k8s nil，无回滚
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
	}).Infof(color.GreenString("任务已入队: %s (队列长度: %d/%d)"), task.DeployRequest.TaskID, q.queue.Len(), q.maxQueueSize)  // 修复: 使用当前 Len()
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