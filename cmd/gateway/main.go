package main

import (
	"fmt"
	"k8s-cicd/internal/api"
	"k8s-cicd/internal/config"
	"log"
	"net/http"
)

// main 是程序的入口函数，负责初始化配置和 HTTP 服务
func main() {
	// 初始化配置
	cfg, err := config.LoadConfig()
	if err != nil {
		log.Fatalf("加载配置失败: %v", err)
	}

	// 初始化 API 服务（支持并发）
	apiServer := api.NewServer(cfg.RedisAddr, cfg)

	// 启动 HTTP 服务
	addr := fmt.Sprintf(":%d", cfg.Port)
	log.Printf("启动 HTTP 服务于 %s", addr)
	if err := http.ListenAndServe(addr, apiServer.Router); err != nil {
		log.Fatalf("HTTP 服务启动失败: %v", err)
	}
}