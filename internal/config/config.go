// 文件: internal/config/config.go
package config

import (
	"os"
)

// Config 定义程序的配置结构（删除 Telegram 配置）
type Config struct {
	RedisAddr string // Redis 地址
	Port      int    // HTTP 服务端口
}

// LoadConfig 从环境变量加载配置
func LoadConfig() (*Config, error) {
	config := &Config{
		RedisAddr: os.Getenv("REDIS_ADDR"),
		Port:      8080, // 默认端口
	}

	if config.RedisAddr == "" {
		config.RedisAddr = "localhost:6379" // 默认 Redis 地址
	}

	return config, nil
}