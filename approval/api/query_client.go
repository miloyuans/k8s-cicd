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
	Service      string   `json:"service"`      // 必填：单个服务名
	Environments []string `json:"environments"` // 必填：环境数组
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

// QueryTasks 调用 /query 接口（service 必填）
func (c *QueryClient) QueryTasks(service string, envs []string) ([]models.DeployRequest, error) {
	startTime := time.Now()

	// 1. 验证 service 非空
	if service == "" {
		return nil, fmt.Errorf("service 不能为空")
	}

	// 2. 构造请求体
	reqBody := QueryRequest{
		Service:      service,
		Environments: envs,
	}
	jsonData, _ := json.Marshal(reqBody)

	// 3. 打印请求（debug）
	logrus.WithFields(logrus.Fields{
		"time":     time.Now().Format("2006-01-02 15:04:05"),
		"method":   "QueryTasks",
		"service":  service,
		"envs":     envs,
		"url":      fmt.Sprintf("%s/query", c.BaseURL),
	}).Debugf("发送 /query 请求:\n%s", string(jsonData))

	// 4. 发送请求
	url := fmt.Sprintf("%s/query", c.BaseURL)
	resp, err := c.HTTPClient.Post(url, "application/json", bytes.NewBuffer(jsonData))
	if err != nil {
		return nil, fmt.Errorf("网络错误: %v", err)
	}
	defer resp.Body.Close()

	// 5. 读取响应
	body, _ := io.ReadAll(resp.Body)
	respStr := string(body)

	// 6. 打印响应
	logrus.WithFields(logrus.Fields{
		"time":   time.Now().Format("2006-01-02 15:04:05"),
		"method": "QueryTasks",
		"status": resp.StatusCode,
		"took":   time.Since(startTime),
	}).Debugf("收到 /query 响应:\n%s", respStr)

	// 7. 处理错误
	if resp.StatusCode != http.StatusOK {
		errMsg := respStr
		if errMsg == "" {
			errMsg = "空响应"
		}
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, errMsg)
	}

	// 8. 解析任务数组
	var tasks []models.DeployRequest
	if err := json.Unmarshal(body, &tasks); err != nil {
		return nil, fmt.Errorf("解析 JSON 失败: %v, 响应: %s", err, respStr)
	}

	logrus.WithFields(logrus.Fields{
		"time":    time.Now().Format("2006-01-02 15:04:05"),
		"method":  "QueryTasks",
		"count":   len(tasks),
		"took":    time.Since(startTime),
	}).Infof("查询成功")
	return tasks, nil
}