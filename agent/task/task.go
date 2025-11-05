// 文件: task/task.go
// 修改: 
// 1. checkNewPodStatusAfterUpdate 升级为多 Pod + 事件检查
//    - 过滤新镜像 Pod
//    - 任意新 Pod Running → 成功
//    - 60 秒后仍有 Pending 或事件异常 → 回滚
//    - 收集 Pod 事件异常信息
// 2. Telegram 通知附加异常事件（可选）
// 3. 保留所有原有功能

package task

import (
	"container/list"
	"context"  // 必须添加
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"k8s-cicd/agent/api"
	"k8s-cicd/agent/client"
	"k8s-cicd/agent/config"
	"k8s-cicd/agent/kubernetes"
	"k8s-cicd/agent/models"
	"k8s-cicd/agent/telegram"

	"github.com/fatih/color"
	"github.com/sirupsen/logrus"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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

// TaskQueue 任务队列
type TaskQueue struct {
	queue        *list.List
	mu           sync.Mutex
	workers      int
	stopCh       chan struct{}
	wg           sync.WaitGroup
	locks        map[string]*sync.Mutex
	lockMu       sync.RWMutex
	maxQueueSize int
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

// worker 执行任务（防止worker 进入sleep）
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

			// 生成 TaskID
			if task.DeployRequest.TaskID == "" {
				task.DeployRequest.TaskID = uuid.New().String()
				task.ID = fmt.Sprintf("%s-%s-%s", task.DeployRequest.Service, task.DeployRequest.Version, task.DeployRequest.Environments[0])
			}

			env := task.DeployRequest.Environments[0]

			// 防重复
			_ = mongo.UpdateConfirmationStatus(task.DeployRequest.TaskID, "已执行")
			_ = mongo.UpdateTaskStatus(task.DeployRequest.Service, task.DeployRequest.Version, env, task.DeployRequest.User, "running")

			// 串行锁
			lock := q.getLock(task.DeployRequest.Service, task.DeployRequest.Namespace)
			lock.Lock()
			defer lock.Unlock()

			// 执行
			err := q.executeTask(cfg, mongo, k8s, apiClient, botMgr, task)
			if err != nil {
				task.Retries++
				if task.Retries < cfg.Task.MaxRetries {
					time.Sleep(time.Duration(cfg.Task.RetryDelay) * time.Second)
					q.Enqueue(task)
				} else {
					q.handlePermanentFailure(k8s, mongo, apiClient, botMgr, task)
				}
			}

			// 继续下一个任务
			continue
		}
	}
}

// executeTask 执行核心任务逻辑（优化等待逻辑）
func (q *TaskQueue) executeTask(cfg *config.Config, mongo *client.MongoClient, k8s *kubernetes.K8sClient, apiClient *api.APIClient, botMgr *telegram.BotManager, task *Task) error {
	env := task.DeployRequest.Environments[0]
	oldTag, err := k8s.SnapshotAndStoreImage(task.DeployRequest.Service, task.DeployRequest.Namespace, task.DeployRequest.TaskID, mongo)
	if err != nil {
		q.handleException(mongo, apiClient, botMgr, task, oldTag, env)
		return err
	}

	// 1. 更新镜像
	if err := k8s.UpdateWorkloadImage(task.DeployRequest.Service, task.DeployRequest.Namespace, task.DeployRequest.Version); err != nil {
		q.handleFailure(k8s, mongo, apiClient, botMgr, task, oldTag, env, "")
		return err
	}

	// 2. 构造预期新镜像完整路径
	oldImage, _ := k8s.GetCurrentImage(task.DeployRequest.Service, task.DeployRequest.Namespace)
	newImage := kubernetes.BuildNewImage(oldImage, task.DeployRequest.Version)

	// 3. 快速判断：多 Pod + 事件检查
	quickResult, eventMsg := q.checkNewPodStatusWithEvents(k8s, task.DeployRequest.Service, task.DeployRequest.Namespace, newImage, oldTag, env)
	if quickResult != "" {
		if quickResult == "success" {
			q.handleSuccess(mongo, apiClient, botMgr, task, oldTag, env, "")
			return nil
		} else if quickResult == "rollback" {
			q.handleFailure(k8s, mongo, apiClient, botMgr, task, oldTag, env, eventMsg)
			return fmt.Errorf("发布失败: %s", eventMsg)
		}
	}

	// 4. 快速判断未通过 → 按原逻辑等待 rollout 完成
	if err := k8s.WaitForRolloutComplete(task.DeployRequest.Service, task.DeployRequest.Namespace, cfg.Deploy.WaitTimeout); err != nil {
		q.handleFailure(k8s, mongo, apiClient, botMgr, task, oldTag, env, "")
		return err
	}

	q.handleSuccess(mongo, apiClient, botMgr, task, oldTag, env, "")
	return nil
}

// checkNewPodStatusWithEvents 多 Pod + 事件检查
func (q *TaskQueue) checkNewPodStatusWithEvents(k8s *kubernetes.K8sClient, service, namespace, expectedImage, oldTag, env string) (string, string) {
	ctx := context.Background()
	labelSelector := fmt.Sprintf("app=%s", service)

	// 等待 5 秒让 Pod 开始调度
	time.Sleep(5 * time.Second)

	startTime := time.Now()
	pendingDeadline := startTime.Add(60 * time.Second)
	eventMsgs := []string{}

	for time.Now().Before(pendingDeadline) {
		pods, err := k8s.Clientset.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{LabelSelector: labelSelector})
		if err != nil {
			logrus.Errorf("查询 Pod 失败: %v", err)
			time.Sleep(3 * time.Second)
			continue
		}

		// 过滤新镜像 Pod
		newPods := []v1.Pod{}
		for _, pod := range pods.Items {
			for _, container := range pod.Spec.Containers {
				if container.Image == expectedImage {
					newPods = append(newPods, pod)
					break
				}
			}
		}

		if len(newPods) == 0 {
			time.Sleep(3 * time.Second)
			continue
		}

		// 检查 Pod 状态
		hasRunning := false
		hasPending := false
		for _, pod := range newPods {
			if pod.Status.Phase == v1.PodRunning {
				hasRunning = true
			}
			if pod.Status.Phase == v1.PodPending {
				hasPending = true
			}
		}

		if hasRunning {
			logrus.Info("新版本 Pod 已出现 Running 状态，发布成功")
			return "success", ""
		}

		// 检查事件
		events, err := k8s.Clientset.CoreV1().Events(namespace).List(ctx, metav1.ListOptions{
			FieldSelector: fmt.Sprintf("involvedObject.kind=Pod,involvedObject.name=%s", strings.Join(getPodNames(newPods), ",")),
		})
		if err == nil {
			for _, event := range events.Items {
				if event.Type == "Warning" || strings.Contains(event.Message, "Err") || strings.Contains(event.Message, "Failed") {
					msg := fmt.Sprintf("%s: %s", event.Reason, event.Message)
					if !contains(eventMsgs, msg) {
						eventMsgs = append(eventMsgs, msg)
					}
				}
			}
		}

		if hasPending {
			logrus.Info("新版本 Pod 处于 Pending 状态，等待中...")
		}

		time.Sleep(3 * time.Second)
	}

	// 超时或异常事件 → 回滚
	msg := "60 秒后新版本 Pod 仍未进入 Running 状态"
	if len(eventMsgs) > 0 {
		msg = strings.Join(eventMsgs, "\n")
	}
	logrus.Warnf("触发快速回滚: %s", msg)
	return "rollback", msg
}

// getPodNames 提取 Pod 名称
func getPodNames(pods []v1.Pod) []string {
	names := []string{}
	for _, pod := range pods {
		names = append(names, pod.Name)
	}
	return names
}

// contains 检查字符串是否存在
func contains(slice []string, item string) bool {
	for _, s := range slice {
		if s == item {
			return true
		}
	}
	return false
}

// handleSuccess 成功处理
func (q *TaskQueue) handleSuccess(mongo *client.MongoClient, apiClient *api.APIClient, botMgr *telegram.BotManager, task *Task, oldTag string, env string, extra string) {
	_ = mongo.UpdateTaskStatus(task.DeployRequest.Service, task.DeployRequest.Version, env, task.DeployRequest.User, "执行成功")
	_ = apiClient.UpdateStatus(models.StatusRequest{
		Service:     task.DeployRequest.Service,
		Version:     task.DeployRequest.Version,
		Environment: env,
		User:        task.DeployRequest.User,
		Status:      "执行成功",
	})
	_ = botMgr.SendNotification(task.DeployRequest.Service, env, task.DeployRequest.User, getImageOrUnknown(oldTag), task.DeployRequest.Version, true, extra)
}

// handleFailure 失败处理
func (q *TaskQueue) handleFailure(k8s *kubernetes.K8sClient, mongo *client.MongoClient, apiClient *api.APIClient, botMgr *telegram.BotManager, task *Task, oldTag string, env string, extra string) {
	if k8s != nil && oldTag != "" {
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

	mongoErr := mongo.UpdateTaskStatus(task.DeployRequest.Service, task.DeployRequest.Version, env, task.DeployRequest.User, "执行失败")
	if mongoErr != nil {
		logrus.Errorf(color.RedString("更新MongoDB状态失败 [%s]: %v"), env, mongoErr)
	}

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

	botMgr.SendNotification(task.DeployRequest.Service, env, task.DeployRequest.User, getImageOrUnknown(oldTag), task.DeployRequest.Version, false, extra)
}

// handleException 异常处理
func (q *TaskQueue) handleException(mongo *client.MongoClient, apiClient *api.APIClient, botMgr *telegram.BotManager, task *Task, oldTag string, env string) {
	_ = mongo.UpdateTaskStatus(task.DeployRequest.Service, task.DeployRequest.Version, env, task.DeployRequest.User, "异常")
	_ = apiClient.UpdateStatus(models.StatusRequest{
		Service:     task.DeployRequest.Service,
		Version:     task.DeployRequest.Version,
		Environment: env,
		User:        task.DeployRequest.User,
		Status:      "异常",
	})
	_ = botMgr.SendNotification(task.DeployRequest.Service, env, task.DeployRequest.User, getImageOrUnknown(oldTag), task.DeployRequest.Version, false, "")
}

// handlePermanentFailure 永久失败处理
func (q *TaskQueue) handlePermanentFailure(k8s *kubernetes.K8sClient, mongo *client.MongoClient, apiClient *api.APIClient, botMgr *telegram.BotManager, task *Task) {
	q.handleFailure(k8s, mongo, apiClient, botMgr, task, "unknown", task.DeployRequest.Environments[0], "")
}

// getImageOrUnknown 获取镜像或默认
func getImageOrUnknown(tag string) string {
	if tag != "" {
		return tag
	}
	return "无"
}

// Enqueue 入队
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
	}).Infof(color.GreenString("任务已入队: %s (队列长度: %d/%d)"), task.DeployRequest.TaskID, q.queue.Len(), q.maxQueueSize)
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

// Stop 停止队列
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

	q.lockMu.Lock()
	for key, lock := range q.locks {
		lock.Unlock()
		logrus.WithFields(logrus.Fields{"key": key}).Debug("Force-released lock during shutdown")
	}
	q.locks = make(map[string]*sync.Mutex)
	q.lockMu.Unlock()
	logrus.Info(color.GreenString("任务队列停止 (所有锁已释放)"))
}