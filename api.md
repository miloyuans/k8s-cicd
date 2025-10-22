# k8s-cicd API 接口文档

## 1. 环境变量配置（外部服务调用前准备）

```bash
# 必须配置
export TELEGRAM_TOKEN="your_bot_token"
export TELEGRAM_GROUP_ID="your_group_id"

# 可选（IP 白名单，逗号分隔，支持 CIDR）
export WHITELIST_IPS="192.168.1.0/24,10.0.0.1"

# Redis（默认 localhost:6379）
export REDIS_ADDR="localhost:6379"

# 服务端口（默认 8080）
export PORT=8080
```

## 2. API 基础信息

- **Base URL**: `http://your-server:8080`
- **Content-Type**: `application/json`
- **请求方法**: **仅支持 POST**
- **响应格式**: JSON

---

## 3. 接口详细说明

### **3.1 /push - 推送服务/环境/部署数据**

**用途**: 外部 CI/CD 系统推送服务列表、环境列表到 Redis（**不进入部署队列**）

#### **请求示例**
```bash
curl -X POST http://localhost:8080/push \
  -H "Content-Type: application/json" \
  -d '{
    "services": ["API-GATEWAY", "USER-SERVICE", "ORDER-SERVICE"],
    "environments": ["PROD", "STAGING", "DEV"],
    "deployments": [
      {
        "service": "API-GATEWAY",
        "environments": ["PROD"],
        "version": "v1.2.3",
        "user": "deployer",
        "status": "pending"
      }
    ]
  }'
```

#### **请求数据结构**
```json
{
  "services": ["API-GATEWAY", "USER-SERVICE"],        // 服务列表（可选）
  "environments": ["PROD", "STAGING"],                // 环境列表（可选）
  "deployments": [                                    // 部署数据（可选）
    {
      "service": "API-GATEWAY",
      "environments": ["PROD"],
      "version": "v1.2.3",
      "user": "deployer",
      "status": "pending"
    }
  ]
}
```

#### **响应示例**
```json
{
  "message": "数据推送成功"
}
```

---

### **3.2 /query - 查询部署任务**

**用途**: 查询指定用户在指定环境中的**待处理任务**（状态 ≠ success/failure）

#### **请求示例**
```bash
curl -X POST http://localhost:8080/query \
  -H "Content-Type: application/json" \
  -d '{
    "environment": "PROD",
    "user": "john.doe"
  }'
```

#### **请求数据结构**
```json
{
  "environment": "PROD",  // 必填：环境名
  "user": "john.doe"     // 必填：用户名
}
```

#### **响应示例 - 有任务**
```json
[
  {
    "service": "API-GATEWAY",
    "environments": ["PROD"],
    "version": "v1.2.3",
    "user": "john.doe",
    "status": "pending"
  }
]
```

#### **响应示例 - 无任务**
```json
{
  "message": "暂无任务，请继续等待"
}
```

---

### **3.3 /status - 更新任务状态**

**用途**: CI/CD 系统完成部署后，**更新任务状态**（success/failure/no_action）

#### **请求示例**
```bash
curl -X POST http://localhost:8080/status \
  -H "Content-Type: application/json" \
  -d '{
    "service": "API-GATEWAY",
    "version": "v1.2.3",
    "environment": "PROD",
    "user": "john.doe",
    "status": "success"
  }'
```

#### **请求数据结构**
```json
{
  "service": "API-GATEWAY",       // 必填：服务名
  "version": "v1.2.3",           // 必填：版本号
  "environment": "PROD",         // 必填：环境名
  "user": "john.doe",            // 必填：用户名
  "status": "success"            // 必填：状态 (success/failure/no_action)
}
```

#### **响应示例**
```json
{
  "message": "状态更新成功"
}
```

---

## 4. **完整交互流程示例**

### **阶段 1: 初始化数据（外部 CI/CD 调用）**
```bash
# 推送服务和环境列表
curl -X POST http://localhost:8080/push -H "Content-Type: application/json" -d '{
  "services": ["API", "USER", "ORDER"],
  "environments": ["PROD", "STAGING"]
}'
```

### **阶段 2: 轮询查询任务**
```bash
# 每 30 秒查询一次
while true; do
  response=$(curl -s -X POST http://localhost:8080/query -H "Content-Type: application/json" -d '{
    "environment": "PROD",
    "user": "deployer"
  }')
  
  if [[ "$response" != *"暂无任务"* ]]; then
    echo "任务已确认，开始部署: $response"
    break
  fi
  sleep 30
done
```

### **阶段 3: 执行部署后更新状态**
```bash
# 部署成功
curl -X POST http://localhost:8080/status -H "Content-Type: application/json" -d '{
  "service": "API",
  "version": "v1.0.0",
  "environment": "PROD",
  "user": "deployer",
  "status": "success"
}'
```

---

## 5. **状态值说明**

| 状态 | 含义 | 谁设置 |
|------|------|--------|
| `pending` | 等待确认 | 部署队列 |
| `success` | 部署成功 | `/status` 接口 |
| `failure` | 部署失败 | `/status` 接口 |
| `no_action` | 无需操作 | `/status` 接口 |

---

## 6. **错误响应示例**

### **IP 白名单错误**
```json
{
  "error": "IP 不在白名单内"
}
```
**HTTP 状态码**: `403 Forbidden`

### **请求数据错误**
```json
{
  "error": "无效的请求数据"
}
```
**HTTP 状态码**: `400 Bad Request`

---

## 7. **数据结构定义**

### **PushRequest**
```go
type PushRequest struct {
	Services     []string        `json:"services"`
	Environments []string        `json:"environments"`
	Deployments  []DeployRequest `json:"deployments"`
}
```

### **DeployRequest**
```go
type DeployRequest struct {
	Service      string   `json:"service"`
	Environments []string `json:"environments"`
	Version      string   `json:"version"`
	User         string   `json:"user"`
	Status       string   `json:"status"`
}
```

### **QueryRequest**
```go
type QueryRequest struct {
	Environment string `json:"environment"`
	User        string `json:"user"`
}
```

### **StatusRequest**
```go
type StatusRequest struct {
	Service     string `json:"service"`
	Version     string `json:"version"`
	Environment string `json:"environment"`
	User        string `json:"user"`
	Status      string `json:"status"`
}
```

---

**文档版本**: v1.0  
**更新日期**: 2025-10-22  
**支持的 CI/CD 工具**: Jenkins, GitLab CI, GitHub Actions, ArgoCD