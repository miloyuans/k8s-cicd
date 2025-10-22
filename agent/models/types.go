package models

import (
	"encoding/json"
	"time"
)

// PushRequest 推送请求数据结构
type PushRequest struct {
	Services     []string       `json:"services"`
	Environments []string       `json:"environments"`
	Deployments  []DeployRequest `json:"deployments"`
}

// DeployRequest 单个部署任务请求
type DeployRequest struct {
	Service      string   `json:"service"`
	Environments []string `json:"environments"`
	Version      string   `json:"version"`
	User         string   `json:"user"`
	Status       string   `json:"status"`
}

// QueryRequest 查询请求数据结构
type QueryRequest struct {
	Environment string `json:"environment"`
	User        string `json:"user"`
}

// StatusRequest 状态更新请求数据结构
type StatusRequest struct {
	Service     string `json:"service"`
	Version     string `json:"version"`
	Environment string `json:"environment"`
	User        string `json:"user"`
	Status      string `json:"status"`
}

// Task 任务队列中的任务结构
type Task struct {
	DeployRequest
	ID        string    `json:"id"`
	CreatedAt time.Time `json:"created_at"`
	Retries   int       `json:"retries"`
}

// DeploymentStatus 部署状态结果
type DeploymentStatus struct {
	OldVersion string `json:"old_version"`
	NewVersion string `json:"new_version"`
	IsSuccess  bool   `json:"is_success"`
	Message    string `json:"message"`
}

// MarshalDeploy 将DeployRequest序列化为JSON字节
func MarshalDeploy(deploy DeployRequest) ([]byte, error) {
	return json.Marshal(deploy)
}

// UnmarshalDeploy 从JSON字节反序列化DeployRequest
func UnmarshalDeploy(data []byte) (DeployRequest, error) {
	var deploy DeployRequest
	err := json.Unmarshal(data, &deploy)
	return deploy, err
}