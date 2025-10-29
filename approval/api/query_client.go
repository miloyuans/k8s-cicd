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
	Services     []string `json:"services"`
	Environments []string `json:"environments"`
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

// QueryTasks 调用 /query 接口（打印原始请求和响应）
func (c *QueryClient) QueryTasks(services []string, envs []string) ([]models.DeployRequest, error) {
	startTime := time.Now()

	// 1. 构造请求体
	reqBody := QueryRequest{
		Services:     services,
		Environments: envs,
	}
	jsonData, _ := json.Marshal(reqBody)

	// 2. 打印请求
	logrus.WithFields(logrus.Fields{
		"time":    time.Now().Format("2006-01-02 15:04:05"),
		"method":  "QueryTasks",
		"services": services,
		"envs":     envs,
		"url":      fmt.Sprintf("%s/query", c.BaseURL),
	}).Debugf("发送 /query 请求:\n%s", string(jsonData))

	// 3. 发送请求
	url := fmt.Sprintf("%s/query", c.BaseURL)
	resp, err := c.HTTPClient.Post(url, "application/json", bytes.NewBuffer(jsonData))
	if err != nil {
		logrus.Errorf("网络错误: %v", err)
		return nil, err
	}
	defer resp.Body.Close()

	// 4. 读取完整响应
	body, _ := io.ReadAll(resp.Body)
	respStr := string(body)

	// 5. 打印响应
	logrus.WithFields(logrus.Fields{
		"time":   time.Now().Format("2006-01-02 15:04:05"),
		"method": "QueryTasks",
		"status": resp.StatusCode,
		"took":   time.Since(startTime),
	}).Debugf("收到 /query 响应:\n%s", respStr)

	// 6. 处理错误
	if resp.StatusCode != http.StatusOK {
		errMsg := respStr
		if errMsg == "" {
			errMsg = "空响应"
		}
		logrus.WithFields(logrus.Fields{
			"time":     time.Now().Format("2006-01-02 15:04:05"),
			"method":   "QueryTasks",
			"services": services,
			"envs":     envs,
			"status":   resp.StatusCode,
			"response": errMsg,
			"took":     time.Since(startTime),
		}).Errorf("查询失败")
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, errMsg)
	}

	// 7. 解析任务
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