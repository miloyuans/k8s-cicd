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

	"github.com/fatih/color"
	"github.com/sirupsen/logrus"
)

// APIClient 统一API客户端
type APIClient struct {
	cfg    *config.APIConfig
	client *http.Client
	baseURL string
}

// NewAPIClient 创建API客户端
func NewAPIClient(cfg *config.APIConfig) *APIClient {
	return &APIClient{
		cfg:     cfg,
		client:  &http.Client{Timeout: 10 * time.Second},
		baseURL: cfg.BaseURL,
	}
}

// PushData POST /push - 推送K8s发现的数据
func (c *APIClient) PushData(req models.PushRequest) error {
	logrus.Infof("📤 === POST /push ===")
	logrus.Infof("请求数据: %+v", req)

	jsonData, err := json.Marshal(req)
	if err != nil {
		return fmt.Errorf("JSON序列化失败: %v", err)
	}

	resp, err := c.client.Post(c.baseURL+"/push", "application/json", bytes.NewBuffer(jsonData))
	if err != nil {
		red := color.New(color.FgRed)
		red.Printf("❌ /push 网络错误: %v\n", err)
		return err
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	
	green := color.New(color.FgGreen)
	green.Printf("✅ /push 成功 [Status:%d]\n", resp.StatusCode)
	green.Printf("响应: %s\n", string(body))
	
	return nil
}

// QueryTasks POST /query - 查询待处理任务
func (c *APIClient) QueryTasks(req models.QueryRequest) ([]models.DeployRequest, error) {
	logrus.Infof("🔍 === POST /query ===")
	logrus.Infof("请求: environment=%s, user=%s", req.Environment, req.User)

	jsonData, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("JSON序列化失败: %v", err)
	}

	resp, err := c.client.Post(c.baseURL+"/query", "application/json", bytes.NewBuffer(jsonData))
	if err != nil {
		red := color.New(color.FgRed)
		red.Printf("❌ /query 网络错误: %v\n", err)
		return nil, err
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	
	if resp.StatusCode == http.StatusOK {
		var tasks []models.DeployRequest
		if err := json.Unmarshal(body, &tasks); err != nil {
			// 可能是 {"message": "暂无任务"}
			green := color.New(color.FgGreen)
			green.Printf("✅ /query 无任务 [Status:%d]\n", resp.StatusCode)
			green.Printf("响应: %s\n", string(body))
			return []models.DeployRequest{}, nil
		}
		
		green := color.New(color.FgGreen)
		green.Printf("✅ /query 成功 [Status:%d] %d个任务\n", resp.StatusCode, len(tasks))
		logrus.Infof("响应数据: %+v", tasks)
		return tasks, nil
	}

	red := color.New(color.FgRed)
	red.Printf("❌ /query HTTP错误 [Status:%d]\n", resp.StatusCode)
	red.Printf("响应: %s\n", string(body))
	return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
}

// UpdateStatus POST /status - 更新部署状态
func (c *APIClient) UpdateStatus(req models.StatusRequest) error {
	logrus.Infof("📤 === POST /status ===")
	logrus.Infof("请求: status=%s service=%s version=%s environment=%s user=%s", 
		req.Status, req.Service, req.Version, req.Environment, req.User)

	jsonData, err := json.Marshal(req)
	if err != nil {
		return fmt.Errorf("JSON序列化失败: %v", err)
	}

	resp, err := c.client.Post(c.baseURL+"/status", "application/json", bytes.NewBuffer(jsonData))
	if err != nil {
		red := color.New(color.FgRed)
		red.Printf("❌ /status 网络错误: %v\n", err)
		return err
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	
	green := color.New(color.FgGreen)
	green.Printf("✅ /status 成功 [Status:%d]\n", resp.StatusCode)
	green.Printf("响应: %s\n", string(body))
	
	return nil
}