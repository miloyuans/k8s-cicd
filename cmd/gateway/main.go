package main

import (
	"fmt"
	"k8s-cicd/internal/api"
	"k8s-cicd/internal/config"
	"k8s-cicd/internal/storage"
	"log"
	"net/http"
)

// *** 修复：全局存储实例 ***
var globalStorage *storage.RedisStorage

func main() {
	// 1. 初始化配置
	cfg, err := config.LoadConfig()
	if err != nil {
		log.Fatalf("加载配置失败: %v", err)
	}

	// 2. *** 修复：先初始化全局 Redis ***
	globalStorage, err = storage.NewRedisStorage(cfg.RedisAddr)
	if err != nil {
		log.Fatalf("初始化 Redis 失败: %v", err)
	}
	log.Println("✅ 全局 Redis 初始化完成")

	// 3. *** 修复：再初始化 API 服务（现在 globalStorage 已就绪）***
	apiServer := api.NewServer(cfg.RedisAddr, cfg)

	// 4. 启动 HTTP 服务
	addr := fmt.Sprintf(":%d", cfg.Port)
	log.Printf("🚀 启动 HTTP 服务于 %s", addr)
	if err := http.ListenAndServe(addr, apiServer.Router); err != nil {
		log.Fatalf("HTTP 服务启动失败: %v", err)
	}
}