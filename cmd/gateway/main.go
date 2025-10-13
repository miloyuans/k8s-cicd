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
	log.Printf("Gateway server starting on %s", cfg.GatewayListenAddr)
	log.Fatal(http.ListenAndServe(cfg.GatewayListenAddr, nil))
}

var (
	taskQueue sync.Map // map[string][]dialog.DeployRequest
)

func handleTasks(cfg *config.Config) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		clientIP := getClientIP(r)
		// Only restrict GET requests from k8s-cicd, allow POST without restriction
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

			// Log interaction on gateway side
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

func getClientIP(r *http.Request) string {
	if forwarded := r.Header.Get("X-Forwarded-For"); forwarded != "" {
		ips := strings.Split(forwarded, ",")
		return strings.TrimSpace(ips[0])
	}
	ip, _, _ := net.SplitHostPort(r.RemoteAddr)
	return ip
}