package main

import (
	"encoding/json"
	"log"
	"net/http"
	"sync"

	"k8s-cicd/internal/config"
	"k8s-cicd/internal/dialog"
	"k8s-cicd/internal/storage"
)

func main() {
	cfg := config.LoadConfig("config.yaml")
	if err := storage.EnsureStorageDir(); err != nil {
		log.Fatalf("Failed to create storage dir: %v", err)
	}

	go storage.DailyMaintenanceGateway(cfg)
	go telegram.StartBot(cfg)

	http.HandleFunc("/tasks", handleTasks)
	log.Println("Gateway server starting on :8081")
	log.Fatal(http.ListenAndServe(":8081", nil))
}

var (
	taskQueue sync.Map // map[string][]dialog.DeployRequest
)

func handleTasks(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		Env string `json:"env"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid JSON", http.StatusBadRequest)
		return
	}

	tasks := []dialog.DeployRequest{}
	taskQueue.Range(func(key, value interface{}) bool {
		taskList := value.([]dialog.DeployRequest)
		for _, task := range taskList {
			if task.Env == req.Env {
				tasks = append(tasks, task)
			}
		}
		return true
	})

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(tasks)

	// Log interaction
	storage.LogInteraction(cfg, map[string]interface{}{
		"endpoint": "/tasks",
		"env":      req.Env,
		"tasks":    len(tasks),
		"timestamp": time.Now().Format(time.RFC3339),
	})
}