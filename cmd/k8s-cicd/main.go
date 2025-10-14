package main

import (
	"bytes"
	"context"
	"encoding/json"
	"log"
	"net/http"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"k8s-cicd/internal/config"
	k8shttp "k8s-cicd/internal/http"
	"k8s-cicd/internal/k8s"
	"k8s-cicd/internal/storage"
	"k8s-cicd/internal/telegram"
)

var (
	cfg            *config.Config
	k8sClient      *k8s.Client
	taskQueue      chan *storage.DeployRequest
	activeTasks    sync.Map
	globalCounter  int64
	processedTasks sync.Map // map[string]bool to track processed tasks
)

func main() {
	cfg = config.LoadConfig("config.yaml")
	cfg.StorageDir = "cicd_storage" // Set k8s-cicd-specific storage directory
	k8sClient = k8s.NewClient(cfg.KubeConfigPath)

	if err := storage.EnsureStorageDir(cfg.StorageDir); err != nil {
		log.Fatalf("Failed to create storage dir: %v", err)
	}

	// Initialize service lists (though not used in cicd, keeping for consistency)
	if _, err := config.LoadServiceLists(cfg.ServicesDir, cfg.TelegramBots); err != nil {
		log.Printf("Failed to initialize service lists: %v", err)
	}

	// Initialize all daily files and update deployment versions
	storage.InitAllDailyFiles(cfg, k8sClient.Client())
	storage.UpdateAllDeploymentVersions(cfg, k8sClient.Client())
	reportToGateway(cfg)

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

func reportToGateway(cfg *config.Config) {
	fileName := storage.GetDailyFileName(time.Now(), "deploy", cfg.StorageDir)
	data, err := os.ReadFile(fileName)
	if err != nil {
		log.Printf("Failed to read deploy file for reporting: %v", err)
		return
	}
	var infos []storage.DeploymentInfo
	if err := json.Unmarshal(data, &infos); err != nil {
		log.Printf("Failed to unmarshal deploy file for reporting: %v", err)
		return
	}

	reportData, err := json.Marshal(infos)
	if err != nil {
		log.Printf("Failed to marshal report data: %v", err)
		return
	}

	req, err := http.NewRequest("POST", cfg.GatewayURL+"/report", bytes.NewBuffer(reportData))
	if err != nil {
		log.Printf("Failed to create report request: %v", err)
		return
	}
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		log.Printf("Failed to send report to gateway: %v", err)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		log.Printf("Gateway report failed with status: %d", resp.StatusCode)
	} else {
		log.Printf("Successfully reported to gateway")
	}
}

func retryPendingTasks() {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	for env := range cfg.Environments {
		log.Printf("Retrying pending tasks for env %s", env)
		tasks, err := k8shttp.FetchTasks(ctx, cfg.GatewayURL, env)
		if err != nil {
			log.Printf("Failed to fetch pending tasks for env %s: %v", env, err)
			continue
		}
		log.Printf("Fetched %d pending tasks for env %s", len(tasks), env)

		for _, task := range tasks {
			if task.Service == "" || task.Env == "" || task.Version == "" {
				log.Printf("Invalid task parameters: service=%s, env=%s, version=%s", task.Service, task.Env, task.Version)
				continue
			}
			taskKey := task.Service + "-" + task.Env + "-" + task.Version + "-" + task.Timestamp.Format(time.RFC3339)
			if _, processed := processedTasks.Load(taskKey); processed {
				log.Printf("Skipping already processed task: %s", taskKey)
				continue
			}
			if _, loaded := activeTasks.Load(task.Service); loaded {
				log.Printf("Service %s is already deploying, skipping retry", task.Service)
				continue
			}
			log.Printf("Retrying pending task: %s (service=%s, env=%s, version=%s)", taskKey, task.Service, task.Env, task.Version)
			taskQueue <- &task
		}
	}
}

func pollGateway() {
	ticker := time.NewTicker(time.Duration(cfg.PollInterval) * time.Second)
	defer ticker.Stop()

	for range ticker.C {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		for env := range cfg.Environments {
			log.Printf("Polling gateway for tasks in env %s", env)
			tasks, err := k8shttp.FetchTasks(ctx, cfg.GatewayURL, env)
			if err != nil {
				log.Printf("Failed to fetch tasks for env %s: %v", env, err)
				continue
			}
			log.Printf("Fetched %d tasks for env %s", len(tasks), env)

			for _, task := range tasks {
				if task.Service == "" || task.Env == "" || task.Version == "" {
					log.Printf("Invalid task parameters: service=%s, env=%s, version=%s", task.Service, task.Env, task.Version)
					continue
				}
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
				log.Printf("Queuing new task: %s (service=%s, env=%s, version=%s)", taskKey, task.Service, task.Env, task.Version)
				taskQueue <- &task

				// Log interaction on k8s-cicd side
				storage.PersistInteraction(cfg, map[string]interface{}{
					"endpoint":  "pollGateway",
					"task_key":  taskKey,
					"service":   task.Service,
					"env":       task.Env,
					"version":   task.Version,
					"timestamp": time.Now().Format(time.RFC3339),
				})
			}
			if len(tasks) == 0 {
				log.Printf("No tasks fetched for env %s", env)
			}
		}
	}
}

func worker() {
	for task := range taskQueue {
		taskKey := task.Service + "-" + task.Env + "-" + task.Version + "-" + task.Timestamp.Format(time.RFC3339)
		log.Printf("Worker starting to process task: %s (service=%s, env=%s, version=%s)", taskKey, task.Service, task.Env, task.Version)
		if _, processed := processedTasks.LoadOrStore(taskKey, true); processed {
			log.Printf("Task already processed: %s", taskKey)
			return
		}
		activeTasks.Store(task.Service, nil)

		result := &storage.DeployResult{
			Request: *task,
			Envs:    make(map[string]string),
		}

		namespace, ok := cfg.Environments[task.Env]
		if !ok {
			log.Printf("No namespace found for env %s, skipping task %s", task.Env, taskKey)
			activeTasks.Delete(task.Service)
			return
		}

		newImage, err := k8sClient.GetNewImage(task.Service, task.Version, namespace)
		if err != nil {
			result.Success = false
			result.ErrorMsg = err.Error()
			log.Printf("Failed to get new image for task %s: %v", taskKey, err)
			storage.PersistDeployment(cfg, task.Service, task.Env, "", "failed", task.UserName)
			go telegram.SendTelegramNotification(cfg, result)
			activeTasks.Delete(task.Service)
			reportToGateway(cfg)
			return
		}
		log.Printf("Got new image %s for task %s", newImage, taskKey)

		storage.PersistDeployment(cfg, task.Service, task.Env, newImage, "pending", task.UserName)

		ctx, cancel := context.WithTimeout(context.Background(), time.Duration(cfg.TimeoutSeconds)*time.Second)
		defer cancel()

		log.Printf("Updating deployment for task %s", taskKey)
		result.Success, result.ErrorMsg, result.OldImage = k8sClient.UpdateDeployment(ctx, task.Service, newImage, namespace)

		if result.Success {
			log.Printf("Waiting for rollout for task %s", taskKey)
			result.Success = k8sClient.WaitForRollout(ctx, task.Service, namespace)
			if !result.Success {
				result.ErrorMsg = "Rollout failed or timed out"
				log.Printf("Rollout failed for task %s, restoring", taskKey)
				k8sClient.RestoreDeployment(ctx, task.Service, result.OldImage, namespace)
			} else {
				log.Printf("Rollout successful for task %s", taskKey)
			}
		} else {
			log.Printf("Update deployment failed for task %s: %s", taskKey, result.ErrorMsg)
		}

		if !result.Success {
			log.Printf("Getting diagnostics for failed task %s", taskKey)
			result.Events, result.Logs, result.Envs = k8sClient.GetDeploymentDiagnostics(ctx, task.Service, namespace)
			log.Printf("Restoring deployment for failed task %s", taskKey)
			k8sClient.RestoreDeployment(ctx, task.Service, result.OldImage, namespace)
		}

		status := "success"
		if !result.Success {
			status = "failed"
		}
		storage.PersistDeployment(cfg, task.Service, task.Env, newImage, status, task.UserName)

		go telegram.SendTelegramNotification(cfg, result)

		activeTasks.Delete(task.Service)
		reportToGateway(cfg)
		log.Printf("Finished processing task %s with status %s", taskKey, status)
	}
}