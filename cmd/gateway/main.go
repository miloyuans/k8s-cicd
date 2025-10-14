package main

import (
	"encoding/json"
	"log"
	"net"
	"net/http"
	"strings"
	"time"

	"k8s-cicd/internal/config"
	"k8s-cicd/internal/dialog"
	"k8s-cicd/internal/queue"
	"k8s-cicd/internal/storage"
	"k8s-cicd/internal/telegram"
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

	// Initialize service lists
	if _, err := config.LoadServiceLists(cfg.ServicesDir, cfg.TelegramBots); err != nil {
		log.Printf("Failed to initialize service lists: %v", err)
	}

	// Initialize task queue
	taskQueue = queue.NewQueue(cfg, 100)

	go telegram.StartBot(cfg)

	http.HandleFunc("/tasks", handleTasks(cfg))
	http.HandleFunc("/report", handleReport(cfg))
	http.HandleFunc("/complete", handleComplete(cfg))
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

			tasks := taskQueue.GetPendingTasks(env)
			log.Printf("Serving %d pending tasks for env %s", len(tasks), env)

			w.Header().Set("Content-Type", "application/json")
			if err := json.NewEncoder(w).Encode(tasks); err != nil {
				log.Printf("Failed to encode tasks response for IP %s: %v", clientIP, err)
				http.Error(w, "Internal server error", http.StatusInternalServerError)
				return
			}

			storage.PersistInteraction(cfg, map[string]interface{}{
				"endpoint":  "/tasks",
				"method":    "GET",
				"env":       env,
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
		if err := json.Unmarshal(data, &existingInfos); err != nil {
			log.Printf("Failed to unmarshal deploy file %s: %v", fileName, err)
			http.Error(w, "Internal server error", http.StatusInternalServerError)
			return
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

		// Store merged data using storage package
		storage.UpdateDeploymentInfo(cfg, existingInfos)
		log.Printf("Processed report with %d deployment infos", len(infos))

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
		log.Printf("Received completion for task %s", req.TaskKey)

		w.WriteHeader(http.StatusOK)
	}
}

func getClientIP(r *http.Request) string {
	if forwarded := r.Header.Get("X-Forwarded-For"); forwarded != "" {
		ips := strings.Split(forwarded, ",")
		return strings.TrimSpace(ips[0])
	}
	ip, _, _ := net.SplitHostPort(r.RemoteAddr)
	return ip
}