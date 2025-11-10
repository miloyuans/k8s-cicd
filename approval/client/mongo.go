// 文件: mongo.go
package client

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"k8s-cicd/approval/config"
	"k8s-cicd/approval/models"

	"github.com/google/uuid"
	"github.com/sirupsen/logrus"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

// MongoClient MongoDB 客户端
type MongoClient struct {
	client *mongo.Client
	cfg    *config.MongoConfig
}

// GetTaskCollection 返回任务集合（基于 clean_env: replace "-" with "_"）
func (m *MongoClient) GetTaskCollection(env string) *mongo.Collection {
	cleanEnv := strings.ReplaceAll(env, "-", "_")
	collName := fmt.Sprintf("tasks_%s", cleanEnv)
	logrus.WithFields(logrus.Fields{
		"time":       time.Now().Format("2006-01-02 15:04:05"),
		"method":     "GetTaskCollection",
		"env":        env,
		"clean_env":  cleanEnv,
		"coll_name":  collName,
	}).Debugf("任务集合映射: %s -> %s", env, collName)
	return m.client.Database("cicd").Collection(collName)
}

// GetClient 返回底层 mongo.Client
func (m *MongoClient) GetClient() *mongo.Client {
	return m.client
}

// validateAndCleanData 清理 null environment 和重复
func validateAndCleanData(client *mongo.Client, cfg *config.MongoConfig) error {
	ctx := context.Background()

	for env := range cfg.EnvMapping.Mappings {
		cleanEnv := strings.ReplaceAll(env, "-", "_")
		collection := client.Database("cicd").Collection(fmt.Sprintf("tasks_%s", cleanEnv))

		logrus.WithFields(logrus.Fields{
			"time":      time.Now().Format("2006-01-02 15:04:05"),
			"method":    "validateAndCleanData",
			"env":       env,
			"coll_name": fmt.Sprintf("tasks_%s", cleanEnv),
		}).Infof("开始数据清理: %s", env)

		// 1. 更新 environment=null 为当前 env
		updateResult, err := collection.UpdateMany(ctx, bson.M{"environment": nil}, bson.M{"$set": bson.M{"environment": env}})
		if err != nil {
			return fmt.Errorf("清理 null environment 失败 [%s]: %v", env, err)
		}
		logrus.WithFields(logrus.Fields{
			"time":           time.Now().Format("2006-01-02 15:04:05"),
			"method":         "validateAndCleanData",
			"env":            env,
			"updated_null_env": updateResult.ModifiedCount,
		}).Debugf("已更新 %d 个 null environment 为 %s", updateResult.ModifiedCount, env)

		// 2. 删除重复 (保留最早 created_at)
		pipeline := bson.A{
			bson.M{"$group": bson.M{
				"_id": bson.M{
					"service":     "$service",
					"version":     "$version",
					"environment": "$environment",
					"created_at":  "$created_at",
				},
				"docs":  bson.M{"$push": bson.M{"_id": "$_id"}},
				"count": bson.M{"$sum": 1},
			}},
			bson.M{"$match": bson.M{"count": bson.M{"$gt": 1}}},
		}
		cursor, err := collection.Aggregate(ctx, pipeline)
		if err != nil {
			return fmt.Errorf("查找重复失败 [%s]: %v", env, err)
		}
		defer cursor.Close(ctx)

		var dupGroups []bson.M
		if err := cursor.All(ctx, &dupGroups); err != nil {
			return fmt.Errorf("解码重复组失败 [%s]: %v", env, err)
		}

		deleted := 0
		for _, group := range dupGroups {
			count := group["count"].(int32)
			docs := group["docs"].(bson.A)
			for i := 1; i < len(docs); i++ {
				docID := docs[i].(bson.M)["_id"].(primitive.ObjectID)
				_, err := collection.DeleteOne(ctx, bson.M{"_id": docID})
				if err == nil {
					deleted++
				}
			}
			logrus.WithFields(logrus.Fields{
				"time":           time.Now().Format("2006-01-02 15:04:05"),
				"method":         "validateAndCleanData",
				"env":            env,
				"group_id":       group["_id"],
				"deleted_in_group": int(count) - 1,
			}).Debugf("删除重复组中 %d 个文档", int(count)-1)
		}

		logrus.WithFields(logrus.Fields{
			"time":         time.Now().Format("2006-01-02 15:04:05"),
			"method":       "validateAndCleanData",
			"env":          env,
			"total_deleted": deleted,
		}).Infof("数据清理完成: 删除 %d 个重复文档", deleted)
	}

	return nil
}

// dropIndexByKeys 删除匹配 Keys 的索引
func dropIndexByKeys(ctx context.Context, collection *mongo.Collection, keys bson.D, indexType string) error {
	indexView := collection.Indexes()
	cursor, err := indexView.List(ctx)
	if err != nil {
		return fmt.Errorf("列出索引失败: %v", err)
	}
	defer cursor.Close(ctx)

	var indexes []bson.M
	if err := cursor.All(ctx, &indexes); err != nil {
		return fmt.Errorf("解码索引列表失败: %v", err)
	}

	for _, idx := range indexes {
		idxKeys, ok := idx["key"].(bson.D)
		if !ok {
			continue
		}
		if len(idxKeys) != len(keys) {
			continue
		}
		match := true
		for i, k := range keys {
			if idxKeys[i].Key != k.Key || idxKeys[i].Value != k.Value {
				match = false
				break
			}
		}
		if match {
			name, _ := idx["name"].(string)
			_, err := indexView.DropOne(ctx, name)
			if err != nil {
				return fmt.Errorf("删除索引 %s 失败: %v", name, err)
			}
			logrus.WithFields(logrus.Fields{
				"time":       time.Now().Format("2006-01-02 15:04:05"),
				"method":     "dropIndexByKeys",
				"coll_name":  collection.Name(),
				"index_name": name,
			}).Infof("已删除索引: %s", name)
			return nil
		}
	}
	return nil
}

// createIndexes 创建/更新索引
func createIndexes(client *mongo.Client, cfg *config.MongoConfig) error {
	ctx := context.Background()

	// 1. 任务集合索引
	for env := range cfg.EnvMapping.Mappings {
		cleanEnv := strings.ReplaceAll(env, "-", "_")
		collection := client.Database("cicd").Collection(fmt.Sprintf("tasks_%s", cleanEnv))

		// 唯一索引
		uniqueKeys := bson.D{
			{Key: "service", Value: 1},
			{Key: "environment", Value: 1},
			{Key: "version", Value: 1},
			{Key: "created_at", Value: 1},
		}
		if err := dropIndexByKeys(ctx, collection, uniqueKeys, "unique"); err != nil {
			return err
		}
		uniqueOpts := options.Index().SetUnique(true)
		_, err := collection.Indexes().CreateOne(ctx, mongo.IndexModel{
			Keys:    uniqueKeys,
			Options: uniqueOpts,
		})
		if err != nil {
			return fmt.Errorf("创建唯一索引失败 [%s]: %v", env, err)
		}

		// TTL 索引
		ttlKeys := bson.D{{Key: "created_at", Value: 1}}
		if err := dropIndexByKeys(ctx, collection, ttlKeys, "ttl"); err != nil {
			return err
		}
		ttlOpts := options.Index().SetExpireAfterSeconds(int32(cfg.TTL.Seconds())) // 修复
		_, err = collection.Indexes().CreateOne(ctx, mongo.IndexModel{
			Keys:    ttlKeys,
			Options: ttlOpts,
		})
		if err != nil {
			return fmt.Errorf("创建 TTL 索引失败 [%s]: %v", env, err)
		}
	}

	// 2. popup_data TTL
	popupColl := client.Database("cicd").Collection("popup_data")
	ttlKey := bson.D{{Key: "sent_at", Value: 1}}
	if err := dropIndexByKeys(ctx, popupColl, ttlKey, "ttl"); err != nil {
		return err
	}
	_, err := popupColl.Indexes().CreateOne(ctx, mongo.IndexModel{
		Keys:    ttlKey,
		Options: options.Index().SetExpireAfterSeconds(7 * 24 * 3600), // 7天
	})
	if err != nil {
		return fmt.Errorf("创建 popup_data TTL 索引失败: %v", err)
	}

	return nil
}

// StoreTask 存储任务（支持 isConfirmEnv 决定 popup_sent）
func (m *MongoClient) StoreTask(task *models.DeployRequest, isConfirmEnv bool) error {
	startTime := time.Now()
	ctx := context.Background()

	// 基础字段
	task.ConfirmationStatus = "待确认"
	task.PopupSent = !isConfirmEnv
	task.PopupSentAt = time.Time{}
	task.PopupMessageID = 0
	task.PopupBotName = ""

	// 生成 task_id
	if task.TaskID == "" {
		task.TaskID = uuid.New().String()
		logrus.WithFields(logrus.Fields{
			"time":     time.Now().Format("2006-01-02 15:04:05"),
			"method":   "StoreTask",
			"service":  task.Service,
			"env":      task.Environment,
			"version":  task.Version,
			"task_id":  task.TaskID,
		}).Debugf("生成新 TaskID: %s", task.TaskID)
	}

	// 环境映射
	if ns, ok := m.cfg.EnvMapping.Mappings[task.Environment]; ok {
		task.Namespace = ns
	} else {
		task.Namespace = task.Environment
	}

	// 集合
	coll := m.GetTaskCollection(task.Environment)

	// Upsert
	filter := bson.M{
		"service":     task.Service,
		"environment": task.Environment,
		"version":     task.Version,
	}
	update := bson.M{"$set": task, "$setOnInsert": bson.M{"created_at": time.Now()}}
	opts := options.Update().SetUpsert(true)

	result, err := coll.UpdateOne(ctx, filter, update, opts)
	if err != nil {
		return fmt.Errorf("upsert 失败: %v", err)
	}

	// 日志
	if result.UpsertedCount > 0 {
		logrus.WithFields(logrus.Fields{
			"time":      time.Now().Format("2006-01-02 15:04:05"),
			"method":    "StoreTask",
			"coll_name": coll.Name(),
			"env":       task.Environment,
			"task_id":   task.TaskID,
			"took":      time.Since(startTime),
		}).Info("新任务存储成功 (upsert)")
	} else if result.MatchedCount > 0 && result.ModifiedCount == 0 {
		logrus.WithFields(logrus.Fields{
			"time":      time.Now().Format("2006-01-02 15:04:05"),
			"method":    "StoreTask",
			"coll_name": coll.Name(),
			"env":       task.Environment,
			"task_id":   task.TaskID,
			"matched":   result.MatchedCount,
			"modified":  result.ModifiedCount,
			"took":      time.Since(startTime),
		}).Debug("任务已存在，无需 upsert")
	}
	return nil
}

// UpdateTaskStatus 更新状态（支持 env + popup_sent）
func (m *MongoClient) UpdateTaskStatus(taskID, status, user, env string, popupSent *bool) error {
	startTime := time.Now()
	ctx := context.Background()

	coll := m.GetTaskCollection(env)

	update := bson.M{
		"$set": bson.M{
			"confirmation_status": status,
			"updated_at":          time.Now(),
			"updated_by":          user,
		},
	}
	if popupSent != nil {
		update["$set"].(bson.M)["popup_sent"] = *popupSent
	}

	result, err := coll.UpdateOne(ctx, bson.M{"task_id": taskID}, update)
	if err != nil {
		return fmt.Errorf("mongo update error: %v", err)
	}
	if result.MatchedCount == 0 {
		logrus.WithFields(logrus.Fields{
			"time":    time.Now().Format("2006-01-02 15:04:05"),
			"method":  "UpdateTaskStatus",
			"task_id": taskID,
			"env":     env,
			"took":    time.Since(startTime),
		}).Warnf("未找到任务以更新状态 task_id=%s", taskID)
		return fmt.Errorf("task not found")
	}

	logrus.WithFields(logrus.Fields{
		"time":   time.Now().Format("2006-01-02 15:04:05"),
		"method": "UpdateTaskStatus",
		"status": status,
		"user":   user,
		"task_id": taskID,
		"env":    env,
		"took":   time.Since(startTime),
	}).Infof("任务状态更新成功: %s by %s", status, user)
	return nil
}

// GetTaskByID 按 env 查询任务
func (m *MongoClient) GetTaskByID(taskID, env string) (*models.DeployRequest, error) {
	coll := m.GetTaskCollection(env)
	var task models.DeployRequest
	err := coll.FindOne(context.Background(), bson.M{"task_id": taskID}).Decode(&task)
	if err != nil {
		if err == mongo.ErrNoDocuments {
			return nil, fmt.Errorf("task not found")
		}
		return nil, err
	}
	return &task, nil
}

// GetPendingTasks 查询待确认任务
func (m *MongoClient) GetPendingTasks(env string) ([]models.DeployRequest, error) {
	coll := m.GetTaskCollection(env)
	ctx := context.Background()
	cursor, err := coll.Find(ctx, bson.M{
		"confirmation_status": "待确认",
		"popup_sent":          bson.M{"$ne": true},
	}, options.Find().SetSort(bson.D{{Key: "created_at", Value: 1}}))
	if err != nil {
		return nil, err
	}
	defer cursor.Close(ctx)

	var tasks []models.DeployRequest
	for cursor.Next(ctx) {
		var task models.DeployRequest
		if err := cursor.Decode(&task); err != nil {
			return nil, err
		}
		tasks = append(tasks, task)
	}
	if err := cursor.Err(); err != nil {
		return nil, err
	}
	return tasks, nil
}

// MarkPopupSent 标记弹窗已发送
func (m *MongoClient) MarkPopupSent(taskID, messageID int, botName, env string) error {
	ctx := context.Background()
	coll := m.GetTaskCollection(env)

	update := bson.M{
		"$set": bson.M{
			"popup_sent":      true,
			"popup_message_id": messageID,
			"popup_sent_at":   time.Now(),
			"popup_bot_name":  botName,
		},
	}
	_, err := coll.UpdateOne(ctx, bson.M{"task_id": taskID}, update)
	if err != nil {
		return err
	}

	// 插入 popup_data 记录
	popupColl := m.client.Database("cicd").Collection("popup_data")
	_, err = popupColl.InsertOne(ctx, bson.M{
		"task_id":    taskID,
		"message_id": messageID,
		"sent_at":    time.Now(),
	})
	return err
}

// DeleteTask 删除任务
func (m *MongoClient) DeleteTask(taskID string) error {
	startTime := time.Now()
	ctx := context.Background()
	deleted := false

	for env := range m.cfg.EnvMapping.Mappings {
		coll := m.GetTaskCollection(env)
		result, err := coll.DeleteOne(ctx, bson.M{"task_id": taskID})
		if err != nil {
			return err
		}
		if result.DeletedCount > 0 {
			deleted = true
			break
		}
	}

	if !deleted {
		logrus.WithFields(logrus.Fields{
			"time":   time.Now().Format("2006-01-02 15:04:05"),
			"method": "DeleteTask",
			"took":   time.Since(startTime),
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

// NewMongoClient 创建客户端
func NewMongoClient(cfg *config.MongoConfig) (*MongoClient, error) {
	startTime := time.Now()
	ctx := context.Background()

	logrus.WithFields(logrus.Fields{
		"time": time.Now().Format("2006-01-02 15:04:05"),
		"method": "NewMongoClient",
		"uri":  cfg.URI,
	}).Info("=== 开始初始化 MongoDB 客户端 ===")

	var mongoClient *mongo.Client
	var err error
	for attempt := 0; attempt <= cfg.MaxRetries; attempt++ {
		clientOptions := options.Client().
			ApplyURI(cfg.URI).
			SetMaxPoolSize(10).
			SetServerSelectionTimeout(5 * time.Second)

		mongoClient, err = mongo.Connect(ctx, clientOptions)
		if err == nil {
			if err := mongoClient.Ping(ctx, nil); err == nil {
				logrus.WithFields(logrus.Fields{
					"time": time.Now().Format("2006-01-02 15:04:05"),
					"method": "NewMongoClient",
					"took": time.Since(startTime),
				}).Info("MongoDB 连接成功")
				break
			} else {
				err = fmt.Errorf("ping 失败: %v", err)
			}
		}

		logrus.WithFields(logrus.Fields{
			"time":        time.Now().Format("2006-01-02 15:04:05"),
			"method":      "NewMongoClient",
			"attempt":     attempt + 1,
			"max_retries": cfg.MaxRetries,
			"error":       err.Error(),
		}).Warnf("MongoDB 连接失败，重试中...")

		if attempt < cfg.MaxRetries {
			time.Sleep(time.Duration(attempt+1) * time.Second)
		} else {
			return nil, fmt.Errorf("MongoDB 连接失败（已重试 %d 次）: %v", cfg.MaxRetries+1, err)
		}
	}

	if err := createIndexes(mongoClient, cfg); err != nil {
		return nil, err
	}
	logrus.WithFields(logrus.Fields{
		"time": time.Now().Format("2006-01-02 15:04:05"),
		"method": "NewMongoClient",
		"took": time.Since(startTime),
	}).Info("索引创建完成")

	if err := initPushData(mongoClient, cfg); err != nil {
		return nil, err
	}
	logrus.WithFields(logrus.Fields{
		"time": time.Now().Format("2006-01-02 15:04:05"),
		"method": "NewMongoClient",
		"took": time.Since(startTime),
	}).Info("push_data 初始化完成")

	m := &MongoClient{
		client: mongoClient,
		cfg:    cfg,
	}

	logrus.WithFields(logrus.Fields{
		"time": time.Now().Format("2006-01-02 15:04:05"),
		"method": "NewMongoClient",
		"took": time.Since(startTime),
	}).Info("MongoDB 客户端初始化成功")
	return m, nil
}

// initPushData 动态生成服务-环境组合
func initPushData(client *mongo.Client, cfg *config.MongoConfig) error {
	ctx := context.Background()
	collection := client.Database("cicd").Collection("push_data")

	count, err := collection.CountDocuments(ctx, bson.D{})
	if err != nil {
		return fmt.Errorf("检查 push_data 计数失败: %v", err)
	}
	if count > 0 {
		logrus.WithFields(logrus.Fields{
			"time":  time.Now().Format("2006-01-02 15:04:05"),
			"method": "initPushData",
			"count": count,
		}).Debug("push_data 已存在数据，跳过初始化")
		return nil
	}

	defaultServices := []string{"example-service", "app1", "app2"}
	envs := make([]string, 0, len(cfg.EnvMapping.Mappings))
	for env := range cfg.EnvMapping.Mappings {
		envs = append(envs, env)
	}

	initialData := make([]interface{}, 0)
	for _, svc := range defaultServices {
		for _, env := range envs {
			filter := bson.M{"service": svc, "environment": env}
			if count, _ := collection.CountDocuments(ctx, filter); count == 0 {
				initialData = append(initialData, bson.M{
					"service":     svc,
					"environment": env,
					"created_at":  time.Now(),
				})
			}
		}
	}

	if len(initialData) == 0 {
		logrus.WithFields(logrus.Fields{
			"time":   time.Now().Format("2006-01-02 15:04:05"),
			"method": "initPushData",
		}).Warn("无新组合数据生成，push_data 保持空")
		return nil
	}

	_, err = collection.InsertMany(ctx, initialData)
	if err != nil {
		return fmt.Errorf("初始化 push_data 数据失败: %v", err)
	}

	logrus.WithFields(logrus.Fields{
		"time":     time.Now().Format("2006-01-02 15:04:05"),
		"method":   "initPushData",
		"services": defaultServices,
		"envs":     envs,
		"inserted": len(initialData),
	}).Infof("push_data 动态组合数据插入成功 (服务 x 环境: %d 组合)", len(initialData))
	return nil
}

// GetPushedServicesAndEnvs 获取 push_data 中的服务和环境
func (m *MongoClient) GetPushedServicesAndEnvs() ([]string, []string, error) {
	ctx := context.Background()
	coll := m.client.Database("cicd").Collection("push_data")

	pipeline := bson.A{
		bson.M{"$group": bson.M{
			"_id":       nil,
			"services":  bson.M{"$addToSet": "$service"},
			"envs":      bson.M{"$addToSet": "$environment"},
		}},
	}

	cursor, err := coll.Aggregate(ctx, pipeline)
	if err != nil {
		return nil, nil, err
	}
	defer cursor.Close(ctx)

	if !cursor.Next(ctx) {
		return []string{}, []string{}, nil
	}

	var result struct {
		Services []string `bson:"services"`
		Envs     []string `bson:"envs"`
	}
	if err := cursor.Decode(&result); err != nil {
		return nil, nil, err
	}

	sort.Strings(result.Services)
	sort.Strings(result.Envs)
	return result.Services, result.Envs, nil
}

// GetEnvMappings 获取环境映射
func (m *MongoClient) GetEnvMappings() map[string]string {
	if m.cfg == nil || m.cfg.EnvMapping.Mappings == nil {
		return make(map[string]string)
	}
	return m.cfg.EnvMapping.Mappings
}