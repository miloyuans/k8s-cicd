package main

import (
	"encoding/json"
	"log"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"k8s-cicd/internal/config"
	"k8s-cicd/internal/dialog"
	"k8s-cicd/internal/storage"
	"k8s-cicd/internal/telegram"
	"os"
)

func main() {
	cfg := config.LoadConfig("config.yaml")
	cfg.StorageDir = "gateway_storage" // Set gateway-specific storage directory
	if err := storage.EnsureStorageDir(cfg.StorageDir); err != nil {
		log.Fatalf("Failed to create storage dir: %v", err)
	}

	// Initialize service lists
	if _, err := config.LoadServiceLists(cfg.ServicesDir, cfg.TelegramBots); err != nil {
		log.Printf("Failed to initialize service lists: %v", err)
	}

	go telegram.StartBot(cfg)

	http.HandleFunc("/tasks", handleTasks(cfg))
	http.HandleFunc("/report", handleReport(cfg))
	log.Printf("Gateway server starting on %s", cfg.GatewayListenAddr)
	log.Fatal(http.ListenAndServe(cfg.GatewayListenAddr, nil))
}

var (
	taskQueue sync.Map // map[string][]dialog.DeployRequest
)

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

			tasks := []dialog.DeployRequest{}
			taskQueue.Range(func(key, value interface{}) bool {
				taskList := value.([]dialog.DeployRequest)
				for _, task := range taskList {
					if task.Env == env {
						tasks = append(tasks, task)
					}
				}
				return true
			})

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