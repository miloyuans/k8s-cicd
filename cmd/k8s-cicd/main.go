// cmd/cicd/main.go
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
	"sort"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1"

	"k8s-cicd/internal/config"
	k8shttp "k8s-cicd/internal/http"
	"k8s-cicd/internal/k8s"
	"k8s-cicd/internal/queue"
	"k8s-cicd/internal/storage"
	"k8s-cicd/internal/telegram"
	"k8s-cicd/internal/types"
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

	storage.InitAllDailyFiles(cfg, k8sClient.DynamicClient())
	storage.UpdateAllDeploymentVersions(cfg, k8sClient.DynamicClient())
	go reportToGateway(cfg)

	taskQueue = queue.NewQueue(cfg, 100)

	log.Println("Performing initial task poll")
	retryPendingTasks()

	for i := 0; i < cfg.MaxConcurrency; i++ {
		go worker()
	}

	go storage.DailyMaintenance(cfg, k8sClient.DynamicClient())
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
	storage.UpdateAllDeploymentVersions(cfg, k8sClient.DynamicClient())
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

func collectAndClassifyServices(cfg *config.Config) (map[string][]string, error) {
	services := make(map[string][]string)
	for env, namespace := range cfg.Environments {
		deployments, err := k8sClient.DynamicClient().Resource(schema.GroupVersionResource{
			Group:    "apps",
			Version:  "v1",
			Resource: "deployments",
		}).Namespace(namespace).List(context.Background(), metav1.ListOptions{})
		if err != nil {
			return nil, fmt.Errorf("failed to list deployments for env %s: %v", env, err)
		}
		for _, dep := range deployments.Items {
			service := dep.GetName()
			category := telegram.ClassifyService(service, cfg.ServiceKeywords)
			services[category] = append(services[category], service)
		}
	}
	for category := range services {
		sort.Strings(services[category])
	}
	return services, nil
}

func reportEnvironmentsToGateway(cfg *config.Config) {
	data, err := json.Marshal(cfg.Environments)
	if err != nil {
		log.Printf("Failed to marshal environments: %v", err)
		return
	}
	attempt := 1
	for {
		req, err := http.NewRequest("POST", cfg.GatewayURL+"/environments", bytes.NewBuffer(data))
		if err != nil {
			log.Printf("Failed to create environments request (attempt %d): %v", attempt, err)
			time.Sleep(getBackoff(attempt))
			attempt++
			continue
		}
		req.Header.Set("Content-Type", "application/json")

		client := &http.Client{Timeout: 10 * time.Second}
		resp, err := client.Do(req)
		if err != nil {
			log.Printf("Failed to send environments to gateway (attempt %d): %v", attempt, err)
			time.Sleep(getBackoff(attempt))
			attempt++
			continue
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			log.Printf("Gateway environments failed with status %d (attempt %d)", resp.StatusCode, attempt)
			time.Sleep(getBackoff(attempt))
			attempt++
			continue
		}

		log.Printf("Successfully reported environments to gateway")
		return
	}
}

func verifyDataFromGateway(cfg *config.Config) bool {
	attempt := 1
	for {
		resp, err := http.Get(cfg.GatewayURL + "/verify-data")
		if err != nil {
			log.Printf("Failed to verify data from gateway (attempt %d): %v", attempt, err)
			time.Sleep(getBackoff(attempt))
			attempt++
			if attempt > 5 {
				return false
			}
			continue
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			log.Printf("Gateway verify-data failed with status %d (attempt %d)", resp.StatusCode, attempt)
			time.Sleep(getBackoff(attempt))
			attempt++
			if attempt > 5 {
				return false
			}
			continue
		}

		var status map[string]bool
		if err := json.NewDecoder(resp.Body).Decode(&status); err != nil {
			log.Printf("Failed to decode verify-data response (attempt %d): %v", attempt, err)
			time.Sleep(getBackoff(attempt))
			attempt++
			if attempt > 5 {
				return false
			}
			continue
		}

		if status["environments_empty"] || status["services_empty"] {
			return false
		}
		return true
	}
}

func pollGateway() {
	ticker := time.NewTicker(time.Duration(cfg.PollInterval) * time.Second)
	defer ticker.Stop()

	for range ticker.C {
		for env := range cfg.Environments {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			tasks, respMap, err := k8shttp.FetchTasks(ctx, cfg.GatewayURL, env)
			cancel()
			if err != nil {
				log.Printf("Failed to fetch tasks for env %s: %v", env, err)
				continue
			}
			if status, ok := respMap["status"].(string); ok && status == "restarted" {
				log.Printf("Gateway restarted for env %s, skipping tasks", env)
				continue
			}
			for _, task := range tasks {
				taskKey := queue.ComputeTaskKey(task)
				if !taskQueue.Exists(taskKey) {
					taskQueue.Enqueue(queue.Task{DeployRequest: task})
				}
			}
		}
	}
}

func worker() {
	for task := range taskQueue.Dequeue() {
		processTask(task)
	}
}

func processTask(task queue.Task) {
	taskKey := queue.ComputeTaskKey(task.DeployRequest)
	log.Printf("Processing task %s", taskKey)

	namespace, ok := cfg.Environments[task.DeployRequest.Env]
	if !ok {
		log.Printf("Invalid environment %s for task %s", task.DeployRequest.Env, taskKey)
		return
	}

	updateCtx, updateCancel := context.WithTimeout(context.Background(), time.Duration(cfg.TimeoutSeconds)*time.Second)
	defer updateCancel()

	newImage, err := k8sClient.GetNewImage(task.DeployRequest.Service, task.DeployRequest.Version, namespace)
	if err != nil {
		log.Printf("Failed to get new image for task %s: %v", taskKey, err)
		return
	}

	success, errMsg, oldImage := k8sClient.UpdateDeployment(updateCtx, task.DeployRequest.Service, newImage, namespace)
	result := storage.DeployResult{
		Request:  task.DeployRequest,
		Success:  success,
		ErrorMsg: errMsg,
		OldImage: oldImage,
		Events:   k8sClient.GetPodEvents(updateCtx, task.DeployRequest.Service, namespace),
		Logs:     "",
		Envs:     make(map[string]string),
	}

	if success {
		checkCtx, checkCancel := context.WithTimeout(context.Background(), time.Duration(cfg.TimeoutSeconds)*time.Second)
		defer checkCancel()

		ready, readyErr := k8sClient.CheckNewPodStatus(checkCtx, task.DeployRequest.Service, newImage, namespace)
		if ready {
			result.Success = true
			result.ErrorMsg = ""
			storage.PersistDeployment(cfg, result.Request.Service, result.Request.Env, newImage, "success", result.Request.UserName)
			if err := telegram.SendTelegramNotification(cfg, &result); err != nil {
				log.Printf("Failed to send Telegram notification for task %s: %v", taskKey, err)
			}
			feedbackCompleteToGateway(cfg, taskKey)
		} else {
			result.Success = false
			result.ErrorMsg = fmt.Sprintf("Pod status check failed: %v", readyErr)
			pods := k8sClient.GetPodsForDeployment(checkCtx, task.DeployRequest.Service, namespace)
			if len(pods) > 0 {
				result.Logs = k8sClient.GetPodLogs(checkCtx, pods[0].Name, namespace)
				result.Envs = k8sClient.GetPodEnv(checkCtx, pods[0].Name, namespace)
			}
			maxRollbackAttempts := 3
			var rollbackErr error
			for attempt := 1; attempt <= maxRollbackAttempts; attempt++ {
				rollbackCtx, rollbackCancel := context.WithTimeout(context.Background(), time.Duration(cfg.TimeoutSeconds)*time.Second)
				rollbackErr = k8sClient.RollbackDeployment(rollbackCtx, task.DeployRequest.Service, namespace)
				rollbackCancel()
				if rollbackErr == nil {
					break
				}
				log.Printf("Rollback failed for task %s (attempt %d/%d): %v", taskKey, attempt, maxRollbackAttempts, rollbackErr)
				if attempt < maxRollbackAttempts {
					time.Sleep(5 * time.Second)
				}
			}
			if rollbackErr != nil {
				result.ErrorMsg += fmt.Sprintf("; Rollback failed after %d attempts: %v", maxRollbackAttempts, rollbackErr)
			} else {
				rollbackReady, rollbackErr := k8sClient.CheckNewPodStatus(checkCtx, task.DeployRequest.Service, oldImage, namespace)
				if rollbackReady {
					result.ErrorMsg += fmt.Sprintf("; Rollback succeeded to version: %s", getVersionFromImage(oldImage))
				} else {
					result.ErrorMsg += fmt.Sprintf("; Rollback verification failed: %v", rollbackErr)
				}
			}

			storage.PersistDeployment(cfg, result.Request.Service, result.Request.Env, newImage, "failed", result.Request.UserName)
			go func() {
				if err := telegram.SendTelegramNotification(cfg, &result); err != nil {
					log.Printf("Failed to send Telegram notification for task %s: %v", taskKey, err)
				}
				if err := telegram.NotifyDeployTeam(cfg, &result); err != nil {
					log.Printf("Failed to send deploy team notification for task %s: %v", taskKey, err)
				}
			}()
			feedbackCompleteToGateway(cfg, taskKey)
		}
	} else {
		result.Success = false
		result.ErrorMsg = errMsg
		result.OldImage = oldImage
		result.Events = k8sClient.GetPodEvents(updateCtx, task.DeployRequest.Service, namespace)
		result.Logs = "Deployment update failed"
		storage.PersistDeployment(cfg, result.Request.Service, result.Request.Env, newImage, "failed", result.Request.UserName)
		go func() {
			if err := telegram.SendTelegramNotification(cfg, &result); err != nil {
				log.Printf("Failed to send Telegram notification for task %s: %v", taskKey, err)
			}
			if err := telegram.NotifyDeployTeam(cfg, &result); err != nil {
				log.Printf("Failed to send deploy team notification for task %s: %v", taskKey, err)
			}
		}()
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

func getBackoff(attempt int) time.Duration {
	return time.Duration(attempt*attempt) * time.Second
}

func retryPendingTasks() {
	taskQueue.LoadPendingTasks()
}