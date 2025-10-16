// cmd/k8s-cicd/main.go
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
	"sort" // Added import for sort package
	"strings"
	"time"

	"k8s-cicd/internal/config"
	k8shttp "k8s-cicd/internal/http"
	"k8s-cicd/internal/k8s"
	"k8s-cicd/internal/queue"
	"k8s-cicd/internal/storage"
	"k8s-cicd/internal/telegram"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
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
		seenServices[info.Service] = true
		category := classifyService(info.Service, cfg.ServiceKeywords)
		if category == "" {
			category = "other" // Fallback category
		}
		classified[category] = append(classified[category], info.Service)
	}

	// Sort lists for consistency
	for cat := range classified {
		sort.Strings(classified[cat])
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
	return ""
}

func pollGateway() {
	ticker := time.NewTicker(time.Duration(cfg.PollInterval) * time.Second)
	defer ticker.Stop()

	for range ticker.C {
		retryPendingTasks()
	}
}

func retryPendingTasks() {
	for env := range cfg.Environments {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		tasks, err := k8shttp.FetchTasks(ctx, cfg.GatewayURL, env)
		if err != nil {
			log.Printf("Failed to fetch tasks for env %s: %v", env, err)
			continue
		}
		for _, task := range tasks {
			taskKey := queue.ComputeTaskKey(task)
			if !taskQueue.Exists(taskKey) { // Use Exists method instead of taskMap
				taskQueue.Enqueue(queue.Task{DeployRequest: task})
			}
		}
	}
}

func worker() {
	for task := range taskQueue.Dequeue() {
		taskKey := queue.ComputeTaskKey(task.DeployRequest)
		log.Printf("Processing task %s: service=%s, env=%s, version=%s", taskKey, task.DeployRequest.Service, task.DeployRequest.Env, task.DeployRequest.Version)
		ctx, cancel := context.WithTimeout(context.Background(), time.Duration(cfg.TimeoutSeconds)*time.Second)
		defer cancel()

		namespace := cfg.Environments[task.DeployRequest.Env]
		newImage, err := k8sClient.GetNewImage(task.DeployRequest.Service, task.DeployRequest.Version, namespace)
		if err != nil {
			log.Printf("Failed to get new image for task %s: %v", taskKey, err)
			feedbackCompleteToGateway(cfg, taskKey)
			continue
		}

		var result storage.DeployResult
		result.Request = task.DeployRequest
		result.Success, result.ErrorMsg, result.OldImage = k8sClient.UpdateDeployment(ctx, task.DeployRequest.Service, newImage, namespace)

		if result.Success {
			// Observe stability for 2 minutes
			observeCtx, observeCancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer observeCancel()
			stable, err := observeDeploymentStability(observeCtx, k8sClient, task.DeployRequest.Service, namespace, task.DeployRequest.Version)
			if stable {
				result.Success = true
				log.Printf("Deployment stable for task %s", taskKey)
				storage.PersistDeployment(cfg, task.DeployRequest.Service, task.DeployRequest.Env, newImage, "success", task.DeployRequest.UserName)
				go telegram.SendTelegramNotification(cfg, &result)
				feedbackCompleteToGateway(cfg, taskKey)
			} else {
				result.Success = false
				result.ErrorMsg = fmt.Sprintf("Deployment unstable: %v", err)
				handleDeploymentFailure(cfg, &result, taskKey, namespace, newImage)
			}
		} else {
			handleDeploymentFailure(cfg, &result, taskKey, namespace, newImage)
		}
	}
}

func handleDeploymentFailure(cfg *config.Config, result *storage.DeployResult, taskKey, namespace, newImage string) {
	// Get diagnostics
	result.Events, result.Logs, result.Envs = k8sClient.GetDeploymentDiagnostics(context.Background(), result.Request.Service, namespace)

	// Attempt restart
	log.Printf("Attempting restart for failed deployment %s in env %s", result.Request.Service, result.Request.Env)
	k8sClient.RestartDeployment(context.Background(), result.Request.Service, namespace)

	// Wait for restart to take effect
	time.Sleep(30 * time.Second) // Arbitrary wait, adjust as needed

	// Retry update
	retryCtx, retryCancel := context.WithTimeout(context.Background(), time.Duration(cfg.TimeoutSeconds)*time.Second)
	defer retryCancel()
	retrySuccess, retryErrMsg, _ := k8sClient.UpdateDeployment(retryCtx, result.Request.Service, newImage, namespace)

	if retrySuccess {
		// Observe stability again
		observeCtx, observeCancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer observeCancel()
		stable, err := observeDeploymentStability(observeCtx, k8sClient, result.Request.Service, namespace, result.Request.Version)
		if stable {
			result.Success = true
			result.ErrorMsg = ""
			log.Printf("Retry successful for task %s after restart", taskKey)
			storage.PersistDeployment(cfg, result.Request.Service, result.Request.Env, newImage, "success", result.Request.UserName)
			go telegram.SendTelegramNotification(cfg, result)
			feedbackCompleteToGateway(cfg, taskKey)
			return
		} else {
			result.ErrorMsg = fmt.Sprintf("Retry unstable after restart: %v", err)
		}
	} else {
		result.ErrorMsg = fmt.Sprintf("Retry failed after restart: %s", retryErrMsg)
	}

	// If still failed, rollback, notify deploy team, and complete task
	log.Printf("Deployment failed after retry for task %s: %s", taskKey, result.ErrorMsg)
	k8sClient.RestoreDeployment(context.Background(), result.Request.Service, result.OldImage, namespace)
	rollbackReady, rollbackErr := k8sClient.CheckPodReadiness(context.Background(), result.Request.Service, namespace)
	if !rollbackReady {
		result.ErrorMsg += fmt.Sprintf("; Rollback to version %s failed: %v", getVersionFromImage(result.OldImage), rollbackErr)
	} else {
		result.ErrorMsg += fmt.Sprintf("; Rollback to version %s succeeded, service restored.", getVersionFromImage(result.OldImage))
	}
	storage.PersistDeployment(cfg, result.Request.Service, result.Request.Env, newImage, "failed", result.Request.UserName)
	go telegram.SendTelegramNotification(cfg, result)
	go telegram.NotifyDeployTeam(cfg, result) // New function to notify deploy team
	feedbackCompleteToGateway(cfg, taskKey)
}

// observeDeploymentStability checks deployment and pod readiness for 2 minutes
func observeDeploymentStability(ctx context.Context, client *k8s.Client, service, namespace, version string) (bool, error) {
	ticker := time.NewTicker(10 * time.Second) // Check every 10 seconds
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return false, fmt.Errorf("observation timed out after 2 minutes")
		case <-ticker.C:
			// Check deployment status
			rolloutOK := client.WaitForRollout(ctx, service, namespace)
			if !rolloutOK {
				return false, fmt.Errorf("deployment status not available")
			}
			// Check pod readiness
			ready, err := client.CheckPodReadiness(ctx, service, namespace)
			if !ready {
				return false, fmt.Errorf("pods not ready: %v", err)
			}
			// Verify deployment image matches requested version
			gvr := schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"}
			dep, err := client.Client().Resource(gvr).Namespace(namespace).Get(ctx, service, metav1.GetOptions{})
			if err != nil {
				return false, fmt.Errorf("failed to get deployment: %v", err)
			}
			containers, _, _ := unstructured.NestedSlice(dep.Object, "spec", "template", "spec", "containers")
			if len(containers) == 0 {
				return false, fmt.Errorf("no containers found in deployment")
			}
			currentImage, _, _ := unstructured.NestedString(containers[0].(map[string]interface{}), "image")
			if !strings.Contains(currentImage, ":"+version) {
				return false, fmt.Errorf("deployment image %s does not match requested version %s", currentImage, version)
			}
		}
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

func reportToGateway(cfg *config.Config) {
	fileName := storage.GetDailyFileName(time.Now(), "deploy", cfg.StorageDir)
	data, err := os.ReadFile(fileName)
	if err != nil {
		log.Printf("Failed to read deploy file for report: %v", err)
		return
	}
	var infos []storage.DeploymentInfo
	if err := json.Unmarshal(data, &infos); err != nil {
		log.Printf("Failed to unmarshal deploy file for report: %v", err)
		return
	}

	reportData, err := json.Marshal(infos)
	if err != nil {
		log.Printf("Failed to marshal report: %v", err)
		return
	}

	// Retry logic
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
			time.Sleep(time.Duration(attempt) * time.Second)
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