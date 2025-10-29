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

// QueryRequest /query 请求体（包含数组）
type QueryRequest struct {
	Services     []string `json:"services"`      // 必填数组
	Environments []string `json:"environments"`  // 必填数组
	User         string   `json:"user"`          // 必填
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

// QueryTasks 调用 /query 接口
func (c *QueryClient) QueryTasks(services []string, envs []string, user string) ([]models.DeployRequest, error) {
	startTime := time.Now()

	reqBody := QueryRequest{
		Services:     services,
		Environments: envs,
		User:         user,
	}
	jsonData, _ := json.Marshal(reqBody)

	url := fmt.Sprintf("%s/query", c.BaseURL)
	resp, err := c.HTTPClient.Post(url, "application/json", bytes.NewBuffer(jsonData))
	if err != nil {
		return nil, fmt.Errorf("网络错误: %v", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusOK {
		errMsg := string(bytes.TrimSpace(body))
		if errMsg == "" {
			errMsg = "空响应"
		}
		logrus.WithFields(logrus.Fields{
			"time":     time.Now().Format("2006-01-02 15:04:05"),
			"method":   "QueryTasks",
			"services": services,
			"envs":     envs,
			"user":     user,
			"status":   resp.StatusCode,
			"response": errMsg,
			"took":     time.Since(startTime),
		}).Errorf("查询失败")
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, errMsg)
	}

	var tasks []models.DeployRequest
	if err := json.Unmarshal(body, &tasks); err != nil {
		return nil, fmt.Errorf("解析 JSON 失败: %v, 响应: %s", err, string(body))
	}

	logrus.WithFields(logrus.Fields{
		"time":    time.Now().Format("2006-01-02 15:04:05"),
		"method":  "QueryTasks",
		"count":   len(tasks),
		"took":    time.Since(startTime),
	}).Infof("查询成功")
	return tasks, nil
}