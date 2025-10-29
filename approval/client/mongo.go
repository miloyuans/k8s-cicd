package client

import (
	"context"
	"fmt"
	"time"

	"k8s-cicd/approval/config"
	"k8s-cicd/approval/models"

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
	}).Info("k8s-approval MongoDB 连接成功")
	return &MongoClient{client: client, cfg: cfg}, nil
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

	// 2. 为 popup_message 集合创建 TTL 索引
	popupColl := client.Database("cicd").Collection("popup_messages")
	_, err := popupColl.Indexes().CreateOne(ctx, mongo.IndexModel{
		Keys:    bson.D{{Key: "sent_at", Value: 1}},
		Options: options.Index().SetExpireAfterSeconds(7 * 24 * 60 * 60), // 7 天
	})
	if err != nil {
		return fmt.Errorf("创建弹窗消息 TTL 索引失败: %v", err)
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

// GetPendingTasks 查询 pending 且未发送弹窗的任务
func (m *MongoClient) GetPendingTasks(env string) ([]models.DeployRequest, error) {
	startTime := time.Now()

	coll := m.GetClient().Database("cicd").Collection(fmt.Sprintf("tasks_%s", env))
	filter := bson.M{
		"environment":         env,
		"confirmation_status": "pending",
		"popup_sent":          bson.M{"$ne": true}, // 关键：未发送弹窗
	}

	cursor, err := coll.Find(context.Background(), filter)
	if err != nil {
		logrus.Errorf("查询 %s 待确认任务失败: %v", env, err)
		return nil, err
	}
	defer cursor.Close(context.Background())

	var tasks []models.DeployRequest
	if err := cursor.All(context.Background(), &tasks); err != nil {
		return nil, err
	}

	logrus.WithFields(logrus.Fields{
		"time":   time.Now().Format("2006-01-02 15:04:05"),
		"method": "GetPendingTasks",
		"env":    env,
		"count":  len(tasks),
		"took":   time.Since(startTime),
	}).Infof("查询到 %d 个待确认任务", len(tasks))

	return tasks, nil
}

// MarkPopupSent 标记任务已发送弹窗
func (m *MongoClient) MarkPopupSent(taskID string, messageID int) error {
	startTime := time.Now()
	ctx := context.Background()

	// 1. 更新任务集合
	for env := range m.cfg.EnvMapping.Mappings {
		collection := m.client.Database("cicd").Collection(fmt.Sprintf("tasks_%s", env))
		filter := bson.M{"task_id": taskID}
		update := bson.M{
			"$set": bson.M{
				"popup_sent":      true,
				"popup_message_id": messageID,
				"popup_sent_at":   time.Now(),
			},
		}
		result, err := collection.UpdateOne(ctx, filter, update)
		if err != nil {
			continue
		}
		if result.MatchedCount > 0 {
			logrus.WithFields(logrus.Fields{
				"time":   time.Now().Format("2006-01-02 15:04:05"),
				"method": "MarkPopupSent",
				"task_id": taskID,
				"took":    time.Since(startTime),
			}).Infof("任务弹窗标记成功")
			break
		}
	}

	// 2. 记录弹窗消息（用于防重）
	popupColl := m.client.Database("cicd").Collection("popup_messages")
	_, err := popupColl.InsertOne(ctx, bson.M{
		"task_id":     taskID,
		"message_id":  messageID,
		"sent_at":     time.Now(),
	})
	if err != nil {
		logrus.Warnf("记录弹窗消息失败: %v", err)
	}

	return nil
}

// UpdateTaskStatus 更新任务确认状态
func (m *MongoClient) UpdateTaskStatus(taskID, status string) error {
	startTime := time.Now()
	ctx := context.Background()

	for env := range m.cfg.EnvMapping.Mappings {
		collection := m.client.Database("cicd").Collection(fmt.Sprintf("tasks_%s", env))
		filter := bson.M{"task_id": taskID}
		update := bson.M{
			"$set": bson.M{
				"confirmation_status": status,
				"updated_at":          time.Now(),
			},
		}
		result, err := collection.UpdateOne(ctx, filter, update)
		if err != nil {
			continue
		}
		if result.MatchedCount > 0 {
			logrus.WithFields(logrus.Fields{
				"time":   time.Now().Format("2006-01-02 15:04:05"),
				"method": "UpdateTaskStatus",
				"task_id": taskID,
				"status":  status,
				"took":    time.Since(startTime),
			}).Infof("任务状态更新成功")
			return nil
		}
	}

	logrus.WithFields(logrus.Fields{
		"time":   time.Now().Format("2006-01-02 15:04:05"),
		"method": "UpdateTaskStatus",
		"took":   time.Since(startTime),
	}).Warnf("未找到任务 task_id=%s", taskID)
	return fmt.Errorf("任务未找到")
}

// GetPushedServicesAndEnvs 获取所有已 push 的服务和环境
func (m *MongoClient) GetPushedServicesAndEnvs() ([]string, []string, error) {
	services := make(map[string]bool)
	envs := make(map[string]bool)

	for env := range m.cfg.EnvMapping.Mappings {
		coll := m.client.Database("cicd").Collection(fmt.Sprintf("tasks_%s", env))
		cursor, err := coll.Find(context.Background(), bson.M{})
		if err != nil {
			continue
		}
		var tasks []models.DeployRequest
		cursor.All(context.Background(), &tasks)
		for _, t := range tasks {
			services[t.Service] = true
			envs[env] = true
		}
	}

	var serviceList, envList []string
	for s := range services {
		serviceList = append(serviceList, s)
	}
	for e := range envs {
		envList = append(envList, e)
	}
	return serviceList, envList, nil
}

// StoreTaskIfNotExists 存储任务（防重：service+version+env 唯一）
func (m *MongoClient) StoreTaskIfNotExists(task models.DeployRequest) error {
	startTime := time.Now()
	task.PopupSent = false
	// 1. 验证环境
	if len(task.Environments) == 0 {
		return fmt.Errorf("task.Environments 不能为空")
	}
	env := task.Environments[0]

	// 2. 获取集合
	coll := m.GetClient().Database("cicd").Collection(fmt.Sprintf("tasks_%s", env))

	// 3. 唯一键过滤
	filter := bson.M{
		"service":     task.Service,
		"version":     task.Version,
		"environment": env,
	}

	// 4. 仅在不存在时插入
	opts := options.Update().SetUpsert(true)
	update := bson.M{"$setOnInsert": task}

	result, err := coll.UpdateOne(context.Background(), filter, update, opts)
	if err != nil {
		logrus.WithFields(logrus.Fields{
			"time":    time.Now().Format("2006-01-02 15:04:05"),
			"method":  "StoreTaskIfNotExists",
			"task_id": task.TaskID,
			"took":    time.Since(startTime),
		}).Errorf("存储任务失败: %v", err)
		return err
	}

	// 5. 判断是否插入
	if result.UpsertedCount > 0 {
		logrus.WithFields(logrus.Fields{
			"time":    time.Now().Format("2006-01-02 15:04:05"),
			"method":  "StoreTaskIfNotExists",
			"task_id": task.TaskID,
			"took":    time.Since(startTime),
		}).Infof("任务存储成功（新插入）")
	} else {
		logrus.WithFields(logrus.Fields{
			"time":    time.Now().Format("2006-01-02 15:04:05"),
			"method":  "StoreTaskIfNotExists",
			"task_id": task.TaskID,
			"took":    time.Since(startTime),
		}).Debugf("任务已存在（跳过）")
	}

	return nil
}

// GetTaskByID 根据 task_id 获取任务
func (m *MongoClient) GetTaskByID(taskID string) (*models.DeployRequest, error) {
	startTime := time.Now()
	ctx := context.Background()

	for env := range m.cfg.EnvMapping.Mappings {
		collection := m.client.Database("cicd").Collection(fmt.Sprintf("tasks_%s", env))
		filter := bson.M{
			"task_id": taskID,
			"confirmation_status": bson.M{"$in": []string{"pending", "confirmed", "rejected"}},
		}
		var task models.DeployRequest
		if err := collection.FindOne(ctx, filter).Decode(&task); err == nil {
			logrus.WithFields(logrus.Fields{
				"time":   time.Now().Format("2006-01-02 15:04:05"),
				"method": "GetTaskByID",
				"task_id": taskID,
				"took":    time.Since(startTime),
			}).Infof("任务查询成功")
			return &task, nil
		}
	}

	logrus.WithFields(logrus.Fields{
		"time":   time.Now().Format("2006-01-02 15:04:05"),
		"method": "GetTaskByID",
		"took":   time.Since(startTime),
	}).Warnf("任务未找到 task_id=%s", taskID)
	return nil, fmt.Errorf("task not found")
}

// Close 关闭连接
func (m *MongoClient) Close() error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return m.client.Disconnect(ctx)
}