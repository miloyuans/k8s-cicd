package storage

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"k8s-cicd/internal/config"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
)

var storageMutex sync.Mutex

type DeploymentInfo struct {
	Service   string    `json:"service"`
	Env       string    `json:"env"`
	Image     string    `json:"image"`
	Timestamp time.Time `json:"timestamp"`
	Status    string    `json:"status"`
}

type DeployRequest struct {
	Service   string    `json:"service"`
	Env       string    `json:"env"`
	Version   string    `json:"version"`
	Timestamp time.Time `json:"timestamp"`
}

type DeployResult struct {
	Request   DeployRequest
	Success   bool
	ErrorMsg  string
	OldImage  string
	Events    string
	Logs      string
	Envs      map[string]string
}

type TelegramMessage struct {
	UserID    int64     `json:"user_id"`
	ChatID    int64     `json:"chat_id"`
	Content   string    `json:"content"`
	Timestamp time.Time `json:"timestamp"`
}

func EnsureStorageDir() error {
	if _, err := os.Stat("storage"); err == nil {
		fmt.Printf("Using existing storage directory: storage\n")
		return nil
	}
	if err := os.MkdirAll("storage", 0755); err != nil {
		fmt.Printf("Failed to create storage directory: %v\n", err)
		return err
	}
	fmt.Printf("Initialized storage directory: storage\n")
	return nil
}

func InitAllDailyFiles(cfg *config.Config, client dynamic.Interface) {
	storageMutex.Lock()
	defer storageMutex.Unlock()

	now := time.Now()
	// Initialize deploy file
	deployFile := getDailyFileName(now, "deploy")
	if _, err := os.Stat(deployFile); os.IsNotExist(err) {
		InitDailyFile(deployFile, client, cfg)
	} else {
		fmt.Printf("Using existing deploy file: %s\n", deployFile)
	}

	// Initialize telegram file
	telegramFile := getDailyFileName(now, "telegram")
	if _, err := os.Stat(telegramFile); os.IsNotExist(err) {
		if err := os.WriteFile(telegramFile, []byte("[]"), 0644); err != nil {
			fmt.Printf("Failed to initialize telegram log file %s: %v\n", telegramFile, err)
		} else {
			fmt.Printf("Initialized empty telegram log file: %s\n", telegramFile)
		}
	} else {
		fmt.Printf("Using existing telegram log file: %s\n", telegramFile)
	}

	// Initialize interaction file
	interactionFile := getDailyFileName(now, "interaction")
	if _, err := os.Stat(interactionFile); os.IsNotExist(err) {
		if err := os.WriteFile(interactionFile, []byte("[]"), 0644); err != nil {
			fmt.Printf("Failed to initialize interaction log file %s: %v\n", interactionFile, err)
		} else {
			fmt.Printf("Initialized empty interaction log file: %s\n", interactionFile)
		}
	} else {
		fmt.Printf("Using existing interaction log file: %s\n", interactionFile)
	}
}

func UpdateAllDeploymentVersions(cfg *config.Config, client dynamic.Interface) {
	storageMutex.Lock()
	defer storageMutex.Unlock()

	now := time.Now()
	fileName := getDailyFileName(now, "deploy")
	data, err := os.ReadFile(fileName)
	if err != nil {
		fmt.Printf("Failed to read deploy file %s: %v\n", fileName, err)
		return
	}
	var infos []DeploymentInfo
	if err := json.Unmarshal(data, &infos); err != nil {
		fmt.Printf("Failed to unmarshal deploy file %s: %v\n", fileName, err)
		return
	}

	deployments, err := client.Resource(schema.GroupVersionResource{
		Group:    "apps",
		Version:  "v1",
		Resource: "deployments",
	}).Namespace(cfg.Namespace).List(context.Background(), metav1.ListOptions{})
	if err != nil {
		fmt.Printf("Failed to list deployments for update: %v\n", err)
		return
	}

	updated := false
	for _, dep := range deployments.Items {
		service := dep.GetName()
		containers, _, _ := unstructured.NestedSlice(dep.Object, "spec", "template", "spec", "containers")
		if len(containers) == 0 {
			continue
		}
		container := containers[0].(map[string]interface{})
		image, _, _ := unstructured.NestedString(container, "image")
		found := false
		for i := range infos {
			if infos[i].Service == service {
				infos[i].Image = image
				infos[i].Timestamp = now
				infos[i].Status = "current"
				found = true
				break
			}
		}
		if !found {
			infos = append(infos, DeploymentInfo{
				Service:   service,
				Env:       "prod",
				Image:     image,
				Timestamp: now,
				Status:    "current",
			})
		}
		updated = true
	}

	if updated {
		newData, err := json.MarshalIndent(infos, "", "  ")
		if err != nil {
			fmt.Printf("Failed to marshal deploy file %s: %v\n", fileName, err)
			return
		}
		if err := os.WriteFile(fileName, newData, 0644); err != nil {
			fmt.Printf("Failed to write deploy file %s: %v\n", fileName, err)
			return
		}
		fmt.Printf("Updated deploy file %s with current deployment versions\n", fileName)
	}
}

func DailyMaintenance(cfg *config.Config, client dynamic.Interface) {
	ticker := time.NewTicker(24 * time.Hour)
	defer ticker.Stop()

	for range ticker.C {
		storageMutex.Lock()
		now := time.Now()
		fileName := getDailyFileName(now, "deploy")
		if _, err := os.Stat(fileName); os.IsNotExist(err) {
			InitDailyFile(fileName, client, cfg)
		} else {
			fmt.Printf("Using existing deploy file: %s\n", fileName)
		}
		cleanupOldFiles("deploy")
		storageMutex.Unlock()
	}
}

func DailyMaintenanceGateway(cfg *config.Config) {
	ticker := time.NewTicker(24 * time.Hour)
	defer ticker.Stop()

	for range ticker.C {
		storageMutex.Lock()
		now := time.Now()
		fileName := getDailyFileName(now, "telegram")
		if _, err := os.Stat(fileName); os.IsNotExist(err) {
			if err := os.WriteFile(fileName, []byte("[]"), 0644); err != nil {
				fmt.Printf("Failed to initialize telegram log file %s: %v\n", fileName, err)
			} else {
				fmt.Printf("Initialized empty telegram log file: %s\n", fileName)
			}
		} else {
			fmt.Printf("Using existing telegram log file: %s\n", fileName)
		}
		fileName = getDailyFileName(now, "interaction")
		if _, err := os.Stat(fileName); os.IsNotExist(err) {
			if err := os.WriteFile(fileName, []byte("[]"), 0644); err != nil {
				fmt.Printf("Failed to initialize interaction log file %s: %v\n", fileName, err)
			} else {
				fmt.Printf("Initialized empty interaction log file: %s\n", fileName)
			}
		} else {
			fmt.Printf("Using existing interaction log file: %s\n", fileName)
		}
		cleanupOldFiles("telegram")
		cleanupOldFiles("interaction")
		storageMutex.Unlock()
	}
}

func getDailyFileName(t time.Time, prefix string) string {
	return filepath.Join("storage", fmt.Sprintf("%s_%s.json", prefix, t.Format("2006-01-02")))
}

func InitDailyFile(fileName string, client dynamic.Interface, cfg *config.Config) {
	deployments, err := client.Resource(schema.GroupVersionResource{
		Group:    "apps",
		Version:  "v1",
		Resource: "deployments",
	}).Namespace(cfg.Namespace).List(context.Background(), metav1.ListOptions{})
	if err != nil {
		fmt.Printf("Failed to list deployments: %v\n", err)
		return
	}

	infos := []DeploymentInfo{}
	for _, dep := range deployments.Items {
		service := dep.GetName()
		containers, _, _ := unstructured.NestedSlice(dep.Object, "spec", "template", "spec", "containers")
		if len(containers) == 0 {
			continue
		}
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

	data, err := json.MarshalIndent(infos, "", "  ")
	if err != nil {
		fmt.Printf("Failed to marshal daily file %s: %v\n", fileName, err)
		return
	}
	if err := os.WriteFile(fileName, data, 0644); err != nil {
		fmt.Printf("Failed to write daily file %s: %v\n", fileName, err)
		return
	}
	fmt.Printf("Initialized daily file: %s\n", fileName)
}

func cleanupOldFiles(prefix string) {
	storageMutex.Lock()
	defer storageMutex.Unlock()

	files, err := os.ReadDir("storage")
	if err != nil {
		fmt.Printf("Failed to read storage directory: %v\n", err)
		return
	}

	now := time.Now()
	for _, file := range files {
		if !file.IsDir() && strings.HasPrefix(file.Name(), prefix+"_") {
			dateStr := strings.TrimSuffix(strings.TrimPrefix(file.Name(), prefix+"_"), ".json")
			fileDate, err := time.Parse("2006-01-02", dateStr)
			if err == nil && now.Sub(fileDate) > 30*24*time.Hour {
				if err := os.Remove(filepath.Join("storage", file.Name())); err != nil {
					fmt.Printf("Failed to remove old file %s: %v\n", file.Name(), err)
				} else {
					fmt.Printf("Removed old file: %s\n", file.Name())
				}
			}
		}
	}
}

func PersistDeployment(cfg *config.Config, service, env, image, status string) {
	storageMutex.Lock()
	defer storageMutex.Unlock()

	fileName := getDailyFileName(time.Now(), "deploy")
	data, err := os.ReadFile(fileName)
	if err != nil {
		fmt.Printf("Failed to read deploy file %s: %v\n", fileName, err)
		return
	}
	var infos []DeploymentInfo
	if err := json.Unmarshal(data, &infos); err != nil {
		fmt.Printf("Failed to unmarshal deploy file %s: %v\n", fileName, err)
		return
	}

	found := false
	for i := range infos {
		if infos[i].Service == service && infos[i].Env == env {
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

	newData, err := json.MarshalIndent(infos, "", "  ")
	if err != nil {
		fmt.Printf("Failed to marshal deploy file %s: %v\n", fileName, err)
		return
	}
	if err := os.WriteFile(fileName, newData, 0644); err != nil {
		fmt.Printf("Failed to write deploy file %s: %v\n", fileName, err)
	}
}

func PersistTelegramMessage(cfg *config.Config, msg TelegramMessage) {
	storageMutex.Lock()
	defer storageMutex.Unlock()

	fileName := getDailyFileName(time.Now(), "telegram")
	data, err := os.ReadFile(fileName)
	if err != nil {
		fmt.Printf("Failed to read telegram file %s: %v\n", fileName, err)
		return
	}
	var messages []TelegramMessage
	if err := json.Unmarshal(data, &messages); err != nil {
		fmt.Printf("Failed to unmarshal telegram file %s: %v\n", fileName, err)
		return
	}

	messages = append(messages, msg)
	newData, err := json.MarshalIndent(messages, "", "  ")
	if err != nil {
		fmt.Printf("Failed to marshal telegram file %s: %v\n", fileName, err)
		return
	}
	if err := os.WriteFile(fileName, newData, 0644); err != nil {
		fmt.Printf("Failed to write telegram file %s: %v\n", fileName, err)
	}
}

func PersistInteraction(cfg *config.Config, logEntry map[string]interface{}) {
	storageMutex.Lock()
	defer storageMutex.Unlock()

	fileName := getDailyFileName(time.Now(), "interaction")
	data, err := os.ReadFile(fileName)
	if err != nil {
		fmt.Printf("Failed to read interaction file %s: %v\n", fileName, err)
		return
	}
	var logs []map[string]interface{}
	if err := json.Unmarshal(data, &logs); err != nil {
		fmt.Printf("Failed to unmarshal interaction file %s: %v\n", fileName, err)
		return
	}

	logs = append(logs, logEntry)
	newData, err := json.MarshalIndent(logs, "", "  ")
	if err != nil {
		fmt.Printf("Failed to marshal interaction file %s: %v\n", fileName, err)
		return
	}
	if err := os.WriteFile(fileName, newData, 0644); err != nil {
		fmt.Printf("Failed to write interaction file %s: %v\n", fileName, err)
	}
}