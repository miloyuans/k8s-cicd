// 文件: internal/config/config.go
package config

import (
	"os"
	"strconv"
)

// Config 定义程序的配置结构
type Config struct {
	MongoURI       string // MongoDB 连接 URI
	Port           int    // HTTP 服务端口
	TTLH           int    // 部署队列TTL小时，默认24
	TelegramToken  string // Telegram Bot Token
	TelegramChatID int64  // Telegram Chat ID
}

// LoadConfig 从环境变量加载配置
func LoadConfig() (*Config, error) {
	config := &Config{
		MongoURI:      os.Getenv("MONGO_URI"),
		Port:          8080,
		TTLH:          24,
		TelegramToken: os.Getenv("TELEGRAM_TOKEN"),
	}

	if config.MongoURI == "" {
		config.MongoURI = "mongodb://localhost:27017"
	}

	if portStr := os.Getenv("PORT"); portStr != "" {
		port, err := strconv.Atoi(portStr)
		if err != nil {
			return nil, err
		}
		config.Port = port
	}

	if ttlStr := os.Getenv("TTL_HOURS"); ttlStr != "" {
		ttl, err := strconv.Atoi(ttlStr)
		if err != nil {
			return nil, err
		}
		config.TTLH = ttl
	}

	if chatStr := os.Getenv("TELEGRAM_CHAT_ID"); chatStr != "" {
		id, err := strconv.ParseInt(chatStr, 10, 64)
		if err != nil {
			return nil, err
		}
		config.TelegramChatID = id
	}

	return config, nil
}