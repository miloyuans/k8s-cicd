// pusher.go
//pusher.go (api.go)
package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"k8s-cicd/agent/config"
	"k8s-cicd/agent/models"

	"github.com/fatih/color"
	"github.com/sirupsen/logrus"
)

// APIClient 统一API客户端
type APIClient struct {
	cfg     *config.APIConfig // API配置
	client  *http.Client      // HTTP客户端
	baseURL string            // 基础URL
}

// NewAPIClient 创建API客户端
func NewAPIClient(cfg *config.APIConfig) *APIClient {
	startTime := time.Now()
	// 步骤1：初始化客户端结构
	client := &APIClient{
		cfg:     cfg,
		client:  &http.Client{Timeout: 10 * time.Second},
		baseURL: cfg.BaseURL,
	}
	logrus.WithFields(logrus.Fields{
		"time":   time.Now().Format("2006-01-02 15:04:05"),
		"method": "NewAPIClient",
		"took":   time.Since(startTime),
	}).Info(color.GreenString("API客户端创建成功"))
	return client
}

// PushData POST /push - 推送K8s发现的数据
func (c *APIClient) PushData(req models.PushRequest) error {
	startTime := time.Now()
	// 步骤1：序列化请求数据
	jsonData, err := json.Marshal(req)
	if err != nil {
		logrus.WithFields(logrus.Fields{
			"time":   time.Now().Format("2006-01-02 15:04:05"),
			"method": "PushData",
			"took":   time.Since(startTime),
		}).Errorf(color.RedString("推送失败: JSON序列化错误: %v", err))
		return fmt.Errorf("JSON序列化失败: %v", err)
	}

	// 步骤2：发送POST请求
	resp, err := c.client.Post(c.baseURL+"/push", "application/json", bytes.NewBuffer(jsonData))
	if err != nil {
		logrus.WithFields(logrus.Fields{
			"time":   time.Now().Format("2006-01-02 15:04:05"),
			"method": "PushData",
			"took":   time.Since(startTime),
		}).Errorf(color.RedString("推送失败: HTTP请求错误: %v", err))
		return err
	}
	defer resp.Body.Close()

	// 步骤3：读取响应体
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		logrus.WithFields(logrus.Fields{
			"time":   time.Now().Format("2006-01-02 15:04:05"),
			"method": "PushData",
			"took":   time.Since(startTime),
		}).Errorf(color.RedString("推送失败: 读取响应错误: %v", err))
		return err
	}

	// 步骤4：检查响应状态
	if resp.StatusCode != http.StatusOK {
		logrus.WithFields(logrus.Fields{
			"time":   time.Now().Format("2006-01-02 15:04:05"),
			"method": "PushData",
			"took":   time.Since(startTime),
			"data": logrus.Fields{
				"status": resp.StatusCode,
				"body":   string(body),
			},
		}).Errorf(color.RedString("推送失败: HTTP错误: %d", resp.StatusCode))
		return fmt.Errorf("HTTP错误: %d - %s", resp.StatusCode, string(body))
	}

	// 步骤5：总结推送结果
	uniqueServices := make(map[string]struct{})
	for _, s := range req.Services {
		uniqueServices[s] = struct{}{}
	}
	uniqueEnvs := make(map[string]struct{})
	for _, e := range req.Environments {
		uniqueEnvs[e] = struct{}{}
	}
	logrus.WithFields(logrus.Fields{
		"time":   time.Now().Format("2006-01-02 15:04:05"),
		"method": "PushData",
		"took":   time.Since(startTime),
		"data": logrus.Fields{
			"services":      uniqueServices,
			"service_count": len(uniqueServices),
			"environments":  uniqueEnvs,
			"env_count":     len(uniqueEnvs),
		},
	}).Infof(color.GreenString("推送 /push 接口成功，完成数据交互，服务数: %d, 环境数: %d", len(uniqueServices), len(uniqueEnvs)))
	return nil
}

// UpdateStatus POST /status - 更新部署状态
func (c *APIClient) UpdateStatus(req models.StatusRequest) error {
	startTime := time.Now()
	// 步骤1：序列化请求数据
	jsonData, err := json.Marshal(req)
	if err != nil {
		logrus.WithFields(logrus.Fields{
			"time":   time.Now().Format("2006-01-02 15:04:05"),
			"method": "UpdateStatus",
			"took":   time.Since(startTime),
		}).Errorf(color.RedString("状态更新失败: JSON序列化错误: %v", err))
		return fmt.Errorf("JSON序列化失败: %v", err)
	}

	// 步骤2：发送POST请求
	resp, err := c.client.Post(c.baseURL+"/status", "application/json", bytes.NewBuffer(jsonData))
	if err != nil {
		logrus.WithFields(logrus.Fields{
			"time":   time.Now().Format("2006-01-02 15:04:05"),
			"method": "UpdateStatus",
			"took":   time.Since(startTime),
		}).Errorf(color.RedString("状态更新失败: HTTP请求错误: %v", err))
		return err
	}
	defer resp.Body.Close()

	// 步骤3：读取响应体
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		logrus.WithFields(logrus.Fields{
			"time":   time.Now().Format("2006-01-02 15:04:05"),
			"method": "UpdateStatus",
			"took":   time.Since(startTime),
		}).Errorf(color.RedString("状态更新失败: 读取响应错误: %v", err))
		return err
	}

	// 步骤4：检查响应状态
	if resp.StatusCode != http.StatusOK {
		logrus.WithFields(logrus.Fields{
			"time":   time.Now().Format("2006-01-02 15:04:05"),
			"method": "UpdateStatus",
			"took":   time.Since(startTime),
			"data": logrus.Fields{
				"status": resp.StatusCode,
				"body":   string(body),
				"request": req,
			},
		}).Errorf(color.RedString("状态更新失败: HTTP错误: %d", resp.StatusCode))
		return fmt.Errorf("HTTP错误: %d - %s", resp.StatusCode, string(body))
	}

	// 步骤5：总结状态更新结果
	logrus.WithFields(logrus.Fields{
		"time":   time.Now().Format("2006-01-02 15:04:05"),
		"method": "UpdateStatus",
		"took":   time.Since(startTime),
		"data": logrus.Fields{
			"service":     req.Service,
			"environment": req.Environment,
			"version":     req.Version,
			"status":      req.Status,
			"user":        req.User,
		},
	}).Infof(color.GreenString("状态更新 /status 接口成功"))
	return nil
}