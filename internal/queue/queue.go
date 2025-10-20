// internal/queue/queue.go
package queue

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"k8s-cicd/internal/config"
	"k8s-cicd/internal/types"
)

type Task struct {
	DeployRequest types.DeployRequest
}

type Queue struct {
	tasks     chan Task
	taskMap   sync.Map // map[string]Task for tracking uniqueness
	perKey    sync.Map // map[string]chan Task for per-service-env channels
	cfg       *config.Config
	storageMu sync.Mutex
}

func NewQueue(cfg *config.Config, capacity int) *Queue {
	q := &Queue{
		tasks:   make(chan Task, capacity),
		cfg:     cfg,
	}
	go q.loadPendingTasks()
	go q.cleanupExpiredTasks()
	return q
}

func (q *Queue) Enqueue(task Task) {
	taskKey := ComputeTaskKey(task.DeployRequest)
	if _, exists := q.taskMap.Load(taskKey); exists {
		log.Printf("Task %s already exists, skipping enqueue", taskKey)
		return
	}
	q.taskMap.Store(taskKey, task)
	key := task.DeployRequest.Service + "-" + task.DeployRequest.Env
	ch, loaded := q.perKey.LoadOrStore(key, make(chan Task, 100))
	taskCh := ch.(chan Task)
	taskCh <- task
	if !loaded {
		go q.processKey(key, taskCh)
	}
	q.persistTask(task)
	log.Printf("Enqueued task %s for key %s (service=%s, env=%s, version=%s)", taskKey, key, task.DeployRequest.Service, task.DeployRequest.Env, task.DeployRequest.Version)
}

func (q *Queue) processKey(key string, ch chan Task) {
	var pending []Task
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case task := <-ch:
			pending = append(pending, task)
		case <-ticker.C:
			if len(pending) > 0 {
				sort.Slice(pending, func(i, j int) bool {
					return pending[i].DeployRequest.Timestamp.Before(pending[j].DeployRequest.Timestamp)
				})
				for i, t := range pending {
					if t.DeployRequest.Status == "pending" {
						q.tasks <- t
						if i < len(pending)-1 {
							time.Sleep(1 * time.Minute) // 1-minute interval between tasks for same service-env
						}
					}
				}
				pending = nil
			}
		}
	}
}

func (q *Queue) Dequeue() <-chan Task {
	return q.tasks
}

func (q *Queue) GetPendingTasks(env string) []types.DeployRequest {
	var tasks []types.DeployRequest
	q.taskMap.Range(func(key, value interface{}) bool {
		task := value.(Task)
		if task.DeployRequest.Env == env && task.DeployRequest.Status == "pending" { // 优化：大小写敏感
			tasks = append(tasks, task.DeployRequest)
		}
		return true
	})
	log.Printf("Returning %d pending tasks for env %s", len(tasks), env)
	return tasks
}

func (q *Queue) CompleteTask(taskKey string) {
	if task, exists := q.taskMap.Load(taskKey); exists {
		t := task.(Task)
		t.DeployRequest.Status = "completed"
		q.taskMap.Store(taskKey, t)
		q.persistTask(t)
		log.Printf("Marked task %s as completed", taskKey)
		q.taskMap.Delete(taskKey)
	}
}

func (q *Queue) ConfirmTask(task types.DeployRequest) {
	taskKey := ComputeTaskKey(task)
	if t, exists := q.taskMap.Load(taskKey); exists {
		existingTask := t.(Task)
		existingTask.DeployRequest.Status = "pending"
		q.taskMap.Store(taskKey, existingTask)
		q.persistTask(existingTask)
		key := existingTask.DeployRequest.Service + "-" + existingTask.DeployRequest.Env
		if ch, loaded := q.perKey.Load(key); loaded {
			taskCh := ch.(chan Task)
			taskCh <- existingTask
		}
		log.Printf("Confirmed task %s, status set to pending", taskKey)
	}
}

func (q *Queue) Exists(taskKey string) bool {
	_, exists := q.taskMap.Load(taskKey)
	return exists
}

func (q *Queue) persistTask(task Task) {
	q.storageMu.Lock()
	defer q.storageMu.Unlock()

	fileName := filepath.Join(q.cfg.StorageDir, fmt.Sprintf("tasks_%s.json", time.Now().Format("2006-01-02")))
	if _, err := os.Stat(fileName); os.IsNotExist(err) {
		if err := os.WriteFile(fileName, []byte("[]"), 0644); err != nil {
			log.Printf("Failed to initialize tasks file %s: %v", fileName, err)
			return
		}
	}

	data, err := os.ReadFile(fileName)
	if err != nil {
		log.Printf("Failed to read tasks file %s: %v", fileName, err)
		return
	}
	var tasks []types.DeployRequest
	if err := json.Unmarshal(data, &tasks); err != nil {
		log.Printf("Failed to unmarshal tasks file %s: %v", fileName, err)
		return
	}

	taskKey := ComputeTaskKey(task.DeployRequest)
	found := false
	for i := range tasks {
		if ComputeTaskKey(tasks[i]) == taskKey {
			tasks[i] = task.DeployRequest
			found = true
			break
		}
	}
	if !found {
		tasks = append(tasks, task.DeployRequest)
	}

	newData, err := json.MarshalIndent(tasks, "", "  ")
	if err != nil {
		log.Printf("Failed to marshal tasks file %s: %v", fileName, err)
		return
	}
	if err := os.WriteFile(fileName, newData, 0644); err != nil {
		log.Printf("Failed to write tasks file %s: %v", fileName, err)
	}
}

func (q *Queue) loadPendingTasks() {
	fileName := filepath.Join(q.cfg.StorageDir, fmt.Sprintf("tasks_%s.json", time.Now().Format("2006-01-02")))
	if _, err := os.Stat(fileName); os.IsNotExist(err) {
		return
	}

	data, err := os.ReadFile(fileName)
	if err != nil {
		log.Printf("Failed to read tasks file %s: %v", fileName, err)
		return
	}
	var tasks []types.DeployRequest
	if err := json.Unmarshal(data, &tasks); err != nil {
		log.Printf("Failed to unmarshal tasks file %s: %v", fileName, err)
		return
	}

	for _, t := range tasks {
		if t.Status == "pending" || t.Status == "pending_confirmation" {
			taskKey := ComputeTaskKey(t)
			if _, exists := q.taskMap.Load(taskKey); !exists {
				task := Task{DeployRequest: t}
				if t.Status == "pending" {
					q.Enqueue(task)
				} else {
					q.taskMap.Store(taskKey, task)
					q.persistTask(task)
				}
			}
		}
	}
}

func (q *Queue) cleanupExpiredTasks() {
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()

	for range ticker.C {
		now := time.Now()
		q.taskMap.Range(func(key, value interface{}) bool {
			task := value.(Task)
			if (task.DeployRequest.Status == "pending" || task.DeployRequest.Status == "pending_confirmation") && now.Sub(task.DeployRequest.Timestamp) > 30*time.Minute {
				log.Printf("Cleaning up expired task %s (status: %s, age: %v)", key, task.DeployRequest.Status, now.Sub(task.DeployRequest.Timestamp))
				task.DeployRequest.Status = "expired"
				q.persistTask(task)
				q.taskMap.Delete(key)
			}
			return true
		})
	}
}

func ComputeTaskKey(task types.DeployRequest) string {
	return task.Service + "-" + task.Env + "-" + task.Version + "-" + task.Timestamp.Format(time.RFC3339)
}