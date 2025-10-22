package task

import (
	"container/list"
	"fmt"
	"sync"
	"time"

	"k8s-cicd/agent/api"
	"k8s-cicd/agent/config"
	"k8s-cicd/agent/client"
	"k8s-cicd/agent/kubernetes"
	"k8s-cicd/agent/models"
	"k8s-cicd/agent/telegram"

	"github.com/fatih/color"
	"github.com/sirupsen/logrus"
)

// TaskQueue 任务队列结构（FIFO先进先出，支持并发执行）
type TaskQueue struct {
	queue    *list.List        // 任务列表（双向链表实现FIFO）
	mu       sync.Mutex        // 队列锁（保证线程安全）
	workers  int               // 并发worker数量
	stopCh   chan struct{}     // 停止信号通道
	wg       sync.WaitGroup    // 等待组（等待所有worker完成）
}

// NewTaskQueue 创建新的任务队列
func NewTaskQueue(workers int) *TaskQueue {
	q := &TaskQueue{
		queue:   list.New(),
		workers: workers,
		stopCh:  make(chan struct{}),
	}

	logrus.Infof("任务队列初始化完成，worker数量: %d", workers)
	return q
}

// StartWorkers 启动所有任务worker
func (q *TaskQueue) StartWorkers(cfg *config.Config, redis *client.RedisClient, k8s *kubernetes.K8sClient, botMgr *telegram.BotManager, apiPusher *api.APIPusher) {
	green := color.New(color.FgGreen).SprintFunc()
	logrus.Infof("%s 启动 %d 个任务worker", green("👷"), q.workers)

	q.wg.Add(q.workers)
	for i := 0; i < q.workers; i++ {
		go q.worker(cfg, redis, k8s, botMgr, apiPusher, i+1)
	}

	q.wg.Wait()
	logrus.Info("所有任务worker已停止")
}

// worker 单个任务worker（无限循环执行任务）
func (q *TaskQueue) worker(cfg *config.Config, redis *client.RedisClient, k8s *kubernetes.K8sClient, botMgr *telegram.BotManager, apiPusher *api.APIPusher, workerID int) {
	defer q.wg.Done()

	yellow := color.New(color.FgYellow).SprintFunc()
	logrus.Infof("%s Worker-%d 启动", yellow("👷"), workerID)

	for {
		select {
		case <-q.stopCh:
			green := color.New(color.FgGreen).SprintFunc()
			logrus.Infof("%s Worker-%d 收到停止信号", green("🛑"), workerID)
			return
		default:
		}

		task, ok := q.Dequeue()
		if !ok {
			time.Sleep(1 * time.Second)
			continue
		}

		cyan := color.New(color.FgCyan).SprintFunc()
		logrus.Infof("%s Worker-%d 执行任务: %s", cyan("🔨"), workerID, task.ID)

		err := executeTask(cfg, redis, k8s, apiPusher, task, botMgr)
		if err != nil {
			red := color.New(color.FgRed).SprintFunc()
			logrus.Errorf("%s Worker-%d 任务执行失败: %v", red("💥"), workerID, err)

			if task.Retries < cfg.Task.MaxRetries {
				task.Retries++
				retryDelay := time.Duration(cfg.Task.RetryDelay*task.Retries) * time.Second
				green := color.New(color.FgGreen).SprintFunc()
				logrus.Infof("%s Worker-%d 任务重试 [%d/%d]，%d秒后重试: %s",
					green("🔄"), workerID, task.Retries, cfg.Task.MaxRetries, retryDelay.Seconds(), task.ID)
				
				time.Sleep(retryDelay)
				q.Enqueue(task)
			} else {
				red := color.New(color.FgRed).SprintFunc()
				logrus.Errorf("%s Worker-%d 任务永久失败，已达最大重试次数: %s", red("🪦"), workerID, task.ID)
			}
		} else {
			green := color.New(color.FgGreen).SprintFunc()
			logrus.Infof("%s Worker-%d 任务成功完成: %s", green("🎉"), workerID, task.ID)
		}
	}
}

// Enqueue 将任务加入队列尾部（FIFO）
func (q *TaskQueue) Enqueue(task models.Task) {
	q.mu.Lock()
	defer q.mu.Unlock()

	q.queue.PushBack(task)
	
	green := color.New(color.FgGreen).SprintFunc()
	logrus.Debugf("%s 任务入队: %s (队列长度: %d)", green("📥"), task.ID, q.queue.Len())
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
	
	yellow := color.New(color.FgYellow).SprintFunc()
	logrus.Debugf("%s 任务出队: %s (剩余: %d)", yellow("📤"), task.ID, q.queue.Len())
	
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
	
	red := color.New(color.FgRed).SprintFunc()
	logrus.Infof("%s 任务队列已停止 (剩余任务: %d)", red("🛑"), q.Len())
}

// IsEmpty 检查队列是否为空
func (q *TaskQueue) IsEmpty() bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.queue.Len() == 0
}