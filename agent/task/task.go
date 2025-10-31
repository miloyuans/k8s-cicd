// 修改后的 task/task.go：增强 handleFailure 中的通知逻辑，确保回滚后明确触发失败通知（已回滚），添加日志确认通知触发；模板已包含"已回滚"。

package task

import (
	"container/list"
	"fmt"     // 用于 fmt.Sprintf
	"sync"
	"time"    // 用于 time.Now(), time.Sleep, time.Since

	"github.com/google/uuid" // UUID v4

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
	queue   *list.List
	mu      sync.Mutex
	workers int
	stopCh  chan struct{}
	wg      sync.WaitGroup
	locks   map[string]*sync.Mutex // 键: "service-namespace"，值: Mutex for 串行
	lockMu  sync.RWMutex          // 保护 locks map
}

// NewTaskQueue 创建任务队列
func NewTaskQueue(workers int) *TaskQueue {
	q := &TaskQueue{
		queue:   list.New(),
		workers: workers,
		stopCh:  make(chan struct{}),
		locks:   make(map[string]*sync.Mutex),
	}
	logrus.Infof(color.GreenString("任务队列初始化完成，worker数量: %d"), workers)
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

// worker 执行任务
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

			// 生成 TaskID 如果为空 (UUID v4)
			if task.DeployRequest.TaskID == "" {
				task.DeployRequest.TaskID = uuid.New().String()
				task.ID = fmt.Sprintf("%s-%s-%s", task.DeployRequest.Service, task.DeployRequest.Version, task.DeployRequest.Environments[0]) // 保留复合键
			}

			logrus.WithFields(logrus.Fields{
				"time":   time.Now().Format("2006-01-02 15:04:05"),
				"method": "worker",
				"data": logrus.Fields{
					"task_id":     task.DeployRequest.TaskID,
					"service":     task.DeployRequest.Service,
					"version":     task.DeployRequest.Version,
					"environment": task.DeployRequest.Environments[0],
					"user":        task.DeployRequest.User,
					"namespace":   task.DeployRequest.Namespace,
				},
			}).Infof(color.GreenString("Worker-%d 开始执行任务: %s"), workerID, task.DeployRequest.TaskID)

			// 获取 per-service-namespace lock 确保串行
			lock := q.getLock(task.DeployRequest.Service, task.DeployRequest.Namespace)
			lock.Lock()
			defer lock.Unlock()

			err := q.executeTask(cfg, mongo, k8s, apiClient, botMgr, task)
			if err != nil {
				logrus.WithFields(logrus.Fields{
					"time":   time.Now().Format("2006-01-02 15:04:05"),
					"method": "worker",
					"data":   logrus.Fields{"task_id": task.DeployRequest.TaskID},
				}).Errorf(color.RedString("Worker-%d 任务失败: %s, 错误: %v"), workerID, task.DeployRequest.TaskID, err)

				if task.Retries < cfg.Task.MaxRetries {
					task.Retries++
					retryDelay := time.Duration(cfg.Task.RetryDelay*task.Retries) * time.Second
					logrus.WithFields(logrus.Fields{
						"time":   time.Now().Format("2006-01-02 15:04:05"),
						"method": "worker",
						"data":   logrus.Fields{"task_id": task.DeployRequest.TaskID},
					}).Infof(color.YellowString("Worker-%d 任务重试 [%d/%d]，%ds后重试: %s"), workerID, task.Retries, cfg.Task.MaxRetries, int(retryDelay.Seconds()), task.DeployRequest.TaskID)
					time.Sleep(retryDelay)
					q.Enqueue(task)
				} else {
					logrus.WithFields(logrus.Fields{
						"time":   time.Now().Format("2006-01-02 15:04:05"),
						"method": "worker",
						"data":   logrus.Fields{"task_id": task.DeployRequest.TaskID},
					}).Errorf(color.RedString("Worker-%d 任务永久失败: %s"), workerID, task.DeployRequest.TaskID)
					q.handlePermanentFailure(mongo, apiClient, botMgr, task)
				}
			}
		}
	}
}

// executeTask 执行部署任务（使用 env 在日志等；回滚时确保 handleFailure 触发通知）
func (q *TaskQueue) executeTask(cfg *config.Config, mongo *client.MongoClient, k8s *kubernetes.K8sClient, apiClient *api.APIClient, botMgr *telegram.BotManager, task *Task) error {
	startTime := time.Now()
	env := task.DeployRequest.Environments[0] // 使用 env
	namespace := task.DeployRequest.Namespace

	defer func() {
		if r := recover(); r != nil {
			// 捕获 panic 作为异常，使用 env
			logrus.WithFields(logrus.Fields{
				"time":   time.Now().Format("2006-01-02 15:04:05"),
				"method": "executeTask",
				"took":   time.Since(startTime),
				"data":   logrus.Fields{"task_id": task.DeployRequest.TaskID, "env": env, "panic": r},
			}).Errorf(color.RedString("任务执行异常: %s [%s]"), task.DeployRequest.TaskID, env)
			q.handleException(mongo, apiClient, botMgr, task, "unknown", env)
		}
	}()

	// 步骤1：捕获快照
	snapshot, err := k8s.CaptureImageSnapshot(task.DeployRequest.Service, namespace)
	if err != nil {
		q.handleFailure(mongo, apiClient, botMgr, task, "unknown", env)
		return err
	}
	oldImage := kubernetes.ExtractTag(getImageOrUnknown(snapshot))

	// 步骤2：存储快照
	if err := mongo.StoreImageSnapshot(snapshot, task.DeployRequest.TaskID); err != nil {
		logrus.Warnf(color.YellowString("存储快照失败 [%s]: %v"), env, err)
	}

	// 步骤3：更新镜像
	if err := k8s.UpdateWorkloadImage(task.DeployRequest.Service, namespace, task.DeployRequest.Version); err != nil {
		q.handleFailure(mongo, apiClient, botMgr, task, oldImage, env)
		return err
	}

	// 步骤4：等待 rollout
	if err := k8s.WaitForRolloutComplete(task.DeployRequest.Service, namespace, cfg.Deploy.WaitTimeout); err != nil {
		// 回滚：使用旧镜像 tag 重新执行更新操作
		logrus.WithFields(logrus.Fields{
			"time":   time.Now().Format("2006-01-02 15:04:05"),
			"method": "executeTask",
			"took":   time.Since(startTime),
			"data":   logrus.Fields{"task_id": task.DeployRequest.TaskID, "env": env},
		}).Infof(color.YellowString("Rollout 失败，执行回滚: %s [%s]"), task.DeployRequest.TaskID, env)

		rollbackErr := k8s.RollbackWithSnapshot(task.DeployRequest.Service, namespace, snapshot)
		if rollbackErr != nil {
			logrus.WithFields(logrus.Fields{
				"time":   time.Now().Format("2006-01-02 15:04:05"),
				"method": "executeTask",
				"took":   time.Since(startTime),
				"data":   logrus.Fields{"task_id": task.DeployRequest.TaskID, "env": env},
			}).Errorf(color.RedString("回滚失败: %v"), rollbackErr)
			// 回滚失败仍触发失败通知（包含回滚尝试信息）
		} else {
			logrus.WithFields(logrus.Fields{
				"time":   time.Now().Format("2006-01-02 15:04:05"),
				"method": "executeTask",
				"took":   time.Since(startTime),
				"data":   logrus.Fields{"task_id": task.DeployRequest.TaskID, "env": env, "old_tag": oldImage},
			}).Infof(color.YellowString("回滚成功，使用旧镜像: %s [%s]"), oldImage, env)
		}
		q.handleFailure(mongo, apiClient, botMgr, task, oldImage, env)
		return err
	}

	// 步骤5：成功处理
	q.handleSuccess(mongo, apiClient, botMgr, task, oldImage, env)

	logrus.WithFields(logrus.Fields{
		"time":   time.Now().Format("2006-01-02 15:04:05"),
		"method": "executeTask",
		"took":   time.Since(startTime),
		"data": logrus.Fields{
			"task_id":  task.DeployRequest.TaskID,
			"old_tag":  kubernetes.ExtractTag(oldImage),
			"new_tag":  task.DeployRequest.Version,
			"env":      env,
		},
	}).Infof(color.GreenString("任务执行完成: %s [%s], 状态: 执行成功"), task.DeployRequest.TaskID, env)
	return nil
}

// handleSuccess 成功处理（确保通知触发）
func (q *TaskQueue) handleSuccess(mongo *client.MongoClient, apiClient *api.APIClient, botMgr *telegram.BotManager, task *Task, oldImage string, env string) {
	// 1. 更新 Mongo 状态
	if err := mongo.UpdateTaskStatus(task.DeployRequest.Service, task.DeployRequest.Version, env, task.DeployRequest.User, "执行成功"); err != nil {
		logrus.Errorf(color.RedString("更新MongoDB状态失败 [%s]: %v"), env, err)
	}

	// 2. 调用 /status 接口
	if err := apiClient.UpdateStatus(models.StatusRequest{
		Service:     task.DeployRequest.Service,
		Version:     task.DeployRequest.Version,
		Environment: env,
		User:        task.DeployRequest.User,
		Status:      "执行成功",
	}); err != nil {
		logrus.Errorf(color.RedString("推送成功状态失败 [%s]: %v"), env, err)
	}

	// 3. 发送通知（成功）
	notifyErr := botMgr.SendNotification(task.DeployRequest.Service, env, task.DeployRequest.User, oldImage, task.DeployRequest.Version, true)
	if notifyErr != nil {
		logrus.Errorf(color.RedString("发送成功通知失败 [%s]: %v"), env, notifyErr)
	} else {
		logrus.WithFields(logrus.Fields{
			"time":   time.Now().Format("2006-01-02 15:04:05"),
			"method": "handleSuccess",
			"data":   logrus.Fields{"task_id": task.DeployRequest.TaskID, "env": env},
		}).Infof(color.GreenString("成功通知已触发发送到 Telegram 群组"))
	}
}

// handleFailure 失败处理（增强：确保回滚后通知触发，日志确认）
func (q *TaskQueue) handleFailure(mongo *client.MongoClient, apiClient *api.APIClient, botMgr *telegram.BotManager, task *Task, oldImage string, env string) {
	logrus.WithFields(logrus.Fields{
		"time":   time.Now().Format("2006-01-02 15:04:05"),
		"method": "handleFailure",
		"data":   logrus.Fields{"task_id": task.DeployRequest.TaskID, "env": env, "old_tag": oldImage},
	}).Infof(color.YellowString("开始处理失败逻辑，包括回滚通知: %s [%s]"), task.DeployRequest.TaskID, env)

	// 1. 更新 Mongo 状态
	mongoErr := mongo.UpdateTaskStatus(task.DeployRequest.Service, task.DeployRequest.Version, env, task.DeployRequest.User, "执行失败")
	if mongoErr != nil {
		logrus.Errorf(color.RedString("更新MongoDB状态失败 [%s]: %v"), env, mongoErr)
	}

	// 2. 调用 /status 接口
	statusErr := apiClient.UpdateStatus(models.StatusRequest{
		Service:     task.DeployRequest.Service,
		Version:     task.DeployRequest.Version,
		Environment: env,
		User:        task.DeployRequest.User,
		Status:      "执行失败",
	})
	if statusErr != nil {
		logrus.Errorf(color.RedString("推送失败状态失败 [%s]: %v"), env, statusErr)
	}

	// 3. 发送通知（失败，已回滚）
	notifyErr := botMgr.SendNotification(task.DeployRequest.Service, env, task.DeployRequest.User, oldImage, task.DeployRequest.Version, false)
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

// handleException 异常处理（使用 env）
func (q *TaskQueue) handleException(mongo *client.MongoClient, apiClient *api.APIClient, botMgr *telegram.BotManager, task *Task, oldImage string, env string) {
	// 1. 更新 Mongo 状态
	_ = mongo.UpdateTaskStatus(task.DeployRequest.Service, task.DeployRequest.Version, env, task.DeployRequest.User, "异常")

	// 2. 调用 /status 接口
	_ = apiClient.UpdateStatus(models.StatusRequest{
		Service:     task.DeployRequest.Service,
		Version:     task.DeployRequest.Version,
		Environment: env,
		User:        task.DeployRequest.User,
		Status:      "异常",
	})

	// 3. 发送通知（失败样式）
	_ = botMgr.SendNotification(task.DeployRequest.Service, env, task.DeployRequest.User, oldImage, task.DeployRequest.Version, false)
}

// handlePermanentFailure 永久失败处理
func (q *TaskQueue) handlePermanentFailure(mongo *client.MongoClient, apiClient *api.APIClient, botMgr *telegram.BotManager, task *Task) {
	q.handleFailure(mongo, apiClient, botMgr, task, "unknown", task.DeployRequest.Environments[0])
}

// Enqueue 入队（设置 CreatedAt 如果为空）
func (q *TaskQueue) Enqueue(task *Task) {
	if task.DeployRequest.CreatedAt.IsZero() {
		task.DeployRequest.CreatedAt = time.Now() // 精确到纳秒
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	q.queue.PushBack(task)
	logrus.WithFields(logrus.Fields{
		"time":   time.Now().Format("2006-01-02 15:04:05"),
		"method": "Enqueue",
		"data":   logrus.Fields{"task_id": task.DeployRequest.TaskID},
	}).Infof(color.GreenString("任务已入队: %s"), task.DeployRequest.TaskID)
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

// Stop 停止队列（清理 locks）
func (q *TaskQueue) Stop() {
	close(q.stopCh)
	q.wg.Wait()
	q.lockMu.Lock()
	for _, lock := range q.locks {
		lock.Unlock() // 确保释放
	}
	q.lockMu.Unlock()
	logrus.Info(color.GreenString("任务队列停止"))
}

// getImageOrUnknown 获取镜像或返回 unknown
func getImageOrUnknown(s *models.ImageSnapshot) string {
	if s != nil && s.Image != "" {
		return s.Image
	}
	return "unknown"
}