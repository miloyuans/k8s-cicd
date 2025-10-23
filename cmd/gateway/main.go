package main

import (
	"fmt"
	"k8s-cicd/internal/api"
	"k8s-cicd/internal/config"
	"k8s-cicd/internal/storage"
	"log"
	"net/http"
)

var globalStorage *storage.RedisStorage // 全局存储

func main() {
	// 初始化配置
	cfg, err := config.LoadConfig()
	if err != nil {
		log.Fatalf("加载配置失败: %v", err)
	}

	// *** 修复：初始化全局 Redis 存储 ***
	globalStorage, err = storage.NewRedisStorage(cfg.RedisAddr)
	if err != nil {
		log.Fatalf("初始化 Redis 失败: %v", err)
	}

	// 初始化 API 服务
	apiServer := api.NewServer(cfg.RedisAddr, cfg)

	// 启动 HTTP 服务
	addr := fmt.Sprintf(":%d", cfg.Port)
	log.Printf("启动 HTTP 服务于 %s", addr)
	if err := http.ListenAndServe(addr, apiServer.Router); err != nil {
		log.Fatalf("HTTP 服务启动失败: %v", err)
	}
}