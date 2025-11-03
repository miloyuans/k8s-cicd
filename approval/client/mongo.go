// 文件: mongo.go (恢复 GetPushedServicesAndEnvs 函数，从 pushlist.push_data 查询服务/环境列表；
// 其他功能保留不变)
package client

import (
	"context"
	"fmt"
	"sort"
	"time"

	"k8s-cicd/approval/config"
	"k8s-cicd/approval/models"

	"github.com/google/uuid"
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

// 修复: NewMongoClient 完整，确保 client 定义和传递
func NewMongoClient(cfg *config.MongoConfig) (*MongoClient, error) {
	startTime := time.Now()

	// 步骤1：创建客户端配置
	clientOptions := options.Client().
		ApplyURI(cfg.URI).
		SetConnectTimeout(5*time.Second).
		SetMaxPoolSize(10).
		SetMinPoolSize(2)

	// 步骤2：连接 MongoDB
	client, err := mongo.Connect(context.Background(), clientOptions) // client 定义
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
	if err := client.Ping(ctx, readpref.Primary()); err != nil { // readpref 使用
		logrus.WithFields(logrus.Fields{
			"time":   time.Now().Format("2006-01-02 15:04:05"),
			"method": "NewMongoClient",
			"took":   time.Since(startTime),
		}).Errorf("MongoDB ping 失败: %v", err)
		return nil, fmt.Errorf("MongoDB ping 失败: %v", err)
	}

	// 步骤4：创建索引
	if err := createIndexes(client, cfg); err != nil { // client 传递
		logrus.WithFields(logrus.Fields{
			"time":   time.Now().Format("2006-01-02 15:04:05"),
			"method": "NewMongoClient",
			"took":   time.Since(startTime),
		}).Errorf("创建索引失败: %v", err)
		return nil, err
	}

	logrus.WithFields(logrus.Fields{
		"time":   time.Now().Format("2006-01-02 15:04:05"),
		"method": "NewMongoClient",
		"took":   time.Since(startTime),
	}).Info("k8s-approval MongoDB 连接成功")
	return &MongoClient{client: client, cfg: cfg}, nil
}

// 新增: createIndexes 创建 TTL + 复合唯一索引 + created_at 排序索引
// 修复: createIndexes 完整，确保 client param 定义 (line 33/47: client.Database)
// 修复: createIndexes 移除单独 created_at_asc 索引 (TTL 已支持 {created_at: 1} 排序)
func createIndexes(client *mongo.Client, cfg *config.MongoConfig) error { // client param
	ctx := context.Background()

	// 1. 为每个环境的任务集合创建索引
	for env := range cfg.EnvMapping.Mappings {
		collection := client.Database("cicd").Collection(fmt.Sprintf("tasks_%s", env)) // client 定义

		// TTL 索引 (keys: created_at:1 + expireAfterSeconds，支持排序)
		_, err := collection.Indexes().CreateOne(ctx, mongo.IndexModel{
			Keys:    bson.D{{Key: "created_at", Value: 1}},
			Options: options.Index().SetExpireAfterSeconds(int32(cfg.TTL.Seconds())),
		})
		if err != nil {
			return fmt.Errorf("创建任务 TTL 索引失败 [%s]: %v", env, err)
		}

		// 复合唯一索引 (防重: service+version+environment+created_at，支持排序)
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
			return fmt.Errorf("创建复合唯一索引失败 [%s]: %v", env, err)
		}

		// 移除: 单独 created_at_asc 索引 (TTL/复合已覆盖排序需求，避免冲突)
	}

	// 2. popup_messages TTL (不变)
	popupColl := client.Database("cicd").Collection("popup_messages") // client 定义
	_, err := popupColl.Indexes().CreateOne(ctx, mongo.IndexModel{
		Keys:    bson.D{{Key: "sent_at", Value: 1}},
		Options: options.Index().SetExpireAfterSeconds(7 * 24 * 60 * 60),
	})
	if err != nil {
		return fmt.Errorf("创建弹窗消息 TTL 索引失败: %v", err)
	}

	return nil
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
// 修复: GetPendingTasks 过滤 "confirmation_status": "待确认"，确保匹配弹窗逻辑
// 确认: GetPendingTasks 严格过滤 "confirmation_status": "待确认"，用于弹窗触发
// 修改: GetPendingTasks 添加排序支持，按 created_at 升序
func (m *MongoClient) GetPendingTasks(env string) ([]models.DeployRequest, error) {
	startTime := time.Now()
	ctx := context.Background()

	collection := m.client.Database("cicd").Collection(fmt.Sprintf("tasks_%s", env))

	filter := bson.M{
		"confirmation_status": "待确认",
		"popup_sent":          bson.M{"$ne": true}, // 未发送弹窗
	}

	// 排序: created_at 升序 (最早的任务优先)
	opts := options.Find().SetSort(bson.D{{Key: "created_at", Value: 1}})

	cursor, err := collection.Find(ctx, filter, opts)
	if err != nil {
		logrus.WithFields(logrus.Fields{
			"time":   time.Now().Format("2006-01-02 15:04:05"),
			"method": "GetPendingTasks",
			"env":    env,
		}).Errorf("查询待确认任务失败: %v", err)
		return nil, err
	}
	defer cursor.Close(ctx)

	var tasks []models.DeployRequest
	if err := cursor.All(ctx, &tasks); err != nil {
		logrus.WithFields(logrus.Fields{
			"time":   time.Now().Format("2006-01-02 15:04:05"),
			"method": "GetPendingTasks",
			"env":    env,
		}).Errorf("解码任务列表失败: %v", err)
		return nil, err
	}

	logrus.WithFields(logrus.Fields{
		"time":   time.Now().Format("2006-01-02 15:04:05"),
		"method": "GetPendingTasks",
		"env":    env,
		"count":  len(tasks),
		"took":   time.Since(startTime),
	}).Infof("查询到 %d 个待确认任务 (popup_sent=false, sorted by created_at asc)", len(tasks))

	return tasks, nil
}

// 修改: GetPushedServicesAndEnvs 扫描 push_data 集合，提取唯一服务和环境列表（支持排序）
func (m *MongoClient) GetPushedServicesAndEnvs() ([]string, []string, error) {
	startTime := time.Now()
	ctx := context.Background()

	// 获取 push_data 集合
	collection := m.client.Database("pushlist").Collection("push_data")

	// 查询所有文档
	cursor, err := collection.Find(ctx, bson.M{})
	if err != nil {
		logrus.WithFields(logrus.Fields{
			"time":   time.Now().Format("2006-01-02 15:04:05"),
			"method": "GetPushedServicesAndEnvs",
		}).Errorf("查询 push_data 失败: %v", err)
		return nil, nil, err
	}
	defer cursor.Close(ctx)

	// 使用 map 去重
	serviceSet := make(map[string]struct{})
	envSet := make(map[string]struct{})

	var doc bson.M
	for cursor.Next(ctx) {
		if err := cursor.Decode(&doc); err != nil {
			logrus.WithFields(logrus.Fields{
				"time":   time.Now().Format("2006-01-02 15:04:05"),
				"method": "GetPushedServicesAndEnvs",
			}).Errorf("解码文档失败: %v", err)
			continue
		}

		// 提取 service 和 environment（假设字段名为 "service" 和 "environment"）
		if service, ok := doc["service"].(string); ok && service != "" {
			serviceSet[service] = struct{}{}
		}
		if env, ok := doc["environment"].(string); ok && env != "" {
			envSet[env] = struct{}{}
		}
	}

	if err := cursor.Err(); err != nil {
		logrus.WithFields(logrus.Fields{
			"time":   time.Now().Format("2006-01-02 15:04:05"),
			"method": "GetPushedServicesAndEnvs",
		}).Errorf("游标错误: %v", err)
		return nil, nil, err
	}

	// 转换为排序后的切片
	serviceList := make([]string, 0, len(serviceSet))
	for service := range serviceSet {
		serviceList = append(serviceList, service)
	}
	sort.Strings(serviceList)

	envList := make([]string, 0, len(envSet))
	for env := range envSet {
		envList = append(envList, env)
	}
	sort.Strings(envList)

	logrus.WithFields(logrus.Fields{
		"time":     time.Now().Format("2006-01-02 15:04:05"),
		"method":   "GetPushedServicesAndEnvs",
		"services": serviceList,
		"envs":     envList,
		"took":     time.Since(startTime),
	}).Infof("扫描完成: 服务 %v, 环境 %v (排序支持)", serviceList, envList)

	return serviceList, envList, nil
}

// 修改: StoreTaskIfNotExistsEnv 生成 UUID TaskID + 设置 CreatedAt (并发安全)
func (m *MongoClient) StoreTaskIfNotExistsEnv(task models.DeployRequest, env string) error {
	startTime := time.Now()

	// 验证环境
	if len(task.Environments) == 0 || task.Environments[0] != env {
		return fmt.Errorf("task.Environments[0] 必须为 %s", env)
	}

	// 新增: 生成 UUID TaskID (全局唯一，避免并发冲突)
	if task.TaskID == "" {
		task.TaskID = uuid.New().String() // UUID v4
	}

	// 新增: 设置 CreatedAt (当前时间，纳秒级，用于排序)
	task.CreatedAt = time.Now()

	// 获取集合 (env-specific)
	coll := m.GetClient().Database("cicd").Collection(fmt.Sprintf("tasks_%s", env))

	// 唯一键过滤 (env-specific + created_at 防同秒重)
	filter := bson.M{
		"service":     task.Service,
		"version":     task.Version,
		"environment": env,
		"created_at":  task.CreatedAt, // 包含时间精确防重
	}

	// 仅在不存在时插入
	opts := options.Update().SetUpsert(true)
	update := bson.M{"$setOnInsert": task}

	result, err := coll.UpdateOne(context.Background(), filter, update, opts)
	if err != nil {
		logrus.WithFields(logrus.Fields{
			"time":    time.Now().Format("2006-01-02 15:04:05"),
			"method":  "StoreTaskIfNotExistsEnv",
			"task_id": task.TaskID,
			"env":     env,
			"status":  task.ConfirmationStatus,
			"popup_sent": task.PopupSent,
			"created_at": task.CreatedAt,
			"took":    time.Since(startTime),
		}).Errorf("存储任务失败: %v", err)
		return err
	}

	// 判断是否插入
	if result.UpsertedCount > 0 {
		logrus.WithFields(logrus.Fields{
			"time":    time.Now().Format("2006-01-02 15:04:05"),
			"method":  "StoreTaskIfNotExistsEnv",
			"task_id": task.TaskID, // 新 UUID
			"env":     env,
			"status":  task.ConfirmationStatus,
			"popup_sent": task.PopupSent,
			"created_at": task.CreatedAt.Format("2006-01-02 15:04:05.000000000"),
			"took":    time.Since(startTime),
		}).Infof("任务存储成功（新插入） - env-specific (UUID 生成, CreatedAt 排序)")
	} else {
		logrus.WithFields(logrus.Fields{
			"time":    time.Now().Format("2006-01-02 15:04:05"),
			"method":  "StoreTaskIfNotExistsEnv",
			"task_id": task.TaskID,
			"env":     env,
			"took":    time.Since(startTime),
		}).Debugf("任务已存在（跳过） - env-specific")
	}

	return nil
}

// StoreTaskIfNotExists 存储任务（防重：service+version+env 唯一）
//(兼容调用时用 Environments[0])
func (m *MongoClient) StoreTaskIfNotExists(task models.DeployRequest) error {
	if len(task.Environments) == 0 {
		return fmt.Errorf("task.Environments 不能为空")
	}
	return m.StoreTaskIfNotExistsEnv(task, task.Environments[0])
}

// 确认: GetTaskByID 已包含 PopupMessageID 字段，确保日志打印
func (m *MongoClient) GetTaskByID(taskID string) (*models.DeployRequest, error) {
	startTime := time.Now()
	ctx := context.Background()

	for env := range m.cfg.EnvMapping.Mappings {
		collection := m.client.Database("cicd").Collection(fmt.Sprintf("tasks_%s", env))
		filter := bson.M{
			"task_id": taskID,
			"confirmation_status": bson.M{"$in": []string{"待确认", "已确认", "已拒绝"}}, // 修复: 中文状态过滤
		}
		var task models.DeployRequest
		if err := collection.FindOne(ctx, filter).Decode(&task); err == nil {
			logrus.WithFields(logrus.Fields{
				"time":   time.Now().Format("2006-01-02 15:04:05"),
				"method": "GetTaskByID",
				"task_id": taskID,
				"env":     env,
				"popup_message_id": task.PopupMessageID, // 确保包含用于删除
				"status": task.ConfirmationStatus, // 日志当前状态
				"full_task": fmt.Sprintf("%+v", task), // 打印完整任务数据
				"took":    time.Since(startTime),
			}).Debugf("任务查询成功: status=%s", task.ConfirmationStatus)
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

// 新增: MarkPopupSent 标记弹窗已发送（更新 popup_sent=true, popup_message_id, popup_sent_at）
func (m *MongoClient) MarkPopupSent(taskID string, messageID int) error {
	startTime := time.Now()
	ctx := context.Background()

	updated := false
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
			logrus.WithFields(logrus.Fields{
				"time":    time.Now().Format("2006-01-02 15:04:05"),
				"method":  "MarkPopupSent",
				"task_id": taskID,
				"env":     env,
				"message_id": messageID,
			}).Errorf("标记弹窗已发送失败: %v", err)
			continue
		}
		if result.ModifiedCount > 0 {
			updated = true
			logrus.WithFields(logrus.Fields{
				"time":    time.Now().Format("2006-01-02 15:04:05"),
				"method":  "MarkPopupSent",
				"task_id": taskID,
				"env":     env,
				"message_id": messageID,
				"took":    time.Since(startTime),
			}).Infof("弹窗标记成功: popup_sent=true, id=%d", messageID)
			break // 找到后停止遍历
		}
	}

	if !updated {
		logrus.WithFields(logrus.Fields{
			"time":    time.Now().Format("2006-01-02 15:04:05"),
			"method":  "MarkPopupSent",
			"took":    time.Since(startTime),
		}).Warnf("未找到任务以标记弹窗 task_id=%s", taskID)
		return fmt.Errorf("task not found")
	}
	return nil
}

// 新增: UpdateTaskStatus 更新任务状态（confirmation_status + 更新时间）
func (m *MongoClient) UpdateTaskStatus(taskID, status, user string) error {
	startTime := time.Now()
	ctx := context.Background()

	updated := false
	for env := range m.cfg.EnvMapping.Mappings {
		collection := m.client.Database("cicd").Collection(fmt.Sprintf("tasks_%s", env))
		filter := bson.M{"task_id": taskID}
		update := bson.M{
			"$set": bson.M{
				"confirmation_status": status,
				"updated_at":         time.Now(),
				"updated_by":         user,
			},
		}
		result, err := collection.UpdateOne(ctx, filter, update)
		if err != nil {
			logrus.WithFields(logrus.Fields{
				"time":    time.Now().Format("2006-01-02 15:04:05"),
				"method":  "UpdateTaskStatus",
				"task_id": taskID,
				"env":     env,
				"status":  status,
				"user":    user,
			}).Errorf("更新任务状态失败: %v", err)
			continue
		}
		if result.ModifiedCount > 0 {
			updated = true
			logrus.WithFields(logrus.Fields{
				"time":    time.Now().Format("2006-01-02 15:04:05"),
				"method":  "UpdateTaskStatus",
				"task_id": taskID,
				"env":     env,
				"status":  status,
				"user":    user,
				"took":    time.Since(startTime),
			}).Infof("任务状态更新成功: %s by %s", status, user)
			break // 找到后停止遍历
		}
	}

	if !updated {
		logrus.WithFields(logrus.Fields{
			"time":    time.Now().Format("2006-01-02 15:04:05"),
			"method":  "UpdateTaskStatus",
			"took":    time.Since(startTime),
		}).Warnf("未找到任务以更新状态 task_id=%s", taskID)
		return fmt.Errorf("task not found")
	}
	return nil
}

// 修复: DeleteTask 完整实现
func (m *MongoClient) DeleteTask(taskID string) error {
	startTime := time.Now()
	ctx := context.Background()

	deleted := false
	for env := range m.cfg.EnvMapping.Mappings {
		collection := m.client.Database("cicd").Collection(fmt.Sprintf("tasks_%s", env))
		filter := bson.M{"task_id": taskID}
		result, err := collection.DeleteOne(ctx, filter)
		if err != nil {
			logrus.WithFields(logrus.Fields{
				"time":  time.Now().Format("2006-01-02 15:04:05"),
				"env":   env,
				"task_id": taskID,
			}).Errorf("删除任务失败: %v", err)
			continue
		}
		if result.DeletedCount > 0 {
			deleted = true
			logrus.WithFields(logrus.Fields{
				"time":    time.Now().Format("2006-01-02 15:04:05"),
				"method":  "DeleteTask",
				"task_id": taskID,
				"env":     env,
				"took":    time.Since(startTime),
			}).Infof("任务删除成功")
			break // 找到后停止遍历
		}
	}

	if !deleted {
		logrus.WithFields(logrus.Fields{
			"time":    time.Now().Format("2006-01-02 15:04:05"),
			"method":  "DeleteTask",
			"took":    time.Since(startTime),
		}).Warnf("未找到要删除的任务 task_id=%s", taskID)
		return fmt.Errorf("task not found")
	}
	return nil
}

// Close 关闭连接
func (m *MongoClient) Close() error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return m.client.Disconnect(ctx)
}