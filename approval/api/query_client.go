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

// QueryRequest /query 请求体
type QueryRequest struct {
	Service     string `json:"service"`
	Environment string `json:"environment"`
}

// QueryResponse /query 返回体
type QueryResponse struct {
	Tasks []models.DeployRequest `json:"tasks"`
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
func (c *QueryClient) QueryTasks(service, env string) ([]models.DeployRequest, error) {
	startTime := time.Now()

	reqBody := QueryRequest{
		Service:     service,
		Environment: env,
	}
	jsonData, _ := json.Marshal(reqBody)

	url := fmt.Sprintf("%s/query", c.BaseURL)
	resp, err := c.HTTPClient.Post(url, "application/json", bytes.NewBuffer(jsonData))
	if err != nil {
		logrus.Errorf("调用 /query 失败 [%s@%s]: %v", service, env, err)
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}

	var result QueryResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}

	logrus.With highlighter.WithFields(logrus.Fields{
		"time":   time.Now().Format("2006-01-02 15:04:05"),
		"method": "QueryTasks",
		"took":   time.Since(startTime),
	}).Infof("查询到 %d 个任务 [%s@%s]", len(result.Tasks), service, env)

	return result.Tasks, nil
}