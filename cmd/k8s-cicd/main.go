package main

import (
	"context"
	"encoding/json"
	"log"
	"sync"
	"sync/atomic"
	"time"

	"github.com/yourusername/k8s-cicd/internal/config"
	"github.com/yourusername/k8s-cicd/internal/http"
	"github.com/yourusername/k8s-cicd/internal/k8s"
	"github.com/yourusername/k8s-cicd/internal/storage"
)

type DeployRequest struct {
	Service   string    `json:"service"`
	Env       string    `json:"env"`
	Version   string    `json:"version"`
	Timestamp time.Time `json:"timestamp"`
}

type DeployResult struct {
	Request   DeployRequest
	Success   bool
	ErrorMsg  string
	OldImage  string
	Events    string
	Logs      string
	Envs      map[string]string
}

var (
	cfg         *config.Config
	k8sClient   *k8s.Client
	taskQueue   chan *DeployRequest
	activeTasks sync.Map
	wg          sync.WaitGroup
	globalCounter int64
)

func main() {
	cfg = config.LoadConfig("config.yaml")
	k8sClient = k8s.NewClient(cfg)

	if err := storage.EnsureStorageDir(); err != nil {
		log.Fatalf("Failed to create storage dir: %v", err)
	}

	taskQueue = make(chan *DeployRequest, 100)
	for i := 0; i < cfg.MaxConcurrency; i++ {
		go worker()
	}

	go storage.DailyMaintenance(cfg, k8sClient)
	go pollGateway()

	select {} // Keep running
}

func pollGateway() {
	ticker := time.NewTicker(time.Duration(cfg.PollInterval) * time.Second)
	defer ticker.Stop()

	for range ticker.C {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		tasks, err := httpclient.FetchTasks(ctx, cfg.GatewayURL)
		if err != nil {
			log.Printf("Failed to fetch tasks: %v", err)
			cancel()
			continue
		}
		cancel()

		for _, task := range tasks {
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
		go func(t *DeployRequest) {
			defer wg.Done()
			activeTasks.Store(t.Service, nil)

			result := &DeployResult{
				Request: *t,
				Envs:    make(map[string]string),
			}

			newImage, err := k8sClient.GetNewImage(t.Service, t.Version)
			if err != nil {
				result.Success = false
				result.ErrorMsg = err.Error()
				storage.PersistDeployment(cfg, t.Service, t.Env, "", "failed")
				go k8sClient.SendTelegramNotification(cfg, result)
				activeTasks.Delete(t.Service)
				return
			}

			storage.PersistDeployment(cfg, t.Service, t.Env, newImage, "pending")

			ctx, cancel := context.WithTimeout(context.Background(), time.Duration(cfg.TimeoutSeconds)*time.Second)
			defer cancel()

			result.Success, result.ErrorMsg, result.OldImage = k8sClient.UpdateDeployment(ctx, t.Service, newImage)

			if result.Success {
				result.Success = k8sClient.WaitForRollout(ctx, t.Service)
				if !result.Success {
					result.ErrorMsg = "Rollout failed or timed out"
					k8sClient.RestoreDeployment(ctx, t.Service, result.OldImage)
				}
			}

			if !result.Success {
				result.Events, result.Logs, result.Envs = k8sClient.GetDeploymentDiagnostics(ctx, t.Service)
				k8sClient.RestoreDeployment(ctx, t.Service, result.OldImage)
			}

			status := "success"
			if !result.Success {
				status = "failed"
			}
			storage.PersistDeployment(cfg, t.Service, t.Env, newImage, status)

			go k8sClient.SendTelegramNotification(cfg, result)

			activeTasks.Delete(t.Service)
		}(task)
	}
}