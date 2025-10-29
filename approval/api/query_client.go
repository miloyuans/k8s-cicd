package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"k8s-cicd/approval/models"

	"github.com/sirupsen/logrus"
)

// QueryRequest /query 请求体（严格遵循 API 文档）
type QueryRequest struct {
	Environment string `json:"environment"` // 必填
	User        string `json:"user"`        // 必填
}

// ErrorResponse API 错误响应
type ErrorResponse struct {
	Error   string `json:"error"`
	Message string `json:"message,omitempty"`
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

// QueryTasks 调用 /query 接口（返回 []DeployRequest）
func (c *QueryClient) QueryTasks(service, env, user string) ([]models.DeployRequest, error) {
	startTime := time.Now()

	// 1. 构造请求体（必须包含 environment 和 user）
	reqBody := QueryRequest{
		Environment: env,
		User:        user,
	}
	jsonData, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("序列化请求失败: %v", err)
	}

	// 2. 发送请求
	url := fmt.Sprintf("%s/query", c.BaseURL)
	resp, err := c.HTTPClient.Post(url, "application/json", bytes.NewBuffer(jsonData))
	if err != nil {
		logrus.WithFields(logrus.Fields{
			"time":    time.Now().Format("2006-01-02 15:04:05"),
			"method":  "QueryTasks",
			"service": service,
			"env":     env,
			"user":    user,
			"took":    time.Since(startTime),
		}).Errorf("调用 /query 网络错误: %v", err)
		return nil, err
	}
	defer resp.Body.Close()

	// 3. 处理非 200 响应
	if resp.StatusCode != http.StatusOK {
		var errResp ErrorResponse
		if decodeErr := json.NewDecoder(resp.Body).Decode(&errResp); decodeErr != nil {
			return nil, fmt.Errorf("HTTP %d, 解析错误响应失败", resp.StatusCode)
		}
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, errResp.Error)
	}

	// 4. 解析成功响应：直接为数组
	var tasks []models.DeployRequest
	if err := json.NewDecoder(resp.Body).Decode(&tasks); err != nil {
		logrus.WithFields(logrus.Fields{
			"time":    time.Now().Format("2006-01-02 15:04:05"),
			"method":  "QueryTasks",
			"service": service,
			"env":     env,
			"user":    user,
			"took":    time.Since(startTime),
		}).Errorf("解析 /query 响应失败: %v", err)
		return nil, err
	}

	logrus.WithFields(logrus.Fields{
		"time":    time.Now().Format("2006-01-02 15:04:05"),
		"method":  "QueryTasks",
		"service": service,
		"env":     env,
		"user":    user,
		"count":   len(tasks),
		"took":    time.Since(startTime),
	}).Infof("查询到 %d 个任务", len(tasks))

	return tasks, nil
}