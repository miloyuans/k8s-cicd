package storage

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/yourusername/k8s-cicd/internal/config"
	"github.com/yourusername/k8s-cicd/internal/k8s"
)

type DeploymentInfo struct {
	Service   string    `json:"service"`
	Env       string    `json:"env"`
	Image     string    `json:"image"`
	Timestamp time.Time `json:"timestamp"`
	Status    string    `json:"status"`
}

type TelegramMessage struct {
	UserID    int64     `json:"user_id"`
	ChatID    int64     `json:"chat_id"`
	Content   string    `json:"content"`
	Timestamp time.Time `json:"timestamp"`
}

func EnsureStorageDir() error {
	return os.MkdirAll("storage", 0755)
}

func DailyMaintenance(cfg *config.Config, k8sClient *k8s.Client) {
	ticker := time.NewTicker(24 * time.Hour)
	defer ticker.Stop()

	for range ticker.C {
		now := time.Now()
		fileName := getDailyFileName(now, "deploy")
		if _, err := os.Stat(fileName); os.IsNotExist(err) {
			initDailyFile(fileName, k8sClient, cfg)
		}
		cleanupOldFiles("deploy")
	}
}

func DailyMaintenanceGateway(cfg *config.Config) {
	ticker := time.NewTicker(24 * time.Hour)
	defer ticker.Stop()

	for range ticker.C {
		now := time.Now()
		fileName := getDailyFileName(now, "telegram")
		if _, err := os.Stat(fileName); os.IsNotExist(err) {
			os.WriteFile(fileName, []byte("[]"), 0644)
		}
		fileName = getDailyFileName(now, "interaction")
		if _, err := os.Stat(fileName); os.IsNotExist(err) {
			os.WriteFile(fileName, []byte("[]"), 0644)
		}
		cleanupOldFiles("telegram")
		cleanupOldFiles("interaction")
	}
}

func getDailyFileName(t time.Time, prefix string) string {
	return filepath.Join("storage", fmt.Sprintf("%s_%s.json", prefix, t.Format("2006-01-02")))
}

func initDailyFile(fileName string, k8sClient *k8s.Client, cfg *config.Config) {
	deployments, err := k8sClient.client.Resource(schema.GroupVersionResource{
		Group:    "apps",
		Version:  "v1",
		Resource: "deployments",
	}).Namespace(cfg.Namespace).List(context.Background(), metav1.ListOptions{})
	if err != nil {
		log.Printf("Failed to list deployments: %v", err)
		return
	}

	infos := []DeploymentInfo{}
	for _, dep := range deployments.Items {
		service := dep.GetName()
		containers, _, _ := unstructured.NestedSlice(dep.Object, "spec", "template", "spec", "containers")
		if len(containers) > 0 {
			container := containers[0].(map[string]interface{})
			image, _, _ := unstructured.NestedString(container, "image")
			infos = append(infos, DeploymentInfo{
				Service:   service,
				Env:       "prod",
				Image:     image,
				Timestamp: time.Now(),
				Status:    "current",
			})
		}
	}

	data, _ := json.MarshalIndent(inos, "", "  ")
	os.WriteFile(fileName, data, 0644)
	log.Printf("Initialized daily file: %s", fileName)
}

func cleanupOldFiles(prefix string) {
	files, err := os.ReadDir("storage")
	if err != nil {
		return
	}

	now := time.Now()
	for _, file := range files {
		if !file.IsDir() && strings.HasPrefix(file.Name(), prefix+"_") {
			dateStr := strings.TrimSuffix(strings.TrimPrefix(file.Name(), prefix+"_"), ".json")
			fileDate, err := time.Parse("2006-01-02", dateStr)
			if err == nil && now.Sub(fileDate) > 30*24*time.Hour {
				os.Remove(filepath.Join("storage", file.Name()))
			}
		}
	}
}

func PersistDeployment(cfg *config.Config, service, env, image, status string) {
	fileName := getDailyFileName(time.Now(), "deploy")
	data, _ := os.ReadFile(fileName)
	var infos []DeploymentInfo
	json.Unmarshal(data, &infos)

	found := false
	for i := range infos {
		if infos[i].Service == service {
			infos[i] = DeploymentInfo{
				Service:   service,
				Env:       env,
				Image:     image,
				Timestamp: time.Now(),
				Status:    status,
			}
			found = true
			break
		}
	}
	if !found && image != "" {
		infos = append(infos, DeploymentInfo{
			Service:   service,
			Env:       env,
			Image:     image,
			Timestamp: time.Now(),
			Status:    status,
		})
	}

	newData, _ := json.MarshalIndent(infos, "", "  ")
	os.WriteFile(fileName, newData, 0644)
}

func PersistTelegramMessage(cfg *config.Config, msg TelegramMessage) {
	fileName := getDailyFileName(time.Now(), "telegram")
	data, _ := os.ReadFile(fileName)
	var messages []TelegramMessage
	json.Unmarshal(data, &messages)

	messages = append(messages, msg)
	newData, _ := json.MarshalIndent(messages, "", "  ")
	os.WriteFile(fileName, newData, 0644)
}

func LogInteraction(cfg *config.Config, logEntry map[string]interface{}) {
	fileName := getDailyFileName(time.Now(), "interaction")
	data, _ := os.ReadFile(fileName)
	var logs []map[string]interface{}
	json.Unmarshal(data, &logs)

	logs = append(logs, logEntry)
	newData, _ := json.MarshalIndent(logs, "", "  ")
	os.WriteFile(fileName, newData, 0644)
}