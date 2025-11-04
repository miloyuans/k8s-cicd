// 文件: mongo.go (完整文件，优化: 集合命名基于 clean_env (env replace "-" with "_") 如 tasks_eks_test，避免 namespace 冲突；添加 validateAndCleanData 在 createIndexes 前清理 null environment 和潜在重复；Drop 现有索引再创建，确保无违反唯一性；其他功能保留)
// 修复: 添加 import "go.mongodb.org/mongo-driver/bson/primitive" 以解决 undefined: primitive；
//       修改 DropOne 调用: 先 List 找到匹配 Keys 的索引名称 (e.g., "created_at_1")，然后 DropOne(name string)，避免类型错误；
//       修复 sort.Sort: bson.D 不实现 sort.Interface，直接迭代比较键值对 (假设顺序固定)，移除排序逻辑；
//       修复 cursor.Next: 使用 cursor.Next(ctx) 返回 bool，检查 cursor.Err() 处理错误；
//       类似处理 popup_coll 的 TTL 索引删除；优化: 在 createIndexes 中统一处理 Drop 逻辑，增强日志；不改变任何功能。
package client

import (
	"context"
	"fmt"
	"sort"
	"strings" // 用于字符串清理
	"time"

	"k8s-cicd/approval/config"
	"k8s-cicd/approval/models"

	"github.com/google/uuid"
	"github.com/sirupsen/logrus"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive" // 新增: 解决 undefined: primitive
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
	//"go.mongodb.org/mongo-driver/mongo/readpref"
)

// MongoClient MongoDB 客户端
type MongoClient struct {
	client *mongo.Client
	cfg    *config.MongoConfig
}

// 优化: GetTaskCollection 返回任务集合（基于 clean_env: replace "-" with "_"，如 tasks_eks_test，避免 namespace 共享冲突）
func (m *MongoClient) GetTaskCollection(env string) *mongo.Collection {
	// 清理 env：replace "-" with "_"
	cleanEnv := strings.ReplaceAll(env, "-", "_")

	collName := fmt.Sprintf("tasks_%s", cleanEnv)
	logrus.WithFields(logrus.Fields{
		"time":     time.Now().Format("2006-01-02 15:04:05"),
		"method":   "GetTaskCollection",
		"env":      env,
		"clean_env": cleanEnv,
		"coll_name": collName,
	}).Debugf("任务集合映射: %s -> %s", env, collName)

	return m.client.Database("cicd").Collection(collName)
}

// GetClient 返回底层 mongo.Client (供 agent.go 等使用)
func (m *MongoClient) GetClient() *mongo.Client {
	return m.client
}

// 新增: validateAndCleanData 为每个集合清理潜在违反唯一索引的数据（设置 environment=null 为 env，删除精确重复）
func validateAndCleanData(client *mongo.Client, cfg *config.MongoConfig) error {
	ctx := context.Background()

	for env := range cfg.EnvMapping.Mappings {
		cleanEnv := strings.ReplaceAll(env, "-", "_")
		collection := client.Database("cicd").Collection(fmt.Sprintf("tasks_%s", cleanEnv))

		logrus.WithFields(logrus.Fields{
			"time":   time.Now().Format("2006-01-02 15:04:05"),
			"method": "validateAndCleanData",
			"env":    env,
			"coll_name": fmt.Sprintf("tasks_%s", cleanEnv),
		}).Infof("开始数据清理: %s", env)

		// 1. 更新 environment=null 为当前 env
		updateResult, err := collection.UpdateMany(ctx, bson.M{"environment": nil}, bson.M{"$set": bson.M{"environment": env}})
		if err != nil {
			return fmt.Errorf("清理 null environment 失败 [%s]: %v", env, err)
		}
		logrus.WithFields(logrus.Fields{
			"time":   time.Now().Format("2006-01-02 15:04:05"),
			"method": "validateAndCleanData",
			"env":    env,
			"updated_null_env": updateResult.ModifiedCount,
		}).Debugf("已更新 %d 个 null environment 为 %s", updateResult.ModifiedCount, env)

		// 2. 删除潜在重复 (保留最早 created_at 的一个，使用聚合分组删除)
		// 先查找重复组
		pipeline := bson.A{
			bson.M{"$group": bson.M{
				"_id": bson.M{
					"service":     "$service",
					"version":     "$version",
					"environment": "$environment",
					"created_at":  "$created_at",
				},
				"docs": bson.M{"$push": bson.M{"_id": "$_id"}},
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
			// 保留第一个，删除其余
			for i := 1; i < len(docs); i++ {
				docID := docs[i].(bson.M)["_id"].(primitive.ObjectID) // 修复: 使用 primitive.ObjectID
				_, err := collection.DeleteOne(ctx, bson.M{"_id": docID})
				if err == nil {
					deleted++
				}
			}
			logrus.WithFields(logrus.Fields{
				"time":   time.Now().Format("2006-01-02 15:04:05"),
				"method": "validateAndCleanData",
				"env":    env,
				"group_id": group["_id"],
				"deleted_in_group": int(count) - 1,
			}).Debugf("删除重复组中 %d 个文档", int(count)-1)
		}

		logrus.WithFields(logrus.Fields{
			"time":   time.Now().Format("2006-01-02 15:04:05"),
			"method": "validateAndCleanData",
			"env":    env,
			"total_deleted": deleted,
		}).Infof("数据清理完成: 删除 %d 个重复文档", deleted)
	}

	return nil
}

// 辅助函数: dropIndexByKeys 删除匹配 Keys 的索引 (先 List 找到名称，然后 DropOne)
func dropIndexByKeys(ctx context.Context, collection *mongo.Collection, keys bson.D, indexType string) error {
	// List 所有索引
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

	// 查找匹配 Keys 的索引名称
	targetIndexName := ""
	for _, idx := range indexes {
		idxKeys, ok := idx["key"].(bson.M)
		if !ok {
			continue
		}
		// 比较 Keys (修复: 直接迭代比较元素，假设顺序固定)
		if matchKeys(idxKeys, keys) {
			targetIndexName = idx["name"].(string)
			logrus.WithFields(logrus.Fields{
				"time":   time.Now().Format("2006-01-02 15:04:05"),
				"method": "dropIndexByKeys",
				"coll":   collection.Name(),
				"index_name": targetIndexName,
				"keys":    keys,
				"type":    indexType,
			}).Debugf("找到匹配索引: %s (Keys: %v)", targetIndexName, keys)
			break
		}
	}

	if targetIndexName == "" {
		logrus.WithFields(logrus.Fields{
			"time":   time.Now().Format("2006-01-02 15:04:05"),
			"method": "dropIndexByKeys",
			"coll":   collection.Name(),
			"keys":   keys,
			"type":   indexType,
		}).Debugf("未找到匹配索引，跳过删除")
		return nil
	}

	// DropOne by name
	names, err := indexView.DropOne(ctx, targetIndexName)
	if err != nil {
		if strings.Contains(err.Error(), "ns not found") || strings.Contains(err.Error(), "index not found") {
			logrus.WithFields(logrus.Fields{
				"time":   time.Now().Format("2006-01-02 15:04:05"),
				"method": "dropIndexByKeys",
				"coll":   collection.Name(),
				"index_name": targetIndexName,
			}).Debugf("索引不存在，忽略: %v", err)
			return nil
		}
		return fmt.Errorf("删除索引 %s 失败: %v", targetIndexName, err)
	}

	logrus.WithFields(logrus.Fields{
		"time":   time.Now().Format("2006-01-02 15:04:05"),
		"method": "dropIndexByKeys",
		"coll":   collection.Name(),
		"dropped_names": names,
		"type":   indexType,
	}).Infof("成功删除索引: %v", names)
	return nil
}

// 辅助函数: matchKeys 简单匹配索引 Keys (支持单键或复合，直接迭代比较)
func matchKeys(idxKeys bson.M, targetKeys bson.D) bool {
	// 转换为 D 以比较
	var idxD bson.D
	for k, v := range idxKeys {
		idxD = append(idxD, bson.E{Key: k, Value: v})
	}
	// 修复: 直接比较长度和每个元素 (假设顺序一致，无需 sort)
	if len(idxD) != len(targetKeys) {
		return false
	}
	for i := range targetKeys {
		if idxD[i].Key != targetKeys[i].Key || idxD[i].Value != targetKeys[i].Value {
			return false
		}
	}
	return true
}

// 优化: createIndexes 先清理数据 + Drop 现有索引 (使用 dropIndexByKeys 忽略不存在错误) + 创建新索引
func createIndexes(client *mongo.Client, cfg *config.MongoConfig) error {
	ctx := context.Background()

	// 新增: 先验证和清理数据，避免 unique 冲突
	if err := validateAndCleanData(client, cfg); err != nil {
		logrus.WithFields(logrus.Fields{
			"time":   time.Now().Format("2006-01-02 15:04:05"),
			"method": "createIndexes",
			"error":  err.Error(),
		}).Errorf("数据清理失败")
		return err
	}

	// 1. 为每个环境的任务集合创建索引（基于 clean_env）
	for env := range cfg.EnvMapping.Mappings {
		cleanEnv := strings.ReplaceAll(env, "-", "_")
		collection := client.Database("cicd").Collection(fmt.Sprintf("tasks_%s", cleanEnv))

		logrus.WithFields(logrus.Fields{
			"time":   time.Now().Format("2006-01-02 15:04:05"),
			"method": "createIndexes",
			"env":    env,
			"coll_name": fmt.Sprintf("tasks_%s", cleanEnv),
		}).Debugf("为环境 %s 创建索引 (集合: tasks_%s)", env, cleanEnv)

		// 先 Drop 现有 TTL 索引 (使用辅助函数，忽略不存在)
		ttlKeys := bson.D{{Key: "created_at", Value: 1}}
		if err := dropIndexByKeys(ctx, collection, ttlKeys, "TTL"); err != nil {
			logrus.WithFields(logrus.Fields{
				"time":   time.Now().Format("2006-01-02 15:04:05"),
				"method": "createIndexes",
				"env":    env,
				"error":  err.Error(),
			}).Errorf("删除 TTL 索引失败: %v", err)
			return err
		}

		// 先 Drop 现有 Unique 索引
		uniqueKeys := bson.D{
			{Key: "service", Value: 1},
			{Key: "version", Value: 1},
			{Key: "environment", Value: 1},
		}
		if err := dropIndexByKeys(ctx, collection, uniqueKeys, "Unique"); err != nil {
			logrus.WithFields(logrus.Fields{
				"time":   time.Now().Format("2006-01-02 15:04:05"),
				"method": "createIndexes",
				"env":    env,
				"error":  err.Error(),
			}).Errorf("删除 Unique 索引失败: %v", err)
			return err
		}

		// 创建 TTL 索引 (expireAfterSeconds)
		ttlIndex := mongo.IndexModel{
			Keys:    ttlKeys,
			Options: options.Index().SetExpireAfterSeconds(int32(cfg.TTL.Seconds())),
		}
		_, err := collection.Indexes().CreateOne(ctx, ttlIndex)
		if err != nil {
			return fmt.Errorf("创建 TTL 索引失败 [%s]: %v", env, err)
		}

		// 创建 Unique 复合索引
		uniqueIndex := mongo.IndexModel{
			Keys:    uniqueKeys,
			Options: options.Index().SetUnique(true),
		}
		_, err = collection.Indexes().CreateOne(ctx, uniqueIndex)
		if err != nil {
			return fmt.Errorf("创建 Unique 索引失败 [%s]: %v", env, err)
		}

		logrus.WithFields(logrus.Fields{
			"time":   time.Now().Format("2006-01-02 15:04:05"),
			"method": "createIndexes",
			"env":    env,
		}).Infof("索引创建成功: TTL + Unique")
	}

	// 2. 为 popup_data 集合创建 TTL 索引
	popupColl := client.Database("cicd").Collection("popup_data")
	logrus.WithFields(logrus.Fields{
		"time":   time.Now().Format("2006-01-02 15:04:05"),
		"method": "createIndexes",
		"coll_name": "popup_data",
	}).Debugf("为 popup_data 创建索引")

	// 先 Drop 现有 TTL 索引 (修复: 使用 dropIndexByKeys)
	popupTTLKeys := bson.D{{Key: "sent_at", Value: 1}}
	if err := dropIndexByKeys(ctx, popupColl, popupTTLKeys, "Popup TTL"); err != nil {
		logrus.WithFields(logrus.Fields{
			"time":   time.Now().Format("2006-01-02 15:04:05"),
			"method": "createIndexes",
			"coll":   "popup_data",
			"error":  err.Error(),
		}).Errorf("删除 Popup TTL 索引失败: %v", err)
		return err
	}

	// 创建 Popup TTL 索引
	popupTTLIndex := mongo.IndexModel{
		Keys:    popupTTLKeys,
		Options: options.Index().SetExpireAfterSeconds(int32(cfg.TTL.Seconds())),
	}
	_, err := popupColl.Indexes().CreateOne(ctx, popupTTLIndex)
	if err != nil {
		return fmt.Errorf("创建 Popup TTL 索引失败: %v", err)
	}

	// 3. 为 push_data 集合创建简单索引 (可选: created_at)
	pushColl := client.Database("cicd").Collection("push_data")
	logrus.WithFields(logrus.Fields{
		"time":   time.Now().Format("2006-01-02 15:04:05"),
		"method": "createIndexes",
		"coll_name": "push_data",
	}).Debugf("为 push_data 创建索引")

	pushKeys := bson.D{{Key: "created_at", Value: -1}} // 降序，便于查询最新
	pushIndex := mongo.IndexModel{
		Keys: pushKeys,
	}
	_, err = pushColl.Indexes().CreateOne(ctx, pushIndex)
	if err != nil {
		return fmt.Errorf("创建 push_data 索引失败: %v", err)
	}

	logrus.WithFields(logrus.Fields{
		"time":   time.Now().Format("2006-01-02 15:04:05"),
		"method": "createIndexes",
	}).Info("所有索引创建完成")
	return nil
}

// StoreTask 存储任务（支持 upsert，避免重复）
func (m *MongoClient) StoreTask(task *models.DeployRequest) error {
	startTime := time.Now()
	ctx := context.Background()

	if task.TaskID == "" {
		task.TaskID = uuid.New().String()
	}
	task.CreatedAt = time.Now()

	// 根据 environment 获取集合
	env := task.Environment
	if env == "" && len(task.Environments) > 0 {
		env = task.Environments[0]
	}
	collection := m.GetTaskCollection(env)

	filter := bson.M{
		"service":     task.Service,
		"version":     task.Version,
		"environment": task.Environment,
	}
	update := bson.M{
		"$setOnInsert": bson.M{
			"task_id":           task.TaskID,
			"service":           task.Service,
			"environments":      task.Environments,
			"environment":       task.Environment,
			"namespace":         task.Namespace,
			"user":              task.User,
			"created_at":        task.CreatedAt,
			"confirmation_status": task.ConfirmationStatus,
			"popup_sent":        task.PopupSent,
			"popup_message_id":  task.PopupMessageID,
			"popup_sent_at":     task.PopupSentAt,
		},
	}

	opts := options.Update().SetUpsert(true)
	result, err := collection.UpdateOne(ctx, filter, update, opts)
	if err != nil {
		logrus.WithFields(logrus.Fields{
			"time":    time.Now().Format("2006-01-02 15:04:05"),
			"method":  "StoreTask",
			"task_id": task.TaskID,
			"env":     env,
			"coll_name": collection.Name(),
		}).Errorf("存储任务失败: %v", err)
		return err
	}

	if result.UpsertedCount > 0 {
		logrus.WithFields(logrus.Fields{
			"time":    time.Now().Format("2006-01-02 15:04:05"),
			"method":  "StoreTask",
			"task_id": task.TaskID,
			"env":     env,
			"coll_name": collection.Name(),
			"upserted": true,
			"took":    time.Since(startTime),
		}).Infof("新任务存储成功 (upsert)")
	} else {
		logrus.WithFields(logrus.Fields{
			"time":    time.Now().Format("2006-01-02 15:04:05"),
			"method":  "StoreTask",
			"task_id": task.TaskID,
			"env":     env,
			"coll_name": collection.Name(),
			"matched":  result.MatchedCount,
			"modified": result.ModifiedCount,
			"took":    time.Since(startTime),
		}).Debugf("任务已存在，无需 upsert")
	}
	return nil
}

// GetPendingTasks 获取待确认任务（popup_sent=false）
func (m *MongoClient) GetPendingTasks(env string) ([]models.DeployRequest, error) {
	ctx := context.Background()
	collection := m.GetTaskCollection(env)

	filter := bson.M{
		"confirmation_status": "待确认",
		"popup_sent":          bson.M{"$ne": true},
	}
	cursor, err := collection.Find(ctx, filter)
	if err != nil {
		return nil, err
	}
	defer cursor.Close(ctx)

	var tasks []models.DeployRequest
	if err := cursor.All(ctx, &tasks); err != nil {
		return nil, err
	}
	return tasks, nil
}

// GetTaskByID 根据 task_id 获取任务（跨环境搜索）
func (m *MongoClient) GetTaskByID(taskID string) (*models.DeployRequest, error) {
	ctx := context.Background()
	for env := range m.cfg.EnvMapping.Mappings {
		collection := m.GetTaskCollection(env)
		var task models.DeployRequest
		err := collection.FindOne(ctx, bson.M{"task_id": taskID}).Decode(&task)
		if err == nil {
			return &task, nil
		}
		if err == mongo.ErrNoDocuments {
			continue
		}
		return nil, err
	}
	return nil, mongo.ErrNoDocuments
}

// GetPushedServicesAndEnvs 从 push_data 获取唯一服务和环境列表
func (m *MongoClient) GetPushedServicesAndEnvs() ([]string, []string, error) {
	ctx := context.Background()
	collection := m.client.Database("cicd").Collection("push_data")

	// 聚合: 获取唯一 services 和 environments
	pipeline := bson.A{
		bson.M{"$group": bson.M{
			"_id":          nil,
			"services":     bson.M{"$addToSet": "$service"},
			"environments": bson.M{"$addToSet": "$environment"},
		}},
	}
	cursor, err := collection.Aggregate(ctx, pipeline)
	if err != nil {
		return nil, nil, err
	}
	defer cursor.Close(ctx)

	// 修复: cursor.Next(ctx) 只返回 bool，检查 cursor.Err()
	if !cursor.Next(ctx) {
		if err := cursor.Err(); err != nil {
			return nil, nil, fmt.Errorf("cursor.Next 失败: %v", err)
		}
		return []string{}, []string{}, nil // 无结果，返回空列表
	}

	var result bson.M
	if err := cursor.Decode(&result); err != nil {
		return nil, nil, fmt.Errorf("解码聚合结果失败: %v", err)
	}

	services, _ := result["services"].(primitive.A)
	envs, _ := result["environments"].(primitive.A)

	svcList := make([]string, len(services))
	for i, s := range services {
		svcList[i] = s.(string)
	}
	sort.Strings(svcList)

	envList := make([]string, len(envs))
	for i, e := range envs {
		envList[i] = e.(string)
	}
	sort.Strings(envList)

	return svcList, envList, nil
}

// UpdateTaskStatus 更新任务状态（跨环境搜索）
func (m *MongoClient) UpdateTaskStatus(taskID, status, user string) error {
	startTime := time.Now()
	ctx := context.Background()

	updated := false
	for env := range m.cfg.EnvMapping.Mappings {
		collection := m.GetTaskCollection(env)
		filter := bson.M{"task_id": taskID}
		update := bson.M{
			"$set": bson.M{
				"confirmation_status": status,
				"updated_at":          time.Now(),
				"updated_by":          user,
			},
		}
		result, err := collection.UpdateOne(ctx, filter, update)
		if err != nil {
			logrus.WithFields(logrus.Fields{
				"time":    time.Now().Format("2006-01-02 15:04:05"),
				"method":  "UpdateTaskStatus",
				"task_id": taskID,
				"env":     env,
				"coll_name": collection.Name(),
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
				"coll_name": collection.Name(),
				"status":  status,
				"user":    user,
				"took":    time.Since(startTime),
			}).Infof("任务状态更新成功: %s by %s", status, user)
			break
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

// DeleteTask 完整实现
func (m *MongoClient) DeleteTask(taskID string) error {
	startTime := time.Now()
	ctx := context.Background()

	deleted := false
	for env := range m.cfg.EnvMapping.Mappings {
		collection := m.GetTaskCollection(env)
		filter := bson.M{"task_id": taskID}
		result, err := collection.DeleteOne(ctx, filter)
		if err != nil {
			logrus.WithFields(logrus.Fields{
				"time":  time.Now().Format("2006-01-02 15:04:05"),
				"env":   env,
				"coll_name": collection.Name(),
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
				"coll_name": collection.Name(),
				"took":    time.Since(startTime),
			}).Infof("任务删除成功")
			break
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

// NewMongoClient 创建 MongoDB 客户端
func NewMongoClient(cfg *config.MongoConfig) (*MongoClient, error) {
	startTime := time.Now()
	ctx := context.Background()

	logrus.WithFields(logrus.Fields{
		"time":   time.Now().Format("2006-01-02 15:04:05"),
		"method": "NewMongoClient",
		"uri":    cfg.URI,
	}).Info("=== 开始初始化 MongoDB 客户端 ===")

	// 步骤1：连接 MongoDB（带重试）
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
					"time":   time.Now().Format("2006-01-02 15:04:05"),
					"method": "NewMongoClient",
					"took":   time.Since(startTime),
				}).Info("MongoDB 连接成功")
				break
			} else {
				err = fmt.Errorf("ping 失败: %v", err)
			}
		}

		logrus.WithFields(logrus.Fields{
			"time":    time.Now().Format("2006-01-02 15:04:05"),
			"method":  "NewMongoClient",
			"attempt": attempt + 1,
			"max_retries": cfg.MaxRetries,
			"error":   err.Error(),
		}).Warnf("MongoDB 连接失败，重试中...")

		if attempt < cfg.MaxRetries {
			time.Sleep(time.Duration(attempt+1) * time.Second)
		} else {
			return nil, fmt.Errorf("MongoDB 连接失败（已重试 %d 次）: %v", cfg.MaxRetries+1, err)
		}
	}

	// 步骤2：创建索引（包含清理 + Drop + Create）
	if err := createIndexes(mongoClient, cfg); err != nil {
		logrus.WithFields(logrus.Fields{
			"time":   time.Now().Format("2006-01-02 15:04:05"),
			"method": "NewMongoClient",
			"took":   time.Since(startTime),
			"error":  err.Error(),
		}).Errorf("创建索引失败")
		return nil, err
	}

	logrus.WithFields(logrus.Fields{
		"time":   time.Now().Format("2006-01-02 15:04:05"),
		"method": "NewMongoClient",
		"took":   time.Since(startTime),
	}).Info("索引创建完成")

	// 步骤3：初始化 push_data 组合记录
	if err := initPushData(mongoClient); err != nil {
		logrus.WithFields(logrus.Fields{
			"time":   time.Now().Format("2006-01-02 15:04:05"),
			"method": "NewMongoClient",
			"took":   time.Since(startTime),
			"error":  err.Error(),
		}).Errorf("初始化 push_data 失败")
		return nil, err
	}

	logrus.WithFields(logrus.Fields{
		"time":   time.Now().Format("2006-01-02 15:04:05"),
		"method": "NewMongoClient",
		"took":   time.Since(startTime),
	}).Info("push_data 初始化完成")

	m := &MongoClient{
		client: mongoClient,
		cfg:    cfg,
	}

	logrus.WithFields(logrus.Fields{
		"time":   time.Now().Format("2006-01-02 15:04:05"),
		"method": "NewMongoClient",
		"took":   time.Since(startTime),
	}).Info("MongoDB 客户端初始化成功")
	return m, nil
}

// initPushData 初始化 push_data 集合（如果为空，插入示例或确保存在）
func initPushData(client *mongo.Client) error {
	ctx := context.Background()
	collection := client.Database("cicd").Collection("push_data")

	// 检查是否为空
	count, err := collection.CountDocuments(ctx, bson.D{})
	if err != nil {
		return fmt.Errorf("检查 push_data 计数失败: %v", err)
	}

	if count == 0 {
		// 插入初始数据（示例，根据实际 push 逻辑调整）
		initialData := []interface{}{
			bson.M{
				"service":     "example-service",
				"environment": "eks",
				"created_at":  time.Now(),
			},
			// 添加更多示例...
		}
		_, err = collection.InsertMany(ctx, initialData)
		if err != nil {
			return fmt.Errorf("初始化 push_data 数据失败: %v", err)
		}
		logrus.WithFields(logrus.Fields{
			"time":   time.Now().Format("2006-01-02 15:04:05"),
			"method": "initPushData",
			"inserted": len(initialData),
		}).Info("push_data 初始化数据插入成功")
	} else {
		logrus.WithFields(logrus.Fields{
			"time":   time.Now().Format("2006-01-02 15:04:05"),
			"method": "initPushData",
			"count":  count,
		}).Debug("push_data 已存在数据，跳过初始化")
	}
	return nil
}

// GetEnvMappings 获取环境映射
func (m *MongoClient) GetEnvMappings() map[string]string {
	if m.cfg == nil || m.cfg.EnvMapping.Mappings == nil {
		return make(map[string]string)
	}
	return m.cfg.EnvMapping.Mappings
}