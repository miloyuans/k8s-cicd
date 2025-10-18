package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"sort"
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
	cfg.StorageDir = "cicd_storage"
	k8sClient = k8s.NewClient(cfg.KubeConfigPath)

	if err := storage.EnsureStorageDir(cfg.StorageDir); err != nil {
		log.Fatalf("Failed to create storage dir: %v", err)
	}

	initServicesDir(cfg)

	storage.InitAllDailyFiles(cfg, k8sClient.Client())
	storage.UpdateAllDeploymentVersions(cfg, k8sClient.Client())
	go reportToGateway(cfg)

	taskQueue = queue.NewQueue(cfg, 100)

	log.Println("Performing initial task poll")
	retryPendingTasks()

	for i := 0; i < cfg.MaxConcurrency; i++ {
		go worker()
	}

	go storage.DailyMaintenance(cfg, k8sClient.Client())
	go pollGateway()

	updateAndReportServices()
	go periodicUpdateServices()

	select {}
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
		filePath := filepath.Join(cfg.ServicesDir, "other.svc.list")
		if err := os.WriteFile(filePath, []byte(""), 0644); err != nil {
			log.Printf("Failed to init service file %s: %v", filePath, err)
		}
		log.Println("Initialized services directory and files")
	}
}

func updateAndReportServices() {
	log.Println("Immediate updating services")
	storage.UpdateAllDeploymentVersions(cfg, k8sClient.Client())
	go reportServicesToGateway(cfg)
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

	attempt := 1
	for {
		req, err := http.NewRequest("POST", cfg.GatewayURL+"/services", bytes.NewBuffer(data))
		if err != nil {
			log.Printf("Failed to create services request (attempt %d): %v", attempt, err)
			time.Sleep(getBackoff(attempt))
			attempt++
			continue
		}
		req.Header.Set("Content-Type", "application/json")

		client := &http.Client{Timeout: 10 * time.Second}
		resp, err := client.Do(req)
		if err != nil {
			log.Printf("Failed to send services to gateway (attempt %d): %v", attempt, err)
			time.Sleep(getBackoff(attempt))
			attempt++
			continue
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			log.Printf("Gateway services failed with status %d (attempt %d)", resp.StatusCode, attempt)
			time.Sleep(getBackoff(attempt))
			attempt++
			continue
		}

		log.Printf("Successfully reported services to gateway")
		return
	}
}

func getBackoff(attempt int) time.Duration {
	backoff := time.Duration(1<<uint(attempt-1)) * time.Second
	if backoff > 60*time.Second {
		return 60 * time.Second
	}
	return backoff
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
	seenServices := make(map[string]bool)
	for _, info := range infos {
		if seenServices[info.Service] {
			continue
		}
		seenServices[info.Service] = true
		category := classifyService(info.Service, cfg.ServiceKeywords)
		if category == "" {
			category = "other"
		}
		classified[category] = append(classified[category], info.Service)
	}
	for _, services := range classified {
		sort.Strings(services)
	}
	return classified, nil
}

func classifyService(service string, keywords map[string][]string) string {
	lowerService := strings.ToLower(service)
	for category, patterns := range keywords {
		for _, pattern := range patterns {
			lowerPattern := strings.ToLower(pattern)
			if strings.HasPrefix(lowerPattern, "^") || strings.HasSuffix(lowerPattern, "$") || strings.Contains(lowerPattern, ".*") {
				re, err := regexp.Compile(lowerPattern)
				if err == nil && re.MatchString(lowerService) {
					return category
				}
			} else if strings.Contains(lowerService, lowerPattern) {
				return category
			}
		}
	}
	return "other"
}

func retryPendingTasks() {
	for _, env := range cfg.Environments {
		tasks, err := k8shttp.FetchTasks(context.Background(), cfg.GatewayURL, env)
		if err != nil {
			log.Printf("Failed to fetch tasks for env %s: %v", env, err)
			continue
		}
		for _, task := range tasks {
			if task.Status == "pending" {
				taskKey := queue.ComputeTaskKey(task)
				log.Printf("Enqueued task %s for key %s", taskKey, taskKey)
				taskQueue.Enqueue(queue.Task{DeployRequest: task})
			}
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
		log.Printf("Processing task %s", taskKey)

		result := storage.DeployResult{
			Request: task.DeployRequest,
		}
		namespace := cfg.Environments[task.DeployRequest.Env]
		if namespace == "" {
			log.Printf("No namespace for env %s, using default namespace 'international'", task.DeployRequest.Env)
			namespace = "international"
		}

		newImage, err := k8sClient.GetNewImage(task.DeployRequest.Service, task.DeployRequest.Version, namespace)
		if err != nil {
			log.Printf("Failed to get new image for task %s: %v", taskKey, err)
			continue
		}
		result.OldImage = strings.Split(newImage, ":")[0] + ":latest"

		ctx, cancel := context.WithTimeout(context.Background(), time.Duration(cfg.TimeoutSeconds)*time.Second)
		defer cancel()
		success, errMsg, oldImage := k8sClient.UpdateDeployment(ctx, task.DeployRequest.Service, newImage, namespace)
		result.OldImage = oldImage
		if !success {
			result.Success = false
			result.ErrorMsg = errMsg
			log.Printf("Failed to update deployment for task %s: %s", taskKey, errMsg)
			storage.PersistDeployment(cfg, result.Request.Service, result.Request.Env, newImage, "failed", result.Request.UserName)
			go telegram.SendTelegramNotification(cfg, &result)
			feedbackCompleteToGateway(cfg, taskKey)
			continue
		}

		checkCtx, checkCancel := context.WithTimeout(context.Background(), 1*time.Minute)
		defer checkCancel()
		ready, err := k8sClient.CheckNewPodStatus(checkCtx, result.Request.Service, newImage, namespace)
		if ready {
			result.Success = true
			result.ErrorMsg = ""
			log.Printf("Deployment successful for task %s: service=%s, env=%s, version=%s, user=%s, oldImage=%s",
				taskKey, result.Request.Service, result.Request.Env, result.Request.Version, result.Request.UserName, result.OldImage)
			storage.PersistDeployment(cfg, result.Request.Service, result.Request.Env, newImage, "success", result.Request.UserName)
			go telegram.SendTelegramNotification(cfg, &result)
		} else {
			result.Success = false
			result.ErrorMsg = fmt.Sprintf("Pods not ready: %v", err)
			result.Events, result.Logs, result.Envs = k8sClient.GetDeploymentDiagnostics(checkCtx, result.Request.Service, namespace)
			log.Printf("Deployment failed for task %s: service=%s, env=%s, version=%s, user=%s, error=%s, events=%s, logs=%s, envs=%v",
				taskKey, result.Request.Service, result.Request.Env, result.Request.Version, result.Request.UserName, result.ErrorMsg, result.Events, result.Logs, result.Envs)
			err = k8sClient.RollbackDeployment(checkCtx, result.Request.Service, namespace)
			if err != nil {
				result.ErrorMsg += fmt.Sprintf("; Rollback failed: %v", err)
				log.Printf("Rollback failed for task %s: %v", taskKey, err)
			} else {
				rollbackCtx, rollbackCancel := context.WithTimeout(context.Background(), 1*time.Minute)
				defer rollbackCancel()
				rollbackReady, rollbackErr := k8sClient.CheckNewPodStatus(rollbackCtx, result.Request.Service, "", namespace)
				if rollbackReady {
					result.ErrorMsg += "; Rollback succeeded."
					log.Printf("Rollback succeeded for task %s", taskKey)
				} else {
					result.ErrorMsg += fmt.Sprintf("; Rollback failed: %v", rollbackErr)
					log.Printf("Rollback check failed for task %s: %v", taskKey, rollbackErr)
				}
			}
			storage.PersistDeployment(cfg, result.Request.Service, result.Request.Env, newImage, "failed", result.Request.UserName)
			go telegram.SendTelegramNotification(cfg, &result)
			go telegram.NotifyDeployTeam(cfg, &result)
		}
		feedbackCompleteToGateway(cfg, taskKey)
	}
}

func getVersionFromImage(image string) string {
	parts := strings.Split(image, ":")
	if len(parts) == 2 {
		return parts[1]
	}
	return "unknown"
}

func feedbackCompleteToGateway(cfg *config.Config, taskKey string) {
	data, err := json.Marshal(map[string]string{"task_key": taskKey})
	if err != nil {
		log.Printf("Failed to marshal complete data for task %s: %v", taskKey, err)
		return
	}

	attempt := 1
	for {
		req, err := http.NewRequest("POST", cfg.GatewayURL+"/complete", bytes.NewBuffer(data))
		if err != nil {
			log.Printf("Failed to create complete request for task %s (attempt %d): %v", taskKey, attempt, err)
			time.Sleep(getBackoff(attempt))
			attempt++
			continue
		}
		req.Header.Set("Content-Type", "application/json")

		client := &http.Client{Timeout: 10 * time.Second}
		resp, err := client.Do(req)
		if err != nil {
			log.Printf("Failed to send complete to gateway for task %s (attempt %d): %v", taskKey, attempt, err)
			time.Sleep(getBackoff(attempt))
			attempt++
			continue
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			log.Printf("Gateway complete failed with status %d for task %s (attempt %d)", resp.StatusCode, taskKey, attempt)
			time.Sleep(getBackoff(attempt))
			attempt++
			continue
		}

		log.Printf("Feedback complete for task %s", taskKey)
		return
	}
}

func reportToGateway(cfg *config.Config) {
	fileName := storage.GetDailyFileName(time.Now(), "deploy", cfg.StorageDir)
	attempt := 1
	for {
		data, err := os.ReadFile(fileName)
		if err != nil {
			log.Printf("Failed to read deploy file for report (attempt %d): %v", attempt, err)
			time.Sleep(getBackoff(attempt))
			attempt++
			continue
		}
		var infos []storage.DeploymentInfo
		if err := json.Unmarshal(data, &infos); err != nil {
			log.Printf("Failed to unmarshal deploy file for report (attempt %d): %v", attempt, err)
			time.Sleep(getBackoff(attempt))
			attempt++
			continue
		}

		reportData, err := json.Marshal(infos)
		if err != nil {
			log.Printf("Failed to marshal report (attempt %d): %v", attempt, err)
			time.Sleep(getBackoff(attempt))
			attempt++
			continue
		}

		req, err := http.NewRequest("POST", cfg.GatewayURL+"/report", bytes.NewBuffer(reportData))
		if err != nil {
			log.Printf("Failed to create report request (attempt %d): %v", attempt, err)
			time.Sleep(getBackoff(attempt))
			attempt++
			continue
		}
		req.Header.Set("Content-Type", "application/json")

		client := &http.Client{Timeout: 10 * time.Second}
		resp, err := client.Do(req)
		if err != nil {
			log.Printf("Failed to send report to gateway (attempt %d): %v", attempt, err)
			time.Sleep(getBackoff(attempt))
			attempt++
			continue
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			log.Printf("Gateway report failed with status %d (attempt %d)", resp.StatusCode, attempt)
			time.Sleep(getBackoff(attempt))
			attempt++
			continue
		}

		log.Printf("Successfully reported to gateway")
		return
	}
}