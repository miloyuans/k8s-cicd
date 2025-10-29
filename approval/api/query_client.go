package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"k8s-cicd/approval/models"

	"github.com/sirupsen/logrus"
)

// QueryRequest /query 请求体
type QueryRequest struct {
	Service      string   `json:"service"`
	Environments []string `json:"environments"`
}

// EmptyResponse 无任务响应
type EmptyResponse struct {
	Message string `json:"message"`
}

// QueryClient 查询客户端
type QueryClient struct {
	BaseURL    string
	HTTPClient *http.Client
}

// NewQueryClient 创建客户端
func NewQueryClient(baseURL string) *QueryClient {
	return &QueryClient{
		BaseURL: baseURL,
		HTTPClient: &http.Client{
			Timeout: 10 * time.Second,
		},
	}
}

// QueryTasks 调用 /query 接口（兼容数组和对象）
func (c *QueryClient) QueryTasks(service string, envs []string) ([]models.DeployRequest, error) {
	startTime := time.Now()

	reqBody := QueryRequest{
		Service:      service,
		Environments: envs,
	}
	jsonData, _ := json.Marshal(reqBody)

	logrus.WithFields(logrus.Fields{
		"time":     time.Now().Format("2006-01-02 15:04:05"),
		"method":   "QueryTasks",
		"service":  service,
		"envs":     envs,
		"url":      fmt.Sprintf("%s/query", c.BaseURL),
	}).Debugf("发送 /query 请求:\n%s", string(jsonData))

	url := fmt.Sprintf("%s/query", c.BaseURL)
	resp, err := c.HTTPClient.Post(url, "application/json", bytes.NewBuffer(jsonData))
	if err != nil {
		return nil, fmt.Errorf("网络错误: %v", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	respStr := string(body)

	logrus.WithFields(logrus.Fields{
		"time":   time.Now().Format("2006-01-02 15:04:05"),
		"method": "QueryTasks",
		"status": resp.StatusCode,
		"took":   time.Since(startTime),
	}).Debugf("收到 /query 响应:\n%s", respStr)

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, respStr)
	}

	// 1. 尝试解析为数组
	var tasks []models.DeployRequest
	if err := json.Unmarshal(body, &tasks); err == nil {
		logrus.WithFields(logrus.Fields{
			"time":   time.Now().Format("2006-01-02 15:04:05"),
			"method": "QueryTasks",
			"count":  len(tasks),
			"took":   time.Since(startTime),
		}).Infof("查询成功")
		return tasks, nil
	}

	// 2. 尝试解析为对象（无任务）
	var empty EmptyResponse
	if err := json.Unmarshal(body, &empty); err == nil {
		if empty.Message == "暂无待处理任务" {
			logrus.WithFields(logrus.Fields{
				"time":   time.Now().Format("2006-01-02 15:04:05"),
				"method": "QueryTasks",
				"took":   time.Since(startTime),
			}).Infof("无待处理任务")
			return []models.DeployRequest{}, nil
		}
	}

	return nil, fmt.Errorf("未知响应格式: %s", respStr)
}