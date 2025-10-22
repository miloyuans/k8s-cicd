package task

import (
	"container/list"
	"fmt"
	"sync"
	"time"

	"k8s-cicd/agent/config"
	"k8s-cicd/client"
	"k8s-cicd/kubernetes"
	"k8s-cicd/models"
	"k8s-cicd/telegram"

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
// 参数：workers - 并发worker数量
// 返回：初始化好的任务队列
func NewTaskQueue(workers int) *TaskQueue {
	// 步骤1：初始化队列结构
	q := &TaskQueue{
		queue:   list.New(),           // 创建空链表
		workers: workers,              // 设置worker数量
		stopCh:  make(chan struct{}),  // 创建停止信号通道
	}

	// 步骤2：预创建worker（稍后启动）
	logrus.Infof("任务队列初始化完成，worker数量: %d", workers)
	return q
}

// StartWorkers 启动所有任务worker
// 功能：
// 1. 为每个worker启动一个goroutine
// 2. 等待所有worker完成（阻塞直到Stop调用）
func (q *TaskQueue) StartWorkers(cfg *config.Config, redis *client.RedisClient, k8s *kubernetes.K8sClient, botMgr *telegram.BotManager) {
	green := color.New(color.FgGreen).SprintFunc()
	logrus.Infof("%s 启动 %d 个任务worker", green("👷"), q.workers)

	// 步骤1：启动指定数量的worker
	q.wg.Add(q.workers)
	for i := 0; i < q.workers; i++ {
		go q.worker(cfg, redis, k8s, botMgr, i+1) // 传递worker编号
	}

	// 步骤2：等待所有worker完成
	q.wg.Wait()
	logrus.Info("所有任务worker已停止")
}

// worker 单个任务worker（无限循环执行任务）
func (q *TaskQueue) worker(cfg *config.Config, redis *client.RedisClient, k8s *kubernetes.K8sClient, botMgr *telegram.BotManager, workerID int) {
	defer q.wg.Done() // worker退出时通知等待组

	yellow := color.New(color.FgYellow).SprintFunc()
	logrus.Infof("%s Worker-%d 启动", yellow("👷"), workerID)

	for {
		// ================================
		// 步骤1：检查停止信号
		// ================================
		select {
		case <-q.stopCh:
			green := color.New(color.FgGreen).SprintFunc()
			logrus.Infof("%s Worker-%d 收到停止信号", green("🛑"), workerID)
			return
		default:
			// 继续执行任务
		}

		// ================================
		// 步骤2：从队列获取任务（阻塞式）
		// ================================
		task, ok := q.Dequeue()
		if !ok {
			// 队列为空，休眠1秒后继续轮询
			time.Sleep(1 * time.Second)
			continue
		}

		// ================================
		// 步骤3：执行任务
		// ================================
		cyan := color.New(color.FgCyan).SprintFunc()
		logrus.Infof("%s Worker-%d 执行任务: %s", cyan("🔨"), workerID, task.ID)

		err := executeTask(cfg, redis, k8s, task, botMgr)
		if err != nil {
			red := color.New(color.FgRed).SprintFunc()
			logrus.Errorf("%s Worker-%d 任务执行失败: %v", red("💥"), workerID, err)

			// ================================
			// 步骤4：重试逻辑
			// ================================
			if task.Retries < cfg.Task.MaxRetries {
				task.Retries++ // 增加重试次数
				
				// 指数退避：第1次10s，第2次20s，第3次30s
				retryDelay := time.Duration(cfg.Task.RetryDelay*task.Retries) * time.Second
				green := color.New(color.FgGreen).SprintFunc()
				logrus.Infof("%s Worker-%d 任务重试 [%d/%d]，%d秒后重试: %s",
					green("🔄"), workerID, task.Retries, cfg.Task.MaxRetries, retryDelay.Seconds(), task.ID)
				
				time.Sleep(retryDelay)
				q.Enqueue(task) // 重新入队
			} else {
				// 达到最大重试次数，永久失败
				red := color.New(color.FgRed).SprintFunc()
				logrus.Errorf("%s Worker-%d 任务永久失败，已达最大重试次数: %s", red("🪦"), workerID, task.ID)
			}
		} else {
			// 任务成功完成
			green := color.New(color.FgGreen).SprintFunc()
			logrus.Infof("%s Worker-%d 任务成功完成: %s", green("🎉"), workerID, task.ID)
		}
	}
}

// Enqueue 将任务加入队列尾部（FIFO）
func (q *TaskQueue) Enqueue(task models.Task) {
	q.mu.Lock()
	defer q.mu.Unlock()

	// 步骤1：添加到链表尾部
	q.queue.PushBack(task)
	
	green := color.New(color.FgGreen).SprintFunc()
	logrus.Debugf("%s 任务入队: %s (队列长度: %d)", green("📥"), task.ID, q.queue.Len())
}

// Dequeue 从队列头部取出任务
// 返回：
// - task: 取出的任务
// - ok: 是否成功（false表示队列为空）
func (q *TaskQueue) Dequeue() (models.Task, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()

	// 步骤1：检查队列是否为空
	if q.queue.Len() == 0 {
		return models.Task{}, false
	}

	// 步骤2：从链表头部移除并返回
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
// 功能：
// 1. 发送停止信号
// 2. 等待所有worker优雅退出
func (q *TaskQueue) Stop() {
	// 步骤1：关闭停止通道
	close(q.stopCh)
	
	// 步骤2：等待所有worker完成
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