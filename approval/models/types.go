package models

import (
	"time"
)

// DeployRequest 部署任务（从 k8s-cd 同步）
type DeployRequest struct {
	Service            string    `json:"service" bson:"service"`                       // 服务名
	Environments       []string  `json:"environments" bson:"environments"`             // 环境列表（多环境拆分时单值）
	Namespace          string    `json:"namespace" bson:"namespace"`                   // 命名空间
	Version            string    `json:"version" bson:"version"`                       // 镜像版本
	User               string    `json:"user" bson:"user"`                             // 操作人
	CreatedAt          time.Time `json:"created_at" bson:"created_at,index"`           // 创建时间 (排序字段，升序索引)
	ConfirmationStatus string    `json:"confirmation_status" bson:"confirmation_status"` // 待确认, 已确认, 已拒绝
	PopupSent          bool      `json:"popup_sent" bson:"popup_sent"`                 // 是否已发送弹窗
	PopupMessageID     int       `json:"popup_message_id" bson:"popup_message_id"`     // Telegram 消息ID
	PopupSentAt        time.Time `json:"popup_sent_at" bson:"popup_sent_at"`           // 弹窗发送时间
	TaskID             string    `json:"task_id" bson:"task_id,unique"`                // UUID v4 唯一ID (全局唯一，避免并发冲突)
}

// PopupMessage 弹窗消息记录（防重）
type PopupMessage struct {
	TaskID     string    `bson:"task_id" json:"task_id"`         // 任务ID
	MessageID  int       `bson:"message_id" json:"message_id"`   // Telegram 消息ID
	SentAt     time.Time `bson:"sent_at" json:"sent_at"`         // 发送时间
}

// TaskStatusUpdate 任务状态更新
type TaskStatusUpdate struct {
	TaskID  string `json:"task_id"`
	Status  string `json:"status"`  // confirmed / rejected
	User    string `json:"user"`
	UpdatedAt time.Time `json:"updated_at"`
}