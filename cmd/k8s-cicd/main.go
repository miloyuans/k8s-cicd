package main

import (
	"context"
	"log"
	"sync"
	"sync/atomic"
	"time"

	"k8s-cicd/internal/config"
	"k8s-cicd/internal/http"
	"k8s-cicd/internal/k8s"
	"k8s-cicd/internal/storage"
	"k8s-cicd/internal/telegram"
)

var (
	cfg            *config.Config
	k8sClient      *k8s.Client
	taskQueue      chan *storage.DeployRequest
	activeTasks    sync.Map
	wg             sync.WaitGroup
	globalCounter  int64
	processedTasks sync.Map // map[string]bool to track processed tasks
)

func main() {
	cfg = config.LoadConfig("config.yaml")
	k8sClient = k8s.NewClient(cfg.KubeConfigPath)

	if err := storage.EnsureStorageDir(); err != nil {
		log.Fatalf("Failed to create storage dir: %v", err)
	}

	// Initialize service lists
	if _, err := config.LoadServiceLists(cfg.ServicesDir, cfg.TelegramBots); err != nil {
		log.Printf("Failed to initialize service lists: %v", err)
	}

	// Initialize all daily files and update deployment versions
	storage.InitAllDailyFiles(cfg, k8sClient.Client())
	storage.UpdateAllDeploymentVersions(cfg, k8sClient.Client())

	// Retry pending tasks from gateway
	retryPendingTasks()

	taskQueue = make(chan *storage.DeployRequest, 100)
	for i := 0; i < cfg.MaxConcurrency; i++ {
		go worker()
	}

	go storage.DailyMaintenance(cfg, k8sClient.Client())
	go pollGateway()

	select {} // Keep running
}

func retryPendingTasks() {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	tasks, err := http.FetchTasks(ctx, cfg.GatewayURL)
	if err != nil {
		log.Printf("Failed to fetch pending tasks for retry: %v", err)
		return
	}

	for _, task := range tasks {
		taskKey := task.Service + "-" + task.Env + "-" + task.Version + "-" + task.Timestamp.Format(time.RFC3339)
		if _, processed := processedTasks.Load(taskKey); processed {
			log.Printf("Skipping already processed task: %s", taskKey)
			continue
		}
		if _, loaded := activeTasks.Load(task.Service); loaded {
			log.Printf("Service %s is already deploying, skipping retry", task.Service)
			continue
		}
		log.Printf("Retrying pending task: %s", taskKey)
		taskQueue <- &task
	}
}

func pollGateway() {
	ticker := time.NewTicker(time.Duration(cfg.PollInterval) * time.Second)
	defer ticker.Stop()

	for range ticker.C {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		tasks, err := http.FetchTasks(ctx, cfg.GatewayURL)
		if err != nil {
			log.Printf("Failed to fetch tasks: %v", err)
			cancel()
			continue
		}
		cancel()

		for _, task := range tasks {
			taskKey := task.Service + "-" + task.Env + "-" + task.Version + "-" + task.Timestamp.Format(time.RFC3339)
			if _, processed := processedTasks.Load(taskKey); processed {
				log.Printf("Skipping already processed task: %s", taskKey)
				continue
			}
			if _, loaded := activeTasks.Load(task.Service); loaded {
				log.Printf("Service %s is already deploying", task.Service)
				continue
			}
			task.Timestamp = time.Now()
			atomic.AddInt64(&globalCounter, 1)
			taskQueue <- &task
		}
	}
}

func worker() {
	for task := range taskQueue {
		wg.Add(1)
		go func(t *storage.DeployRequest) {
			defer wg.Done()
			taskKey := t.Service + "-" + t.Env + "-" + t.Version + "-" + t.Timestamp.Format(time.RFC3339)
			if _, processed := processedTasks.LoadOrStore(taskKey, true); processed {
				log.Printf("Task already processed: %s", taskKey)
				return
			}
			activeTasks.Store(t.Service, nil)

			result := &storage.DeployResult{
				Request: *t,
				Envs:    make(map[string]string),
			}

			newImage, err := k8sClient.GetNewImage(t.Service, t.Version, cfg.Namespace)
			if err != nil {
				result.Success = false
				result.ErrorMsg = err.Error()
				storage.PersistDeployment(cfg, t.Service, t.Env, "", "failed")
				go telegram.SendTelegramNotification(cfg, result)
				activeTasks.Delete(t.Service)
				return
			}

			storage.PersistDeployment(cfg, t.Service, t.Env, newImage, "pending")

			ctx, cancel := context.WithTimeout(context.Background(), time.Duration(cfg.TimeoutSeconds)*time.Second)
			defer cancel()

			result.Success, result.ErrorMsg, result.OldImage = k8sClient.UpdateDeployment(ctx, t.Service, newImage, cfg.Namespace)

			if result.Success {
				result.Success = k8sClient.WaitForRollout(ctx, t.Service, cfg.Namespace)
				if !result.Success {
					result.ErrorMsg = "Rollout failed or timed out"
					k8sClient.RestoreDeployment(ctx, t.Service, result.OldImage, cfg.Namespace)
				}
			}

			if !result.Success {
				result.Events, result.Logs, result.Envs = k8sClient.GetDeploymentDiagnostics(ctx, t.Service, cfg.Namespace)
				k8sClient.RestoreDeployment(ctx, t.Service, result.OldImage, cfg.Namespace)
			}

			status := "success"
			if !result.Success {
				status = "failed"
			}
			storage.PersistDeployment(cfg, t.Service, t.Env, newImage, status)

			go telegram.SendTelegramNotification(cfg, result)

			activeTasks.Delete(t.Service)
		}(task)
	}
}