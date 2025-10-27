// types.go
package models

import (
	"encoding/json"
	"time"
)

// PushRequest 推送请求数据结构
type PushRequest struct {
	Services     []string `json:"services" bson:"services"`         // 服务列表
	Environments []string `json:"environments" bson:"environments"` // 环境列表
}

// DeployRequest 单个部署任务请求
type DeployRequest struct {
	Service           string    `json:"service" bson:"service"`           // 服务名
	Environments      []string  `json:"environments" bson:"environments"` // 环境列表
	Namespace         string    `json:"namespace" bson:"namespace"`       // 命名空间
	Version           string    `json:"version" bson:"version"`           // 版本（完整image:tag）
	User              string    `json:"user" bson:"user"`                 // 用户
	Status            string    `json:"status" bson:"status"`             // 状态
	CreatedAt         time.Time `json:"created_at" bson:"created_at"`     // 创建时间
	ConfirmationStatus string    `json:"confirmation_status" bson:"confirmation_status"` // 弹窗状态: pending, success, confirmed, rejected, failed
}

// QueryRequest 查询请求数据结构
type QueryRequest struct {
	Environment string `json:"environment" bson:"environment"` // 环境
	User        string `json:"user" bson:"user"`               // 用户
	Service     string `json:"service" bson:"service"`         // 服务名
}

// StatusRequest 状态更新请求数据结构
type StatusRequest struct {
	Service     string `json:"service" bson:"service"`         // 服务名
	Version     string `json:"version" bson:"version"`         // 版本
	Environment string `json:"environment" bson:"environment"` // 环境
	User        string `json:"user" bson:"user"`               // 用户
	Status      string `json:"status" bson:"status"`           // 状态
}

// Task 任务队列中的任务结构
type Task struct {
	DeployRequest
	ID        string `json:"id" bson:"_id"`           // 任务ID
	Retries   int    `json:"retries" bson:"retries"` // 重试次数
}

// DeploymentStatus 部署状态结果
type DeploymentStatus struct {
	OldVersion string `json:"old_version" bson:"old_version"` // 旧版本
	NewVersion string `json:"new_version" bson:"new_version"` // 新版本
	IsSuccess  bool   `json:"is_success" bson:"is_success"`   // 是否成功
	Message    string `json:"message" bson:"message"`         // 消息
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