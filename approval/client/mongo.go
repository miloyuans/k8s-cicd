// 文件: mongo.go (完整文件，添加了 DeleteTask 函数)
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
// 修复: GetPendingTasks 过滤 "confirmation_status": "待确认"，确保匹配弹窗逻辑
// 确认: GetPendingTasks 严格过滤 "confirmation_status": "待确认"，用于弹窗触发
func (m *MongoClient) GetPendingTasks(env string) ([]models.DeployRequest, error) {
	startTime := time.Now()

	coll := m.GetClient().Database("cicd").Collection(fmt.Sprintf("tasks_%s", env))
	filter := bson.M{
		"environment":         env,
		"confirmation_status": "待确认", // 严格匹配 "待确认" 状态，用于弹窗触发
		"popup_sent":          bson.M{"$ne": true},
	}

	logrus.WithFields(logrus.Fields{
		"time":   time.Now().Format("2006-01-02 15:04:05"),
		"method": "GetPendingTasks",
		"env":    env,
		"filter": filter,
	}).Debug("执行待确认任务查询 (用于弹窗触发: 状态=待确认 + 未发送弹窗)")

	cursor, err := coll.Find(context.Background(), filter)
	if err != nil {
		logrus.WithFields(logrus.Fields{
			"time":   time.Now().Format("2006-01-02 15:04:05"),
			"method": "GetPendingTasks",
			"env":    env,
		}).Errorf("查询 %s 待确认任务失败: %v", env, err)
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
		"status_filter": "待确认",
	}).Infof("查询到 %d 个待确认任务 (弹窗触发候选): %v", len(tasks), tasks) // 打印任务列表

	return tasks, nil
}

// 修复: MarkPopupSent 完整实现，无截断
func (m *MongoClient) MarkPopupSent(taskID string, messageID int) error {
	startTime := time.Now()
	ctx := context.Background()

	updated := false
	for env := range m.cfg.EnvMapping.Mappings {
		collection := m.client.Database("cicd").Collection(fmt.Sprintf("tasks_%s", env))
		filter := bson.M{"task_id": taskID}
		update := bson.M{
			"$set": bson.M{
				"popup_sent":        true,
				"popup_message_id":  messageID,
				"popup_sent_at":     time.Now(),
			},
		}
		result, err := collection.UpdateOne(ctx, filter, update)
		if err != nil {
			logrus.WithFields(logrus.Fields{
				"time":  time.Now().Format("2006-01-02 15:04:05"),
				"env":   env,
				"task_id": taskID,
			}).Errorf("更新任务失败: %v", err)
			continue
		}
		if result.MatchedCount > 0 {
			updated = true
			logrus.WithFields(logrus.Fields{
				"time":     time.Now().Format("2006-01-02 15:04:05"),
				"method":   "MarkPopupSent",
				"task_id":  taskID,
				"env":      env,
				"message_id": messageID,
				"took":     time.Since(startTime),
			}).Infof("任务弹窗标记成功")
			break // 找到后停止遍历
		}
	}

	if !updated {
		logrus.WithFields(logrus.Fields{
			"time":   time.Now().Format("2006-01-02 15:04:05"),
			"method": "MarkPopupSent",
			"took":   time.Since(startTime),
		}).Warnf("未找到任务 task_id=%s", taskID)
		return fmt.Errorf("任务未找到")
	}
	return nil
}

// 修复: UpdateTaskStatus 完整实现 (假设原代码有截断，这里补全)
func (m *MongoClient) UpdateTaskStatus(taskID, status, user string) error {
	startTime := time.Now()
	ctx := context.Background()

	updated := false
	for env := range m.cfg.EnvMapping.Mappings {
		collection := m.client.Database("cicd").Collection(fmt.Sprintf("tasks_%s", env))
		filter := bson.M{"task_id": taskID}
		update := bson.M{
			"$set": bson.M{
				"confirmation_status": status, // 支持 "待确认", "已确认", "已拒绝"
				"updated_by":          user,
				"updated_at":          time.Now(),
			},
		}
		result, err := collection.UpdateOne(ctx, filter, update)
		if err != nil {
			logrus.WithFields(logrus.Fields{
				"time":  time.Now().Format("2006-01-02 15:04:05"),
				"env":   env,
				"task_id": taskID,
			}).Errorf("更新任务状态失败: %v", err)
			continue
		}
		if result.MatchedCount > 0 {
			updated = true
			logrus.WithFields(logrus.Fields{
				"time":     time.Now().Format("2006-01-02 15:04:05"),
				"method":   "UpdateTaskStatus",
				"task_id":  taskID,
				"status":   status, // 日志中文状态
				"user":     user, // 记录更新者 (system 或 用户名)
				"env":      env,
				"took":     time.Since(startTime),
			}).Infof("任务状态更新成功: %s (by %s)", status, user)
			break
		}
	}

	if !updated {
		logrus.WithFields(logrus.Fields{
			"time":   time.Now().Format("2006-01-02 15:04:05"),
			"method": "UpdateTaskStatus",
			"took":   time.Since(startTime),
		}).Warnf("未找到任务 task_id=%s", taskID)
		return fmt.Errorf("任务未找到")
	}
	return nil
}


// GetPushedServicesAndEnvs 获取所有已 push 的服务和环境
func (m *MongoClient) GetPushedServicesAndEnvs() ([]string, []string, error) {
	startTime := time.Now()
	services := make(map[string]bool)
	envs := make(map[string]bool)

	logrus.WithFields(logrus.Fields{
		"time":   time.Now().Format("2006-01-02 15:04:05"),
		"method": "GetPushedServicesAndEnvs",
	}).Debugf("开始扫描所有环境的任务集合")

	for env := range m.cfg.EnvMapping.Mappings {
		coll := m.client.Database("cicd").Collection(fmt.Sprintf("tasks_%s", env))
		logrus.WithFields(logrus.Fields{
			"time":   time.Now().Format("2006-01-02 15:04:05"),
			"method": "GetPushedServicesAndEnvs",
			"env":    env,
			"coll":   fmt.Sprintf("tasks_%s", env),
		}).Debugf("扫描环境 %s 的任务集合", env)

		cursor, err := coll.Find(context.Background(), bson.M{})
		if err != nil {
			logrus.WithFields(logrus.Fields{
				"time":   time.Now().Format("2006-01-02 15:04:05"),
				"method": "GetPushedServicesAndEnvs",
				"env":    env,
			}).Errorf("扫描集合失败: %v", err)
			continue
		}
		var tasks []models.DeployRequest
		if err := cursor.All(context.Background(), &tasks); err != nil {
			logrus.Warnf("解析任务失败 [%s]: %v", env, err)
			continue
		}
		cursor.Close(context.Background())

		logrus.WithFields(logrus.Fields{
			"time":   time.Now().Format("2006-01-02 15:04:05"),
			"method": "GetPushedServicesAndEnvs",
			"env":    env,
			"task_count": len(tasks),
		}).Debugf("环境 %s 有 %d 个任务", env, len(tasks))

		for _, t := range tasks {
			services[t.Service] = true
			envs[env] = true
			logrus.WithFields(logrus.Fields{
				"time":   time.Now().Format("2006-01-02 15:04:05"),
				"method": "GetPushedServicesAndEnvs",
				"service": t.Service,
				"env":     env,
			}).Tracef("发现服务: %s in %s", t.Service, env) // Trace 级日志，避免过多
		}
	}

	var serviceList, envList []string
	for s := range services {
		serviceList = append(serviceList, s)
	}
	for e := range envs {
		envList = append(envList, e)
	}

	logrus.WithFields(logrus.Fields{
		"time":     time.Now().Format("2006-01-02 15:04:05"),
		"method":   "GetPushedServicesAndEnvs",
		"services": serviceList,
		"envs":     envList,
		"took":     time.Since(startTime),
	}).Infof("扫描完成: 服务 %v, 环境 %v", serviceList, envList)

	return serviceList, envList, nil
}

// 完善: StoreTaskIfNotExistsEnv 支持传入的动态状态/PopupSent (无变更，直接使用 task 的值)
func (m *MongoClient) StoreTaskIfNotExistsEnv(task models.DeployRequest, env string) error {
	startTime := time.Now()

	// 验证环境
	if len(task.Environments) == 0 || task.Environments[0] != env {
		return fmt.Errorf("task.Environments[0] 必须为 %s", env)
	}

	// 获取集合 (env-specific)
	coll := m.GetClient().Database("cicd").Collection(fmt.Sprintf("tasks_%s", env))

	// 唯一键过滤 (env-specific)
	filter := bson.M{
		"service":     task.Service,
		"version":     task.Version,
		"environment": env,
	}

	// 仅在不存在时插入 (使用传入的 ConfirmationStatus 和 PopupSent)
	opts := options.Update().SetUpsert(true)
	update := bson.M{"$setOnInsert": task}

	result, err := coll.UpdateOne(context.Background(), filter, update, opts)
	if err != nil {
		logrus.WithFields(logrus.Fields{
			"time":    time.Now().Format("2006-01-02 15:04:05"),
			"method":  "StoreTaskIfNotExistsEnv",
			"task_id": task.TaskID,
			"env":     env,
			"status":  task.ConfirmationStatus, // 日志动态状态
			"popup_sent": task.PopupSent,
			"took":    time.Since(startTime),
		}).Errorf("存储任务失败: %v", err)
		return err
	}

	// 判断是否插入
	if result.UpsertedCount > 0 {
		logrus.WithFields(logrus.Fields{
			"time":    time.Now().Format("2006-01-02 15:04:05"),
			"method":  "StoreTaskIfNotExistsEnv",
			"task_id": task.TaskID,
			"env":     env,
			"status":  task.ConfirmationStatus,
			"popup_sent": task.PopupSent,
			"took":    time.Since(startTime),
		}).Infof("任务存储成功（新插入） - env-specific (状态: %s)", task.ConfirmationStatus)
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