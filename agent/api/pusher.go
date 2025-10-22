package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"k8s-cicd/agent/config"
	"k8s-cicd/agent/models"

	"github.com/fatih/color"
	"github.com/sirupsen/logrus"
)

// APIPusher API推送器
type APIPusher struct {
	cfg     *config.APIConfig
	client  *http.Client
	baseURL string
}

// NewAPIPusher 创建API推送器
func NewAPIPusher(cfg *config.APIConfig) *APIPusher {
	return &APIPusher{
		cfg:     cfg,
		client:  &http.Client{Timeout: 10 * time.Second},
		baseURL: cfg.BaseURL,
	}
}

// Start 启动API推送循环
func (p *APIPusher) Start(deployments []models.DeploymentStatus) {
	green := color.New(color.FgGreen).SprintFunc()
	logrus.Infof("%s API推送器启动，间隔: %v", green("📤"), p.cfg.PushInterval)

	ticker := time.NewTicker(p.cfg.PushInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			p.pushDeployments(deployments)
		}
	}
}

// pushDeployments 推送部署状态（无限重试）
func (p *APIPusher) pushDeployments(deployments []models.DeploymentStatus) {
	var retries int
	for {
		// 步骤1：准备推送数据
		payload := map[string]interface{}{
			"deployments": deployments,
			"timestamp":   time.Now().Unix(),
		}

		// 步骤2：序列化JSON
		jsonData, err := json.Marshal(payload)
		if err != nil {
			logrus.Errorf("JSON序列化失败: %v", err)
			time.Sleep(p.cfg.PushInterval)
			continue
		}

		// 步骤3：发送HTTP请求
		resp, err := p.client.Post(p.baseURL+"/api/deployments/status", 
			"application/json", bytes.NewBuffer(jsonData))
		
		if err == nil && resp.StatusCode == http.StatusOK {
			// 推送成功
			green := color.New(color.FgGreen)
			green.Printf("✅ API推送成功: %d 条部署状态\n", len(deployments))
			return
		}

		// 推送失败，重试
		retries++
		red := color.New(color.FgRed)
		red.Printf("❌ API推送失败 [%d]: %v，%v后重试\n", 
			retries, err, p.cfg.PushInterval)
		
		time.Sleep(p.cfg.PushInterval)
		
		// 如果MaxRetries > 0，达到上限则退出
		if p.cfg.MaxRetries > 0 && retries >= p.cfg.MaxRetries {
			logrus.Fatalf("API推送达到最大重试次数: %d", p.cfg.MaxRetries)
		}
	}
}