package queue

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"k8s-cicd/internal/config"
	"k8s-cicd/internal/dialog"
)

type Task struct {
	dialog.DeployRequest
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
	taskKey := computeTaskKey(task.DeployRequest)
	if _, exists := q.taskMap.Load(taskKey); exists {
		fmt.Printf("Task %s already exists, skipping enqueue\n", taskKey)
		return
	}
	task.Status = "pending"
	q.taskMap.Store(taskKey, task)
	q.tasks <- task
	q.persistTask(task)
	fmt.Printf("Enqueued task %s (service=%s, env=%s, version=%s)\n", taskKey, task.Service, task.Env, task.Version)
}

func (q *Queue) Dequeue() <-chan Task {
	return q.tasks
}

func (q *Queue) GetPendingTasks(env string) []dialog.DeployRequest {
	var tasks []dialog.DeployRequest
	q.taskMap.Range(func(key, value interface{}) bool {
		task := value.(Task)
		if task.Env == env && task.Status == "pending" {
			tasks = append(tasks, task.DeployRequest)
		}
		return true
	})
	return tasks
}

func (q *Queue) CompleteTask(taskKey string) {
	if task, exists := q.taskMap.Load(taskKey); exists {
		t := task.(Task)
		t.Status = "completed"
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
	var tasks []dialog.DeployRequest
	if err := json.Unmarshal(data, &tasks); err != nil {
		fmt.Printf("Failed to unmarshal tasks file %s: %v\n", fileName, err)
		return
	}

	taskKey := computeTaskKey(task.DeployRequest)
	found := false
	for i := range tasks {
		if computeTaskKey(tasks[i]) == taskKey {
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
	var tasks []dialog.DeployRequest
	if err := json.Unmarshal(data, &tasks); err != nil {
		fmt.Printf("Failed to unmarshal tasks file %s: %v\n", fileName, err)
		return
	}

	for _, t := range tasks {
		if t.Status == "pending" {
			taskKey := computeTaskKey(t)
			if _, exists := q.taskMap.Load(taskKey); !exists {
				task := Task{DeployRequest: t}
				q.taskMap.Store(taskKey, task)
				q.tasks <- task
				fmt.Printf("Loaded pending task %s (service=%s, env=%s, version=%s)\n", taskKey, t.Service, t.Env, t.Version)
			}
		}
	}
}

func computeTaskKey(task dialog.DeployRequest) string {
	return task.Service + "-" + task.Env + "-" + task.Version + "-" + task.Timestamp.Format(time.RFC3339)
}