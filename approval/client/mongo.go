// 文件: mongo.go (完整文件，优化: 集合命名基于 clean_env (env replace "-" with "_") 如 tasks_eks_test，避免 namespace 冲突；添加 validateAndCleanData 在 createIndexes 前清理 null environment 和潜在重复；Drop 现有索引再创建，确保无违反唯一性；其他功能保留)
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
				docID := docs[i].(bson.M)["_id"].(primitive.ObjectID)
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

// 优化: createIndexes 先清理数据 + Drop 现有索引 (忽略不存在错误) + 创建新索引
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

		// 先 Drop 现有 TTL 索引 (忽略不存在)
		_, err := collection.Indexes().DropOne(ctx, mongo.IndexModel{
			Keys: bson.D{{Key: "created_at", Value: 1}},
		})
		if err != nil && !strings.Contains(err.Error(), "ns not found") {
			logrus.WithFields(logrus.Fields{
				"time":   time.Now().Format("2006-01-02 15:04:05"),
				"method": "createIndexes",
				"env":    env,
				"error":  err.Error(),
			}).Warnf("Drop TTL 索引忽略错误: %v", err)
		}

		// Drop 复合唯一索引
		_, err = collection.Indexes().DropOne(ctx, mongo.IndexModel{
			Keys: bson.D{
				{Key: "service", Value: 1},
				{Key: "version", Value: 1},
				{Key: "environment", Value: 1},
				{Key: "created_at", Value: 1},
			},
		})
		if err != nil && !strings.Contains(err.Error(), "ns not found") {
			logrus.WithFields(logrus.Fields{
				"time":   time.Now().Format("2006-01-02 15:04:05"),
				"method": "createIndexes",
				"env":    env,
				"error":  err.Error(),
			}).Warnf("Drop 复合索引忽略错误: %v", err)
		}

		// 创建 TTL 索引
		_, err = collection.Indexes().CreateOne(ctx, mongo.IndexModel{
			Keys:    bson.D{{Key: "created_at", Value: 1}},
			Options: options.Index().SetExpireAfterSeconds(int32(cfg.TTL.Seconds())),
		})
		if err != nil {
			return fmt.Errorf("创建任务 TTL 索引失败 [%s]: %v", env, err)
		}

		// 创建复合唯一索引
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
	}

	// 2. popup_messages TTL (不变，Drop 先)
	popupColl := client.Database("cicd").Collection("popup_messages")
	_, err := popupColl.Indexes().DropOne(ctx, mongo.IndexModel{
		Keys: bson.D{{Key: "sent_at", Value: 1}},
	})
	if err != nil && !strings.Contains(err.Error(), "ns not found") {
		logrus.WithFields(logrus.Fields{
			"time":   time.Now().Format("2006-01-02 15:04:05"),
			"method": "createIndexes",
			"error":  err.Error(),
		}).Warnf("Drop popup TTL 索引忽略错误: %v", err)
	}

	_, err = popupColl.Indexes().CreateOne(ctx, mongo.IndexModel{
		Keys:    bson.D{{Key: "sent_at", Value: 1}},
		Options: options.Index().SetExpireAfterSeconds(7 * 24 * 60 * 60),
	})
	if err != nil {
		return fmt.Errorf("创建弹窗消息 TTL 索引失败: %v", err)
	}

	return nil
}

// initPushData 从 push_data 集合查询 _id="global_push_data"，生成组合记录插入同一集合 (忽略 global 记录)
func initPushData(client *mongo.Client) error {
	ctx := context.Background()
	pushColl := client.Database("pushlist").Collection("push_data")

	logrus.WithFields(logrus.Fields{
		"time":   time.Now().Format("2006-01-02 15:04:05"),
		"method": "initPushData",
	}).Info("=== 开始初始化 push_data 组合数据 ===")

	// 查询 global_push_data 文档
	globalDoc := bson.M{}
	err := pushColl.FindOne(ctx, bson.M{"_id": "global_push_data"}).Decode(&globalDoc)
	if err != nil {
		if err == mongo.ErrNoDocuments {
			logrus.WithFields(logrus.Fields{
				"time":   time.Now().Format("2006-01-02 15:04:05"),
				"method": "initPushData",
			}).Warn("未找到 _id=global_push_data 文档，跳过初始化")
			return nil
		}
		logrus.WithFields(logrus.Fields{
			"time":   time.Now().Format("2006-01-02 15:04:05"),
			"method": "initPushData",
			"error":  err.Error(),
		}).Errorf("查询 global_push_data 失败: %v", err)
		return err
	}

	logrus.WithFields(logrus.Fields{
		"time":   time.Now().Format("2006-01-02 15:04:05"),
		"method": "initPushData",
		"global_doc": fmt.Sprintf("%+v", globalDoc),
	}).Debug("找到 global_push_data 文档")

	// 提取 services 和 environments (处理 primitive.A 或 []interface{})
	var services []string
	if servicesVal, ok := globalDoc["services"]; ok {
		var servicesSlice []interface{}
		if servicesPrimitive, okPrimitive := servicesVal.(bson.A); okPrimitive {
			servicesSlice = servicesPrimitive
		} else if servicesArray, okArray := servicesVal.([]interface{}); okArray {
			servicesSlice = servicesArray
		}
		for _, s := range servicesSlice {
			if str, okStr := s.(string); okStr && str != "" {
				services = append(services, str)
			}
		}
		logrus.WithFields(logrus.Fields{
			"time":   time.Now().Format("2006-01-02 15:04:05"),
			"method": "initPushData",
			"services_len": len(services),
		}).Debugf("提取 services 成功: %d 个", len(services))
	}

	var environments []string
	if envsVal, ok := globalDoc["environments"]; ok {
		var envsSlice []interface{}
		if envsPrimitive, okPrimitive := envsVal.(bson.A); okPrimitive {
			envsSlice = envsPrimitive
		} else if envsArray, okArray := envsVal.([]interface{}); okArray {
			envsSlice = envsArray
		}
		for _, e := range envsSlice {
			if str, okStr := e.(string); okStr && str != "" {
				environments = append(environments, str)
			}
		}
		logrus.WithFields(logrus.Fields{
			"time":        time.Now().Format("2006-01-02 15:04:05"),
			"method":      "initPushData",
			"environments": environments,
			"envs_len":    len(environments),
		}).Debugf("提取 environments 成功: %v (%d 个)", environments, len(environments))
	}

	if len(services) == 0 || len(environments) == 0 {
		logrus.WithFields(logrus.Fields{
			"time":   time.Now().Format("2006-01-02 15:04:05"),
			"method": "initPushData",
			"services_len": len(services),
			"envs_len":     len(environments),
		}).Warn("services 或 environments 为空，跳过生成组合")
		return nil
	}

	// 删除现有组合记录 (保留 global_push_data)
	_, err = pushColl.DeleteMany(ctx, bson.M{"_id": bson.M{"$ne": "global_push_data"}})
	if err != nil {
		logrus.WithFields(logrus.Fields{
			"time":   time.Now().Format("2006-01-02 15:04:05"),
			"method": "initPushData",
			"error":  err.Error(),
		}).Errorf("删除现有组合记录失败: %v", err)
		return err
	}
	logrus.WithFields(logrus.Fields{
		"time":   time.Now().Format("2006-01-02 15:04:05"),
		"method": "initPushData",
	}).Debug("组合记录已清空 (保留 global_push_data)")

	// 生成组合记录并插入
	generated := 0
	for _, service := range services {
		for _, env := range environments {
			comboDoc := bson.M{
				"_id":        fmt.Sprintf("%s-%s", service, env), // 组合ID
				"service":    service,
				"environment": env,
				"created_at": time.Now().UnixMilli(),
			}
			_, err = pushColl.InsertOne(ctx, comboDoc)
			if err != nil {
				logrus.WithFields(logrus.Fields{
					"time":   time.Now().Format("2006-01-02 15:04:05"),
					"method": "initPushData",
					"service": service,
					"env":     env,
					"error":   err.Error(),
				}).Errorf("插入组合记录失败: %v", err)
				continue
			}
			generated++
			logrus.WithFields(logrus.Fields{
				"time":   time.Now().Format("2006-01-02 15:04:05"),
				"method": "initPushData",
				"service": service,
				"env":     env,
				"combo_id": comboDoc["_id"],
			}).Debugf("生成组合记录成功")
		}
	}

	logrus.WithFields(logrus.Fields{
		"time":     time.Now().Format("2006-01-02 15:04:05"),
		"method":   "initPushData",
		"generated": generated,
		"total_combos": len(services) * len(environments),
	}).Infof("push_data 组合生成完成: %d / %d", generated, len(services)*len(environments))
	return nil
}

// GetPushedServicesAndEnvs 从 push_data 获取已 push 的 services 和 environments（遍历组合记录）
func (m *MongoClient) GetPushedServicesAndEnvs() ([]string, []string, error) {
	startTime := time.Now()
	ctx := context.Background()

	logrus.WithFields(logrus.Fields{
		"time":   time.Now().Format("2006-01-02 15:04:05"),
		"method": "GetPushedServicesAndEnvs",
	}).Info("=== 开始扫描 pushlist.push_data 组合记录 ===")

	pushColl := m.client.Database("pushlist").Collection("push_data")

	// 查询所有组合记录 (排除 global_push_data)
	cursor, err := pushColl.Find(ctx, bson.M{"_id": bson.M{"$ne": "global_push_data"}})
	if err != nil {
		return nil, nil, fmt.Errorf("查询 push_data 失败: %v", err)
	}
	defer cursor.Close(ctx)

	var comboDocs []bson.M
	if err := cursor.All(ctx, &comboDocs); err != nil {
		return nil, nil, fmt.Errorf("解码 push_data 失败: %v", err)
	}

	services := make(map[string]bool)
	envs := make(map[string]bool)

	logrus.WithFields(logrus.Fields{
		"time":   time.Now().Format("2006-01-02 15:04:05"),
		"method": "GetPushedServicesAndEnvs",
		"combo_count": len(comboDocs),
	}).Debugf("找到 %d 个组合记录", len(comboDocs))

	for i, doc := range comboDocs {
		service, _ := doc["service"].(string)
		env, _ := doc["environment"].(string)

		if service != "" {
			services[service] = true
			logrus.WithFields(logrus.Fields{
				"time":     time.Now().Format("2006-01-02 15:04:05"),
				"method":   "GetPushedServicesAndEnvs",
				"doc_index": i,
				"service":  service,
				"env":      env,
			}).Debugf("提取 service: %s (env=%s)", service, env)
		}

		if env != "" {
			envs[env] = true
			logrus.WithFields(logrus.Fields{
				"time":     time.Now().Format("2006-01-02 15:04:05"),
				"method":   "GetPushedServicesAndEnvs",
				"doc_index": i,
				"service":  service,
				"env":      env,
			}).Debugf("提取 env: %s (service=%s)", env, service)
		}

		// Debug: 打印完整 doc
		logrus.WithFields(logrus.Fields{
			"time":      time.Now().Format("2006-01-02 15:04:05"),
			"method":    "GetPushedServicesAndEnvs",
			"doc_index": i,
			"full_doc":  fmt.Sprintf("%+v", doc),
		}).Debugf("成功解码组合文档 %d", i)
	}

	svcList := make([]string, 0, len(services))
	for s := range services {
		svcList = append(svcList, s)
	}
	sort.Strings(svcList)

	envList := make([]string, 0, len(envs))
	for e := range envs {
		envList = append(envList, e)
	}
	sort.Strings(envList)

	logrus.WithFields(logrus.Fields{
		"time":     time.Now().Format("2006-01-02 15:04:05"),
		"method":   "GetPushedServicesAndEnvs",
		"services": svcList,
		"envs":     envList,
		"count_svc": len(svcList),
		"count_env": len(envList),
		"took":     time.Since(startTime),
	}).Infof("提取 push 数据成功: %d services, %d envs", len(svcList), len(envList))

	logrus.WithFields(logrus.Fields{
		"time":   time.Now().Format("2006-01-02 15:04:05"),
		"method": "GetPushedServicesAndEnvs",
	}).Info("=== 扫描 pushlist.push_data 结束 ===")

	return svcList, envList, nil
}

// GetPendingTasks 获取待确认任务 (状态="待确认" + popup_sent=false)
func (m *MongoClient) GetPendingTasks(env string) ([]models.DeployRequest, error) {
	startTime := time.Now()
	ctx := context.Background()

	collection := m.GetTaskCollection(env)

	filter := bson.M{
		"confirmation_status": "待确认",
		"popup_sent":          bson.M{"$ne": true},
		"environment":         env,
	}

	opts := options.Find().SetSort(bson.D{{Key: "created_at", Value: 1}})

	cursor, err := collection.Find(ctx, filter, opts)
	if err != nil {
		return nil, fmt.Errorf("查询待确认任务失败 [%s]: %v", env, err)
	}
	defer cursor.Close(ctx)

	var tasks []models.DeployRequest
	if err := cursor.All(ctx, &tasks); err != nil {
		return nil, fmt.Errorf("解码任务失败 [%s]: %v", env, err)
	}

	logrus.WithFields(logrus.Fields{
		"time":   time.Now().Format("2006-01-02 15:04:05"),
		"method": "GetPendingTasks",
		"env":    env,
		"coll_name": collection.Name(),
		"count":    len(tasks),
		"took":     time.Since(startTime),
	}).Infof("查询待确认任务成功: %d 个 (filter: status=待确认, popup_sent≠true)", len(tasks))

	return tasks, nil
}

// StoreTask 存储/更新任务 (upsert，支持多环境拆分存储)
func (m *MongoClient) StoreTask(task *models.DeployRequest) error {
	startTime := time.Now()
	ctx := context.Background()

	if task.TaskID == "" {
		task.TaskID = uuid.New().String()
	}

	if task.Environment == "" {
		task.Environment = task.Environments[0] // 确保 Environment 设置
	}

	env := task.Environment
	collection := m.GetTaskCollection(env)

	filter := bson.M{
		"service":     task.Service,
		"version":     task.Version,
		"environment": env,
		"task_id":     task.TaskID,
	}

	update := bson.M{
		"$setOnInsert": bson.M{
			"task_id":          task.TaskID,
			"service":          task.Service,
			"environments":     task.Environments,
			"environment":      env,
			"namespace":        task.Namespace,
			"version":          task.Version,
			"user":             task.User,
			"created_at":       task.CreatedAt,
			"confirmation_status": task.ConfirmationStatus,
		},
		"$set": bson.M{
			"updated_at": time.Now(),
		},
	}

	opts := options.Update().SetUpsert(true)
	result, err := collection.UpdateOne(ctx, filter, update, opts)
	if err != nil {
		return fmt.Errorf("存储任务失败 [%s]: %v", env, err)
	}

	if result.UpsertedCount > 0 {
		logrus.WithFields(logrus.Fields{
			"time":      time.Now().Format("2006-01-02 15:04:05"),
			"method":    "StoreTask",
			"task_id":   task.TaskID,
			"env":       env,
			"coll_name": collection.Name(),
			"upserted":  true,
			"took":      time.Since(startTime),
		}).Infof("新任务已插入: %s (service=%s, version=%s)", task.TaskID, task.Service, task.Version)
	} else if result.ModifiedCount > 0 {
		logrus.WithFields(logrus.Fields{
			"time":       time.Now().Format("2006-01-02 15:04:05"),
			"method":     "StoreTask",
			"task_id":    task.TaskID,
			"env":        env,
			"coll_name":  collection.Name(),
			"modified":   true,
			"took":       time.Since(startTime),
		}).Debugf("任务已更新: %s", task.TaskID)
	} else {
		logrus.WithFields(logrus.Fields{
			"time":     time.Now().Format("2006-01-02 15:04:05"),
			"method":   "StoreTask",
			"task_id":  task.TaskID,
			"env":      env,
			"coll_name": collection.Name(),
			"matched":  result.MatchedCount,
			"took":     time.Since(startTime),
		}).Debugf("任务已存在，无变更: %s", task.TaskID)
	}

	return nil
}

// StoreTaskIfNotExistsEnv 生成 UUID TaskID + 设置 CreatedAt (并发安全)
func (m *MongoClient) StoreTaskIfNotExistsEnv(task models.DeployRequest, env string) error {
	startTime := time.Now()

	if len(task.Environments) == 0 || task.Environments[0] != env {
		return fmt.Errorf("task.Environments[0] 必须为 %s", env)
	}

	if task.TaskID == "" {
		task.TaskID = uuid.New().String()
	}

	task.CreatedAt = time.Now()

	coll := m.GetTaskCollection(env)

	filter := bson.M{
		"service":     task.Service,
		"version":     task.Version,
		"environment": env,
		"created_at":  task.CreatedAt,
	}

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

	if result.UpsertedCount > 0 {
		logrus.WithFields(logrus.Fields{
			"time":    time.Now().Format("2006-01-02 15:04:05"),
			"method":  "StoreTaskIfNotExistsEnv",
			"task_id": task.TaskID,
			"env":     env,
			"coll_name": coll.Name(),
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
			"coll_name": coll.Name(),
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

// GetTaskByID 根据 task_id 获取任务（跨环境搜索，状态过滤）
func (m *MongoClient) GetTaskByID(taskID string) (*models.DeployRequest, error) {
	startTime := time.Now()
	ctx := context.Background()

	for env := range m.cfg.EnvMapping.Mappings {
		collection := m.GetTaskCollection(env)
		filter := bson.M{
			"task_id": taskID,
			"confirmation_status": bson.M{"$in": []string{"待确认", "已确认", "已拒绝"}},
		}
		var task models.DeployRequest
		if err := collection.FindOne(ctx, filter).Decode(&task); err == nil {
			logrus.WithFields(logrus.Fields{
				"time":   time.Now().Format("2006-01-02 15:04:05"),
				"method": "GetTaskByID",
				"task_id": taskID,
				"env":     env,
				"coll_name": collection.Name(),
				"popup_message_id": task.PopupMessageID,
				"status": task.ConfirmationStatus,
				"full_task": fmt.Sprintf("%+v", task),
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

// MarkPopupSent 标记弹窗已发送
func (m *MongoClient) MarkPopupSent(taskID string, messageID int) error {
	startTime := time.Now()
	ctx := context.Background()

	updated := false
	for env := range m.cfg.EnvMapping.Mappings {
		collection := m.GetTaskCollection(env)
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
				"coll_name": collection.Name(),
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
				"coll_name": collection.Name(),
				"message_id": messageID,
				"took":    time.Since(startTime),
			}).Infof("弹窗标记成功: popup_sent=true, id=%d", messageID)
			break
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

// UpdateTaskStatus 更新任务状态
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

// GetEnvMappings 获取环境映射
func (m *MongoClient) GetEnvMappings() map[string]string {
	if m.cfg == nil || m.cfg.EnvMapping.Mappings == nil {
		return make(map[string]string)
	}
	return m.cfg.EnvMapping.Mappings
}