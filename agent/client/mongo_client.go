// 修改后的 client/mongo_client.go：
// - 确认所有任务相关操作（如 GetTasksByStatus, StoreTask, UpdateTaskStatus, CheckDuplicateTask）使用 sanitizeEnv(env) 处理环境名中的 "-" 转为 "_"，集合名为 "tasks_${sanitized_env}"。
// - 发布任务功能（轮询/执行）通过 GetTasksByStatus 查询 cicd.tasks_${sanitized_env} 获取任务数据。
// - K8s 命名空间执行使用配置文件 EnvMapping.Mappings[env] = namespace，无需 sanitize（namespace 假设无 - 或已处理）。
// - 保留所有现有功能，包括索引、TTL、push_data 等。

package client

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"k8s-cicd/agent/config"
	"k8s-cicd/agent/models"

	"github.com/fatih/color"
	"github.com/sirupsen/logrus"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
	"go.mongodb.org/mongo-driver/mongo/readpref"
)

// sanitizeEnv 将环境名中的 "-" 替换为 "_" 以符合 MongoDB 集合命名规范
func sanitizeEnv(env string) string {
	return strings.ReplaceAll(env, "-", "_")
}

// MongoClient MongoDB 客户端
type MongoClient struct {
	client *mongo.Client
	cfg    *config.MongoConfig
}

// NewMongoClient 创建 MongoDB 客户端
func NewMongoClient(cfg *config.MongoConfig) (*MongoClient, error) {
	startTime := time.Now()

	// 步骤1：创建客户端配置
	clientOptions := options.Client().
		ApplyURI(cfg.URI).
		SetConnectTimeout(5*time.Second).
		SetMaxPoolSize(10).
		SetMinPoolSize(2)

	// 步骤2：连接 MongoDB
	client, err := mongo.Connect(context.Background(), clientOptions)
	if err != nil {
		logrus.WithFields(logrus.Fields{
			"time":   time.Now().Format("2006-01-02 15:04:05"),
			"method": "NewMongoClient",
			"took":   time.Since(startTime),
		}).Errorf(color.RedString("MongoDB 连接失败: %v"), err)
		return nil, fmt.Errorf("MongoDB 连接失败: %v", err)
	}

	// 步骤3：测试连接
	ctx := context.Background()
	if err := client.Ping(ctx, readpref.Primary()); err != nil {
		logrus.WithFields(logrus.Fields{
			"time":   time.Now().Format("2006-01-02 15:04:05"),
			"method": "NewMongoClient",
			"took":   time.Since(startTime),
		}).Errorf(color.RedString("MongoDB ping 失败: %v"), err)
		return nil, fmt.Errorf("MongoDB ping 失败: %v", err)
	}

	// 步骤4：创建 TTL 索引和排序/唯一索引
	if err := createTTLIndexes(client, cfg); err != nil {
		logrus.WithFields(logrus.Fields{
			"time":   time.Now().Format("2006-01-02 15:04:05"),
			"method": "NewMongoClient",
			"took":   time.Since(startTime),
		}).Errorf(color.RedString("创建索引失败: %v"), err)
		return nil, err
	}

	logrus.WithFields(logrus.Fields{
		"time":   time.Now().Format("2006-01-02 15:04:05"),
		"method": "NewMongoClient",
		"took":   time.Since(startTime),
	}).Info(color.GreenString("MongoDB 连接成功"))
	return &MongoClient{client: client, cfg: cfg}, nil
}

// createTTLIndexes 创建 TTL、排序和唯一索引（使用 sanitized env；push_data 在 cicd，并添加唯一索引）
func createTTLIndexes(client *mongo.Client, cfg *config.MongoConfig) error {
	ctx := context.Background()

	// 1. 为每个环境的任务集合创建 TTL 索引 (created_at 升序，用于排序)
	for env := range cfg.EnvMapping.Mappings {
		sanitizedEnv := sanitizeEnv(env)
		collection := client.Database("cicd").Collection(fmt.Sprintf("tasks_%s", sanitizedEnv))
		
		// TTL 索引 (已包含升序)
		_, err := collection.Indexes().CreateOne(ctx, mongo.IndexModel{
			Keys:    bson.D{{Key: "created_at", Value: 1}},
			Options: options.Index().SetExpireAfterSeconds(int32(cfg.TTL.Seconds())),
		})
		if err != nil {
			return fmt.Errorf("创建任务 TTL 索引失败 [%s]: %v", env, err)
		}

		// 复合唯一索引: service + version + environment + created_at (防重，排序支持)
		_, err = collection.Indexes().CreateOne(ctx, mongo.IndexModel{
			Keys: bson.D{
				{Key: "service", Value: 1},
				{Key: "version", Value: 1},
				{Key: "environment", Value: 1},
				{Key: "created_at", Value: 1},
			},
			Options: options.Index().SetUnique(true),
		})
		if err != nil {
			return fmt.Errorf("创建任务复合唯一索引失败 [%s]: %v", env, err)
		}
	}

	// 2. 为 image_snapshots 创建 TTL 索引 (RecordedAt)
	imageSnapshotsColl := client.Database("cicd").Collection("image_snapshots")
	_, err := imageSnapshotsColl.Indexes().CreateOne(ctx, mongo.IndexModel{
		Keys:    bson.D{{Key: "recorded_at", Value: 1}},
		Options: options.Index().SetExpireAfterSeconds(int32(cfg.TTL.Seconds())),
	})
	if err != nil {
		return fmt.Errorf("创建 image_snapshots TTL 索引失败: %v", err)
	}

	// 3. 为 cicd.push_data 创建 TTL 和唯一索引 (created_at, 每个 service-environment 组合唯一)
	pushDataColl := client.Database("cicd").Collection("push_data")
	// TTL 索引
	_, err = pushDataColl.Indexes().CreateOne(ctx, mongo.IndexModel{
		Keys:    bson.D{{Key: "created_at", Value: 1}},
		Options: options.Index().SetExpireAfterSeconds(int32(cfg.TTL.Seconds())),
	})
	if err != nil {
		return fmt.Errorf("创建 push_data TTL 索引失败: %v", err)
	}
	// 唯一索引: service + environment
	_, err = pushDataColl.Indexes().CreateOne(ctx, mongo.IndexModel{
		Keys:    bson.D{{Key: "service", Value: 1}, {Key: "environment", Value: 1}},
		Options: options.Index().SetUnique(true),
	})
	if err != nil {
		return fmt.Errorf("创建 push_data 唯一索引失败: %v", err)
	}

	// 4. 为版本集合创建唯一索引 (不变)
	versionsColl := client.Database("cicd").Collection("versions")
	_, err = versionsColl.Indexes().CreateOne(ctx, mongo.IndexModel{
		Keys:    bson.D{{Key: "service", Value: 1}, {Key: "version", Value: 1}},
		Options: options.Index().SetUnique(true),
	})
	if err != nil {
		return fmt.Errorf("创建版本唯一索引失败: %v", err)
	}

	return nil
}

// GetTasksByStatus 获取指定状态的任务，按 created_at 升序排序（使用 sanitized env，查询 cicd.tasks_${sanitized_env}）
func (m *MongoClient) GetTasksByStatus(env, status string) ([]models.DeployRequest, error) {
	startTime := time.Now()
	ctx := context.Background()

	sanitizedEnv := sanitizeEnv(env)
	collection := m.client.Database("cicd").Collection(fmt.Sprintf("tasks_%s", sanitizedEnv))
	filter := bson.M{"status": status}
	opts := options.Find().
		SetSort(bson.D{{Key: "created_at", Value: 1}}) // 按 created_at 升序排序

	cursor, err := collection.Find(ctx, filter, opts)
	if err != nil {
		logrus.WithFields(logrus.Fields{
			"time":   time.Now().Format("2006-01-02 15:04:05"),
			"method": "GetTasksByStatus",
			"took":   time.Since(startTime),
		}).Errorf(color.RedString("查询任务失败 [%s, 集合: tasks_%s]: %v"), env, sanitizedEnv, err)
		return nil, err
	}
	defer cursor.Close(ctx)

	var tasks []models.DeployRequest
	if err = cursor.All(ctx, &tasks); err != nil {
		logrus.WithFields(logrus.Fields{
			"time":   time.Now().Format("2006-01-02 15:04:05"),
			"method": "GetTasksByStatus",
			"took":   time.Since(startTime),
		}).Errorf(color.RedString("解码任务失败 [%s]: %v"), env, err)
		return nil, err
	}

	logrus.WithFields(logrus.Fields{
		"time":   time.Now().Format("2006-01-02 15:04:05"),
		"method": "GetTasksByStatus",
		"took":   time.Since(startTime),
	}).Infof(color.GreenString("查询任务成功 [%s, 集合: tasks_%s]: %d 个"), env, sanitizedEnv, len(tasks))
	return tasks, nil
}

// StoreTask 存储任务（使用 sanitized env，插入到 cicd.tasks_${sanitized_env}）
func (m *MongoClient) StoreTask(task models.DeployRequest) error {
	startTime := time.Now()
	ctx := context.Background()

	if task.CreatedAt.IsZero() {
		task.CreatedAt = time.Now()
	}
	if task.TaskID == "" {
		task.TaskID = uuid.New().String()
	}
	task.Status = "pending"

	sanitizedEnv := sanitizeEnv(task.Environments[0])
	collection := m.client.Database("cicd").Collection(fmt.Sprintf("tasks_%s", sanitizedEnv))

	_, err := collection.InsertOne(ctx, task)
	if err != nil {
		logrus.WithFields(logrus.Fields{
			"time":   time.Now().Format("2006-01-02 15:04:05"),
			"method": "StoreTask",
			"took":   time.Since(startTime),
		}).Errorf(color.RedString("存储任务失败 [%s, 集合: tasks_%s]: %v"), task.Environments[0], sanitizedEnv, err)
		return err
	}

	logrus.WithFields(logrus.Fields{
		"time":   time.Now().Format("2006-01-02 15:04:05"),
		"method": "StoreTask",
		"took":   time.Since(startTime),
	}).Infof(color.GreenString("任务存储成功: %s [%s, 集合: tasks_%s]"), task.TaskID, task.Environments[0], sanitizedEnv)
	return nil
}

// UpdateTaskStatus 更新任务状态（使用 sanitized env，更新 cicd.tasks_${sanitized_env}）
func (m *MongoClient) UpdateTaskStatus(service, version, env, user, status string) error {
	startTime := time.Now()
	ctx := context.Background()

	sanitizedEnv := sanitizeEnv(env)
	collection := m.client.Database("cicd").Collection(fmt.Sprintf("tasks_%s", sanitizedEnv))
	filter := bson.M{
		"service":     service,
		"version":     version,
		"environment": env,
		"user":        user,
	}
	update := bson.M{"$set": bson.M{
		"status":     status,
		"updated_at": time.Now(),
	}}

	result, err := collection.UpdateMany(ctx, filter, update)
	if err != nil {
		logrus.WithFields(logrus.Fields{
			"time":   time.Now().Format("2006-01-02 15:04:05"),
			"method": "UpdateTaskStatus",
			"took":   time.Since(startTime),
		}).Errorf(color.RedString("更新任务状态失败 [%s, 集合: tasks_%s]: %v"), env, sanitizedEnv, err)
		return err
	}

	logrus.WithFields(logrus.Fields{
		"time":   time.Now().Format("2006-01-02 15:04:05"),
		"method": "UpdateTaskStatus",
		"took":   time.Since(startTime),
	}).Infof(color.GreenString("任务状态更新成功 [%s, 集合: tasks_%s]: 更新 %d 文档"), env, sanitizedEnv, result.ModifiedCount)
	return nil
}

// StoreImageSnapshot 存储镜像快照（不变，假设 taskID 关联）
func (m *MongoClient) StoreImageSnapshot(snapshot *models.ImageSnapshot) error {
	startTime := time.Now()
	ctx := context.Background()

	collection := m.client.Database("cicd").Collection("image_snapshots")
	snapshot.RecordedAt = time.Now()

	_, err := collection.InsertOne(ctx, snapshot)
	if err != nil {
		logrus.WithFields(logrus.Fields{
			"time":   time.Now().Format("2006-01-02 15:04:05"),
			"method": "StoreImageSnapshot",
			"took":   time.Since(startTime),
		}).Errorf(color.RedString("快照存储失败: %v"), err)
		return err
	}

	logrus.WithFields(logrus.Fields{
		"time":   time.Now().Format("2006-01-02 15:04:05"),
		"method": "StoreImageSnapshot",
		"took":   time.Since(startTime),
	}).Infof(color.GreenString("快照存储成功: %s"), snapshot.TaskID)
	return nil
}

// CheckDuplicateTask 检查任务是否重复（使用 sanitized env，查询 cicd.tasks_${sanitized_env}）
func (m *MongoClient) CheckDuplicateTask(task models.DeployRequest) (bool, error) {
	startTime := time.Now()
	ctx := context.Background()

	sanitizedEnv := sanitizeEnv(task.Environments[0])
	collection := m.client.Database("cicd").Collection(fmt.Sprintf("tasks_%s", sanitizedEnv))
	filter := bson.M{
		"service":     task.Service,
		"version":     task.Version,
		"environment": task.Environments[0],
		"created_at":  task.CreatedAt, // 包含 created_at 防重
	}

	count, err := collection.CountDocuments(ctx, filter)
	if err != nil {
		logrus.WithFields(logrus.Fields{
			"time":   time.Now().Format("2006-01-02 15:04:05"),
			"method": "CheckDuplicateTask",
			"took":   time.Since(startTime),
		}).Errorf(color.RedString("检查重复任务失败 [%s, 集合: tasks_%s]: %v"), task.Environments[0], sanitizedEnv, err)
		return false, err
	}

	isDuplicate := count > 0
	logrus.WithFields(logrus.Fields{
		"time":   time.Now().Format("2006-01-02 15:04:05"),
		"method": "CheckDuplicateTask",
		"took":   time.Since(startTime),
	}).Infof(color.GreenString("任务重复检查 [%s, 集合: tasks_%s]: %t"), task.Environments[0], sanitizedEnv, isDuplicate)
	return isDuplicate, nil
}

// StorePushData 存储推送数据到 cicd.push_data（全量清空 + 插入每个去重 service-environment 组合文档）
func (m *MongoClient) StorePushData(data *models.PushData) error {
	startTime := time.Now()
	ctx := context.Background()

	collection := m.client.Database("cicd").Collection("push_data")

	// 初始化/格式化：清空集合（无论是否存在）
	_, err := collection.DeleteMany(ctx, bson.M{})
	if err != nil {
		logrus.WithFields(logrus.Fields{
			"time":   time.Now().Format("2006-01-02 15:04:05"),
			"method": "StorePushData",
			"took":   time.Since(startTime),
		}).Errorf(color.RedString("清空 push_data 失败: %v"), err)
		return err
	}

	// 去重 services 和 environments
	uniqueServices := make(map[string]struct{})
	for _, s := range data.Services {
		uniqueServices[s] = struct{}{}
	}
	uniqueEnvs := make(map[string]struct{})
	for _, e := range data.Environments {
		uniqueEnvs[e] = struct{}{}
	}

	now := time.Now()
	insertedCount := 0
	for s := range uniqueServices {
		for e := range uniqueEnvs {
			doc := bson.M{
				"service":     s,
				"environment": e,
				"created_at":  now,
			}
			_, err := collection.InsertOne(ctx, doc)
			if err != nil {
				logrus.WithFields(logrus.Fields{
					"time":   time.Now().Format("2006-01-02 15:04:05"),
					"method": "StorePushData",
					"took":   time.Since(startTime),
				}).Errorf(color.RedString("插入 push_data 文档失败 [%s-%s]: %v"), s, e, err)
				return err
			}
			insertedCount++
		}
	}

	data.UpdatedAt = now

	logrus.WithFields(logrus.Fields{
		"time":   time.Now().Format("2006-01-02 15:04:05"),
		"method": "StorePushData",
		"took":   time.Since(startTime),
		"data": logrus.Fields{
			"service_count": len(uniqueServices),
			"env_count":     len(uniqueEnvs),
			"inserted_docs": insertedCount,
		},
	}).Infof(color.GreenString("推送数据全量存储成功到 cicd.push_data（清空并插入 %d 个组合）"), insertedCount)
	return nil
}

// GetPushData 获取存储的推送数据（从 cicd.push_data 收集去重 services 和 environments）
func (m *MongoClient) GetPushData() (*models.PushData, error) {
	startTime := time.Now()
	ctx := context.Background()

	collection := m.client.Database("cicd").Collection("push_data")
	filter := bson.M{}

	cursor, err := collection.Find(ctx, filter)
	if err != nil {
		logrus.WithFields(logrus.Fields{
			"time":   time.Now().Format("2006-01-02 15:04:05"),
			"method": "GetPushData",
			"took":   time.Since(startTime),
		}).Errorf(color.RedString("查询 cicd.push_data 失败: %v"), err)
		return nil, err
	}
	defer cursor.Close(ctx)

	var docs []bson.M
	if err = cursor.All(ctx, &docs); err != nil {
		logrus.WithFields(logrus.Fields{
			"time":   time.Now().Format("2006-01-02 15:04:05"),
			"method": "GetPushData",
			"took":   time.Since(startTime),
		}).Errorf(color.RedString("解码 cicd.push_data 失败: %v"), err)
		return nil, err
	}

	if len(docs) == 0 {
		return nil, nil // 无数据视为首次
	}

	// 收集去重 services 和 environments
	serviceSet := make(map[string]struct{})
	envSet := make(map[string]struct{})
	for _, doc := range docs {
		if s, ok := doc["service"].(string); ok {
			serviceSet[s] = struct{}{}
		}
		if e, ok := doc["environment"].(string); ok {
			envSet[e] = struct{}{}
		}
	}

	services := make([]string, 0, len(serviceSet))
	for s := range serviceSet {
		services = append(services, s)
	}
	envs := make([]string, 0, len(envSet))
	for e := range envSet {
		envs = append(envs, e)
	}

	storedData := &models.PushData{
		Services:     services,
		Environments: envs,
		UpdatedAt:    time.Now(),
	}

	logrus.WithFields(logrus.Fields{
		"time":   time.Now().Format("2006-01-02 15:04:05"),
		"method": "GetPushData",
		"took":   time.Since(startTime),
	}).Infof(color.GreenString("推送数据获取成功从 cicd.push_data (从 %d 文档收集 %d 服务 %d 环境)"), len(docs), len(services), len(envs))
	return storedData, nil
}

// HasChanges 检查当前发现数据是否有变化（返回 true 如果变化）
func (m *MongoClient) HasChanges(currentServices, currentEnvs []string, stored *models.PushData) bool {
	if stored == nil {
		return true // 首次，无存储数据
	}

	// 去重当前
	currentServiceSet := make(map[string]struct{})
	for _, s := range currentServices {
		currentServiceSet[s] = struct{}{}
	}
	currentEnvSet := make(map[string]struct{})
	for _, e := range currentEnvs {
		currentEnvSet[e] = struct{}{}
	}

	// 比较
	storedServiceSet := make(map[string]struct{})
	for _, s := range stored.Services {
		storedServiceSet[s] = struct{}{}
	}
	if len(currentServiceSet) != len(storedServiceSet) {
		return true
	}
	for s := range currentServiceSet {
		if _, ok := storedServiceSet[s]; !ok {
			return true
		}
	}

	storedEnvSet := make(map[string]struct{})
	for _, e := range stored.Environments {
		storedEnvSet[e] = struct{}{}
	}
	if len(currentEnvSet) != len(storedEnvSet) {
		return true
	}
	for e := range currentEnvSet {
		if _, ok := storedEnvSet[e]; !ok {
			return true
		}
	}

	return false
}

// Close 关闭连接
func (m *MongoClient) Close() error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return m.client.Disconnect(ctx)
}