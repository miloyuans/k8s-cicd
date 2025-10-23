package main

import (
	"fmt"
	"k8s-cicd/internal/api"
	"k8s-cicd/internal/config"
	"k8s-cicd/internal/storage"
	"log"
	"net/http"
)

// *** 修复：从 main 移除 globalStorage 定义，由 api 包统一管理 ***
func main() {
	// 1. 初始化配置
	cfg, err := config.LoadConfig()
	if err != nil {
		log.Fatalf("加载配置失败: %v", err)
	}

	// 2. 初始化全局 Redis（传递给 api 包）
	redisStorage, err := storage.NewRedisStorage(cfg.RedisAddr)
	if err != nil {
		log.Fatalf("初始化 Redis 失败: %v", err)
	}
	
	// *** 修复：显式传递给 api 初始化 ***
	log.Println("✅ 全局 Redis 初始化完成")

	// 3. 初始化 API 服务（传入 Redis 实例）
	apiServer := api.NewServer(redisStorage, cfg)

	// 4. 启动 HTTP 服务
	addr := fmt.Sprintf(":%d", cfg.Port)
	log.Printf("🚀 启动 HTTP 服务于 %s", addr)
	if err := http.ListenAndServe(addr, apiServer.Router); err != nil {
		log.Fatalf("HTTP 服务启动失败: %v", err)
	}
}