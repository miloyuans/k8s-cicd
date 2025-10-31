package models

import (
	"time"
)

// PushRequest 推送服务发现数据
type PushRequest struct {
	Services     []string `json:"services" bson:"services"`
	Environments []string `json:"environments" bson:"environments"`
}

// DeployRequest 部署任务
type DeployRequest struct {
	Service      string    `json:"service" bson:"service"`           // 服务名
	Environments []string  `json:"environments" bson:"environments"` // 环境列表（仅一个）
	Namespace    string    `json:"namespace" bson:"namespace"`       // 命名空间
	Version      string    `json:"version" bson:"version"`           // 镜像版本（完整 tag）
	User         string    `json:"user" bson:"user"`                 // 操作人
	CreatedAt    time.Time `json:"created_at" bson:"created_at"`     // 创建时间
	Status       string    `json:"status" bson:"status"`             // 执行状态: pending, running, success, failure
	TaskID       string    `json:"task_id" bson:"task_id"`           // 唯一任务ID
}

// StatusRequest 状态更新请求
type StatusRequest struct {
	Service     string `json:"service" bson:"service"`
	Version     string `json:"version" bson:"version"`
	Environment string `json:"environment" bson:"environment"`
	User        string `json:"user" bson:"user"`
	Status      string `json:"status" bson:"status"` // success / failure
}

// ImageSnapshot 镜像快照（用于回滚）
type ImageSnapshot struct {
	Namespace  string    `bson:"namespace" json:"namespace"`
	Service    string    `bson:"service" json:"service"`
	Container  string    `bson:"container" json:"container"`
	Image      string    `bson:"image" json:"image"`
	Tag        string    `bson:"tag" json:"tag"`
	RecordedAt time.Time `bson:"recorded_at" json:"recorded_at"`
	TaskID     string    `bson:"task_id" json:"task_id"`
}

// PushData 推送数据存储模型（新增）
type PushData struct {
	Services     []string    `bson:"services" json:"services"`
	Environments []string    `bson:"environments" json:"environments"`
	UpdatedAt    time.Time   `bson:"updated_at" json:"updated_at"`
}