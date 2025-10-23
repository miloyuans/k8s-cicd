// 文件: internal/config/config.go
package config

import (
	"os"
	"strconv"
)

// Config 定义程序的配置结构
type Config struct {
	MongoURI string // MongoDB 连接 URI
	Port     int    // HTTP 服务端口
}

// LoadConfig 从环境变量加载配置
func LoadConfig() (*Config, error) {
	config := &Config{
		MongoURI: os.Getenv("MONGO_URI"),
		Port:     8080, // 默认端口
	}

	if config.MongoURI == "" {
		config.MongoURI = "mongodb://localhost:27017" // 默认 MongoDB URI
	}

	// 如果环境变量有端口，覆盖默认值
	if portStr := os.Getenv("PORT"); portStr != "" {
		port, err := strconv.Atoi(portStr)
		if err != nil {
			return nil, err
		}
		config.Port = port
	}

	return config, nil
}