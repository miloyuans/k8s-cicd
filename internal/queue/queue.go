// queue.go
package queue

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
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
	taskMap   sync.Map // map[string]Task for tracking
	cfg       *config.Config
	storageMu sync.Mutex
}

func NewQueue(cfg *config.Config, capacity int) *Queue {
	q := &Queue{
		tasks:   make(chan Task, capacity),
		cfg:     cfg,
	}
	go q.loadPendingTasks()
	return q
}

func (q *Queue) Enqueue(task Task) {
	taskKey := ComputeTaskKey(task.DeployRequest)
	if _, exists := q.taskMap.Load(taskKey); exists {
		fmt.Printf("Task %s already exists, skipping enqueue\n", taskKey)
		return
	}
	task.DeployRequest.Status = "pending"
	q.taskMap.Store(taskKey, task)
	q.tasks <- task
	q.persistTask(task)
	fmt.Printf("Enqueued task %s (service=%s, env=%s, version=%s)\n", taskKey, task.DeployRequest.Service, task.DeployRequest.Env, task.DeployRequest.Version)
	log.Printf("Enqueued task %s for env %s", taskKey, task.DeployRequest.Env)
}

func (q *Queue) Dequeue() <-chan Task {
	return q.tasks
}

func (q *Queue) GetPendingTasks(env string) []types.DeployRequest {
	var tasks []types.DeployRequest
	lowerEnv := strings.ToLower(env)
	q.taskMap.Range(func(key, value interface{}) bool {
		task := value.(Task)
		if strings.ToLower(task.DeployRequest.Env) == lowerEnv && task.DeployRequest.Status == "pending" {
			tasks = append(tasks, task.DeployRequest)
		}
		return true
	})
	log.Printf("Returning %d pending tasks for env %s", len(tasks), lowerEnv)
	return tasks
}

func (q *Queue) CompleteTask(taskKey string) {
	if task, exists := q.taskMap.Load(taskKey); exists {
		t := task.(Task)
		t.DeployRequest.Status = "completed"
		q.taskMap.Store(taskKey, t)
		q.persistTask(t)
		fmt.Printf("Marked task %s as completed\n", taskKey)
	}
}

func (q *Queue) persistTask(task Task) {
	q.storageMu.Lock()
	defer q.storageMu.Unlock()

	fileName := filepath.Join(q.cfg.StorageDir, fmt.Sprintf("tasks_%s.json", time.Now().Format("2006-01-02")))
	if _, err := os.Stat(fileName); os.IsNotExist(err) {
		if err := os.WriteFile(fileName, []byte("[]"), 0644); err != nil {
			fmt.Printf("Failed to initialize tasks file %s: %v\n", fileName, err)
			return
		}
	}

	data, err := os.ReadFile(fileName)
	if err != nil {
		fmt.Printf("Failed to read tasks file %s: %v\n", fileName, err)
		return
	}
	var tasks []types.DeployRequest
	if err := json.Unmarshal(data, &tasks); err != nil {
		fmt.Printf("Failed to unmarshal tasks file %s: %v\n", fileName, err)
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
		fmt.Printf("Failed to marshal tasks file %s: %v\n", fileName, err)
		return
	}
	if err := os.WriteFile(fileName, newData, 0644); err != nil {
		fmt.Printf("Failed to write tasks file %s: %v\n", fileName, err)
	}
}

func (q *Queue) loadPendingTasks() {
	fileName := filepath.Join(q.cfg.StorageDir, fmt.Sprintf("tasks_%s.json", time.Now().Format("2006-01-02")))
	if _, err := os.Stat(fileName); os.IsNotExist(err) {
		return
	}

	data, err := os.ReadFile(fileName)
	if err != nil {
		fmt.Printf("Failed to read tasks file %s: %v\n", fileName, err)
		return
	}
	var tasks []types.DeployRequest
	if err := json.Unmarshal(data, &tasks); err != nil {
		fmt.Printf("Failed to unmarshal tasks file %s: %v\n", fileName, err)
		return
	}

	for _, t := range tasks {
		if t.Status == "pending" {
			taskKey := ComputeTaskKey(t)
			if _, exists := q.taskMap.Load(taskKey); !exists {
				task := Task{DeployRequest: t}
				q.taskMap.Store(taskKey, task)
				q.tasks <- task
				fmt.Printf("Loaded pending task %s (service=%s, env=%s, version=%s)\n", taskKey, t.Service, t.Env, t.Version)
			}
		}
	}
}

func ComputeTaskKey(task types.DeployRequest) string {
	return task.Service + "-" + task.Env + "-" + task.Version + "-" + task.Timestamp.Format(time.RFC3339)
}