//task.go
package task

import (
	"container/list"
	"fmt"
	"strings"
	"sync"
	"time"

	"k8s-cicd/agent/api"
	"k8s-cicd/agent/client"
	"k8s-cicd/agent/config"
	"k8s-cicd/agent/kubernetes"
	"k8s-cicd/agent/models"
	"k8s-cicd/agent/telegram"

	"github.com/sirupsen/logrus"
)

// TaskQueue 任务队列结构
type TaskQueue struct {
	queue    *list.List    // 任务列表
	mu       sync.Mutex    // 队列锁
	workers  int           // worker数量
	stopCh   chan struct{} // 停止通道
	wg       sync.WaitGroup // 等待组
}

// NewTaskQueue 创建任务队列
func NewTaskQueue(workers int) *TaskQueue {
	startTime := time.Now()
	// 步骤1：初始化队列
	q := &TaskQueue{
		queue:   list.New(),
		workers: workers,
		stopCh:  make(chan struct{}),
	}
	logrus.WithFields(logrus.Fields{
		"time":   time.Now().Format("2006-01-02 15:04:05"),
		"method": "NewTaskQueue",
		"took":   time.Since(startTime),
	}).Infof("任务队列初始化完成，worker数量: %d", workers)
	return q
}

// StartWorkers 启动任务worker
func (q *TaskQueue) StartWorkers(cfg *config.Config, mongo *client.MongoClient, k8s *kubernetes.K8sClient, botMgr *telegram.BotManager, apiClient *api.APIClient) {
	startTime := time.Now()
	// 步骤1：添加等待组
	q.wg.Add(q.workers)
	// 步骤2：启动worker
	for i := 0; i < q.workers; i++ {
		go q.worker(cfg, mongo, k8s, botMgr, apiClient, i+1)
	}
	// 步骤3：启动清理任务
	go mongo.CleanCompletedTasks()
	logrus.WithFields(logrus.Fields{
		"time":   time.Now().Format("2006-01-02 15:04:05"),
		"method": "StartWorkers",
		"took":   time.Since(startTime),
	}).Infof("启动 %d 个任务worker", q.workers)
}

// worker 任务worker
func (q *TaskQueue) worker(cfg *config.Config, mongo *client.MongoClient, k8s *kubernetes.K8sClient, botMgr *telegram.BotManager, apiClient *api.APIClient, workerID int) {
	startTime := time.Now()
	defer q.wg.Done()

	logrus.WithFields(logrus.Fields{
		"time":   time.Now().Format("2006-01-02 15:04:05"),
		"method": "worker",
		"took":   time.Since(startTime),
	}).Infof("Worker-%d 启动", workerID)

	for {
		select {
		case <-q.stopCh:
			logrus.WithFields(logrus.Fields{
				"time":   time.Now().Format("2006-01-02 15:04:05"),
				"method": "worker",
				"took":   time.Since(startTime),
			}).Infof("Worker-%d 停止", workerID)
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
				"took":   time.Since(startTime),
			}).Infof("Worker-%d 执行任务: %s", workerID, task.ID)

			err := executeTask(cfg, mongo, k8s, apiClient, task, botMgr)
			if err != nil {
				logrus.WithFields(logrus.Fields{
					"time":   time.Now().Format("2006-01-02 15:04:05"),
					"method": "worker",
					"took":   time.Since(startTime),
				}).Errorf("Worker-%d 任务失败: %v", workerID, err)
				if task.Retries < cfg.Task.MaxRetries {
					task.Retries++
					retryDelay := time.Duration(cfg.Task.RetryDelay*task.Retries) * time.Second
					logrus.WithFields(logrus.Fields{
						"time":   time.Now().Format("2006-01-02 15:04:05"),
						"method": "worker",
						"took":   time.Since(startTime),
					}).Infof("Worker-%d 任务重试 [%d/%d]，%ds后重试: %s", workerID, task.Retries, cfg.Task.MaxRetries, int(retryDelay.Seconds()), task.ID)
					time.Sleep(retryDelay)
					q.Enqueue(task)
				} else {
					logrus.WithFields(logrus.Fields{
						"time":   time.Now().Format("2006-01-02 15:04:05"),
						"method": "worker",
						"took":   time.Since(startTime),
					}).Errorf("Worker-%d 任务永久失败: %s", workerID, task.ID)
				}
			} else {
				logrus.WithFields(logrus.Fields{
					"time":   time.Now().Format("2006-01-02 15:04:05"),
					"method": "worker",
					"took":   time.Since(startTime),
				}).Infof("Worker-%d 任务成功: %s", workerID, task.ID)
			}
		}
	}
}

// Enqueue 任务入队
func (q *TaskQueue) Enqueue(task models.Task) {
	startTime := time.Now()
	q.mu.Lock()
	defer q.mu.Unlock()

	q.queue.PushBack(task)
	logrus.WithFields(logrus.Fields{
		"time":   time.Now().Format("2006-01-02 15:04:05"),
		"method": "Enqueue",
		"took":   time.Since(startTime),
	}).Debugf("任务入队: %s (队列长度: %d)", task.ID, q.queue.Len())
}

// Dequeue 任务出队
func (q *TaskQueue) Dequeue() (models.Task, bool) {
	startTime := time.Now()
	q.mu.Lock()
	defer q.mu.Unlock()

	if q.queue.Len() == 0 {
		return models.Task{}, false
	}

	element := q.queue.Front()
	q.queue.Remove(element)
	task := element.Value.(models.Task)
	logrus.WithFields(logrus.Fields{
		"time":   time.Now().Format("2006-01-02 15:04:05"),
		"method": "Dequeue",
		"took":   time.Since(startTime),
	}).Debugf("任务出队: %s (剩余: %d)", task.ID, q.queue.Len())
	return task, true
}

// Len 返回队列长度
func (q *TaskQueue) Len() int {
	startTime := time.Now()
	q.mu.Lock()
	defer q.mu.Unlock()
	length := q.queue.Len()
	logrus.WithFields(logrus.Fields{
		"time":   time.Now().Format("2006-01-02 15:04:05"),
		"method": "Len",
		"took":   time.Since(startTime),
	}).Debugf("队列长度: %d", length)
	return length
}

// Stop 停止队列
func (q *TaskQueue) Stop() {
	startTime := time.Now()
	close(q.stopCh)
	q.wg.Wait()
	logrus.WithFields(logrus.Fields{
		"time":   time.Now().Format("2006-01-02 15:04:05"),
		"method": "Stop",
		"took":   time.Since(startTime),
	}).Infof("任务队列停止 (剩余任务: %d)", q.Len())
}

// executeTask 执行部署任务
func (q *TaskQueue) executeTask(cfg *config.Config, mongo *client.MongoClient, k8s *kubernetes.K8sClient, apiClient *api.APIClient, task models.Task, botMgr *telegram.BotManager) error {
	startTime := time.Now()
	// 步骤1：检查任务状态
	if task.Status != "pending" {
		logrus.WithFields(logrus.Fields{
			"time":   time.Now().Format("2006-01-02 15:04:05"),
			"method": "executeTask",
			"took":   time.Since(startTime),
		}).Infof("任务非pending状态，跳过执行: %s, 状态: %s", task.ID, task.Status)
		return nil
	}

	// 步骤2：获取命名空间
	namespace := task.Environments[0]
	logrus.WithFields(logrus.Fields{
		"time":   time.Now().Format("2006-01-02 15:04:05"),
		"method": "executeTask",
		"took":   time.Since(startTime),
	}).Infof("执行部署任务: 服务=%s, 环境=%s, 版本=%s, 用户=%s", task.Service, namespace, task.Version, task.User)

	// 步骤3：获取旧版本
	oldVersion, err := k8s.GetCurrentImage(namespace, task.Service)
	if err != nil {
		logrus.WithFields(logrus.Fields{
			"time":   time.Now().Format("2006-01-02 15:04:05"),
			"method": "executeTask",
			"took":   time.Since(startTime),
		}).Warnf("获取旧版本失败: %v", err)
		oldVersion = "unknown"
	}

	// 步骤4：更新镜像
	newImage := task.Version
	err = k8s.UpdateDeploymentImage(namespace, task.Service, newImage)
	if err != nil {
		logrus.WithFields(logrus.Fields{
			"time":   time.Now().Format("2006-01-02 15:04:05"),
			"method": "executeTask",
			"took":   time.Since(startTime),
		}).Errorf("更新镜像失败: %v", err)
		mongo.UpdateTaskStatus(task.Service, task.Version, namespace, task.User, "failure")
		botMgr.SendNotification(task.Service, namespace, task.User, oldVersion, task.Version, false)
		return fmt.Errorf("更新镜像失败: %v", err)
	}

	// 步骤5：等待就绪
	logrus.WithFields(logrus.Fields{
		"time":   time.Now().Format("2006-01-02 15:04:05"),
		"method": "executeTask",
		"took":   time.Since(startTime),
	}).Info("等待新版本就绪")
	success := true
	err = k8s.WaitForDeploymentReady(namespace, task.Service, task.Version)
	if err != nil {
		logrus.WithFields(logrus.Fields{
			"time":   time.Now().Format("2006-01-02 15:04:05"),
			"method": "executeTask",
			"took":   time.Since(startTime),
		}).Errorf("部署超时: %v", err)
		success = false
		err = k8s.RollbackDeployment(namespace, task.Service, oldVersion)
		if err != nil {
			logrus.WithFields(logrus.Fields{
				"time":   time.Now().Format("2006-01-02 15:04:05"),
				"method": "executeTask",
				"took":   time.Since(startTime),
			}).Errorf("回滚失败: %v", err)
		}
	}

	// 步骤6：发送通知
	err = botMgr.SendNotification(task.Service, namespace, task.User, oldVersion, task.Version, success)
	if err != nil {
		logrus.WithFields(logrus.Fields{
			"time":   time.Now().Format("2006-01-02 15:04:05"),
			"method": "executeTask",
			"took":   time.Since(startTime),
		}).Errorf("发送通知失败: %v", err)
	}

	// 步骤7：更新状态
	status := "success"
	if !success {
		status = "failure"
	}
	err = mongo.UpdateTaskStatus(task.Service, task.Version, namespace, task.User, status)
	if err != nil {
		logrus.WithFields(logrus.Fields{
			"time":   time.Now().Format("2006-01-02 15:04:05"),
			"method": "executeTask",
			"took":   time.Since(startTime),
		}).Errorf("更新状态失败: %v", err)
	}

	// 步骤8：推送状态
	statusReq := models.StatusRequest{
		Service:     task.Service,
		Version:     task.Version,
		Environment: namespace,
		User:        task.User,
		Status:      status,
	}
	err = apiClient.UpdateStatus(statusReq)
	if err != nil {
		logrus.WithFields(logrus.Fields{
			"time":   time.Now().Format("2006-01-02 15:04:05"),
			"method": "executeTask",
			"took":   time.Since(startTime),
		}).Errorf("推送状态失败: %v", err)
	}

	// 步骤9：记录总结
	logrus.WithFields(logrus.Fields{
		"time":   time.Now().Format("2006-01-02 15:04:05"),
		"method": "executeTask",
		"took":   time.Since(startTime),
	}).Infof("任务完成: 服务=%s, 状态=%s, 耗时=%v", task.Service, status, time.Since(startTime))
	return nil
}