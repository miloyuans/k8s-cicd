package models

import (
	"encoding/json"
	"time"
)

// PushRequest 推送请求数据结构（优化：Deployments可选，但保留以支持版本推送）
type PushRequest struct {
	Services     []string        `json:"services"`     // 服务列表
	Environments []string        `json:"environments"` // 环境列表
	Deployments  []DeployRequest `json:"deployments,omitempty"` // 部署记录（可选）
}

// DeployRequest 单个部署任务请求
type DeployRequest struct {
	Service      string   `json:"service"`      // 服务名
	Environments []string `json:"environments"` // 环境列表
	Version      string   `json:"version"`      // 版本
	User         string   `json:"user"`         // 用户
	Status       string   `json:"status"`       // 状态
}

// QueryRequest 查询请求数据结构
type QueryRequest struct {
	Environment string `json:"environment"` // 环境
	User        string `json:"user"`        // 用户
}

// StatusRequest 状态更新请求数据结构
type StatusRequest struct {
	Service     string `json:"service"`     // 服务名
	Version     string `json:"version"`     // 版本
	Environment string `json:"environment"` // 环境
	User        string `json:"user"`        // 用户
	Status      string `json:"status"`      // 状态
}

// Task 任务队列中的任务结构
type Task struct {
	DeployRequest
	ID        string    `json:"id"`         // 任务ID
	CreatedAt time.Time `json:"created_at"` // 创建时间
	Retries   int       `json:"retries"`    // 重试次数
}

// DeploymentStatus 部署状态结果
type DeploymentStatus struct {
	OldVersion string `json:"old_version"` // 旧版本
	NewVersion string `json:"new_version"` // 新版本
	IsSuccess  bool   `json:"is_success"`  // 是否成功
	Message    string `json:"message"`     // 消息
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