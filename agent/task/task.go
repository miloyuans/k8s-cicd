// task.go
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
	}).Infof(color.GreenString("任务队列初始化完成，worker数量: %d", workers))
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
	}).Infof(color.GreenString("启动 %d 个任务worker", q.workers))
}

// worker 任务worker
func (q *TaskQueue) worker(cfg *config.Config, mongo *client.MongoClient, k8s *kubernetes.K8sClient, botMgr *telegram.BotManager, apiClient *api.APIClient, workerID int) {
	startTime := time.Now()
	defer q.wg.Done()

	logrus.WithFields(logrus.Fields{
		"time":   time.Now().Format("2006-01-02 15:04:05"),
		"method": "worker",
		"took":   time.Since(startTime),
	}).Infof(color.GreenString("Worker-%d 启动", workerID))

	for {
		select {
		case <-q.stopCh:
			logrus.WithFields(logrus.Fields{
				"time":   time.Now().Format("2006-01-02 15:04:05"),
				"method": "worker",
				"took":   time.Since(startTime),
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
				"took":   time.Since(startTime),
				"data": logrus.Fields{
					"task_id":     task.ID,
					"service":     task.Service,
					"version":     task.Version,
					"environment": task.Environments,
					"user":        task.User,
					"status":      task.Status,
				},
			}).Infof(color.GreenString("Worker-%d 开始执行任务: %s", workerID, task.ID))

			err := q.executeTask(cfg, mongo, k8s, apiClient, task, botMgr)
			if err != nil {
				logrus.WithFields(logrus.Fields{
					"time":   time.Now().Format("2006-01-02 15:04:05"),
					"method": "worker",
					"took":   time.Since(startTime),
				}).Errorf(color.RedString("Worker-%d 任务失败: %s, 错误: %v", workerID, task.ID, err))
				if task.Retries < cfg.Task.MaxRetries {
					task.Retries++
					retryDelay := time.Duration(cfg.Task.RetryDelay*task.Retries) * time.Second
					logrus.WithFields(logrus.Fields{
						"time":   time.Now().Format("2006-01-02 15:04:05"),
						"method": "worker",
						"took":   time.Since(startTime),
					}).Infof(color.GreenString("Worker-%d 任务重试 [%d/%d]，%ds后重试: %s", workerID, task.Retries, cfg.Task.MaxRetries, int(retryDelay.Seconds()), task.ID))
					time.Sleep(retryDelay)
					q.Enqueue(task)
				} else {
					logrus.WithFields(logrus.Fields{
						"time":   time.Now().Format("2006-01-02 15:04:05"),
						"method": "worker",
						"took":   time.Since(startTime),
					}).Errorf(color.RedString("Worker-%d 任务永久失败: %s", workerID, task.ID))
				}
			} else {
				logrus.WithFields(logrus.Fields{
					"time":   time.Now().Format("2006-01-02 15:04:05"),
					"method": "worker",
					"took":   time.Since(startTime),
				}).Infof(color.GreenString("Worker-%d 任务成功: %s", workerID, task.ID))
			}
		}
	}
}

// executeTask 执行单个任务
func (q *TaskQueue) executeTask(cfg *config.Config, mongo *client.MongoClient, k8s *kubernetes.K8sClient, apiClient *api.APIClient, task *models.Task, botMgr *telegram.BotManager) error {
	startTime := time.Now()
	namespace := task.Environments[0]

	// 步骤1：检查工作负载是否运行
	if !k8s.IsWorkloadRunning(namespace, task.Service) {
		logrus.WithFields(logrus.Fields{
			"time":   time.Now().Format("2006-01-02 15:04:05"),
			"method": "executeTask",
			"took":   time.Since(startTime),
			"data": logrus.Fields{
				"task_id": task.ID,
			},
		}).Infof(color.GreenString("无Pod运行，跳过更新"))
		// 更新状态为skipped
		err := mongo.UpdateTaskStatus(task.Service, task.Version, namespace, task.User, "skipped")
		if err != nil {
			logrus.WithFields(logrus.Fields{
				"time":   time.Now().Format("2006-01-02 15:04:05"),
				"method": "executeTask",
				"took":   time.Since(startTime),
				"data": logrus.Fields{
					"task_id": task.ID,
				},
			}).Errorf(color.RedString("更新MongoDB状态失败: %v", err))
		}
		err = apiClient.UpdateStatus(models.StatusRequest{
			Service:     task.Service,
			Version:     task.Version,
			Environment: namespace,
			User:        task.User,
			Status:      "skipped",
		})
		if err != nil {
			logrus.WithFields(logrus.Fields{
				"time":   time.Now().Format("2006-01-02 15:04:05"),
				"method": "executeTask",
				"took":   time.Since(startTime),
				"data": logrus.Fields{
					"task_id": task.ID,
				},
			}).Errorf(color.RedString("推送状态失败: %v", err))
		}
		return nil
	}

	// 步骤2：记录旧版本
	oldVersion := k8s.GetCurrentImage(namespace, task.Service)
	logrus.WithFields(logrus.Fields{
		"time":   time.Now().Format("2006-01-02 15:04:05"),
		"method": "executeTask",
		"took":   time.Since(startTime),
		"data": logrus.Fields{
			"task_id":    task.ID,
			"old_version": oldVersion,
		},
	}).Infof(color.GreenString("记录旧版本: %s", oldVersion))

	// 步骤3：更新镜像
	err := k8s.UpdateWorkloadImage(namespace, task.Service, task.Version)
	if err != nil {
		logrus.WithFields(logrus.Fields{
			"time":   time.Now().Format("2006-01-02 15:04:05"),
			"method": "executeTask",
			"took":   time.Since(startTime),
			"data": logrus.Fields{
				"task_id": task.ID,
			},
		}).Errorf(color.RedString("更新镜像失败: %v", err))
		return err
	}

	// 步骤4：等待就绪
	var success bool
	err = k8s.WaitForWorkloadReady(namespace, task.Service)
	if err != nil {
		logrus.WithFields(logrus.Fields{
			"time":   time.Now().Format("2006-01-02 15:04:05"),
			"method": "executeTask",
			"took":   time.Since(startTime),
			"data": logrus.Fields{
				"task_id": task.ID,
			},
		}).Errorf(color.RedString("部署超时: %v", err))
		success = false
		rollbackErr := k8s.RollbackWorkload(namespace, task.Service, oldVersion)
		if rollbackErr != nil {
			logrus.WithFields(logrus.Fields{
				"time":   time.Now().Format("2006-01-02 15:04:05"),
				"method": "executeTask",
				"took":   time.Since(startTime),
				"data": logrus.Fields{
					"task_id":    task.ID,
					"old_version": oldVersion,
				},
			}).Errorf(color.RedString("回滚失败: %v", rollbackErr))
		} else {
			logrus.WithFields(logrus.Fields{
				"time":   time.Now().Format("2006-01-02 15:04:05"),
				"method": "executeTask",
				"took":   time.Since(startTime),
				"data": logrus.Fields{
					"task_id":    task.ID,
					"old_version": oldVersion,
				},
			}).Infof(color.GreenString("回滚成功: %s", oldVersion))
		}
	} else {
		logrus.WithFields(logrus.Fields{
			"time":   time.Now().Format("2006-01-02 15:04:05"),
			"method": "executeTask",
			"took":   time.Since(startTime),
			"data": logrus.Fields{
				"task_id": task.ID,
			},
		}).Infof(color.GreenString("新版本就绪"))
		success = true
	}

	// 步骤5：发送通知
	err = botMgr.SendNotification(task.Service, namespace, task.User, oldVersion, task.Version, success)
	if err != nil {
		logrus.WithFields(logrus.Fields{
			"time":   time.Now().Format("2006-01-02 15:04:05"),
			"method": "executeTask",
			"took":   time.Since(startTime),
			"data": logrus.Fields{
				"task_id": task.ID,
			},
		}).Errorf(color.RedString("发送通知失败: %v", err))
	} else {
		logrus.WithFields(logrus.Fields{
			"time":   time.Now().Format("2006-01-02 15:04:05"),
			"method": "executeTask",
			"took":   time.Since(startTime),
			"data": logrus.Fields{
				"task_id": task.ID,
			},
		}).Infof(color.GreenString("通知发送成功"))
	}

	// 步骤6：更新状态
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
			"data": logrus.Fields{
				"task_id": task.ID,
			},
		}).Errorf(color.RedString("更新MongoDB状态失败: %v", err))
	} else {
		logrus.WithFields(logrus.Fields{
			"time":   time.Now().Format("2006-01-02 15:04:05"),
			"method": "executeTask",
			"took":   time.Since(startTime),
			"data": logrus.Fields{
				"task_id": task.ID,
				"status":  status,
			},
		}).Infof(color.GreenString("MongoDB状态更新成功: %s", status))
	}

	// 步骤7：推送状态
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
			"data": logrus.Fields{
				"task_id": task.ID,
			},
		}).Errorf(color.RedString("推送状态失败: %v", err))
	} else {
		logrus.WithFields(logrus.Fields{
			"time":   time.Now().Format("2006-01-02 15:04:05"),
			"method": "executeTask",
			"took":   time.Since(startTime),
			"data": logrus.Fields{
				"task_id": task.ID,
				"status":  status,
			},
		}).Infof(color.GreenString("状态推送成功"))
	}

	// 步骤8：总结任务执行结果
	logrus.WithFields(logrus.Fields{
		"time":   time.Now().Format("2006-01-02 15:04:05"),
		"method": "executeTask",
		"took":   time.Since(startTime),
		"data": logrus.Fields{
			"task_id":     task.ID,
			"service":     task.Service,
			"version":     task.Version,
			"environment": namespace,
			"user":        task.User,
			"status":      status,
		},
	}).Infof(color.GreenString("任务执行完成: %s, 状态: %s, 耗时: %v", task.ID, status, time.Since(startTime)))
	return nil
}

// Enqueue 添加任务到队列
func (q *TaskQueue) Enqueue(task *models.Task) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.queue.PushBack(task)
	logrus.WithFields(logrus.Fields{
		"time":   time.Now().Format("2006-01-02 15:04:05"),
		"method": "Enqueue",
		"data": logrus.Fields{
			"task_id": task.ID,
		},
	}).Infof(color.GreenString("任务已入队: %s", task.ID))
}

// Dequeue 从队列取出任务
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
		"data": logrus.Fields{
			"task_id": task.ID,
		},
	}).Infof(color.GreenString("任务已出队: %s", task.ID))
	return task, true
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
	}).Infof(color.GreenString("任务队列停止"))
}