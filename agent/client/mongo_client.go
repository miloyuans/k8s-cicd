package client

import (
	"context"
	"fmt"
	"time"

	"k8s-cicd/agent/config"
	"k8s-cicd/agent/models"

	"github.com/sirupsen/logrus"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
	"go.mongodb.org/mongo-driver/mongo/readpref"
)

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
		}).Errorf("MongoDB 连接失败: %v", err)
		return nil, fmt.Errorf("MongoDB 连接失败: %v", err)
	}

	// 步骤3：测试连接
	ctx := context.Background()
	if err := client.Ping(ctx, readpref.Primary()); err != nil {
		logrus.WithFields(logrus.Fields{
			"time":   time.Now().Format("2006-01-02 15:04:05"),
			"method": "NewMongoClient",
			"took":   time.Since(startTime),
		}).Errorf("MongoDB ping 失败: %v", err)
		return nil, fmt.Errorf("MongoDB ping 失败: %v", err)
	}

	// 步骤4：创建 TTL 索引
	if err := createTTLIndexes(client, cfg); err != nil {
		logrus.WithFields(logrus.Fields{
			"time":   time.Now().Format("2006-01-02 15:04:05"),
			"method": "NewMongoClient",
			"took":   time.Since(startTime),
		}).Errorf("创建 TTL 索引失败: %v", err)
		return nil, err
	}

	logrus.WithFields(logrus.Fields{
		"time":   time.Now().Format("2006-01-02 15:04:05"),
		"method": "NewMongoClient",
		"took":   time.Since(startTime),
	}).Info("MongoDB 连接成功")
	return &MongoClient{client: client, cfg: cfg}, nil
}

// StorePushData 存储推送数据到 pushlist 数据库
func (m *MongoClient) StorePushData(data *models.PushData) error {
	startTime := time.Now()
	ctx := context.Background()

	collection := m.client.Database("pushlist").Collection("push_data")
	data.UpdatedAt = time.Now()

	// 使用 Upsert 模式：替换整个文档
	filter := bson.M{"_id": "global_push_data"} // 固定 ID 用于全量存储
	update := bson.M{"$set": data}

	_, err := collection.UpdateOne(ctx, filter, update, options.Update().SetUpsert(true))
	if err != nil {
		logrus.WithFields(logrus.Fields{
			"time":   time.Now().Format("2006-01-02 15:04:05"),
			"method": "StorePushData",
			"took":   time.Since(startTime),
		}).Errorf("存储推送数据失败: %v", err)
		return err
	}

	logrus.WithFields(logrus.Fields{
		"time":   time.Now().Format("2006-01-02 15:04:05"),
		"method": "StorePushData",
		"took":   time.Since(startTime),
		"data": logrus.Fields{
			"service_count": len(data.Services),
			"env_count":     len(data.Environments),
		},
	}).Infof(color.GreenString("推送数据存储成功"))
	return nil
}

// GetPushData 获取存储的推送数据
func (m *MongoClient) GetPushData() (*models.PushData, error) {
	startTime := time.Now()
	ctx := context.Background()

	collection := m.client.Database("pushlist").Collection("push_data")
	filter := bson.M{"_id": "global_push_data"}

	var data models.PushData
	err := collection.FindOne(ctx, filter).Decode(&data)
	if err == mongo.ErrNoDocuments {
		return nil, nil // 无数据视为首次
	}
	if err != nil {
		logrus.WithFields(logrus.Fields{
			"time":   time.Now().Format("2006-01-02 15:04:05"),
			"method": "GetPushData",
			"took":   time.Since(startTime),
		}).Errorf("获取推送数据失败: %v", err)
		return nil, err
	}

	logrus.WithFields(logrus.Fields{
		"time":   time.Now().Format("2006-01-02 15:04:05"),
		"method": "GetPushData",
		"took":   time.Since(startTime),
	}).Infof(color.GreenString("推送数据获取成功"))
	return &data, nil
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

// createTTLIndexes 创建 TTL 索引
func createTTLIndexes(client *mongo.Client, cfg *config.MongoConfig) error {
	ctx := context.Background()

	// 1. 为每个环境的任务集合创建 TTL 索引
	for env := range cfg.EnvMapping.Mappings {
		collection := client.Database("cicd").Collection(fmt.Sprintf("tasks_%s", env))
		_, err := collection.Indexes().CreateOne(ctx, mongo.IndexModel{
			Keys:    bson.D{{Key: "created_at", Value: 1}},
			Options: options.Index().SetExpireAfterSeconds(int32(cfg.TTL.Seconds())),
		})
		if err != nil {
			return fmt.Errorf("创建任务 TTL 索引失败 [%s]: %v", env, err)
		}
	}

	// 2. 为版本集合创建唯一索引
	versionsColl := client.Database("cicd").Collection("versions")
	_, err := versionsColl.Indexes().CreateOne(ctx, mongo.IndexModel{
		Keys:    bson.D{{Key: "service", Value: 1}, {Key: "version", Value: 1}},
		Options: options.Index().SetUnique(true),
	})
	if err != nil {
		return fmt.Errorf("创建版本唯一索引失败: %v", err)
	}

	return nil
}

// GetClient 获取 MongoDB 客户端
func (m *MongoClient) GetClient() *mongo.Client {
	return m.client
}

// GetEnvMappings 获取环境映射
func (m *MongoClient) GetEnvMappings() map[string]string {
	if m.cfg == nil || m.cfg.EnvMapping.Mappings == nil {
		return make(map[string]string)
	}
	return m.cfg.EnvMapping.Mappings
}

// GetTasksByStatus 查询指定环境和状态的任务
func (m *MongoClient) GetTasksByStatus(env, status string) ([]models.DeployRequest, error) {
	startTime := time.Now()
	ctx := context.Background()

	collection := m.client.Database("cicd").Collection(fmt.Sprintf("tasks_%s", env))
	filter := bson.M{
		"confirmation_status": status,
		"status": bson.M{"$nin": []string{"success", "failure"}}, // 避免重复执行
	}

	cursor, err := collection.Find(ctx, filter)
	if err != nil {
		logrus.WithFields(logrus.Fields{
			"time":   time.Now().Format("2006-01-02 15:04:05"),
			"method": "GetTasksByStatus",
			"env":    env,
			"status": status,
			"took":   time.Since(startTime),
		}).Errorf("查询任务失败: %v", err)
		return nil, err
	}
	defer cursor.Close(ctx)

	var tasks []models.DeployRequest
	if err := cursor.All(ctx, &tasks); err != nil {
		logrus.WithFields(logrus.Fields{
			"time":   time.Now().Format("2006-01-02 15:04:05"),
			"method": "GetTasksByStatus",
			"took":   time.Since(startTime),
		}).Errorf("解码任务失败: %v", err)
		return nil, err
	}

	logrus.WithFields(logrus.Fields{
		"time":   time.Now().Format("2006-01-02 15:04:05"),
		"method": "GetTasksByStatus",
		"env":    env,
		"status": status,
		"count":  len(tasks),
		"took":   time.Since(startTime),
	}).Infof("查询到 %d 个任务", len(tasks))
	return tasks, nil
}

// UpdateTaskStatus 更新任务状态
func (m *MongoClient) UpdateTaskStatus(service, version, env, user, status string) error {
	startTime := time.Now()
	ctx := context.Background()

	collection := m.client.Database("cicd").Collection(fmt.Sprintf("tasks_%s", env))
	filter := bson.M{
		"service":     service,
		"version":     version,
		"environment": env,
		"user":        user,
	}
	update := bson.M{
		"$set": bson.M{
			"status":      status,
			"updated_at":  time.Now(),
		},
	}

	result, err := collection.UpdateOne(ctx, filter, update)
	if err != nil {
		logrus.WithFields(logrus.Fields{
			"time":   time.Now().Format("2006-01-02 15:04:05"),
			"method": "UpdateTaskStatus",
			"took":   time.Since(startTime),
		}).Errorf("更新任务状态失败: %v", err)
		return err
	}

	if result.MatchedCount == 0 {
		logrus.WithFields(logrus.Fields{
			"time":   time.Now().Format("2006-01-02 15:04:05"),
			"method": "UpdateTaskStatus",
			"took":   time.Since(startTime),
		}).Warnf("未找到匹配任务: %s v%s [%s]", service, version, env)
		return fmt.Errorf("任务未找到")
	}

	logrus.WithFields(logrus.Fields{
		"time":   time.Now().Format("2006-01-02 15:04:05"),
		"method": "UpdateTaskStatus",
		"took":   time.Since(startTime),
	}).Infof("任务状态更新成功: %s v%s [%s] -> %s", service, version, env, status)
	return nil
}

// StoreImageSnapshot 存储镜像快照
func (m *MongoClient) StoreImageSnapshot(snapshot *models.ImageSnapshot, taskID string) error {
	startTime := time.Now()
	ctx := context.Background()

	snapshot.TaskID = taskID
	snapshot.RecordedAt = time.Now()

	collection := m.client.Database("cicd").Collection("image_snapshots")
	_, err := collection.InsertOne(ctx, snapshot)
	if err != nil {
		logrus.WithFields(logrus.Fields{
			"time":   time.Now().Format("2006-01-02 15:04:05"),
			"method": "StoreImageSnapshot",
			"took":   time.Since(startTime),
		}).Errorf("存储快照失败: %v", err)
		return err
	}

	logrus.WithFields(logrus.Fields{
		"time":   time.Now().Format("2006-01-02 15:04:05"),
		"method": "StoreImageSnapshot",
		"took":   time.Since(startTime),
	}).Infof("快照存储成功: %s", taskID)
	return nil
}

// CheckDuplicateTask 检查任务是否重复
func (m *MongoClient) CheckDuplicateTask(task models.DeployRequest) (bool, error) {
	startTime := time.Now()
	ctx := context.Background()

	collection := m.client.Database("cicd").Collection(fmt.Sprintf("tasks_%s", task.Environments[0]))
	filter := bson.M{
		"service":     task.Service,
		"version":     task.Version,
		"environment": task.Environments[0],
	}

	count, err := collection.CountDocuments(ctx, filter)
	if err != nil {
		logrus.WithFields(logrus.Fields{
			"time":   time.Now().Format("2006-01-02 15:04:05"),
			"method": "CheckDuplicateTask",
			"took":   time.Since(startTime),
		}).Errorf("检查重复任务失败: %v", err)
		return false, err
	}

	isDuplicate := count > 0
	logrus.WithFields(logrus.Fields{
		"time":   time.Now().Format("2006-01-02 15:04:05"),
		"method": "CheckDuplicateTask",
		"took":   time.Since(startTime),
	}).Infof("任务重复检查: %t", isDuplicate)
	return isDuplicate, nil
}

// Close 关闭连接
func (m *MongoClient) Close() error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return m.client.Disconnect(ctx)
}