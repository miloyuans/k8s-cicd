// cmd/gateway/main.go
package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"k8s-cicd/internal/config"
	"k8s-cicd/internal/dialog"
	"k8s-cicd/internal/queue"
	"k8s-cicd/internal/storage"
	"k8s-cicd/internal/telegram"
	"k8s-cicd/internal/types"
)

var (
	taskQueue   *queue.Queue
	isRestarted bool = true // 默认启动时为true
)

func main() {
	cfg := config.LoadConfig("config.yaml")
	cfg.StorageDir = "gateway_storage"
	if err := storage.EnsureStorageDir(cfg.StorageDir); err != nil {
		log.Fatalf("Failed to create storage dir: %v", err)
	}

	initServicesDir(cfg)
	checkIfFilesEmpty(cfg) // 新增检查

	storage.InitAllDailyFiles(cfg, nil)
	go storage.DailyMaintenance(cfg, nil)

	taskQueue = queue.NewQueue(cfg, 100)
	dialog.GlobalTaskQueue = taskQueue

	go telegram.StartBot(cfg, taskQueue)

	http.HandleFunc("/tasks", handleTasks(cfg))
	http.HandleFunc("/report", handleReport(cfg))
	http.HandleFunc("/complete", handleComplete(cfg))
	http.HandleFunc("/services", handleServices(cfg))
	http.HandleFunc("/submit-task", handleSubmitTask(cfg))
	http.HandleFunc("/verify-data", handleVerifyData(cfg))
	http.HandleFunc("/environments", handleEnvironments(cfg))
	log.Printf("Gateway server starting on %s", cfg.GatewayListenAddr)
	log.Fatal(http.ListenAndServe(cfg.GatewayListenAddr, nil))
}

// 新增函数：检查文件是否为空
func checkIfFilesEmpty(cfg *config.Config) {
	envFile := filepath.Join(cfg.StorageDir, "environments.json")
	if fileEmpty(envFile) {
		isRestarted = true
		log.Printf("environments.json is empty or missing, setting isRestarted=true")
	}
	for category := range cfg.TelegramBots {
		svcFile := filepath.Join(cfg.ServicesDir, fmt.Sprintf("%s.svc.list", category))
		if fileEmpty(svcFile) {
			isRestarted = true
			log.Printf("%s is empty or missing, setting isRestarted=true", svcFile)
		}
	}
	otherFile := filepath.Join(cfg.ServicesDir, "other.svc.list")
	if fileEmpty(otherFile) {
		isRestarted = true
		log.Printf("other.svc.list is empty or missing, setting isRestarted=true")
	}
}

func fileEmpty(path string) bool {
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return true
	}
	data, err := os.ReadFile(path)
	if err != nil || len(data) == 0 || string(data) == "[]" || string(data) == "{}" {
		return true
	}
	return false
}
func initServicesDir(cfg *config.Config) {
	if _, err := os.Stat(cfg.ServicesDir); os.IsNotExist(err) {
		if err := os.MkdirAll(cfg.ServicesDir, 0755); err != nil {
			log.Fatalf("Failed to create services dir %s: %v", cfg.ServicesDir, err)
		}
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
			if isRestarted || fileEmpty(filepath.Join(cfg.StorageDir, "environments.json")) || checkSvcFilesEmpty(cfg) {
				// 返回特殊响应
				resp := map[string]interface{}{
					"status": "restarted",
					"tasks":  []types.DeployRequest{},
				}
				w.Header().Set("Content-Type", "application/json")
				if err := json.NewEncoder(w).Encode(resp); err != nil {
					// ... 错误处理
				}
				isRestarted = false // 重置标志
				log.Printf("Detected restart or empty files, returned 'restarted' status")
				return
			}
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
			})
		}
	}
}

func checkSvcFilesEmpty(cfg *config.Config) bool {
	for category := range cfg.TelegramBots {
		svcFile := filepath.Join(cfg.ServicesDir, fmt.Sprintf("%s.svc.list", category))
		if fileEmpty(svcFile) {
			return true
		}
	}
	otherFile := filepath.Join(cfg.ServicesDir, "other.svc.list")
	return fileEmpty(otherFile)
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
			log.Printf("Failed to decode report request: %v", err)
			http.Error(w, "Invalid JSON", http.StatusBadRequest)
			return
		}

		fileName := storage.GetDailyFileName(time.Now(), "deploy", cfg.StorageDir)
		if err := storage.EnsureDailyFile(fileName, nil, cfg); err != nil {
			log.Printf("Failed to ensure deploy file %s: %v", fileName, err)
			http.Error(w, "Internal server error", http.StatusInternalServerError)
			return
		}

		storage.UpdateDeploymentInfo(cfg, infos)
		log.Printf("Updated deployment info with %d entries", len(infos))
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

		if req.TaskKey == "" {
			log.Printf("Missing task_key in complete request")
			http.Error(w, "Missing task_key", http.StatusBadRequest)
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
			log.Printf("Failed to decode services request: %v", err)
			http.Error(w, "Invalid JSON", http.StatusBadRequest)
			return
		}

		for category, svcList := range services {
			filePath := filepath.Join(cfg.ServicesDir, fmt.Sprintf("%s.svc.list", category))
			clean := make([]string, 0, len(svcList))
			seen := make(map[string]bool)
			for _, svc := range svcList {
				if !seen[svc] {
					seen[svc] = true
					clean = append(clean, svc)
				}
			}
			sort.Strings(clean)
			data := strings.Join(clean, "\n")
			if err := os.WriteFile(filePath, []byte(data), 0644); err != nil {
				log.Printf("Failed to write service list %s: %v", filePath, err)
				http.Error(w, "Internal server error", http.StatusInternalServerError)
				return
			}
			log.Printf("Updated service list %s with %d services", filePath, len(clean))
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

		for _, env := range req.Envs {
			if !validateEnvironment(env, cfg) {
				log.Printf("Invalid environment %s in submit-task request", env)
				http.Error(w, fmt.Sprintf("Invalid environment: %s", env), http.StatusBadRequest)
				return
			}
		}

		category := classifyService(req.Service, cfg.ServiceKeywords)
		chatID, ok := cfg.TelegramChats[category]
		if !ok {
			log.Printf("No chat configured for category %s, trying default chat", category)
			if defaultChatID, ok := cfg.TelegramChats["other"]; ok {
				chatID = defaultChatID
				category = "other"
			} else {
				log.Printf("No default chat configured for category %s", category)
				http.Error(w, "No chat configured for category", http.StatusInternalServerError)
				return
			}
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
				Status:    "pending_confirmation",
			}
			taskKeys = append(taskKeys, queue.ComputeTaskKey(deployReq))
			tasks = append(tasks, deployReq)
		}

		id := uuid.New().String()[:8]
		dialog.PendingConfirmations.Store(id, tasks)

		message := fmt.Sprintf("确认部署服务 %s 到环境 %s，版本 %s，由用户 %s 提交？\nConfirm deployment for service %s to envs %s, version %s by %s?",
			req.Service, strings.Join(req.Envs, ","), req.Version, req.Username,
			req.Service, strings.Join(req.Envs, ","), req.Version, req.Username)
		callbackData := id

		if err := dialog.SendConfirmation(category, chatID, message, callbackData, cfg); err != nil {
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

func validateEnvironment(env string, cfg *config.Config) bool {
	fileName := filepath.Join(cfg.StorageDir, "environments.json")
	if data, err := os.ReadFile(fileName); err == nil && len(data) > 0 {
		var envs map[string]string
		if err := json.Unmarshal(data, &envs); err == nil {
			if _, exists := envs[env]; exists { // 优化：保持大小写
				return true
			}
		}
	} else {
		log.Printf("environments.json empty or error, falling back to config.Environments")
	}
	_, exists := cfg.Environments[env] // 优化：保持大小写，不转小写
	return exists
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

func getClientIP(r *http.Request) string {
	if forwarded := r.Header.Get("X-Forwarded-For"); forwarded != "" {
		ips := strings.Split(forwarded, ",")
		return strings.TrimSpace(ips[0])
	}
	ip, _, _ := net.SplitHostPort(r.RemoteAddr)
	return ip
}

func handleVerifyData(cfg *config.Config) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			log.Printf("Method not allowed for /verify-data: %s", r.Method)
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}

		envFile := filepath.Join(cfg.StorageDir, "environments.json")
		envEmpty := fileEmpty(envFile)

		svcEmpty := checkSvcFilesEmpty(cfg)

		status := map[string]bool{
			"environments_empty": envEmpty,
			"services_empty":     svcEmpty,
		}

		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(status); err != nil {
			log.Printf("Failed to encode verify-data response: %v", err)
			http.Error(w, "Internal server error", http.StatusInternalServerError)
			return
		}
	}
}

func handleEnvironments(cfg *config.Config) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			log.Printf("Method not allowed for /environments: %s", r.Method)
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}

		var envs map[string]string
		if err := json.NewDecoder(r.Body).Decode(&envs); err != nil {
			log.Printf("Failed to decode environments request: %v", err)
			http.Error(w, "Invalid JSON", http.StatusBadRequest)
			return
		}

		filePath := filepath.Join(cfg.StorageDir, "environments.json")
		data, err := json.MarshalIndent(envs, "", "  ")
		if err != nil {
			log.Printf("Failed to marshal environments: %v", err)
			http.Error(w, "Internal server error", http.StatusInternalServerError)
			return
		}
		if err := os.WriteFile(filePath, data, 0644); err != nil {
			log.Printf("Failed to write environments.json: %v", err)
			http.Error(w, "Internal server error", http.StatusInternalServerError)
			return
		}

		log.Printf("Updated environments with %d entries", len(envs))
		w.WriteHeader(http.StatusOK)
	}
}