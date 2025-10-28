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
	startTime := time.Now()
	q := &TaskQueue{
		queue:   list.New(),
		workers: workers,
		stopCh:  make(chan struct{}),
	}
	logrus.WithFields(logrus.Fields{
		"time":   time.Now().Format("2006-01-02 15:04:05"),
		"method": "NewTaskQueue",
		"took":   time.Since(startTime),
	}).Infof(color.GreenString("任务队列初始化完成，worker数量: %d", workers))
	return q
}

// StartWorkers 启动任务worker
func (q *TaskQueue) StartWorkers(cfg *config.Config, mongo *client.MongoClient, k8s *kubernetes.K8sClient, botMgr *telegram.BotManager, apiClient *api.APIClient) {
	startTime := time.Now()
	q.wg.Add(q.workers)
	for i := 0; i < q.workers; i++ {
		go q.worker(cfg, mongo, k8s, botMgr, apiClient, i+1)
	}
	// 保留原清理任务
	go mongo.CleanCompletedTasks()
	logrus.WithFields(logrus.Fields{
		"time":   time.Now().Format("2006-01-02 15:04:05"),
		"method": "StartWorkers",
		"took":   time.Since(startTime),
	}).Infof(color.GreenString("启动 %d 个任务worker", q.workers))
}

// worker 任务worker（保留完整重试逻辑）
func (q *TaskQueue) worker(cfg *config.Config, mongo *client.MongoClient, k8s *kubernetes.K8sClient, botMgr *telegram.BotManager, apiClient *api.APIClient, workerID int) {
	startTime := time.Now()
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
					"time":   time.Now().Format("2006-01-02 15: 04: 05"),
					"method": "worker",
					"data": logrus.Fields{"task_id": task.ID},
				}).Infof(color.GreenString("Worker-%d 任务成功: %s", workerID, task.ID))
			}
		}
	}
}

// executeTask 执行任务（核心优化）
func (q *TaskQueue) executeTask(cfg *config.Config, mongo *client.MongoClient, k8s *kubernetes.K8sClient, apiClient *api.APIClient, task *models.Task, botMgr *telegram.BotManager) error {
	startTime := time.Now()
	env := task.Environments[0]

	// 步骤1：验证命名空间
	if task.Namespace == "" {
		logrus.WithFields(logrus.Fields{
			"time":   time.Now().Format("2006-01-02 15:04:05"),
			"method": "executeTask",
			"data": logrus.Fields{"task_id": task.ID},
		}).Errorf(color.RedString("命名空间为空"))
		return fmt.Errorf("命名空间为空")
	}

	// 步骤2：更新前快照（关键！）
	snapshot, err := k8s.UpdateWorkloadImage(task.Namespace, task.Service, task.Version)
	if err != nil {
		logrus.WithFields(logrus.Fields{
			"time":   time.Now().Format("2006-01-02 15:04:05"),
			"method": "executeTask",
			"data": logrus.Fields{"task_id": task.ID},
		}).Errorf(color.RedString("镜像更新失败: %v", err))

		if snapshot != nil {
			if rollbackErr := k8s.RollbackWithSnapshot(snapshot); rollbackErr != nil {
				logrus.Errorf(color.RedString("回滚失败: %v", rollbackErr))
				return fmt.Errorf("更新失败且回滚失败")
			}
			logrus.Infof(color.GreenString("自动回滚成功至: %s", snapshot.Tag))
		}
		q.handleFailure(mongo, apiClient, botMgr, task, snapshot.Image, task.Version)
		return err
	}
	if snapshot == nil {
		logrus.Warnf(color.YellowString("无运行 Pod，跳过更新: %s", task.Service))
		return nil
	}

	// 步骤3：等待 rollout
	ready, err := k8s.WaitForRolloutComplete(task.Namespace, task.Service, cfg.Deploy.WaitTimeout)
	if err != nil || !ready {
		logrus.Errorf(color.RedString("rollout 超时或失败: %v", err))
		if rollbackErr := k8s.RollbackWithSnapshot(snapshot); rollbackErr != nil {
			logrus.Errorf(color.RedString("回滚失败: %v", rollbackErr))
			return fmt.Errorf("rollout失败且回滚失败")
		}
		logrus.Infof(color.GreenString("自动回滚成功至: %s", snapshot.Tag))
		q.handleFailure(mongo, apiClient, botMgr, task, snapshot.Image, task.Version)
		return err
	}

	// 步骤4：成功处理
	if err := mongo.StoreImageSnapshot(snapshot, task.ID); err != nil {
		logrus.Warnf(color.YellowString("存储快照失败: %v", err))
	}

	if err := botMgr.SendNotification(task.Service, env, task.User, snapshot.Image, task.Version, true); err != nil {
		logrus.Errorf(color.RedString("发送成功通知失败: %v", err))
	} else {
		logrus.Infof(color.GreenString("通知发送成功"))
	}

	if err := mongo.UpdateTaskStatus(task.Service, task.Version, env, task.User, "success"); err != nil {
		logrus.Errorf(color.RedString("更新MongoDB状态失败: %v", err))
	}

	if err := apiClient.UpdateStatus(models.StatusRequest{
		Service:     task.Service,
		Version:     task.Version,
		Environment: env,
		User:        task.User,
		Status:      "success",
	}); err != nil {
		logrus.Errorf(color.RedString("推送成功状态失败: %v", err))
	}

	logrus.WithFields(logrus.Fields{
		"time":   time.Now().Format("2006-01-02 15:04:05"),
		"method": "executeTask",
		"took":   time.Since(startTime),
		"data": logrus.Fields{
			"task_id": task.ID, "old_tag": kubernetes.ExtractTag(snapshot.Image), "new_tag": task.Version,
		},
	}).Infof(color.GreenString("任务执行完成: %s, 状态: success", task.ID))
	return nil
}

// handleFailure 统一失败处理
func (q *TaskQueue) handleFailure(mongo *client.MongoClient, apiClient *api.APIClient, botMgr *telegram.BotManager, task *models.Task, oldImage, newVersion string) {
	env := task.Environments[0]
	// Mongo
	if err := mongo.UpdateTaskStatus(task.Service, newVersion, env, task.User, "failure"); err != nil {
		logrus.Errorf(color.RedString("更新MongoDB状态失败: %v", err))
	}
	// API
	if err := apiClient.UpdateStatus(models.StatusRequest{
		Service:     task.Service,
		Version:     newVersion,
		Environment: env,
		User:        task.User,
		Status:      "failure",
	}); err != nil {
		logrus.Errorf(color.RedString("推送失败状态失败: %v", err))
	}
	// 通知
	if err := botMgr.SendNotification(task.Service, env, task.User, oldImage, newVersion, false); err != nil {
		logrus.Errorf(color.RedString("发送失败通知失败: %v", err))
	}
}

// handlePermanentFailure 永久失败处理（保留原逻辑）
func (q *TaskQueue) handlePermanentFailure(mongo *client.MongoClient, apiClient *api.APIClient, botMgr *telegram.BotManager, task *models.Task) {
	env := task.Environments[0]
	// Mongo
	if err := mongo.UpdateTaskStatus(task.Service, task.Version, env, task.User, "failure"); err != nil {
		logrus.Errorf(color.RedString("更新MongoDB状态失败: %v", err))
	}
	// API
	if err := apiClient.UpdateStatus(models.StatusRequest{
		Service:     task.Service,
		Version:     task.Version,
		Environment: env,
		User:        task.User,
		Status:      "failure",
	}); err != nil {
		logrus.Errorf(color.RedString("推送失败状态失败: %v", err))
	}
	// 通知
	if err := botMgr.SendNotification(task.Service, env, task.User, "unknown", task.Version, false); err != nil {
		logrus.Errorf(color.RedString("发送失败通知失败: %v", err))
	}
}

// Enqueue / Dequeue / Stop（完整保留）
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
	startTime := time.Now()
	close(q.stopCh)
	q.wg.Wait()
	logrus.WithFields(logrus.Fields{
		"time":   time.Now().Format("2006-01-02 15:04:05"),
		"method": "Stop",
		"took":   time.Since(startTime),
	}).Infof(color.GreenString("任务队列停止"))
}