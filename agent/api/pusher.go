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
	}).Info("API客户端创建成功")
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
		}).Errorf("JSON序列化失败: %v", err)
		return fmt.Errorf("JSON序列化失败: %v", err)
	}

	// 步骤2：记录请求日志
	logrus.WithFields(logrus.Fields{
		"time":   time.Now().Format("2006-01-02 15:04:05"),
		"method": "PushData",
		"took":   time.Since(startTime),
		"data":   logrus.Fields{
			"method":  "POST",
			"url":     c.baseURL + "/push",
			"headers": map[string]string{"Content-Type": "application/json"},
			"body":    string(jsonData),
		},
	}).Info("发送HTTP请求")

	// 步骤3：发送POST请求
	resp, err := c.client.Post(c.baseURL+"/push", "application/json", bytes.NewBuffer(jsonData))
	if err != nil {
		logrus.WithFields(logrus.Fields{
			"time":   time.Now().Format("2006-01-02 15:04:05"),
			"method": "PushData",
			"took":   time.Since(startTime),
		}).Errorf("HTTP请求失败: %v", err)
		return err
	}
	defer resp.Body.Close()

	// 步骤4：读取响应体
	body, _ := io.ReadAll(resp.Body)

	// 步骤5：记录响应日志
	logrus.WithFields(logrus.Fields{
		"time":   time.Now().Format("2006-01-02 15:04:05"),
		"method": "PushData",
		"took":   time.Since(startTime),
		"data":   logrus.Fields{
			"status": resp.StatusCode,
			"body":   string(body),
		},
	}).Info("收到HTTP响应")

	// 步骤6：检查响应状态
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP错误: %d - %s", resp.StatusCode, string(body))
	}

	return nil
}

// QueryTasks POST /query - 查询待处理任务
func (c *APIClient) QueryTasks(req models.QueryRequest) ([]models.DeployRequest, error) {
	startTime := time.Now()
	// 步骤1：序列化请求数据
	jsonData, err := json.Marshal(req)
	if err != nil {
		logrus.WithFields(logrus.Fields{
			"time":   time.Now().Format("2006-01-02 15:04:05"),
			"method": "QueryTasks",
			"took":   time.Since(startTime),
		}).Errorf("JSON序列化失败: %v", err)
		return nil, fmt.Errorf("JSON序列化失败: %v", err)
	}

	// 步骤2：记录请求日志
	logrus.WithFields(logrus.Fields{
		"time":   time.Now().Format("2006-01-02 15:04:05"),
		"method": "QueryTasks",
		"took":   time.Since(startTime),
		"data":   logrus.Fields{
			"method":  "POST",
			"url":     c.baseURL + "/query",
			"headers": map[string]string{"Content-Type": "application/json"},
			"body":    string(jsonData),
		},
	}).Info("发送HTTP请求")

	// 步骤3：发送POST请求
	resp, err := c.client.Post(c.baseURL+"/query", "application/json", bytes.NewBuffer(jsonData))
	if err != nil {
		logrus.WithFields(logrus.Fields{
			"time":   time.Now().Format("2006-01-02 15:04:05"),
			"method": "QueryTasks",
			"took":   time.Since(startTime),
		}).Errorf("HTTP请求失败: %v", err)
		return nil, err
	}
	defer resp.Body.Close()

	// 步骤4：读取响应体
	body, _ := io.ReadAll(resp.Body)

	// 步骤5：记录响应日志
	logrus.WithFields(logrus.Fields{
		"time":   time.Now().Format("2006-01-02 15:04:05"),
		"method": "QueryTasks",
		"took":   time.Since(startTime),
		"data":   logrus.Fields{
			"status": resp.StatusCode,
			"body":   string(body),
		},
	}).Info("收到HTTP响应")

	// 步骤6：解析响应
	if resp.StatusCode == http.StatusOK {
		var tasks []models.DeployRequest
		if err := json.Unmarshal(body, &tasks); err != nil {
			logrus.WithFields(logrus.Fields{
				"time":   time.Now().Format("2006-01-02 15:04:05"),
				"method": "QueryTasks",
				"took":   time.Since(startTime),
			}).Info("无任务返回")
			return []models.DeployRequest{}, nil
		}
		return tasks, nil
	}

	logrus.WithFields(logrus.Fields{
		"time":   time.Now().Format("2006-01-02 15:04:05"),
		"method": "QueryTasks",
		"took":   time.Since(startTime),
	}).Errorf("HTTP错误: %d - %s", resp.StatusCode, string(body))
	return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
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
		}).Errorf("JSON序列化失败: %v", err)
		return fmt.Errorf("JSON序列化失败: %v", err)
	}

	// 步骤2：记录请求日志
	logrus.WithFields(logrus.Fields{
		"time":   time.Now().Format("2006-01-02 15:04:05"),
		"method": "UpdateStatus",
		"took":   time.Since(startTime),
		"data":   logrus.Fields{
			"method":  "POST",
			"url":     c.baseURL + "/status",
			"headers": map[string]string{"Content-Type": "application/json"},
			"body":    string(jsonData),
		},
	}).Info("发送HTTP请求")

	// 步骤3：发送POST请求
	resp, err := c.client.Post(c.baseURL+"/status", "application/json", bytes.NewBuffer(jsonData))
	if err != nil {
		logrus.WithFields(logrus.Fields{
			"time":   time.Now().Format("2006-01-02 15:04:05"),
			"method": "UpdateStatus",
			"took":   time.Since(startTime),
		}).Errorf("HTTP请求失败: %v", err)
		return err
	}
	defer resp.Body.Close()

	// 步骤4：读取响应体
	body, _ := io.ReadAll(resp.Body)

	// 步骤5：记录响应日志
	logrus.WithFields(logrus.Fields{
		"time":   time.Now().Format("2006-01-02 15:04:05"),
		"method": "UpdateStatus",
		"took":   time.Since(startTime),
		"data":   logrus.Fields{
			"status": resp.StatusCode,
			"body":   string(body),
		},
	}).Info("收到HTTP响应")

	// 步骤6：检查响应状态
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP错误: %d - %s", resp.StatusCode, string(body))
	}

	return nil
}