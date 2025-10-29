package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"io"
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
// approval/api/query_client.go
func (c *QueryClient) QueryTasks(service, env, user string) ([]models.DeployRequest, error) {
    startTime := time.Now()

    reqBody := QueryRequest{Environment: env, User: user}
    jsonData, _ := json.Marshal(reqBody)

    url := fmt.Sprintf("%s/query", c.BaseURL)
    resp, err := c.HTTPClient.Post(url, "application/json", bytes.NewBuffer(jsonData))
    if err != nil {
        return nil, fmt.Errorf("网络错误: %v", err)
    }
    defer resp.Body.Close()

    // 读取响应体
    body, _ := io.ReadAll(resp.Body)

    if resp.StatusCode != http.StatusOK {
        // 关键：直接返回原始 body，不强行解析 JSON
        errMsg := string(bytes.TrimSpace(body))
        if errMsg == "" {
            errMsg = "空响应"
        }
        logrus.WithFields(logrus.Fields{
            "time":     time.Now().Format("2006-01-02 15:04:05"),
            "method":   "QueryTasks",
            "service":  service,
            "env":      env,
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