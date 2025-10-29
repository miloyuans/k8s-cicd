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

// QueryRequest /query 请求体（字段名必须为 environments）
type QueryRequest struct {
	Service      string   `json:"service"`      // 必填
	Environments []string `json:"environments"` // 必填：数组
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

// QueryTasks 调用 /query 接口（仅传 service 和 environments 数组）
func (c *QueryClient) QueryTasks(service string, envs []string) ([]models.DeployRequest, error) {
	startTime := time.Now()

	// 1. 构造请求体
	reqBody := QueryRequest{
		Service:      service,
		Environments: envs,
	}
	jsonData, _ := json.Marshal(reqBody)

	// 2. 发送请求
	url := fmt.Sprintf("%s/query", c.BaseURL)
	resp, err := c.HTTPClient.Post(url, "application/json", bytes.NewBuffer(jsonData))
	if err != nil {
		logrus.WithFields(logrus.Fields{
			"time":    time.Now().Format("2006-01-02 15:04:05"),
			"method":  "QueryTasks",
			"service": service,
			"envs":    envs,
			"took":    time.Since(startTime),
		}).Errorf("调用 /query 网络错误: %v", err)
		return nil, err
	}
	defer resp.Body.Close()

	// 3. 读取响应体
	body, _ := io.ReadAll(resp.Body)

	// 4. 处理错误响应
	if resp.StatusCode != http.StatusOK {
		errMsg := string(bytes.TrimSpace(body))
		if errMsg == "" {
			errMsg = "空响应"
		}
		logrus.WithFields(logrus.Fields{
			"time":     time.Now().Format("2006-01-02 15:04:05"),
			"method":   "QueryTasks",
			"service":  service,
			"envs":     envs,
			"status":   resp.StatusCode,
			"response": errMsg,
			"took":     time.Since(startTime),
		}).Errorf("查询失败")
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, errMsg)
	}

	// 5. 解析返回数组
	var tasks []models.DeployRequest
	if err := json.Unmarshal(body, &tasks); err != nil {
		return nil, fmt.Errorf("解析 JSON 失败: %v, 响应: %s", err, string(body))
	}

	logrus.WithFields(logrus.Fields{
		"time":    time.Now().Format("2006-01-02 15:04:05"),
		"method":  "QueryTasks",
		"service": service,
		"envs":    envs,
		"count":   len(tasks),
		"took":    time.Since(startTime),
	}).Infof("查询到 %d 个任务", len(tasks))

	return tasks, nil
}