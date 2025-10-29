package task

import (
	"container/list"
	"sync"
	"time"

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
	ID            string // task_id = service-version
	Retries       int
}

// TaskQueue 任务队列
type TaskQueue struct {
	queue   *list.List
	mu      sync.Mutex
	workers int
	stopCh  chan struct{}
	wg      sync.WaitGroup
}

// NewTaskQueue 创建任务队列
func NewTaskQueue(workers int) *TaskQueue {
	q := &TaskQueue{
		queue:   list.New(),
		workers: workers,
		stopCh:  make(chan struct{}),
	}
	logrus.Infof(color.GreenString("任务队列初始化完成，worker数量: %d"), workers)
	return q
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

			logrus.WithFields(logrus.Fields{
				"time":   time.Now().Format("2006-01-02 15:04:05"),
				"method": "worker",
				"data": logrus.Fields{
					"task_id":     task.ID,
					"service":     task.DeployRequest.Service,
					"version":     task.DeployRequest.Version,
					"environment": task.DeployRequest.Environments[0],
					"user":        task.DeployRequest.User,
					"namespace":   task.DeployRequest.Namespace,
				},
			}).Infof(color.GreenString("Worker-%d 开始执行任务: %s"), workerID, task.ID)

			err := q.executeTask(cfg, mongo, k8s, apiClient, botMgr, task)
			if err != nil {
				logrus.WithFields(logrus.Fields{
					"time":   time.Now().Format("2006-01-02 15:04:05"),
					"method": "worker",
					"data":   logrus.Fields{"task_id": task.ID},
				}).Errorf(color.RedString("Worker-%d 任务失败: %s, 错误: %v"), workerID, task.ID, err)

				if task.Retries < cfg.Task.MaxRetries {
					task.Retries++
					retryDelay := time.Duration(cfg.Task.RetryDelay*task.Retries) * time.Second
					logrus.WithFields(logrus.Fields{
						"time":   time.Now().Format("2006-01-02 15:04:05"),
						"method": "worker",
						"data":   logrus.Fields{"task_id": task.ID},
					}).Infof(color.YellowString("Worker-%d 任务重试 [%d/%d]，%ds后重试: %s"), workerID, task.Retries, cfg.Task.MaxRetries, int(retryDelay.Seconds()), task.ID)
					time.Sleep(retryDelay)
					q.Enqueue(task)
				} else {
					logrus.WithFields(logrus.Fields{
						"time":   time.Now().Format("2006-01-02 15:04:05"),
						"method": "worker",
						"data":   logrus.Fields{"task_id": task.ID},
					}).Errorf(color.RedString("Worker-%d 任务永久失败: %s"), workerID, task.ID)
					q.handlePermanentFailure(mongo, apiClient, botMgr, task)
				}
			}
		}
	}
}

// executeTask 执行部署任务
func (q *TaskQueue) executeTask(cfg *config.Config, mongo *client.MongoClient, k8s *kubernetes.K8sClient, apiClient *api.APIClient, botMgr *telegram.BotManager, task *Task) error {
	startTime := time.Now()
	//env := task.DeployRequest.Environments[0]
	namespace := task.DeployRequest.Namespace

	// 步骤1：捕获快照
	snapshot, err := k8s.CaptureImageSnapshot(task.DeployRequest.Service, namespace)
	if err != nil {
		q.handleFailure(mongo, apiClient, botMgr, task, "unknown")
		return err
	}
	oldImage := kubernetes.ExtractTag(getImageOrUnknown(snapshot))

	// 步骤2：存储快照
	if err := mongo.StoreImageSnapshot(snapshot, task.ID); err != nil {
		logrus.Warnf("存储快照失败: %v", err)
	}

	// 步骤3：更新镜像
	if err := k8s.UpdateWorkloadImage(task.DeployRequest.Service, namespace, task.DeployRequest.Version); err != nil {
		q.handleFailure(mongo, apiClient, botMgr, task, oldImage)
		return err
	}

	// 步骤4：等待 rollout
	if err := k8s.WaitForRolloutComplete(task.DeployRequest.Service, namespace, cfg.Deploy.WaitTimeout); err != nil {
		// 回滚
		if rollbackErr := k8s.RollbackWithSnapshot(task.DeployRequest.Service, namespace, snapshot); rollbackErr != nil {
			logrus.Errorf(color.RedString("回滚失败: %v"), rollbackErr)
		}
		q.handleFailure(mongo, apiClient, botMgr, task, oldImage)
		return err
	}

	// 步骤5：成功处理
	q.handleSuccess(mongo, apiClient, botMgr, task, oldImage)

	logrus.WithFields(logrus.Fields{
		"time":   time.Now().Format("2006-01-02 15:04:05"),
		"method": "executeTask",
		"took":   time.Since(startTime),
		"data": logrus.Fields{
			"task_id":  task.ID,
			"old_tag":  kubernetes.ExtractTag(oldImage),
			"new_tag":  task.DeployRequest.Version,
		},
	}).Infof(color.GreenString("任务执行完成: %s, 状态: success"), task.ID)
	return nil
}

// handleSuccess 成功处理
func (q *TaskQueue) handleSuccess(mongo *client.MongoClient, apiClient *api.APIClient, botMgr *telegram.BotManager, task *Task, oldImage string) {
	env := task.DeployRequest.Environments[0]

	// 1. 更新 Mongo 状态
	if err := mongo.UpdateTaskStatus(task.DeployRequest.Service, task.DeployRequest.Version, env, task.DeployRequest.User, "success"); err != nil {
		logrus.Errorf(color.RedString("更新MongoDB状态失败: %v"), err)
	}

	// 2. 调用 /status 接口
	if err := apiClient.UpdateStatus(models.StatusRequest{
		Service:     task.DeployRequest.Service,
		Version:     task.DeployRequest.Version,
		Environment: env,
		User:        task.DeployRequest.User,
		Status:      "success",
	}); err != nil {
		logrus.Errorf(color.RedString("推送成功状态失败: %v"), err)
	}

	// 3. 发送通知
	if err := botMgr.SendNotification(task.DeployRequest.Service, env, task.DeployRequest.User, oldImage, task.DeployRequest.Version, true); err != nil {
		logrus.Errorf(color.RedString("发送成功通知失败: %v"), err)
	} else {
		logrus.Infof(color.GreenString("通知发送成功"))
	}
}

// handleFailure 失败处理
func (q *TaskQueue) handleFailure(mongo *client.MongoClient, apiClient *api.APIClient, botMgr *telegram.BotManager, task *Task, oldImage string) {
	env := task.DeployRequest.Environments[0]

	// 1. 更新 Mongo 状态
	_ = mongo.UpdateTaskStatus(task.DeployRequest.Service, task.DeployRequest.Version, env, task.DeployRequest.User, "failure")

	// 2. 调用 /status 接口
	_ = apiClient.UpdateStatus(models.StatusRequest{
		Service:     task.DeployRequest.Service,
		Version:     task.DeployRequest.Version,
		Environment: env,
		User:        task.DeployRequest.User,
		Status:      "failure",
	})

	// 3. 发送通知
	_ = botMgr.SendNotification(task.DeployRequest.Service, env, task.DeployRequest.User, oldImage, task.DeployRequest.Version, false)
}

// handlePermanentFailure 永久失败处理
func (q *TaskQueue) handlePermanentFailure(mongo *client.MongoClient, apiClient *api.APIClient, botMgr *telegram.BotManager, task *Task) {
	q.handleFailure(mongo, apiClient, botMgr, task, "unknown")
}

// Enqueue 入队
func (q *TaskQueue) Enqueue(task *Task) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.queue.PushBack(task)
	logrus.WithFields(logrus.Fields{
		"time":   time.Now().Format("2006-01-02 15:04:05"),
		"method": "Enqueue",
		"data":   logrus.Fields{"task_id": task.ID},
	}).Infof(color.GreenString("任务已入队: %s"), task.ID)
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
		"data":   logrus.Fields{"task_id": task.ID},
	}).Infof(color.GreenString("任务已出队: %s"), task.ID)
	return task, true
}

// Stop 停止队列
func (q *TaskQueue) Stop() {
	close(q.stopCh)
	q.wg.Wait()
	logrus.Info(color.GreenString("任务队列停止"))
}

// getImageOrUnknown 获取镜像或返回 unknown
func getImageOrUnknown(s *models.ImageSnapshot) string {
	if s != nil && s.Image != "" {
		return s.Image
	}
	return "unknown"
}