// cicd_main.go
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"k8s-cicd/internal/config"
	k8shttp "k8s-cicd/internal/http"
	"k8s-cicd/internal/k8s"
	"k8s-cicd/internal/queue"
	"k8s-cicd/internal/storage"
	"k8s-cicd/internal/telegram"
)

var (
	cfg       *config.Config
	k8sClient *k8s.Client
	taskQueue *queue.Queue
)

func main() {
	cfg = config.LoadConfig("config.yaml")
	cfg.StorageDir = "cicd_storage" // Set k8s-cicd-specific storage directory
	k8sClient = k8s.NewClient(cfg.KubeConfigPath)

	if err := storage.EnsureStorageDir(cfg.StorageDir); err != nil {
		log.Fatalf("Failed to create storage dir: %v", err)
	}

	// Check and initialize services directory if not exist
	initServicesDir(cfg)

	// Initialize all daily files and update deployment versions
	storage.InitAllDailyFiles(cfg, k8sClient.Client())
	storage.UpdateAllDeploymentVersions(cfg, k8sClient.Client())
	reportToGateway(cfg)

	// Initialize task queue
	taskQueue = queue.NewQueue(cfg, 100)

	// Immediate poll to reduce initial delay
	log.Println("Performing initial task poll")
	retryPendingTasks()

	for i := 0; i < cfg.MaxConcurrency; i++ {
		go worker()
	}

	go storage.DailyMaintenance(cfg, k8sClient.Client())
	go pollGateway()

	// Immediate update services and report, then periodic
	updateAndReportServices()
	go periodicUpdateServices()

	select {} // Keep running
}

func initServicesDir(cfg *config.Config) {
	if _, err := os.Stat(cfg.ServicesDir); os.IsNotExist(err) {
		if err := os.MkdirAll(cfg.ServicesDir, 0755); err != nil {
			log.Fatalf("Failed to create services dir: %v", err)
		}
		for category := range cfg.TelegramBots {
			filePath := filepath.Join(cfg.ServicesDir, fmt.Sprintf("%s.svc.list", category))
			if err := os.WriteFile(filePath, []byte(""), 0644); err != nil {
				log.Printf("Failed to init service file %s: %v", filePath, err)
			}
		}
		log.Println("Initialized services directory and files")
	}
}

func updateAndReportServices() {
	log.Println("Immediate updating services")
	storage.UpdateAllDeploymentVersions(cfg, k8sClient.Client()) // Collects services
	reportServicesToGateway(cfg)
}

func periodicUpdateServices() {
	ticker := time.NewTicker(10 * time.Minute)
	defer ticker.Stop()

	for range ticker.C {
		updateAndReportServices()
	}
}

func reportServicesToGateway(cfg *config.Config) {
	services, err := collectAndClassifyServices(cfg)
	if err != nil {
		log.Printf("Failed to collect services: %v", err)
		return
	}

	data, err := json.Marshal(services)
	if err != nil {
		log.Printf("Failed to marshal services: %v", err)
		return
	}

	// Retry logic for gateway connection
	const maxRetries = 3
	for attempt := 1; attempt <= maxRetries; attempt++ {
		req, err := http.NewRequest("POST", cfg.GatewayURL+"/services", bytes.NewBuffer(data))
		if err != nil {
			log.Printf("Failed to create services request (attempt %d/%d): %v", attempt, maxRetries, err)
			continue
		}
		req.Header.Set("Content-Type", "application/json")

		client := &http.Client{Timeout: 10 * time.Second}
		resp, err := client.Do(req)
		if err != nil {
			log.Printf("Failed to send services to gateway (attempt %d/%d): %v", attempt, maxRetries, err)
			if attempt == maxRetries {
				return
			}
			time.Sleep(time.Duration(attempt) * time.Second) // Exponential backoff
			continue
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			log.Printf("Gateway services failed with status %d (attempt %d/%d)", resp.StatusCode, attempt, maxRetries)
			if attempt == maxRetries {
				return
			}
			continue
		}

		log.Printf("Successfully reported services to gateway")
		return
	}
}

func collectAndClassifyServices(cfg *config.Config) (map[string][]string, error) {
	fileName := storage.GetDailyFileName(time.Now(), "deploy", cfg.StorageDir)
	data, err := os.ReadFile(fileName)
	if err != nil {
		return nil, fmt.Errorf("failed to read deploy file: %v", err)
	}
	var infos []storage.DeploymentInfo
	if err := json.Unmarshal(data, &infos); err != nil {
		return nil, fmt.Errorf("failed to unmarshal deploy file: %v", err)
	}

	classified := make(map[string][]string)
	seenServices := make(map[string]bool) // Global service deduplication across namespaces

	for _, info := range infos {
		if seenServices[info.Service] {
			continue // Skip duplicate service across namespaces
		}
		category := classifyService(info.Service, cfg.ServiceKeywords)
		if category == "" {
			log.Printf("Service %s not matched to any category, discarding", info.Service)
			continue // Discard unmatched services
		}
		classified[category] = append(classified[category], info.Service)
		seenServices[info.Service] = true
	}

	// Ensure files are initialized for each category and save classified services locally
	for category, svcs := range classified {
		filePath := filepath.Join(cfg.ServicesDir, fmt.Sprintf("%s.svc.list", category))
		if _, err := os.Stat(filePath); os.IsNotExist(err) {
			if err := os.WriteFile(filePath, []byte(""), 0644); err != nil {
				log.Printf("Failed to init service file %s: %v", filePath, err)
			}
		}
		// Save classified services to local file
		data := strings.Join(svcs, "\n")
		if err := os.WriteFile(filePath, []byte(data), 0644); err != nil {
			log.Printf("Failed to write local service list %s: %v", filePath, err)
		} else {
			log.Printf("Saved %d services to local %s", len(svcs), filePath)
		}
	}

	return classified, nil
}

func classifyService(service string, keywords map[string][]string) string {
	lowerService := strings.ToLower(service)
	for category, patterns := range keywords {
		for _, pattern := range patterns {
			lowerPattern := strings.ToLower(pattern)
			// Try regex match if pattern looks like a regex
			if strings.HasPrefix(lowerPattern, "^") || strings.HasSuffix(lowerPattern, "$") || strings.Contains(lowerPattern, ".*") {
				re, err := regexp.Compile(lowerPattern)
				if err == nil && re.MatchString(lowerService) {
					return category
				}
			} else if strings.Contains(lowerService, lowerPattern) {
				// Fallback to substring match
				return category
			}
		}
	}
	return ""
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

	// Retry logic for gateway report
	const maxRetries = 3
	for attempt := 1; attempt <= maxRetries; attempt++ {
		req, err := http.NewRequest("POST", cfg.GatewayURL+"/report", bytes.NewBuffer(reportData))
		if err != nil {
			log.Printf("Failed to create report request (attempt %d/%d): %v", attempt, maxRetries, err)
			continue
		}
		req.Header.Set("Content-Type", "application/json")

		client := &http.Client{Timeout: 10 * time.Second}
		resp, err := client.Do(req)
		if err != nil {
			log.Printf("Failed to send report to gateway (attempt %d/%d): %v", attempt, maxRetries, err)
			if attempt == maxRetries {
				return
			}
			time.Sleep(time.Duration(attempt) * time.Second) // Exponential backoff
			continue
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			log.Printf("Gateway report failed with status %d (attempt %d/%d)", resp.StatusCode, attempt, maxRetries)
			if attempt == maxRetries {
				return
			}
			continue
		}

		log.Printf("Successfully reported to gateway")
		return
	}
}

func retryPendingTasks() {
	log.Printf("Polling tasks for environments: %v", cfg.Environments)
	for env := range cfg.Environments {
		lowerEnv := strings.ToLower(env)
		tasks, err := k8shttp.FetchTasks(context.Background(), cfg.GatewayURL, lowerEnv)
		if err != nil {
			log.Printf("Failed to fetch pending tasks for env %s: %v", lowerEnv, err)
			continue
		}
		log.Printf("Fetched %d tasks for env %s", len(tasks), lowerEnv)
		for _, task := range tasks {
			if task.Service == "" || task.Env == "" || task.Version == "" {
				log.Printf("Invalid task parameters: service=%s, env=%s, version=%s", task.Service, task.Env, task.Version)
				continue
			}
			task.Env = strings.ToLower(task.Env) // Normalize task env
			taskKey := queue.ComputeTaskKey(task)
			log.Printf("Queuing new task: %s (service=%s, env=%s, version=%s)", taskKey, task.Service, task.Env, task.Version)
			taskQueue.Enqueue(queue.Task{DeployRequest: task})

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
			log.Printf("No tasks fetched for env %s", lowerEnv)
		}
	}
}

func pollGateway() {
	ticker := time.NewTicker(time.Duration(cfg.PollInterval) * time.Second)
	defer ticker.Stop()

	for range ticker.C {
		retryPendingTasks()
	}
}

func worker() {
	for task := range taskQueue.Dequeue() {
		taskKey := queue.ComputeTaskKey(task.DeployRequest)
		log.Printf("Worker starting to process task: %s (service=%s, env=%s, version=%s)", taskKey, task.DeployRequest.Service, task.DeployRequest.Env, task.DeployRequest.Version)

		result := &storage.DeployResult{
			Request: task.DeployRequest,
			Envs:    make(map[string]string),
		}

		namespace, ok := cfg.Environments[strings.ToLower(task.DeployRequest.Env)]
		if !ok {
			log.Printf("No namespace found for env %s, skipping task %s", task.DeployRequest.Env, taskKey)
			return
		}

		newImage, err := k8sClient.GetNewImage(task.DeployRequest.Service, task.DeployRequest.Version, namespace)
		if err != nil {
			result.Success = false
			result.ErrorMsg = err.Error()
			log.Printf("Failed to get new image for task %s: %v", taskKey, err)
			storage.PersistDeployment(cfg, task.DeployRequest.Service, task.DeployRequest.Env, "", "failed", task.DeployRequest.UserName)
			go telegram.SendTelegramNotification(cfg, result)
			return
		}
		log.Printf("Got new image %s for task %s", newImage, taskKey)

		storage.PersistDeployment(cfg, task.DeployRequest.Service, task.DeployRequest.Env, newImage, "pending", task.DeployRequest.UserName)

		ctx, cancel := context.WithTimeout(context.Background(), time.Duration(cfg.TimeoutSeconds)*time.Second)
		defer cancel()

		log.Printf("Updating deployment for task %s", taskKey)
		result.Success, result.ErrorMsg, result.OldImage = k8sClient.UpdateDeployment(ctx, task.DeployRequest.Service, newImage, namespace)

		if result.Success {
			log.Printf("Waiting for rollout for task %s", taskKey)
			result.Success = k8sClient.WaitForRollout(ctx, task.DeployRequest.Service, namespace)
			if !result.Success {
				result.ErrorMsg = "Rollout failed or timed out"
				log.Printf("Rollout failed for task %s, restoring", taskKey)
				k8sClient.RestoreDeployment(ctx, task.DeployRequest.Service, result.OldImage, namespace)
			} else {
				log.Printf("Rollout successful for task %s", taskKey)
			}
		} else {
			log.Printf("Update deployment failed for task %s: %s", taskKey, result.ErrorMsg)
		}

		if !result.Success {
			log.Printf("Getting diagnostics for failed task %s", taskKey)
			result.Events, result.Logs, result.Envs = k8sClient.GetDeploymentDiagnostics(ctx, task.DeployRequest.Service, namespace)
			log.Printf("Restoring deployment for failed task %s", taskKey)
			k8sClient.RestoreDeployment(ctx, task.DeployRequest.Service, result.OldImage, namespace)
		}

		status := "success"
		if !result.Success {
			status = "failed"
		}
		storage.PersistDeployment(cfg, task.DeployRequest.Service, task.DeployRequest.Env, newImage, status, task.DeployRequest.UserName)

		go telegram.SendTelegramNotification(cfg, result) // Always send notification

		if result.Success {
			feedbackCompleteToGateway(cfg, taskKey)
		}

		log.Printf("Finished processing task %s with status %s", taskKey, status)
	}
}

func feedbackCompleteToGateway(cfg *config.Config, taskKey string) {
	data, err := json.Marshal(map[string]string{"task_key": taskKey})
	if err != nil {
		log.Printf("Failed to marshal complete data for task %s: %v", taskKey, err)
		return
	}

	// Retry logic for gateway complete
	const maxRetries = 3
	for attempt := 1; attempt <= maxRetries; attempt++ {
		req, err := http.NewRequest("POST", cfg.GatewayURL+"/complete", bytes.NewBuffer(data))
		if err != nil {
			log.Printf("Failed to create complete request for task %s (attempt %d/%d): %v", taskKey, attempt, maxRetries, err)
			continue
		}
		req.Header.Set("Content-Type", "application/json")

		client := &http.Client{Timeout: 10 * time.Second}
		resp, err := client.Do(req)
		if err != nil {
			log.Printf("Failed to send complete to gateway for task %s (attempt %d/%d): %v", taskKey, attempt, maxRetries, err)
			if attempt == maxRetries {
				return
			}
			time.Sleep(time.Duration(attempt) * time.Second) // Exponential backoff
			continue
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			log.Printf("Gateway complete failed with status %d for task %s (attempt %d/%d)", resp.StatusCode, taskKey, attempt, maxRetries)
			if attempt == maxRetries {
				return
			}
			continue
		}

		log.Printf("Feedback complete for task %s", taskKey)
		return
	}
}