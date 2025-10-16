// cmd/gateway/main.go
package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"path/filepath"
	"regexp"
	"sort" // Added import for sort package
	"strings"
	"time"

	"github.com/google/uuid"

	"k8s-cicd/internal/config"
	"k8s-cicd/internal/dialog"
	"k8s-cicd/internal/queue"
	"k8s-cicd/internal/storage"
	"k8s-cicd/internal/telegram"
	"k8s-cicd/internal/types"
	"os"
)

var (
	taskQueue *queue.Queue
)

func main() {
	cfg := config.LoadConfig("config.yaml")
	cfg.StorageDir = "gateway_storage" // Set gateway-specific storage directory
	if err := storage.EnsureStorageDir(cfg.StorageDir); err != nil {
		log.Fatalf("Failed to create storage dir: %v", err)
	}

	// Initialize all daily files
	storage.InitAllDailyFiles(cfg, nil)

	// Initialize service lists dynamically from reports
	go storage.DailyMaintenance(cfg, nil) // Adjusted for gateway, no k8s client

	// Initialize task queue
	taskQueue = queue.NewQueue(cfg, 100)
	dialog.GlobalTaskQueue = taskQueue

	go telegram.StartBot(cfg, taskQueue)

	http.HandleFunc("/tasks", handleTasks(cfg))
	http.HandleFunc("/report", handleReport(cfg))
	http.HandleFunc("/complete", handleComplete(cfg))
	http.HandleFunc("/services", handleServices(cfg))
	http.HandleFunc("/submit-task", handleSubmitTask(cfg))
	log.Printf("Gateway server starting on %s", cfg.GatewayListenAddr)
	log.Fatal(http.ListenAndServe(cfg.GatewayListenAddr, nil))
}

func handleTasks(cfg *config.Config) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		clientIP := getClientIP(r)
		if r.Method == http.MethodGet && !config.IsIPAllowed(clientIP, cfg.AllowedIPs) {
			log.Printf("Rejected GET request from IP %s: not in allowed_ips", clientIP)
			http.Error(w, "Forbidden: IP not allowed", http.StatusForbidden)
			return
		}

		if r.Method == http.MethodGet {
			env := r.URL.Query().Get("env")
			if env == "" {
				log.Printf("Missing env parameter in GET request from IP %s", clientIP)
				http.Error(w, "Missing env parameter", http.StatusBadRequest)
				return
			}
			lowerEnv := strings.ToLower(env)
			tasks := taskQueue.GetPendingTasks(lowerEnv)
			log.Printf("Serving %d pending tasks for env %s", len(tasks), lowerEnv)

			w.Header().Set("Content-Type", "application/json")
			if err := json.NewEncoder(w).Encode(tasks); err != nil {
				log.Printf("Failed to encode tasks response for IP %s: %v", clientIP, err)
				http.Error(w, "Internal server error", http.StatusInternalServerError)
				return
			}

			storage.PersistInteraction(cfg, map[string]interface{}{
				"endpoint":  "/tasks",
				"method":    "GET",
				"env":       lowerEnv,
				"tasks":     len(tasks),
				"timestamp": time.Now().Format(time.RFC3339),
			})
		} else {
			log.Printf("Method not allowed for request from IP %s: %s", clientIP, r.Method)
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		}
	}
}

func handleReport(cfg *config.Config) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			log.Printf("Method not allowed for /report: %s", r.Method)
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}

		var infos []storage.DeploymentInfo
		if err := json.NewDecoder(r.Body).Decode(&infos); err != nil {
			log.Printf("Failed to decode report: %v", err)
			http.Error(w, "Invalid JSON", http.StatusBadRequest)
			return
		}

		// Read existing data to merge with new report
		fileName := storage.GetDailyFileName(time.Now(), "deploy", cfg.StorageDir)
		if err := storage.EnsureDailyFile(fileName, nil, cfg); err != nil {
			log.Printf("Failed to ensure deploy file %s: %v", fileName, err)
			http.Error(w, "Internal server error", http.StatusInternalServerError)
			return
		}

		data, err := os.ReadFile(fileName)
		if err != nil {
			log.Printf("Failed to read deploy file %s: %v", fileName, err)
			http.Error(w, "Internal server error", http.StatusInternalServerError)
			return
		}
		var existingInfos []storage.DeploymentInfo
		if len(data) > 0 {
			if err := json.Unmarshal(data, &existingInfos); err != nil {
				log.Printf("Failed to unmarshal deploy file %s: %v", fileName, err)
				http.Error(w, "Internal server error", http.StatusInternalServerError)
				return
			}
		}

		// Merge reported data, updating or appending as needed
		for _, newInfo := range infos {
			found := false
			for i, info := range existingInfos {
				if info.Service == newInfo.Service && info.Env == newInfo.Env {
					existingInfos[i] = newInfo
					found = true
					break
				}
			}
			if !found {
				existingInfos = append(existingInfos, newInfo)
			}
		}

		// Save merged data
		updatedData, err := json.MarshalIndent(existingInfos, "", "  ")
		if err != nil {
			log.Printf("Failed to marshal merged deploy file %s: %v", fileName, err)
			http.Error(w, "Internal server error", http.StatusInternalServerError)
			return
		}
		if err := os.WriteFile(fileName, updatedData, 0644); err != nil {
			log.Printf("Failed to write merged deploy file %s: %v", fileName, err)
			http.Error(w, "Internal server error", http.StatusInternalServerError)
			return
		}

		w.WriteHeader(http.StatusOK)
	}
}

func handleComplete(cfg *config.Config) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			log.Printf("Method not allowed for /complete: %s", r.Method)
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}

		var req struct {
			TaskKey string `json:"task_key"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			log.Printf("Failed to decode complete request: %v", err)
			http.Error(w, "Invalid JSON", http.StatusBadRequest)
			return
		}

		taskQueue.CompleteTask(req.TaskKey)
		log.Printf("Completed task %s", req.TaskKey)
		w.WriteHeader(http.StatusOK)
	}
}

func handleServices(cfg *config.Config) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			log.Printf("Method not allowed for /services: %s", r.Method)
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}

		var services map[string][]string
		if err := json.NewDecoder(r.Body).Decode(&services); err != nil {
			log.Printf("Failed to decode services: %v", err)
			http.Error(w, "Invalid JSON", http.StatusBadRequest)
			return
		}

		for category, list := range services {
			// Deduplicate list
			seen := make(map[string]bool)
			var dedup []string
			for _, svc := range list {
				if !seen[svc] {
					seen[svc] = true
					dedup = append(dedup, svc)
				}
			}
			// Sort for consistency
			sort.Strings(dedup)
			// Load existing list to merge
			existingPath := filepath.Join(cfg.ServicesDir, fmt.Sprintf("%s.svc.list", category))
			existingData, err := os.ReadFile(existingPath)
			var existing []string
			if err == nil {
				existing = strings.Split(string(existingData), "\n")
				for i := range existing {
					existing[i] = strings.TrimSpace(existing[i])
				}
			}
			// Merge and dedup again
			for _, svc := range dedup {
				if svc != "" && !config.contains(existing, svc) { // Changed to config.contains
					existing = append(existing, svc)
				}
			}
			// Sort for consistency
			sort.Strings(existing)
			// Remove empty lines
			var clean []string
			for _, s := range existing {
				if s != "" {
					clean = append(clean, s)
				}
			}
			// Save updated list
			filePath := filepath.Join(cfg.ServicesDir, fmt.Sprintf("%s.svc.list", category))
			data := strings.Join(clean, "\n")
			if err := os.WriteFile(filePath, []byte(data), 0644); err != nil {
				log.Printf("Failed to write service list %s: %v", filePath, err)
			} else {
				log.Printf("Updated service list %s with %d services", filePath, len(clean))
			}
		}

		log.Printf("Processed services update with %d categories", len(services))

		w.WriteHeader(http.StatusOK)
	}
}

func handleSubmitTask(cfg *config.Config) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			log.Printf("Method not allowed for /submit-task: %s", r.Method)
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}

		var req struct {
			Service  string   `json:"service"`
			Envs     []string `json:"envs"`
			Version  string   `json:"version"`
			Username string   `json:"username"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			log.Printf("Failed to decode submit-task request: %v", err)
			http.Error(w, "Invalid JSON", http.StatusBadRequest)
			return
		}

		if req.Service == "" || len(req.Envs) == 0 || req.Version == "" || req.Username == "" {
			log.Printf("Missing required fields in submit-task request")
			http.Error(w, "Missing required fields: service, envs, version, username", http.StatusBadRequest)
			return
		}

		// Validate environments
		for _, env := range req.Envs {
			if _, ok := cfg.Environments[strings.ToLower(env)]; !ok {
				log.Printf("Invalid environment %s in submit-task request", env)
				http.Error(w, fmt.Sprintf("Invalid environment: %s", env), http.StatusBadRequest)
				return
			}
		}

		category := classifyService(req.Service, cfg.ServiceKeywords)
		if category == "" {
			log.Printf("No category found for service %s", req.Service)
			http.Error(w, "No category found for service", http.StatusBadRequest)
			return
		}

		chatID, ok := cfg.TelegramChats[category]
		if !ok {
			log.Printf("No chat configured for category %s", category)
			http.Error(w, "No chat configured for category", http.StatusInternalServerError)
			return
		}

		var taskKeys []string
		var tasks []types.DeployRequest
		for _, env := range req.Envs {
			lowerEnv := strings.ToLower(env)
			deployReq := types.DeployRequest{
				Service:   req.Service,
				Env:       lowerEnv,
				Version:   req.Version,
				Timestamp: time.Now(),
				UserName:  req.Username,
				Status:    "pending_confirmation", // Initial status
			}
			taskKeys = append(taskKeys, queue.ComputeTaskKey(deployReq))
			tasks = append(tasks, deployReq)
		}

		id := uuid.New().String()[:8] // Short ID
		dialog.PendingConfirmations.Store(id, tasks)

		message := fmt.Sprintf("确认部署服务 %s 到环境 %s，版本 %s，由用户 %s 提交？\nConfirm deployment for service %s to envs %s, version %s by %s?",
			req.Service, strings.Join(req.Envs, ","), req.Version, req.Username,
			req.Service, strings.Join(req.Envs, ","), req.Version, req.Username)
		callbackData := id

		if err := telegram.SendConfirmation(category, chatID, message, callbackData); err != nil {
			log.Printf("Failed to send confirmation for submit-task: %v", err)
			http.Error(w, "Failed to send confirmation to Telegram", http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{
			"status":          "submitted",
			"message":         "Task submitted, awaiting confirmation in Telegram",
			"confirmation_id": id,
		})
	}
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

func getClientIP(r *http.Request) string {
	if forwarded := r.Header.Get("X-Forwarded-For"); forwarded != "" {
		ips := strings.Split(forwarded, ",")
		return strings.TrimSpace(ips[0])
	}
	ip, _, _ := net.SplitHostPort(r.RemoteAddr)
	return ip
}