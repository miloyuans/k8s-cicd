# 构建
```
go mod tidy
go build -o k8s-cd ./cmd/k8s-cd
```

# 运行（集群内ServiceAccount）
```
kubectl create sa cicd-sa
kubectl create clusterrolebinding cicd-binding --clusterrole=edit --serviceaccount=default:cicd-sa
./k8s-cd -config config.yaml
```

# 运行（kubeconfig）
```
./k8s-cd -config config.yaml
```
```
唯一标识：使用 UUID v4 (标准库 github.com/google/uuid) 作为 TaskID – 全局唯一、无冲突，支持并发生成。保留原有 service-version-env 作为复合键 (防业务重)，但 TaskID 为主键。
排序机制：

存储时：设置 CreatedAt = time.Now() (精确到纳秒)，作为排序字段。
Mongo 索引：为 created_at 添加升序索引 ({created_at: 1})，确保插入顺序 = 查询顺序。
查询时：所有查询方法添加 .Sort(bson.D{{"created_at", 1}}) – 按创建时间升序返回/执行。


并发安全：UUID 生成原子；Mongo Upsert + 唯一复合索引 (service+version+environment+created_at) 防重；Go 协程安全 (当前已用 Mutex 在 sentTasks)。
兼容性：最小修改 – 只改模型/存储/查询；后续执行系统查询时，按 created_at 排序执行。
性能：UUID 生成 O(1)；索引查询 O(log n)；适合高并发 (Mongo 原子操作)。
扩展：若需自定义顺序，可加 SequenceID (原子递增)，但 UUID+时间戳 够用/简单。

## ============

测试示例 (用 code_execution 验证 UUID/排序)：

生成 3 UUID：无冲突。
模拟插入/查询：按 CreatedAt 排序返回。


后续执行系统：查询时加 sort: created_at asc – 按顺序执行。
```