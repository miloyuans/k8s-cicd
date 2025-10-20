package main

import (
	"fmt"
	"k8s-cicd/internal/api"
	"k8s-cicd/internal/config"
	"k8s-cicd/internal/telegram"
	"log"
	"net/http"
)

// main 是程序的入口函数，负责初始化配置、Telegram 机器人和 HTTP 服务
func main() {
	// 初始化配置
	cfg, err := config.LoadConfig()
	if err != nil {
		log.Fatalf("加载配置失败: %v", err)
	}

	// 初始化 Telegram 机器人
	bot, err := telegram.NewBot(cfg.TelegramToken, cfg.TelegramGroupID)
	if err != nil {
		log.Fatalf("初始化 Telegram 机器人失败: %v", err)
	}

	// 启动 Telegram 机器人处理
	go bot.Start()

	// 初始化 API 服务
	apiServer := api.NewServer(cfg.RedisAddr, cfg)

	// 启动 HTTP 服务
	addr := fmt.Sprintf(":%d", cfg.Port)
	log.Printf("启动 HTTP 服务于 %s", addr)
	if err := http.ListenAndServe(addr, apiServer.Router); err != nil {
		log.Fatalf("HTTP 服务启动失败: %v", err)
	}
}