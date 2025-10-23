// 文件: internal/config/config.go
package config

import (
	"fmt"
	"os"
	"strconv"
)

// Config 定义程序的配置结构
type Config struct {
	TelegramToken string // Telegram 机器人 Token
	TelegramGroupID int64 // Telegram 群组 ID
	RedisAddr     string // Redis 地址
	Port          int    // HTTP 服务端口
}

// LoadConfig 从环境变量加载配置
func LoadConfig() (*Config, error) {
	config := &Config{
		TelegramToken: os.Getenv("TELEGRAM_TOKEN"),
		RedisAddr:     os.Getenv("REDIS_ADDR"),
		Port:          8080, // 默认端口
	}

	if config.TelegramToken == "" {
		return nil, fmt.Errorf("TELEGRAM_TOKEN 环境变量未设置")
	}

	groupIDStr := os.Getenv("TELEGRAM_GROUP_ID")
	if groupIDStr != "" {
		groupID, err := strconv.ParseInt(groupIDStr, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("解析 TELEGRAM_GROUP_ID 失败: %v", err)
		}
		config.TelegramGroupID = groupID
	}

	if config.RedisAddr == "" {
		config.RedisAddr = "localhost:6379" // 默认 Redis 地址
	}

	return config, nil
}