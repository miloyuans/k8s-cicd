// config.go
package config

import (
	"fmt"
	"log" // Added import for log package
	"net"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

type Config struct {
	GatewayURL         string              `yaml:"gateway_url"`
	GatewayListenAddr  string              `yaml:"gateway_listen_addr"`
	AllowedIPs         []string            `yaml:"allowed_ips"`
	PollInterval       int                 `yaml:"poll_interval"`
	TelegramBots       map[string]string   `yaml:"telegram_bots"`
	TelegramChats      map[string]int64    `yaml:"telegram_chats"`
	ServiceKeywords    map[string][]string `yaml:"service_keywords"` // keyword: list of patterns
	KubeConfigPath     string              `yaml:"kube_config_path"`
	Namespace          string              `yaml:"namespace"`
	TimeoutSeconds     int                 `yaml:"timeout_seconds"`
	MaxConcurrency     int                 `yaml:"max_concurrency"`
	TriggerKeywords    []string            `yaml:"trigger_keyword"`
	CancelKeywords     []string            `yaml:"cancel_keyword"`
	InvalidResponses   []string            `yaml:"invalid_responses"`
	ServicesDir        string              `yaml:"services_dir"`
	Environments       map[string]string   `yaml:"environments"`
	DialogTimeout      int                 `yaml:"dialog_timeout"`
	StorageDir         string              // Added for configurable storage directory
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
	// Normalize environment keys to lowercase
	for k, v := range cfg.Environments {
		delete(cfg.Environments, k)
		cfg.Environments[strings.ToLower(k)] = v
	}
	// Debug: Log loaded ServiceKeywords
	log.Printf("Loaded ServiceKeywords: %v", cfg.ServiceKeywords)
	if cfg.TimeoutSeconds == 0 {
		cfg.TimeoutSeconds = 300
	}
	if cfg.MaxConcurrency == 0 {
		cfg.MaxConcurrency = 10
	}
	if cfg.PollInterval == 0 {
		cfg.PollInterval = 10 // Reduced to 10s for faster polling
	}
	if cfg.ServicesDir == "" {
		cfg.ServicesDir = "services"
	}
	if cfg.DialogTimeout == 0 {
		cfg.DialogTimeout = 300
	}
	if len(cfg.TriggerKeywords) == 0 {
		cfg.TriggerKeywords = []string{"deploy"}
	}
	if len(cfg.CancelKeywords) == 0 {
		cfg.CancelKeywords = []string{"cancel"}
	}
	if cfg.GatewayListenAddr == "" {
		cfg.GatewayListenAddr = ":8081"
	}
	if cfg.StorageDir == "" {
		cfg.StorageDir = "storage" // Default, overridden by main.go
	}
	if len(cfg.Environments) == 0 {
		cfg.Environments = make(map[string]string)
	}
	return &cfg
}

func LoadServiceLists(servicesDir string, telegramBots map[string]string) (map[string][]string, error) {
	if _, err := os.Stat(servicesDir); err == nil {
	} else if err := os.MkdirAll(servicesDir, 0755); err != nil {
		return nil, fmt.Errorf("failed to create services directory %s: %v", servicesDir, err)
	}

	serviceLists := make(map[string][]string)
	for service := range telegramBots {
		filePath := filepath.Join(servicesDir, fmt.Sprintf("%s.svc.list", service))
		if _, err := os.Stat(filePath); os.IsNotExist(err) {
			if err := os.WriteFile(filePath, []byte(""), 0644); err != nil {
				fmt.Printf("Failed to create service list file %s: %v\n", filePath, err)
			}
		}

		data, err := os.ReadFile(filePath)
		if err != nil {
			fmt.Printf("Failed to read service list file %s: %v\n", filePath, err)
			continue
		}
		lines := strings.Split(string(data), "\n")
		var services []string
		for _, line := range lines {
			if line = strings.TrimSpace(line); line != "" {
				services = append(services, line)
			}
		}
		serviceLists[service] = services
	}
	return serviceLists, nil
}

func contains(slice []string, item string) bool {
	for _, s := range slice {
		if s == item {
			return true
		}
	}
	return false
}

func IsIPAllowed(clientIP string, allowedIPs []string) bool {
	if len(allowedIPs) == 0 {
		return true
	}

	clientAddr := net.ParseIP(clientIP)
	if clientAddr == nil {
		return false
	}

	for _, allowed := range allowedIPs {
		if strings.Contains(allowed, "/") {
			_, ipNet, err := net.ParseCIDR(allowed)
			if err != nil {
				fmt.Printf("Invalid CIDR in allowed_ips: %s\n", allowed)
				continue
			}
			if ipNet.Contains(clientAddr) {
				return true
			}
		} else {
			if allowed == clientIP {
				return true
			}
		}
	}
	return false
}