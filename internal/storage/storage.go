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
	UserName  string    `json:"username"`
}

type DeployRequest struct {
	Service   string    `json:"service"`
	Env       string    `json:"env"`
	Version   string    `json:"version"`
	Timestamp time.Time `json:"timestamp"`
	UserName  string    `json:"username"`
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

func EnsureStorageDir(storageDir string) error {
	if _, err := os.Stat(storageDir); err == nil {
		return nil
	}
	if err := os.MkdirAll(storageDir, 0755); err != nil {
		fmt.Printf("Failed to create storage directory: %v\n", err)
		return err
	}
	return nil
}

func GetDailyFileName(now time.Time, prefix, storageDir string) string {
	return filepath.Join(storageDir, fmt.Sprintf("%s_%s.json", prefix, now.Format("2006-01-02")))
}

func EnsureDailyFile(fileName string, client dynamic.Interface, cfg *config.Config) error {
	if _, err := os.Stat(fileName); os.IsNotExist(err) {
		if prefix := strings.TrimSuffix(filepath.Base(fileName), filepath.Ext(fileName)); prefix == "deploy_"+time.Now().Format("2006-01-02") && client != nil {
			return InitDailyFile(fileName, client, cfg)
		} else {
			if err := os.WriteFile(fileName, []byte("[]"), 0644); err != nil {
				fmt.Printf("Failed to initialize file %s: %v\n", fileName, err)
				return err
			}
		}
	} else if err != nil {
		fmt.Printf("Failed to check file %s: %v\n", fileName, err)
		return err
	}
	return nil
}

func InitAllDailyFiles(cfg *config.Config, client dynamic.Interface) {
	now := time.Now()
	// Initialize deploy file
	deployFile := GetDailyFileName(now, "deploy", cfg.StorageDir)
	if err := EnsureDailyFile(deployFile, client, cfg); err != nil {
		fmt.Printf("Failed to ensure deploy file %s: %v\n", deployFile, err)
	}

	// Initialize telegram file
	telegramFile := GetDailyFileName(now, "telegram", cfg.StorageDir)
	if err := EnsureDailyFile(telegramFile, nil, cfg); err != nil {
		fmt.Printf("Failed to ensure telegram file %s: %v\n", telegramFile, err)
	}

	// Initialize interaction file
	interactionFile := GetDailyFileName(now, "interaction", cfg.StorageDir)
	if err := EnsureDailyFile(interactionFile, nil, cfg); err != nil {
		fmt.Printf("Failed to ensure interaction file %s: %v\n", interactionFile, err)
	}
}

func UpdateAllDeploymentVersions(cfg *config.Config, client dynamic.Interface) {
	fileName := GetDailyFileName(time.Now(), "deploy", cfg.StorageDir)
	if err := EnsureDailyFile(fileName, client, cfg); err != nil {
		fmt.Printf("Failed to ensure deploy file %s: %v\n", fileName, err)
		return
	}

	storageMutex.Lock()
	defer storageMutex.Unlock()

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

	updated := false
	for env, namespace := range cfg.Environments {
		deployments, err := client.Resource(schema.GroupVersionResource{
			Group:    "apps",
			Version:  "v1",
			Resource: "deployments",
		}).Namespace(namespace).List(context.Background(), metav1.ListOptions{})
		if err != nil {
			fmt.Printf("Failed to list deployments for env %s namespace %s: %v\n", env, namespace, err)
			continue
		}

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
				if infos[i].Service == service && infos[i].Env == env {
					infos[i].Image = image
					infos[i].Timestamp = time.Now()
					infos[i].Status = "current"
					found = true
					break
				}
			}
			if !found {
				infos = append(infos, DeploymentInfo{
					Service:   service,
					Env:       env,
					Image:     image,
					Timestamp: time.Now(),
					Status:    "current",
					UserName:  "",
				})
				updated = true
			}
		}
	}

	if updated {
		newData, err := json.MarshalIndent(infos, "", "  ")
		if err != nil {
			fmt.Printf("Failed to marshal deploy file %s: %v\n", fileName, err)
			return
		}
		if err := os.WriteFile(fileName, newData, 0644); err != nil {
			fmt.Printf("Failed to write deploy file %s: %v\n", fileName, err)
		}
	}
}

func InitDailyFile(fileName string, client dynamic.Interface, cfg *config.Config) error {
	var infos []DeploymentInfo
	for env, namespace := range cfg.Environments {
		deployments, err := client.Resource(schema.GroupVersionResource{
			Group:    "apps",
			Version:  "v1",
			Resource: "deployments",
		}).Namespace(namespace).List(context.Background(), metav1.ListOptions{})
		if err != nil {
			fmt.Printf("Failed to list deployments for env %s namespace %s: %v\n", env, namespace, err)
			continue
		}

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
				Env:       env,
				Image:     image,
				Timestamp: time.Now(),
				Status:    "current",
				UserName:  "",
			})
		}
	}

	data, err := json.MarshalIndent(infos, "", "  ")
	if err != nil {
		fmt.Printf("Failed to marshal initial deploy file %s: %v\n", fileName, err)
		return err
	}
	if err := os.WriteFile(fileName, data, 0644); err != nil {
		fmt.Printf("Failed to write daily file %s: %v\n", fileName, err)
		return err
	}
	return nil
}

func DailyMaintenance(cfg *config.Config, client dynamic.Interface) {
	ticker := time.NewTicker(24 * time.Hour)
	defer ticker.Stop()

	for range ticker.C {
		InitAllDailyFiles(cfg, client)
		UpdateAllDeploymentVersions(cfg, client)
		cleanupOldFiles("deploy", cfg.StorageDir)
		cleanupOldFiles("telegram", cfg.StorageDir)
		cleanupOldFiles("interaction", cfg.StorageDir)
	}
}

func cleanupOldFiles(prefix, storageDir string) {
	storageMutex.Lock()
	defer storageMutex.Unlock()

	files, err := os.ReadDir(storageDir)
	if err != nil {
		fmt.Printf("Failed to read storage directory %s: %v\n", storageDir, err)
		return
	}

	now := time.Now()
	for _, file := range files {
		if !file.IsDir() && strings.HasPrefix(file.Name(), prefix+"_") {
			dateStr := strings.TrimSuffix(strings.TrimPrefix(file.Name(), prefix+"_"), ".json")
			fileDate, err := time.Parse("2006-01-02", dateStr)
			if err == nil && now.Sub(fileDate) > 30*24*time.Hour {
				if err := os.Remove(filepath.Join(storageDir, file.Name())); err != nil {
					fmt.Printf("Failed to remove old file %s: %v\n", file.Name(), err)
				}
			}
		}
	}
}

func PersistDeployment(cfg *config.Config, service, env, image, status, userName string) {
	fileName := GetDailyFileName(time.Now(), "deploy", cfg.StorageDir)
	if err := EnsureDailyFile(fileName, nil, cfg); err != nil {
		fmt.Printf("Failed to ensure deploy file %s: %v\n", fileName, err)
		return
	}

	storageMutex.Lock()
	defer storageMutex.Unlock()

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
			infos[i].Image = image
			infos[i].Timestamp = time.Now()
			infos[i].Status = status
			infos[i].UserName = userName
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
			UserName:  userName,
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

func PersistInteraction(cfg *config.Config, logEntry map[string]interface{}) {
	fileName := GetDailyFileName(time.Now(), "interaction", cfg.StorageDir)
	if err := EnsureDailyFile(fileName, nil, cfg); err != nil {
		fmt.Printf("Failed to ensure interaction file %s: %v\n", fileName, err)
		return
	}

	storageMutex.Lock()
	defer storageMutex.Unlock()

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

func UpdateDeploymentInfo(cfg *config.Config, infos []DeploymentInfo) {
	fileName := GetDailyFileName(time.Now(), "deploy", cfg.StorageDir)
	if err := EnsureDailyFile(fileName, nil, cfg); err != nil {
		fmt.Printf("Failed to ensure deploy file %s: %v\n", fileName, err)
		return
	}

	storageMutex.Lock()
	defer storageMutex.Unlock()

	newData, err := json.MarshalIndent(infos, "", "  ")
	if err != nil {
		fmt.Printf("Failed to marshal deploy file %s: %v\n", fileName, err)
		return
	}
	if err := os.WriteFile(fileName, newData, 0644); err != nil {
		fmt.Printf("Failed to write deploy file %s: %v\n", fileName, err)
	}
}