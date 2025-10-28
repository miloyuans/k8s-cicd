// task.go
package task

import (
	"container/list"
	"fmt"
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

// TaskQueue 任务队列结构
type TaskQueue struct {
	queue    *list.List
	mu       sync.Mutex
	workers  int
	stopCh   chan struct{}
	wg       sync.WaitGroup
}

// NewTaskQueue 创建任务队列
func NewTaskQueue(workers int) *TaskQueue {
	q := &TaskQueue{
		queue:   list.New(),
		workers: workers,
		stopCh:  make(chan struct{}),
	}
	logrus.WithFields(logrus.Fields{
		"time":   time.Now().Format("2006-01-02 15:04:05"),
		"method": "NewTaskQueue",
	}).Infof(color.GreenString("任务队列初始化完成，worker数量: %d", workers))
	return q
}

// StartWorkers 启动任务worker
func (q *TaskQueue) StartWorkers(cfg *config.Config, mongo *client.MongoClient, k8s *kubernetes.K8sClient, botMgr *telegram.BotManager, apiClient *api.APIClient) {
	q.wg.Add(q.workers)
	for i := 0; i < q.workers; i++ {
		go q.worker(cfg, mongo, k8s, botMgr, apiClient, i+1)
	}
	go mongo.CleanCompletedTasks()
	logrus.WithFields(logrus.Fields{
		"time":   time.Now().Format("2006-01-02 15:04:05"),
		"method": "StartWorkers",
	}).Infof(color.GreenString("启动 %d 个任务worker", q.workers))
}

// worker 任务worker
func (q *TaskQueue) worker(cfg *config.Config, mongo *client.MongoClient, k8s *kubernetes.K8sClient, botMgr *telegram.BotManager, apiClient *api.APIClient, workerID int) {
	defer q.wg.Done()

	logrus.WithFields(logrus.Fields{
		"time":   time.Now().Format("2006-01-02 15:04:05"),
		"method": "worker",
	}).Infof(color.GreenString("Worker-%d 启动", workerID))

	for {
		select {
		case <-q.stopCh:
			logrus.WithFields(logrus.Fields{
				"time":   time.Now().Format("2006-01-02 15:04:05"),
				"method": "worker",
			}).Infof(color.GreenString("Worker-%d 停止", workerID))
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
					"service":     task.Service,
					"version":     task.Version,
					"environment": task.Environments[0],
					"user":        task.User,
					"status":      task.Status,
					"namespace":   task.Namespace,
				},
			}).Infof(color.GreenString("Worker-%d 开始执行任务: %s", workerID, task.ID))

			err := q.executeTask(cfg, mongo, k8s, apiClient, task, botMgr)
			if err != nil {
				logrus.WithFields(logrus.Fields{
					"time":   time.Now().Format("2006-01-02 15:04:05"),
					"method": "worker",
					"data": logrus.Fields{"task_id": task.ID},
				}).Errorf(color.RedString("Worker-%d 任务失败: %s, 错误: %v", workerID, task.ID, err))

				if task.Retries < cfg.Task.MaxRetries {
					task.Retries++
					retryDelay := time.Duration(cfg.Task.RetryDelay*task.Retries) * time.Second
					logrus.WithFields(logrus.Fields{
						"time":   time.Now().Format("2006-01-02 15:04:05"),
						"method": "worker",
						"data": logrus.Fields{"task_id": task.ID},
					}).Infof(color.YellowString("Worker-%d 任务重试 [%d/%d]，%ds后重试: %s", workerID, task.Retries, cfg.Task.MaxRetries, int(retryDelay.Seconds()), task.ID))
					time.Sleep(retryDelay)
					q.Enqueue(task)
				} else {
					logrus.WithFields(logrus.Fields{
						"time":   time.Now().Format("2006-01-02 15:04:05"),
						"method": "worker",
						"data": logrus.Fields{"task_id": task.ID},
					}).Errorf(color.RedString("Worker-%d 任务永久失败: %s", workerID, task.ID))
					q.handlePermanentFailure(mongo, apiClient, botMgr, task)
				}
			} else {
				logrus.WithFields(logrus.Fields{
					"time":   time.Now().Format("2006-01-02 15:04:05"),
					"method": "worker",
					"data": logrus.Fields{"task_id": task.ID},
				}).Infof(color.GreenString("Worker-%d 任务成功: %s", workerID, task.ID))
			}
		}
	}
}

// executeTask 执行任务（核心）
// executeTask 执行任务
func (q *TaskQueue) executeTask(cfg *config.Config, mongo *client.MongoClient, k8s *kubernetes.K8sClient, apiClient *api.APIClient, task *models.Task, botMgr *telegram.BotManager) error {
	startTime := time.Now()
	env := task.Environments[0]

	// 步骤1：验证命名空间
	if task.Namespace == "" {
		return fmt.Errorf("命名空间为空")
	}

	// 步骤2：快照 + 更新
	snapshot, err := k8s.CaptureAndUpdateImage(task.Namespace, task.Service, task.Version, mongo)
	if err != nil {
		logrus.Errorf(color.RedString("镜像更新失败: %v", err))
		if snapshot != nil {
			k8s.RollbackWithSnapshot(snapshot)
		}
		q.handleFailure(mongo, apiClient, botMgr, task, getImageOrUnknown(snapshot), task.Version)
		return err
	}
	if snapshot == nil {
		return nil
	}

	// 步骤3：等待 rollout
	ready, err := k8s.WaitForRolloutComplete(task.Namespace, task.Service, cfg.Deploy.WaitTimeout)
	if err != nil || !ready {
		logrus.Errorf(color.RedString("rollout 失败: %v", err))
		k8s.RollbackWithSnapshot(snapshot)
		q.handleFailure(mongo, apiClient, botMgr, task, snapshot.Image, task.Version)
		return err
	}

	// 步骤4：成功
	if err := botMgr.SendNotification(task.Service, env, task.User, snapshot.Image, task.Version, true); err != nil {
		logrus.Errorf(color.RedString("通知失败: %v", err))
	}
	mongo.UpdateTaskStatus(task.Service, task.Version, env, task.User, "success")
	apiClient.UpdateStatus(models.StatusRequest{
		Service:     task.Service,
		Version:     task.Version,
		Environment: env,
		User:        task.User,
		Status:      "success",
	})

	logrus.Infof(color.GreenString("任务成功: %s %s -> %s", task.ID, kubernetes.ExtractTag(snapshot.Image), task.Version))
	return nil
}

func getImageOrUnknown(s *models.ImageSnapshot) string {
	if s != nil && s.Image != "" { return s.Image }
	return "unknown"
}

// 辅助函数：安全获取镜像
func getImageOrUnknown(snapshot *models.ImageSnapshot) string {
	if snapshot != nil && snapshot.Image != "" {
		return snapshot.Image
	}
	return "unknown"
}

// handleFailure 统一失败处理
func (q *TaskQueue) handleFailure(mongo *client.MongoClient, apiClient *api.APIClient, botMgr *telegram.BotManager, task *models.Task, oldImage, newVersion string) {
	env := task.Environments[0]
	// Mongo
	_ = mongo.UpdateTaskStatus(task.Service, newVersion, env, task.User, "failure")
	// API
	_ = apiClient.UpdateStatus(models.StatusRequest{
		Service:     task.Service,
		Version:     newVersion,
		Environment: env,
		User:        task.User,
		Status:      "failure",
	})
	// 通知
	_ = botMgr.SendNotification(task.Service, env, task.User, oldImage, newVersion, false)
}

// handlePermanentFailure 永久失败处理
func (q *TaskQueue) handlePermanentFailure(mongo *client.MongoClient, apiClient *api.APIClient, botMgr *telegram.BotManager, task *models.Task) {
	env := task.Environments[0]
	// Mongo
	_ = mongo.UpdateTaskStatus(task.Service, task.Version, env, task.User, "failure")
	// API
	_ = apiClient.UpdateStatus(models.StatusRequest{
		Service:     task.Service,
		Version:     task.Version,
		Environment: env,
		User:        task.User,
		Status:      "failure",
	})
	// 通知
	_ = botMgr.SendNotification(task.Service, env, task.User, "unknown", task.Version, false)
}

// Enqueue / Dequeue / Stop
func (q *TaskQueue) Enqueue(task *models.Task) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.queue.PushBack(task)
	logrus.WithFields(logrus.Fields{
		"time":   time.Now().Format("2006-01-02 15:04:05"),
		"method": "Enqueue",
		"data": logrus.Fields{"task_id": task.ID},
	}).Infof(color.GreenString("任务已入队: %s", task.ID))
}

func (q *TaskQueue) Dequeue() (*models.Task, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.queue.Len() == 0 {
		return nil, false
	}
	e := q.queue.Front()
	task := e.Value.(*models.Task)
	q.queue.Remove(e)
	logrus.WithFields(logrus.Fields{
		"time":   time.Now().Format("2006-01-02 15:04:05"),
		"method": "Dequeue",
		"data": logrus.Fields{"task_id": task.ID},
	}).Infof(color.GreenString("任务已出队: %s", task.ID))
	return task, true
}

func (q *TaskQueue) Stop() {
	close(q.stopCh)
	q.wg.Wait()
	logrus.WithFields(logrus.Fields{
		"time":   time.Now().Format("2006-01-02 15:04:05"),
		"method": "Stop",
	}).Infof(color.GreenString("任务队列停止"))
}