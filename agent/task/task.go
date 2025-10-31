package task

import (
	"context"
	"container/list"
	"fmt"
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

// worker 执行任务（开始时更新 status="running"）
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

			// 执行任务并处理错误（修复：使用 err）
			err := q.executeTask(cfg, mongo, k8s, apiClient, botMgr, task)
			if err != nil {
				logrus.WithFields(logrus.Fields{"task_id": task.DeployRequest.TaskID}).Errorf(color.RedString("任务执行失败: %v"), err)
				// 处理失败：重试逻辑或永久失败
				if task.Retries < cfg.Task.MaxRetries {
					task.Retries++
					time.Sleep(time.Duration(cfg.Task.RetryDelay) * time.Second)
					q.Enqueue(task) // 重入队
					logrus.WithFields(logrus.Fields{"task_id": task.DeployRequest.TaskID, "retries": task.Retries}).Warnf(color.YellowString("任务重试入队: %d/%d"), task.Retries, cfg.Task.MaxRetries)
				} else {
					q.handlePermanentFailure(mongo, apiClient, botMgr, task)
				}
				continue
			}
		}
	}
}

// executeTask 执行部署任务（使用 env 在日志等；回滚时确保 handleFailure 触发通知）
func (q *TaskQueue) executeTask(cfg *config.Config, mongo *client.MongoClient, k8s *kubernetes.K8sClient, apiClient *api.APIClient, botMgr *telegram.BotManager, task *Task) error {
	startTime := time.Now()
	env := task.DeployRequest.Environments[0]
	namespace := task.DeployRequest.Namespace

	logrus.WithFields(logrus.Fields{
		"time":   time.Now().Format("2006-01-02 15:04:05"),
		"method": "executeTask",
		"data":   logrus.Fields{"task_id": task.DeployRequest.TaskID, "env": env},
	}).Infof(color.GreenString("开始执行部署: %s -> %s [%s]"), task.DeployRequest.Service, task.DeployRequest.Version, env)

	ctx := context.Background()

	// 1. 获取当前镜像和容器名（内联逻辑，模拟 GetCurrentImage）
	var oldImage, containerName string
	var err error

	// 尝试 Deployment
	deploy, err := k8s.Clientset.AppsV1().Deployments(namespace).Get(ctx, task.DeployRequest.Service, metav1.GetOptions{})
	if err == nil {
		if len(deploy.Spec.Template.Spec.Containers) > 0 {
			containerName = deploy.Spec.Template.Spec.Containers[0].Name
			oldImage = deploy.Spec.Template.Spec.Containers[0].Image
		}
	} else {
		// 尝试 StatefulSet
		sts, err := k8s.Clientset.AppsV1().StatefulSets(namespace).Get(ctx, task.DeployRequest.Service, metav1.GetOptions{})
		if err == nil {
			if len(sts.Spec.Template.Spec.Containers) > 0 {
				containerName = sts.Spec.Template.Spec.Containers[0].Name
				oldImage = sts.Spec.Template.Spec.Containers[0].Image
			}
		} else {
			// 尝试 DaemonSet
			ds, err := k8s.Clientset.AppsV1().DaemonSets(namespace).Get(ctx, task.DeployRequest.Service, metav1.GetOptions{})
			if err == nil {
				if len(ds.Spec.Template.Spec.Containers) > 0 {
					containerName = ds.Spec.Template.Spec.Containers[0].Name
					oldImage = ds.Spec.Template.Spec.Containers[0].Image
				}
			} else {
				return fmt.Errorf("未找到工作负载: %s in %s", task.DeployRequest.Service, namespace)
			}
		}
	}

	if oldImage == "" {
		return fmt.Errorf("容器为空: %s in %s", task.DeployRequest.Service, namespace)
	}

	oldTag := kubernetes.ExtractTag(oldImage)

	// 1. 存储镜像快照（用于回滚）
	snapshot := &models.ImageSnapshot{
		Namespace:  namespace,
		Service:    task.DeployRequest.Service,
		Container:  containerName,
		Image:      oldImage,
		Tag:        oldTag,
		RecordedAt: time.Now(),
		TaskID:     task.DeployRequest.TaskID,
	}
	if err := mongo.StoreImageSnapshot(snapshot, task.DeployRequest.TaskID); err != nil {
		logrus.WithFields(logrus.Fields{"task_id": task.DeployRequest.TaskID}).Warnf(color.YellowString("存储快照失败: %v"), err)
	}

	// 2. 更新镜像
	if err := k8s.UpdateWorkloadImage(task.DeployRequest.Service, namespace, task.DeployRequest.Version); err != nil {
		logrus.WithFields(logrus.Fields{
			"time":   time.Now().Format("2006-01-02 15:04:05"),
			"method": "executeTask",
			"took":   time.Since(startTime),
			"data":   logrus.Fields{"task_id": task.DeployRequest.TaskID, "env": env},
		}).Errorf(color.RedString("镜像更新失败: %v"), err)
		q.handleFailure(mongo, apiClient, botMgr, task, oldTag, env)
		return err
	}

	// 3. 等待 rollout 完成
	if err := k8s.WaitForRolloutComplete(task.DeployRequest.Service, namespace, cfg.Deploy.WaitTimeout); err != nil {
		logrus.WithFields(logrus.Fields{
			"time":   time.Now().Format("2006-01-02 15:04:05"),
			"method": "executeTask",
			"took":   time.Since(startTime),
			"data":   logrus.Fields{"task_id": task.DeployRequest.TaskID, "env": env},
		}).Errorf(color.RedString("等待 rollout 失败: %v"), err)
		// 回滚
		if rollbackErr := k8s.RollbackWithSnapshot(task.DeployRequest.Service, namespace, snapshot); rollbackErr != nil {
			logrus.WithFields(logrus.Fields{"task_id": task.DeployRequest.TaskID}).Errorf(color.RedString("回滚失败: %v"), rollbackErr)
			q.handleException(mongo, apiClient, botMgr, task, oldTag, env)
			return fmt.Errorf("部署失败并回滚异常: %v", rollbackErr)
		}
		q.handleFailure(mongo, apiClient, botMgr, task, oldTag, env)
		return err
	}

	// 4. 成功：更新状态并通知
	internalStatus := "执行成功"
	presetStatus := mapStatusToPreset(internalStatus)
	if err := mongo.UpdateTaskStatus(task.DeployRequest.Service, task.DeployRequest.Version, env, task.DeployRequest.User, internalStatus); err != nil {
		logrus.WithFields(logrus.Fields{"task_id": task.DeployRequest.TaskID}).Errorf(color.RedString("更新成功状态失败: %v"), err)
	}
	if err := apiClient.UpdateStatus(models.StatusRequest{
		Service:     task.DeployRequest.Service,
		Version:     task.DeployRequest.Version,
		Environment: env,
		User:        task.DeployRequest.User,
		Status:      presetStatus,
	}); err != nil {
		logrus.WithFields(logrus.Fields{"task_id": task.DeployRequest.TaskID}).Errorf(color.RedString("推送成功状态失败: %v"), err)
	}
	if err := botMgr.SendNotification(task.DeployRequest.Service, env, task.DeployRequest.User, oldTag, task.DeployRequest.Version, true); err != nil {
		logrus.WithFields(logrus.Fields{"task_id": task.DeployRequest.TaskID}).Errorf(color.RedString("发送成功通知失败: %v"), err)
	}

	logrus.WithFields(logrus.Fields{
		"time":   time.Now().Format("2006-01-02 15:04:05"),
		"method": "executeTask",
		"took":   time.Since(startTime),
		"data":   logrus.Fields{"task_id": task.DeployRequest.TaskID, "env": env, "old_tag": oldTag},
	}).Infof(color.GreenString("部署成功: %s -> %s [%s]"), task.DeployRequest.Service, task.DeployRequest.Version, env)
	return nil
}

// getImageOrUnknown 辅助函数：安全获取镜像 tag 或 "unknown"（修复 undefined）
func getImageOrUnknown(image string) string {
	if image == "" {
		return "unknown"
	}
	return image
}

// mapStatusToPreset 状态映射：内部状态 -> 预设中文状态（新增优化）
func mapStatusToPreset(internalStatus string) string {
	mapping := map[string]string{
		"pending":    "待执行",
		"running":    "执行中",
		"执行成功":   "执行成功",
		"执行失败":   "执行失败",
		"异常":      "异常",
	}
	if preset, ok := mapping[internalStatus]; ok {
		return preset
	}
	return internalStatus // 默认
}

// handleFailure 失败处理（使用 env；映射状态；基于片段补全）
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