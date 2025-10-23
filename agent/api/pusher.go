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
	// 步骤1：初始化客户端结构
	return &APIClient{
		cfg:     cfg,
		client:  &http.Client{Timeout: 10 * time.Second},
		baseURL: cfg.BaseURL,
	}
}

// PushData POST /push - 推送K8s发现的数据
func (c *APIClient) PushData(req models.PushRequest) error {
	// 步骤1：序列化请求数据
	jsonData, err := json.Marshal(req)
	if err != nil {
		return fmt.Errorf("JSON序列化失败: %v", err)
	}

	// 步骤2：记录请求日志（标准HTTP格式）
	logrus.WithFields(logrus.Fields{
		"method": "POST",
		"url":    c.baseURL + "/push",
		"headers": map[string]string{
			"Content-Type": "application/json",
		},
		"body": string(jsonData),
	}).Info("Sending HTTP request")

	// 步骤3：发送POST请求
	resp, err := c.client.Post(c.baseURL+"/push", "application/json", bytes.NewBuffer(jsonData))
	if err != nil {
		logrus.WithFields(logrus.Fields{
			"error": err,
		}).Error("HTTP request failed")
		return err
	}
	defer resp.Body.Close()

	// 步骤4：读取响应体
	body, _ := io.ReadAll(resp.Body)

	// 步骤5：记录响应日志（标准HTTP格式）
	logrus.WithFields(logrus.Fields{
		"status": resp.StatusCode,
		"body":   string(body),
	}).Info("Received HTTP response")

	// 步骤6：检查响应状态
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP错误: %d - %s", resp.StatusCode, string(body))
	}

	return nil
}

// QueryTasks POST /query - 查询待处理任务
func (c *APIClient) QueryTasks(req models.QueryRequest) ([]models.DeployRequest, error) {
	// 步骤1：序列化请求数据
	jsonData, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("JSON序列化失败: %v", err)
	}

	// 步骤2：记录请求日志
	logrus.WithFields(logrus.Fields{
		"method": "POST",
		"url":    c.baseURL + "/query",
		"headers": map[string]string{
			"Content-Type": "application/json",
		},
		"body": string(jsonData),
	}).Info("Sending HTTP request")

	// 步骤3：发送POST请求
	resp, err := c.client.Post(c.baseURL+"/query", "application/json", bytes.NewBuffer(jsonData))
	if err != nil {
		logrus.WithFields(logrus.Fields{
			"error": err,
		}).Error("HTTP request failed")
		return nil, err
	}
	defer resp.Body.Close()

	// 步骤4：读取响应体
	body, _ := io.ReadAll(resp.Body)

	// 步骤5：记录响应日志
	logrus.WithFields(logrus.Fields{
		"status": resp.StatusCode,
		"body":   string(body),
	}).Info("Received HTTP response")

	// 步骤6：检查响应状态并解析
	if resp.StatusCode == http.StatusOK {
		var tasks []models.DeployRequest
		if err := json.Unmarshal(body, &tasks); err != nil {
			// 可能是无任务消息
			return []models.DeployRequest{}, nil
		}
		return tasks, nil
	}

	return nil, fmt.Errorf("HTTP %d - %s", resp.StatusCode, string(body))
}

// UpdateStatus POST /status - 更新部署状态
func (c *APIClient) UpdateStatus(req models.StatusRequest) error {
	// 步骤1：序列化请求数据
	jsonData, err := json.Marshal(req)
	if err != nil {
		return fmt.Errorf("JSON序列化失败: %v", err)
	}

	// 步骤2：记录请求日志
	logrus.WithFields(logrus.Fields{
		"method": "POST",
		"url":    c.baseURL + "/status",
		"headers": map[string]string{
			"Content-Type": "application/json",
		},
		"body": string(jsonData),
	}).Info("Sending HTTP request")

	// 步骤3：发送POST请求
	resp, err := c.client.Post(c.baseURL+"/status", "application/json", bytes.NewBuffer(jsonData))
	if err != nil {
		logrus.WithFields(logrus.Fields{
			"error": err,
		}).Error("HTTP request failed")
		return err
	}
	defer resp.Body.Close()

	// 步骤4：读取响应体
	body, _ := io.ReadAll(resp.Body)

	// 步骤5：记录响应日志
	logrus.WithFields(logrus.Fields{
		"status": resp.StatusCode,
		"body":   string(body),
	}).Info("Received HTTP response")

	// 步骤6：检查响应状态
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP错误: %d - %s", resp.StatusCode, string(body))
	}

	return nil
}