# k8s-cicd
### 配置文件更新说明（中文）

根据提供的参考 `config.yaml` 和当前的代码逻辑（`cmd/gateway/main.go`, `cmd/k8s-cicd/main.go`, `dialog.go`, `telegram/bot.go` 等），以下是新版本的配置文件，包含所有必要的字段，并为可选字段或具有默认值的字段添加注释。配置文件支持以下功能：
- Gateway 服务器配置（`gateway_url`, `gateway_listen_addr`）。
- IP 白名单（`allowed_ips`）。
- 任务轮询间隔（`poll_interval`）。
- Kubernetes 命名空间（`environments`）。
- Telegram 机器人和聊天配置（`telegram_bots`, `telegram_chats`）。
- 服务分类关键词（`service_keywords`）。
- 交互触发和取消关键词（`trigger_keyword`, `cancel_keyword`）。
- 服务目录和超时设置（`services_dir`, `timeout_seconds`, `dialog_timeout`, `max_concurrency`）。
- 支持动态环境验证（通过 `environments.json`）和默认命名空间回退（`international`）。
- 支持默认聊天 ID 回退（`other`）。

### 新版本配置文件

以下是新版本的 `config.yaml`，包含所有必要字段，并为具有默认值的字段添加注释。字段说明基于当前代码逻辑，确保与 `cmd/gateway/main.go` 和其他文件的实现一致。

```yaml
# Gateway 服务监听地址，格式为 "host:port"，用于 Gateway HTTP 服务器
gateway_listen_addr: "0.0.0.0:8099"

# Gateway 的 URL，供 k8s-cicd 访问（如任务拉取和上报）
gateway_url: "http://18.13.24.10:8081"

# 允许访问 /tasks 端点的 IP 地址或 CIDR 范围
allowed_ips:
  - "10.1.0.0/16"
  - "10.1.2.3/32"
  - "10.1.2.5"

# k8s-cicd 轮询 Gateway /tasks 端点的间隔（秒），默认 60 秒，建议 5-60 秒以平衡延迟和性能
poll_interval: 60
# poll_interval: 5 # 示例：更频繁的轮询以减少任务获取延迟

# 存储目录，用于保存部署信息和服务列表文件
services_dir: "services"
# services_dir: "gateway_services" # 默认值，可自定义

# 存储目录，用于保存 environments.json 和部署日志
storage_dir: "gateway_storage"
# storage_dir: "cicd_storage" # 默认值，可自定义

# 部署操作的超时时间（秒），默认 600 秒
timeout_seconds: 600
# timeout_seconds: 300 # 示例：较短的超时时间

# 最大并发部署任务数，默认 10
max_concurrency: 10
# max_concurrency: 5 # 示例：减少并发以降低资源占用

# Telegram 交互对话超时时间（秒），默认 300 秒
dialog_timeout: 300
# dialog_timeout: 600 # 示例：更长的对话超时时间

# 触发部署的关键词列表
trigger_keyword:
  - "deploy"
  - "发布"
  - "update"
  - "upgrade"
# trigger_keyword: ["start", "deploy"] # 示例：自定义触发关键词

# 取消部署的关键词列表
cancel_keyword:
  - "cancel"
  - "取消"
  - "停止"
  - "tingzhi"
  - "quxiao"
# cancel_keyword: ["stop", "abort"] # 示例：自定义取消关键词

# 无效输入的响应消息（随机选择），可选
invalid_responses:
  - "请输入有效的触发关键词以开始部署。\nPlease enter a valid trigger keyword to start deployment."
  - "无效输入，请使用触发关键词。\nInvalid input, please use a trigger keyword."
# invalid_responses: [] # 默认值：空列表，使用默认响应

# Telegram 机器人 token，按服务类别配置
telegram_bots:
  api-service: "YOUR_BOT_TOKEN"
  web-service: "YOUR_BOT_TOKEN"
  other: "YOUR_DEFAULT_BOT_TOKEN" # 默认类别，建议配置以支持回退
# other: "YOUR_DEFAULT_BOT_TOKEN" # 示例：默认 bot token

# Telegram 聊天 ID，按服务类别配置
telegram_chats:
  api-service: -4829719305
  web-service: -4829719305
  other: -4829719305 # 默认聊天 ID，建议配置以支持回退
# other: -123456789 # 示例：默认聊天 ID

# 服务分类关键词，用于将服务分配到类别（如 api-service, web-service）
service_keywords:
  api-service: ["international", "manager"]
  web-service: ["gaming", "frontend"]
# other: [] # 默认类别，自动分配未匹配的服务

# 环境到 Kubernetes 命名空间的映射
environments:
  eks-test: "international"
  eks-pre: "pre"
  prod: "international"
# eks-sa: "international" # 示例：支持动态环境

# 部署失败通知的默认类别，可选，默认为 "other"
deploy_category: "other"
# deploy_category: "deploy-team" # 示例：自定义部署失败通知类别
```

### 配置文件字段说明（中文）

1. **gateway_listen_addr**：
   - 作用：Gateway HTTP 服务器监听地址，用于接收 `/tasks`, `/report`, `/complete`, `/services`, `/submit-task` 请求。
   - 示例：`"0.0.0.0:8099"`。
   - 必须配置，无默认值。

2. **gateway_url**：
   - 作用：`k8s-cicd` 访问 Gateway 的 URL（如 `http://18.13.24.10:8081`），用于任务拉取和上报。
   - 示例：`"http://18.13.24.10:8081"`。
   - 必须配置，无默认值。

3. **allowed_ips**：
   - 作用：限制 `/tasks` 端点的访问 IP，仅允许指定的 IP 或 CIDR 范围。
   - 示例：`["10.1.0.0/16", "10.1.2.3/32"]`。
   - 可选，默认允许所有 IP。

4. **poll_interval**：
   - 作用：`k8s-cicd` 轮询 Gateway `/tasks` 端点的间隔（秒）。
   - 示例：`60`（默认），建议 `5` 以减少任务获取延迟。
   - 可选，默认值 `60`。

5. **services_dir**：
   - 作用：存储服务列表文件的目录（如 `services/api-service.svc.list`）。
   - 示例：`"services"`。
   - 可选，默认值 `"services"`。

6. **storage_dir**：
   - 作用：存储 `environments.json` 和部署日志的目录。
   - 示例：`"gateway_storage"`。
   - 可选，默认值 `"gateway_storage"`。

7. **timeout_seconds**：
   - 作用：Kubernetes 部署操作的超时时间（秒）。
   - 示例：`600`（默认）。
   - 可选，默认值 `600`。

8. **max_concurrency**：
   - 作用：`k8s-cicd` 并发处理任务的最大数量。
   - 示例：`10`（默认）。
   - 可选，默认值 `10`。

9. **dialog_timeout**：
   - 作用：Telegram 交互对话的超时时间（秒）。
   - 示例：`300`（默认）。
   - 可选，默认值 `300`。

10. **trigger_keyword**：
    - 作用：触发 Telegram 部署对话的关键词。
    - 示例：`["deploy", "发布", "update", "upgrade"]`。
    - 可选，默认值为空（需配置）。

11. **cancel_keyword**：
    - 作用：取消 Telegram 部署对话的关键词。
    - 示例：`["cancel", "取消", "停止", "tingzhi", "quxiao"]`。
    - 可选，默认值为空（需配置）。

12. **invalid_responses**：
    - 作用：对无效输入的随机响应消息。
    - 示例：`["请输入有效的触发关键词以开始部署。", "无效输入，请使用触发关键词。"]`。
    - 可选，默认值为空，使用标准提示。

13. **telegram_bots**：
    - 作用：Telegram 机器人 token，按服务类别配置。
    - 示例：`api-service: "YOUR_BOT_TOKEN", other: "YOUR_DEFAULT_BOT_TOKEN"`。
    - 必须配置 `other` 类别以支持回退。

14. **telegram_chats**：
    - 作用：Telegram 聊天 ID，按服务类别配置。
    - 示例：`api-service: -4829719305, other: -4829719305`。
    - 必须配置 `other` 类别以支持回退。

15. **service_keywords**：
    - 作用：服务分类规则，用于将服务分配到类别（如 `api-service`, `web-service`）。
    - 示例：`api-service: ["international", "manager"]`。
    - 可选，默认将未匹配服务分配到 `other`。

16. **environments**：
    - 作用：环境到 Kubernetes 命名空间的映射。
    - 示例：`eks-test: "international", eks-pre: "pre"`。
    - 可选，默认通过 `environments.json` 支持动态环境，未定义环境回退到 `international`。

17. **deploy_category**：
    - 作用：部署失败通知的 Telegram 类别。
    - 示例：`"other"`（默认）。
    - 可选，默认值 `"other"`。

### 验证
- **配置文件兼容性**：
  - 确保 `gateway_listen_addr` 和 `gateway_url` 与部署环境匹配。
  - 验证 `telegram_bots` 和 `telegram_chats` 包含有效 token 和聊天 ID，尤其是 `other` 类别。
  - 确认 `environments` 包含必要环境（如 `eks-sa: international`），支持动态环境验证。
  - 检查 `poll_interval` 是否足够短（如 5 秒）以减少任务拉取延迟。
- **测试建议**：
  - 使用提供的 `config.yaml` 启动 Gateway，验证 `/submit-task` 接受请求并触发 Telegram 确认弹窗。
  - 测试 `k8s-cicd` 轮询（`poll_interval: 5`），确保任务及时拉取（日志包含 `Processing task ...`）。
  - 移除 `telegram_chats` 中的非 `other` 类别，验证默认聊天 ID 回退。
  - 添加 `eks-sa` 到 `environments.json`，验证环境验证通过。
  - 确认服务列表（`services/*.svc.list`）和 `environments.json` 正确更新。
  - 检查部署日志和通知，验证包含详细诊断信息。
  - 确保 Kubernetes 部署的 `revisionHistoryLimit` ≥ 2 以支持回滚。

### Jenkins Pipeline 示例
```yaml
pipeline {
    agent any
    stages {
        stage('Push Services') {
            steps {
                sh '''
                curl -X POST http://k8s-cicd:8080/push -H "Content-Type: application/json" -d "{
                  \"services\": [\"API\", \"USER\"],
                  \"environments\": [\"PROD\", \"STAGING\"]
                }"
                '''
            }
        }
        stage('Submit Deploy') {
            steps {
                sh '''
                curl -X POST http://k8s-cicd:8080/deploy -H "Content-Type: application/json" -d "{
                  \"service\": \"API\",
                  \"environments\": [\"PROD\"],
                  \"version\": \"${BUILD_NUMBER}\",
                  \"user\": \"jenkins\",
                  \"status\": \"pending\"
                }"
                '''
            }
        }
        stage('Wait Approval') {
            steps {
                script {
                    timeout(time: 10, unit: 'MINUTES') {
                        waitUntil {
                            script {
                                def response = sh(
                                    script: 'curl -s -X POST http://k8s-cicd:8080/query -H "Content-Type: application/json" -d "{\"environment\":\"PROD\",\"user\":\"jenkins\"}"',
                                    returnStdout: true
                                ).trim()
                                return response != '{"message":"暂无任务，请继续等待"}'
                            }
                        }
                    }
                }
            }
        }
        stage('Deploy') {
            steps {
                echo '执行 K8s 部署...'
                // 实际部署命令
            }
        }
        stage('Update Status') {
            steps {
                sh '''
                curl -X POST http://k8s-cicd:8080/status -H "Content-Type: application/json" -d "{
                  \"service\": \"API\",
                  \"version\": \"${BUILD_NUMBER}\",
                  \"environment\": \"PROD\",
                  \"user\": \"jenkins\",
                  \"status\": \"success\"
                }"
                '''
            }
        }
    }
}
```
```
apiVersion: v1
kind: ServiceAccount
metadata:
  name: k8s-cicd-sa
  namespace: monitoring
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: k8s-cicd-role
rules:
- apiGroups:
  - "*"
  resources:
  - "*"
  verbs:
  - get
  - list
  - watch
- apiGroups:
  - apps
  resources:
  - deployments
  verbs:
  - get
  - list
  - watch
  - create
  - update
  - patch
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: k8s-cicd-binding
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: k8s-cicd-role
subjects:
- kind: ServiceAccount
  name: k8s-cicd-sa
  namespace: monitoring

```
### MongoDB Deploy K8S yaml tm
```
apiVersion: v1
kind: Namespace
metadata:
  name: monitoring
---
apiVersion: v1
kind: Service
metadata:
  name: mongodb
  namespace: monitoring
  labels:
    app: mongodb
spec:
  selector:
    app: mongodb
  ports:
    - protocol: TCP
      port: 27017
      targetPort: 27017
  type: ClusterIP
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: mongodb
  namespace: monitoring
  labels:
    app: mongodb
spec:
  replicas: 1
  selector:
    matchLabels:
      app: mongodb
  template:
    metadata:
      labels:
        app: mongodb
    spec:
      containers:
        - name: mongodb
          image: mongo:8.0
          ports:
            - containerPort: 27017
          volumeMounts:
            - name: mongo-data
              mountPath: /data/db
          # 明确禁用认证（MongoDB 8.0 默认已禁用，但加上参数更明确）
          args: ["--bind_ip", "0.0.0.0", "--noauth"]
      volumes:
        - name: mongo-data
          emptyDir: {}
```