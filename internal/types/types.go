package types

import "time"

type DeployRequest struct {
    Service   string    `json:"service"`
    Env       string    `json:"env"`
    Version   string    `json:"version"`
    Timestamp time.Time `json:"timestamp"`
    UserName  string    `json:"username"`
    Status    string    `json:"status"`
}