package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

type Config struct {
	GatewayURL         string            `yaml:"gateway_url"`
	PollInterval       int               `yaml:"poll_interval"`
	TelegramBots       map[string]string `yaml:"telegram_bots"`
	TelegramChats      map[string]int64  `yaml:"telegram_chats"`
	ServiceKeywords    map[string]string `yaml:"service_keywords"`
	KubeConfigPath     string            `yaml:"kube_config_path"`
	Namespace          string            `yaml:"namespace"`
	TimeoutSeconds     int               `yaml:"timeout_seconds"`
	MaxConcurrency     int               `yaml:"max_concurrency"`
	TriggerKeyword     string            `yaml:"trigger_keyword"`
	CancelKeyword      string            `yaml:"cancel_keyword"`
	InvalidResponses   []string          `yaml:"invalid_responses"`
	ServicesDir        string            `yaml:"services_dir"`
	Environments       []string          `yaml:"environments"`
	DialogTimeout      int               `yaml:"dialog_timeout"`
}

func LoadConfig(filePath string) *Config {
	data, err := os.ReadFile(filePath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to read config: %v\n", err)
		os.Exit(1)
	}
	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		fmt.Fprintf(os.Stderr, "Failed to unmarshal config: %v\n", err)
		os.Exit(1)
	}
	if cfg.Namespace == "" {
		cfg.Namespace = "default"
	}
	if cfg.TimeoutSeconds == 0 {
		cfg.TimeoutSeconds = 300
	}
	if cfg.MaxConcurrency == 0 {
		cfg.MaxConcurrency = 10
	}
	if cfg.PollInterval == 0 {
		cfg.PollInterval = 60
	}
	if cfg.ServicesDir == "" {
		cfg.ServicesDir = "services"
	}
	if cfg.DialogTimeout == 0 {
		cfg.DialogTimeout = 300
	}
	return &cfg
}

func LoadServiceLists(servicesDir string) (map[string][]string, error) {
	serviceLists := make(map[string][]string)
	files, err := os.ReadDir(servicesDir)
	if err != nil {
		return nil, err
	}

	for _, file := range files {
		if !file.IsDir() && strings.HasSuffix(file.Name(), ".svc.list") {
			serviceName := strings.TrimSuffix(file.Name(), ".svc.list")
			data, err := os.ReadFile(filepath.Join(servicesDir, file.Name()))
			if err != nil {
				fmt.Printf("Failed to read service list %s: %v\n", file.Name(), err)
				continue
			}
			lines := strings.Split(string(data), "\n")
			var services []string
			for _, line := range lines {
				if line = strings.TrimSpace(line); line != "" {
					services = append(services, line)
				}
			}
			serviceLists[serviceName] = services
		}
	}
	return serviceLists, nil
}