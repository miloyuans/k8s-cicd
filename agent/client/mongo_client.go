// mongo_client.go
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

// MongoClient MongoDB客户端
type MongoClient struct {
	client *mongo.Client      // MongoDB客户端（未导出）
	cfg    *config.MongoConfig // MongoDB配置
}

// GetClient 获取MongoDB客户端
func (m *MongoClient) GetClient() *mongo.Client {
	return m.client
}

// NewMongoClient 创建MongoDB客户端
func NewMongoClient(cfg *config.MongoConfig) (*MongoClient, error) {
	startTime := time.Now()
	// 步骤1：创建MongoDB客户端配置
	clientOptions := options.Client().ApplyURI(cfg.URI).
		SetConnectTimeout(5 * time.Second).
		SetMaxPoolSize(10).
		SetMinPoolSize(2)

	// 步骤2：连接MongoDB
	client, err := mongo.Connect(context.Background(), clientOptions)
	if err != nil {
		logrus.WithFields(logrus.Fields{
			"time":   time.Now().Format("2006-01-02 15:04:05"),
			"method": "NewMongoClient",
			"took":   time.Since(startTime),
		}).Errorf("MongoDB连接失败: %v", err)
		return nil, fmt.Errorf("MongoDB连接失败: %v", err)
	}

	// 步骤3：测试连接
	ctx := context.Background()
	if err := client.Ping(ctx, readpref.Primary()); err != nil {
		logrus.WithFields(logrus.Fields{
			"time":   time.Now().Format("2006-01-02 15:04:05"),
			"method": "NewMongoClient",
			"took":   time.Since(startTime),
		}).Errorf("MongoDB ping失败: %v", err)
		return nil, fmt.Errorf("MongoDB ping失败: %v", err)
	}

	// 步骤4：创建TTL索引
	if err := createTTLIndexes(client, cfg); err != nil {
		logrus.WithFields(logrus.Fields{
			"time":   time.Now().Format("2006-01-02 15:04:05"),
			"method": "NewMongoClient",
			"took":   time.Since(startTime),
		}).Errorf("创建TTL索引失败: %v", err)
		return nil, err
	}

	logrus.WithFields(logrus.Fields{
		"time":   time.Now().Format("2006-01-02 15:04:05"),
		"method": "NewMongoClient",
		"took":   time.Since(startTime),
	}).Info("MongoDB连接成功")
	return &MongoClient{client: client, cfg: cfg}, nil
}

// createTTLIndexes 创建TTL索引
func createTTLIndexes(client *mongo.Client, cfg *config.MongoConfig) error {
	ctx := context.Background()
	// 步骤1：为每个环境创建任务集合的TTL索引
	for env := range cfg.EnvMapping.Mappings {
		collection := client.Database("cicd").Collection(fmt.Sprintf("tasks_%s", env))
		_, err := collection.Indexes().CreateOne(ctx, mongo.IndexModel{
			Keys:    bson.D{{Key: "created_at", Value: 1}},
			Options: options.Index().SetExpireAfterSeconds(int32(cfg.TTL.Seconds())),
		})
		if err != nil {
			return fmt.Errorf("创建任务TTL索引失败 [%s]: %v", env, err)
		}
	}

	// 步骤2：为版本集合创建唯一索引
	versionsColl := client.Database("cicd").Collection("versions")
	_, err := versionsColl.Indexes().CreateOne(ctx, mongo.IndexModel{
		Keys:    bson.D{{Key: "service", Value: 1}, {Key: "version", Value: 1}},
		Options: options.Index().SetUnique(true),
	})
	if err != nil {
		return fmt.Errorf("创建版本唯一索引失败: %v", err)
	}

	// 步骤3：为删除任务集合创建TTL索引
	deleteTasksColl := client.Database("cicd").Collection("delete_tasks")
	_, err = deleteTasksColl.Indexes().CreateOne(ctx, mongo.IndexModel{
		Keys:    bson.D{{Key: "created_at", Value: 1}},
		Options: options.Index().SetExpireAfterSeconds(int32(cfg.TTL.Seconds())),
	})
	if err != nil {
		return fmt.Errorf("创建删除任务TTL索引失败: %v", err)
	}

	return nil
}

// PushDeployments 推送部署任务到MongoDB
func (m *MongoClient) PushDeployments(deploys []models.DeployRequest) error {
	startTime := time.Now()
	ctx := context.Background()

	for _, deploy := range deploys {
		// 步骤1：存储到版本集合
		versionsColl := m.client.Database("cicd").Collection("versions")
		_, err := versionsColl.UpdateOne(ctx,
			bson.M{"service": deploy.Service, "version": deploy.Version},
			bson.M{
				"$set": bson.M{
					"service":    deploy.Service,
					"version":    deploy.Version,
					"updated_at": time.Now(),
				},
			},
			options.Update().SetUpsert(true),
		)
		if err != nil {
			logrus.WithFields(logrus.Fields{
				"time":   time.Now().Format("2006-01-02 15:04:05"),
				"method": "PushDeployments",
				"took":   time.Since(startTime),
			}).Errorf("存储版本失败: %v", err)
			return err
		}

		// 步骤2：存储到环境特定任务集合，并添加confirmation_sent字段
		collection := m.client.Database("cicd").Collection(fmt.Sprintf("tasks_%s", deploy.Environments[0]))
		deploy.CreatedAt = time.Now()
		deploy.ConfirmationSent = false // 新增字段
		_, err = collection.InsertOne(ctx, deploy)
		if err != nil {
			logrus.WithFields(logrus.Fields{
				"time":   time.Now().Format("2006-01-02 15:04:05"),
				"method": "PushDeployments",
				"took":   time.Since(startTime),
			}).Errorf("存储任务失败: %v", err)
			return err
		}
	}

	logrus.WithFields(logrus.Fields{
		"time":   time.Now().Format("2006-01-02 15:04:05"),
		"method": "PushDeployments",
		"took":   time.Since(startTime),
	}).Infof("推送 %d 个部署任务成功", len(deploys))
	return nil
}

// UpdateTaskStatus 更新任务状态
func (m *MongoClient) UpdateTaskStatus(service, version, environment, user, status string) error {
	startTime := time.Now()
	ctx := context.Background()

	collection := m.client.Database("cicd").Collection(fmt.Sprintf("tasks_%s", environment))
	_, err := collection.UpdateOne(ctx,
		bson.M{"service": service, "version": version, "environment": environment, "user": user},
		bson.M{"$set": bson.M{"status": status}},
	)
	if err != nil {
		logrus.WithFields(logrus.Fields{
			"time":   time.Now().Format("2006-01-02 15:04:05"),
			"method": "UpdateTaskStatus",
			"took":   time.Since(startTime),
		}).Errorf("更新任务状态失败: %v", err)
		return err
	}

	logrus.WithFields(logrus.Fields{
		"time":   time.Now().Format("2006-01-02 15:04:05"),
		"method": "UpdateTaskStatus",
		"took":   time.Since(startTime),
	}).Infof("任务状态更新成功: %s v%s [%s/%s] -> %s", service, version, environment, user, status)
	return nil
}

// UpdateConfirmationSent 更新确认发送状态
func (m *MongoClient) UpdateConfirmationSent(service, version, environment, user string) error {
	startTime := time.Now()
	ctx := context.Background()

	collection := m.client.Database("cicd").Collection(fmt.Sprintf("tasks_%s", environment))
	_, err := collection.UpdateOne(ctx,
		bson.M{"service": service, "version": version, "environment": environment, "user": user},
		bson.M{"$set": bson.M{"confirmation_sent": true}},
	)
	if err != nil {
		logrus.WithFields(logrus.Fields{
			"time":   time.Now().Format("2006-01-02 15:04:05"),
			"method": "UpdateConfirmationSent",
			"took":   time.Since(startTime),
		}).Errorf("更新确认发送状态失败: %v", err)
		return err
	}

	logrus.WithFields(logrus.Fields{
		"time":   time.Now().Format("2006-01-02 15:04:05"),
		"method": "UpdateConfirmationSent",
		"took":   time.Since(startTime),
	}).Infof("确认发送状态更新成功: %s v%s [%s/%s]", service, version, environment, user)
	return nil
}

// IsConfirmationSent 检查确认是否已发送
func (m *MongoClient) IsConfirmationSent(deploy models.DeployRequest) (bool, error) {
	startTime := time.Now()
	ctx := context.Background()

	collection := m.client.Database("cicd").Collection(fmt.Sprintf("tasks_%s", deploy.Environments[0]))
	var task models.DeployRequest
	err := collection.FindOne(ctx, bson.M{
		"service":     deploy.Service,
		"version":     deploy.Version,
		"environment": deploy.Environments[0],
		"user":        deploy.User,
	}).Decode(&task)
	if err != nil {
		logrus.WithFields(logrus.Fields{
			"time":   time.Now().Format("2006-01-02 15:04:05"),
			"method": "IsConfirmationSent",
			"took":   time.Since(startTime),
		}).Errorf("检查确认发送状态失败: %v", err)
		return false, err
	}

	return task.ConfirmationSent, nil
}

// CheckDuplicateTask 检查任务是否已存在
func (m *MongoClient) CheckDuplicateTask(deploy models.DeployRequest) (bool, error) {
	startTime := time.Now()
	ctx := context.Background()

	// 步骤1：查询任务集合
	collection := m.client.Database("cicd").Collection(fmt.Sprintf("tasks_%s", deploy.Environments[0]))
	count, err := collection.CountDocuments(ctx, bson.M{
		"service":     deploy.Service,
		"version":     deploy.Version,
		"environment": deploy.Environments[0],
	})
	if err != nil {
		logrus.WithFields(logrus.Fields{
			"time":   time.Now().Format("2006-01-02 15:04:05"),
			"method": "CheckDuplicateTask",
			"took":   time.Since(startTime),
		}).Errorf("检查重复任务失败: %v", err)
		return false, err
	}

	if count > 0 {
		logrus.WithFields(logrus.Fields{
			"time":   time.Now().Format("2006-01-02 15:04:05"),
			"method": "CheckDuplicateTask",
			"took":   time.Since(startTime),
		}).Warnf("任务重复: %s v%s [%s]", deploy.Service, deploy.Version, deploy.Environments[0])
		return true, nil
	}

	return false, nil
}

// StoreTaskWithDeduplication 存储任务（带去重）
func (m *MongoClient) StoreTaskWithDeduplication(deploy models.DeployRequest) error {
	startTime := time.Now()
	// 步骤1：检查重复
	isDuplicate, err := m.CheckDuplicateTask(deploy)
	if err != nil {
		logrus.WithFields(logrus.Fields{
			"time":   time.Now().Format("2006-01-02 15:04:05"),
			"method": "StoreTaskWithDeduplication",
			"took":   time.Since(startTime),
		}).Errorf("检查重复任务失败: %v", err)
		return err
	}
	if isDuplicate {
		return nil // 重复任务直接返回
	}

	// 步骤2：存储任务
	return m.PushDeployments([]models.DeployRequest{deploy})
}

// DeleteTask 删除任务
func (m *MongoClient) DeleteTask(service, version, environment, user string) error {
	startTime := time.Now()
	ctx := context.Background()

	// 步骤1：删除任务记录
	collection := m.client.Database("cicd").Collection(fmt.Sprintf("tasks_%s", environment))
	result, err := collection.DeleteOne(ctx, bson.M{
		"service":     service,
		"version":     version,
		"environment": environment,
		"user":        user,
	})
	if err != nil {
		logrus.WithFields(logrus.Fields{
			"time":   time.Now().Format("2006-01-02 15:04:05"),
			"method": "DeleteTask",
			"took":   time.Since(startTime),
		}).Errorf("删除任务失败: %v", err)
		return err
	}

	if result.DeletedCount == 0 {
		logrus.WithFields(logrus.Fields{
			"time":   time.Now().Format("2006-01-02 15:04:05"),
			"method": "DeleteTask",
			"took":   time.Since(startTime),
		}).Warnf("未找到任务: %s v%s [%s/%s]", service, version, environment, user)
	} else {
		logrus.WithFields(logrus.Fields{
			"time":   time.Now().Format("2006-01-02 15:04:05"),
			"method": "DeleteTask",
			"took":   time.Since(startTime),
		}).Infof("任务删除成功: %s v%s [%s/%s]", service, version, environment, user)
	}
	return nil
}

// CleanCompletedTasks 清理已完成的任务
func (m *MongoClient) CleanCompletedTasks() error {
	startTime := time.Now()
	ctx := context.Background()

	// 步骤1：查询待删除任务
	deleteTasksColl := m.client.Database("cicd").Collection("delete_tasks")
	cursor, err := deleteTasksColl.Find(ctx, bson.M{})
	if err != nil {
		logrus.WithFields(logrus.Fields{
			"time":   time.Now().Format("2006-01-02 15:04:05"),
			"method": "CleanCompletedTasks",
			"took":   time.Since(startTime),
		}).Errorf("查询待删除任务失败: %v", err)
		return err
	}
	defer cursor.Close(ctx)

	// 步骤2：清理任务
	for cursor.Next(ctx) {
		var task struct {
			Service     string `bson:"service"`
			Version     string `bson:"version"`
			Environment string `bson:"environment"`
			User        string `bson:"user"`
			Status      string `bson:"status"`
		}
		if err := cursor.Decode(&task); err != nil {
			continue
		}

		// 删除任务记录
		collection := m.client.Database("cicd").Collection(fmt.Sprintf("tasks_%s", task.Environment))
		_, err := collection.DeleteOne(ctx, bson.M{
			"service":     task.Service,
			"version":     task.Version,
			"environment": task.Environment,
			"user":        task.User,
			"status":      task.Status,
		})
		if err != nil {
			logrus.WithFields(logrus.Fields{
				"time":   time.Now().Format("2006-01-02 15:04:05"),
				"method": "CleanCompletedTasks",
				"took":   time.Since(startTime),
			}).Errorf("删除任务记录失败: %v", err)
			continue
		}

		// 删除待删除记录
		_, err = deleteTasksColl.DeleteOne(ctx, bson.M{
			"service":     task.Service,
			"version":     task.Version,
			"environment": task.Environment,
			"user":        task.User,
			"status":      task.Status,
		})
		if err != nil {
			logrus.WithFields(logrus.Fields{
				"time":   time.Now().Format("2006-01-02 15:04:05"),
				"method": "CleanCompletedTasks",
				"took":   time.Since(startTime),
			}).Errorf("删除待删除记录失败: %v", err)
			continue
		}
	}

	logrus.WithFields(logrus.Fields{
		"time":   time.Now().Format("2006-01-02 15:04:05"),
		"method": "CleanCompletedTasks",
		"took":   time.Since(startTime),
	}).Info("清理已完成任务成功")
	return nil
}

// Close 关闭MongoDB连接
func (m *MongoClient) Close() error {
	startTime := time.Now()
	err := m.client.Disconnect(context.Background())
	if err != nil {
		logrus.WithFields(logrus.Fields{
			"time":   time.Now().Format("2006-01-02 15:04:05"),
			"method": "Close",
			"took":   time.Since(startTime),
		}).Errorf("关闭MongoDB连接失败: %v", err)
		return err
	}
	logrus.WithFields(logrus.Fields{
		"time":   time.Now().Format("2006-01-02 15:04:05"),
		"method": "Close",
		"took":   time.Since(startTime),
	}).Info("MongoDB连接关闭成功")
	return nil
}