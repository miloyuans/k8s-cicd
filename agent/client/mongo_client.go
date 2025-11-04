// 文件: client/mongo_client.go
// 修改: 移除无效的filter增强（依赖update防重复）；保留Limit(100)以防过多任务；新增 UpdateConfirmationStatus 方法。
// 保留所有现有功能。

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

	// 唯一索引: service + environment (防重复组合)
	_, err = pushDataColl.Indexes().CreateOne(ctx, mongo.IndexModel{
		Keys: bson.D{
			{Key: "service", Value: 1},
			{Key: "environment", Value: 1},
		},
		Options: options.Index().SetUnique(true),
	})
	if err != nil {
		return fmt.Errorf("创建 push_data 唯一索引失败: %v", err)
	}

	return nil
}

// GetTasksByStatus 获取指定状态的任务（按 created_at 排序；添加Limit避免过多）
func (m *MongoClient) GetTasksByStatus(env, status string) ([]models.DeployRequest, error) {
	startTime := time.Now()
	ctx := context.Background()
	sanitizedEnv := sanitizeEnv(env)
	collection := m.client.Database("cicd").Collection(fmt.Sprintf("tasks_%s", sanitizedEnv))

	filter := bson.M{"confirmation_status": status}  // 原filter，依赖update防重复

	opts := options.Find().
		SetSort(bson.D{{Key: "created_at", Value: 1}}).  // 按创建时间升序
		SetLimit(100)  // 限制结果数，避免过多

	cursor, err := collection.Find(ctx, filter, opts)
	if err != nil {
		logrus.WithFields(logrus.Fields{
			"time":   time.Now().Format("2006-01-02 15:04:05"),
			"method": "GetTasksByStatus",
			"took":   time.Since(startTime),
			"env":    env,
		}).Errorf(color.RedString("查询任务失败 [%s]: %v"), env, err)
		return nil, err
	}
	defer cursor.Close(ctx)

	var tasks []models.DeployRequest
	if err = cursor.All(ctx, &tasks); err != nil {
		logrus.WithFields(logrus.Fields{
			"time":   time.Now().Format("2006-01-02 15:04:05"),
			"method": "GetTasksByStatus",
			"took":   time.Since(startTime),
			"env":    env,
		}).Errorf(color.RedString("解码任务失败 [%s]: %v"), env, err)
		return nil, err
	}

	logrus.WithFields(logrus.Fields{
		"time":   time.Now().Format("2006-01-02 15:04:05"),
		"method": "GetTasksByStatus",
		"took":   time.Since(startTime),
		"env":    env,
		"count":  len(tasks),
	}).Infof(color.GreenString("查询任务成功 [%s, 集合: tasks_%s]: %d 个"), env, sanitizedEnv, len(tasks))
	return tasks, nil
}

// UpdateTaskStatus 更新任务状态（按 service+version+env）
func (m *MongoClient) UpdateTaskStatus(service, version, env, user, status string) error {
	startTime := time.Now()
	ctx := context.Background()
	sanitizedEnv := sanitizeEnv(env)
	collection := m.client.Database("cicd").Collection(fmt.Sprintf("tasks_%s", sanitizedEnv))

	filter := bson.M{
		"service":     service,
		"version":     version,
		"environment": env,
	}
	update := bson.M{
		"$set": bson.M{
			"status":     status,
			"updated_at": time.Now(),
		},
	}

	result, err := collection.UpdateOne(ctx, filter, update)
	if err != nil {
		logrus.WithFields(logrus.Fields{
			"time":   time.Now().Format("2006-01-02 15:04:05"),
			"method": "UpdateTaskStatus",
			"took":   time.Since(startTime),
			"env":    env,
		}).Errorf(color.RedString("更新任务状态失败 [%s]: %v"), env, err)
		return err
	}
	if result.MatchedCount == 0 {
		return fmt.Errorf("未找到匹配任务: %s-%s in %s", service, version, env)
	}

	logrus.WithFields(logrus.Fields{
		"time":   time.Now().Format("2006-01-02 15:04:05"),
		"method": "UpdateTaskStatus",
		"took":   time.Since(startTime),
		"data":   logrus.Fields{"service": service, "version": version, "env": env, "status": status},
	}).Infof(color.GreenString("任务状态更新成功 [%s]: %s"), env, status)
	return nil
}

// UpdateConfirmationStatus 更新confirmation_status（按TaskID，遍历环境查找）
func (m *MongoClient) UpdateConfirmationStatus(taskID, newStatus string) error {
	startTime := time.Now()
	ctx := context.Background()

	// 遍历所有环境集合，查找并更新TaskID
	for env := range m.cfg.EnvMapping.Mappings {
		sanitizedEnv := sanitizeEnv(env)
		collection := m.client.Database("cicd").Collection(fmt.Sprintf("tasks_%s", sanitizedEnv))

		filter := bson.M{"task_id": taskID}
		update := bson.M{
			"$set": bson.M{
				"confirmation_status": newStatus,
				"updated_at":          time.Now(),
			},
		}

		result, err := collection.UpdateOne(ctx, filter, update)
		if err != nil {
			logrus.WithFields(logrus.Fields{
				"time":   time.Now().Format("2006-01-02 15:04:05"),
				"method": "UpdateConfirmationStatus",
				"took":   time.Since(startTime),
				"env":    env,
			}).Errorf(color.RedString("更新 confirmation_status 失败 [%s]: %v"), env, err)
			continue  // 继续下一个环境
		}
		if result.MatchedCount > 0 {
			logrus.WithFields(logrus.Fields{
				"time":   time.Now().Format("2006-01-02 15:04:05"),
				"method": "UpdateConfirmationStatus",
				"took":   time.Since(startTime),
				"data":   logrus.Fields{"task_id": taskID, "new_status": newStatus},
			}).Infof(color.GreenString("confirmation_status 更新成功: %s -> %s"), taskID, newStatus)
			return nil  // 找到并更新，返回
		}
	}
	return fmt.Errorf("未找到任务ID: %s", taskID)
}

// SnapshotImage 快照镜像（存入 image_snapshots）
func (m *MongoClient) SnapshotImage(snapshot *models.ImageSnapshot) error {
	startTime := time.Now()
	ctx := context.Background()
	collection := m.client.Database("cicd").Collection("image_snapshots")

	_, err := collection.InsertOne(ctx, snapshot)
	if err != nil {
		logrus.WithFields(logrus.Fields{
			"time":   time.Now().Format("2006-01-02 15:04:05"),
			"method": "SnapshotImage",
			"took":   time.Since(startTime),
		}).Errorf(color.RedString("快照插入失败: %v"), err)
		return err
	}

	logrus.WithFields(logrus.Fields{
		"time":   time.Now().Format("2006-01-02 15:04:05"),
		"method": "SnapshotImage",
		"took":   time.Since(startTime),
		"data":   logrus.Fields{"service": snapshot.Service, "namespace": snapshot.Namespace, "image": snapshot.Image},
	}).Info(color.GreenString("镜像快照存储成功"))
	return nil
}

// GetSnapshotByTaskID 获取快照（按 task_id）
func (m *MongoClient) GetSnapshotByTaskID(taskID string) (*models.ImageSnapshot, error) {
	startTime := time.Now()
	ctx := context.Background()
	collection := m.client.Database("cicd").Collection("image_snapshots")

	var snapshot models.ImageSnapshot
	filter := bson.M{"task_id": taskID}
	err := collection.FindOne(ctx, filter).Decode(&snapshot)
	if err != nil {
		if err == mongo.ErrNoDocuments {
			return nil, nil  // 无快照视为正常
		}
		logrus.WithFields(logrus.Fields{
			"time":   time.Now().Format("2006-01-02 15:04:05"),
			"method": "GetSnapshotByTaskID",
			"took":   time.Since(startTime),
		}).Errorf(color.RedString("查询快照失败: %v"), err)
		return nil, err
	}

	logrus.WithFields(logrus.Fields{
		"time":   time.Now().Format("2006-01-02 15:04:05"),
		"method": "GetSnapshotByTaskID",
		"took":   time.Since(startTime),
		"data":   logrus.Fields{"task_id": taskID},
	}).Info(color.GreenString("快照查询成功"))
	return &snapshot, nil
}

// StorePushData 存储推送数据（清空重插去重组合）
func (m *MongoClient) StorePushData(data *models.PushData) error {
	startTime := time.Now()
	ctx := context.Background()
	collection := m.client.Database("cicd").Collection("push_data")

	// 清空旧数据
	_, err := collection.DeleteMany(ctx, bson.M{})
	if err != nil {
		logrus.WithFields(logrus.Fields{
			"time":   time.Now().Format("2006-01-02 15:04:05"),
			"method": "StorePushData",
			"took":   time.Since(startTime),
		}).Errorf(color.RedString("清空 push_data 失败: %v"), err)
		return err
	}

	// 去重
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