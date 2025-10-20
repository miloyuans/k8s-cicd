package config

import (
	"os"
)

// Config 定义程序的配置结构
type Config struct {
	TelegramToken string // Telegram 机器人 Token
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
		config.TelegramToken = "your-telegram-bot-token" // 默认值，需替换
	}
	if config.RedisAddr == "" {
		config.RedisAddr = "localhost:6379" // 默认 Redis 地址
	}

	return config, nil
}