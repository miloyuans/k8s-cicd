// 文件: internal/storage/mongo.go
package storage

import (
	"context"
	"errors"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

// DeployRequest 单环境部署请求（新结构）
type DeployRequest struct {
	Service      string    `json:"service" bson:"service"`
	Environment  string    `json:"environment" bson:"environment"`   // 单环境
	Version      string    `json:"version" bson:"version"`
	User         string    `json:"user" bson:"user"`
	Status       string    `json:"status,omitempty" bson:"status"`
	TaskGroupID  string    `json:"task_group_id" bson:"task_group_id"` // 关联组
	CreatedAt    time.Time `bson:"created_at"`
}

// StatusRequest 状态更新请求
type StatusRequest struct {
	Service     string `json:"service" bson:"service"`
	Version     string `json:"version" bson:"version"`
	Environment string `json:"environment" bson:"environment"`
	User        string `json:"user" bson:"user"`
	Status      string `json:"status" bson:"status"`
}

// MongoStorage 封装主 MongoDB 操作
type MongoStorage struct {
	client *mongo.Client
	db     *mongo.Database
	ctx    context.Context
}

// NewMongoStorage 初始化主 MongoDB 存储
func NewMongoStorage(uri string, ttlHours int) (*MongoStorage, error) {
	ctx := context.Background()
	clientOpts := options.Client().ApplyURI(uri)
	client, err := mongo.Connect(ctx, clientOpts)
	if err != nil {
		return nil, err
	}

	if err := client.Ping(ctx, nil); err != nil {
		return nil, err
	}

	db := client.Database("k8s_cicd")
	s := &MongoStorage{
		client: client,
		db:     db,
		ctx:    ctx,
	}

	err = s.createIndexes(ttlHours)
	if err != nil {
		return nil, err
	}

	return s, nil
}

// createIndexes 创建索引
func (s *MongoStorage) createIndexes(ttlHours int) error {
	svcColl := s.db.Collection("service_environments")
	_, err := svcColl.Indexes().CreateMany(s.ctx, []mongo.IndexModel{
		{Keys: bson.D{{"_id", 1}}},
		{Keys: bson.D{{"environments", 1}}},
	})
	if err != nil {
		return err
	}

	deployColl := s.db.Collection("deploy_queue")
	ttlSeconds := int32(ttlHours * 3600)
	_, err = deployColl.Indexes().CreateMany(s.ctx, []mongo.IndexModel{
		{Keys: bson.D{{"service", 1}}},
		{Keys: bson.D{{"version", 1}}},
		{Keys: bson.D{{"environment", 1}}},  // 新增
		{Keys: bson.D{{"status", 1}}},
		{Keys: bson.D{{"user", 1}}},
		{Keys: bson.D{{"task_group_id", 1}}}, // 新增
		{Keys: bson.D{{"created_at", 1}}, Options: options.Index().SetExpireAfterSeconds(ttlSeconds)},
	})
	return err
}

// StoreServiceEnvironments 存储服务环境
func (s *MongoStorage) StoreServiceEnvironments(services, environments []string) error {
	if len(services) == 0 || len(environments) == 0 {
		return errors.New("服务名和环境必须存在")
	}

	coll := s.db.Collection("service_environments")
	for _, svc := range services {
		filter := bson.D{{"_id", svc}}
		update := bson.D{{"$addToSet", bson.D{{"environments", bson.D{{"$each", environments}}}}}}
		opts := options.Update().SetUpsert(true)
		_, err := coll.UpdateOne(s.ctx, filter, update, opts)
		if err != nil {
			return err
		}
	}
	return nil
}

// GetServices 获取所有服务
func (s *MongoStorage) GetServices() ([]string, error) {
	coll := s.db.Collection("service_environments")
	cursor, err := coll.Distinct(s.ctx, "_id", bson.D{})
	if err != nil {
		return nil, err
	}

	services := make([]string, 0, len(cursor))
	for _, v := range cursor {
		if str, ok := v.(string); ok {
			services = append(services, str)
		}
	}
	return services, nil
}

// GetServiceEnvironments 获取服务环境
func (s *MongoStorage) GetServiceEnvironments(service string) ([]string, error) {
	coll := s.db.Collection("service_environments")
	var result struct {
		Environments []string `bson:"environments"`
	}
	filter := bson.D{{"_id", service}}
	if err := coll.FindOne(s.ctx, filter).Decode(&result); err != nil {
		if errors.Is(err, mongo.ErrNoDocuments) {
			return nil, nil
		}
		return nil, err
	}
	return result.Environments, nil
}

// InsertDeployRequest 插入单环境任务
func (s *MongoStorage) InsertDeployRequest(req DeployRequest) error {
	coll := s.db.Collection("deploy_queue")
	req.CreatedAt = time.Now().UTC()
	_, err := coll.InsertOne(s.ctx, req)
	return err
}

// QueryPendingTasksByEnvs 查询多个环境的所有 pending 任务
func (s *MongoStorage) QueryPendingTasksByEnvs(service string, environments []string, user string) ([]DeployRequest, error) {
	coll := s.db.Collection("deploy_queue")
	filter := bson.D{
		{"service", service},
		{"environment", bson.D{{"$in", environments}}},
		{"status", "pending"},
	}
	if user != "" {
		filter = append(filter, bson.E{"user", user})
	}

	cursor, err := coll.Find(s.ctx, filter)
	if err != nil {
		return nil, err
	}
	defer cursor.Close(s.ctx)

	var results []DeployRequest
	if err := cursor.All(s.ctx, &results); err != nil {
		return nil, err
	}
	return results, nil
}

// QuerySinglePendingTask 查询单条 pending 任务（单环境）
func (s *MongoStorage) QuerySinglePendingTask(service string, environments []string, user string) (*DeployRequest, error) {
	coll := s.db.Collection("deploy_queue")
	filter := bson.D{
		{"service", service},
		{"environment", bson.D{{"$in", environments}}},
		{"status", "pending"},
	}
	if user != "" {
		filter = append(filter, bson.E{"user", user})
	}

	opts := options.FindOne()
	var result DeployRequest
	err := coll.FindOne(s.ctx, filter, opts).Decode(&result)
	if err != nil {
		if errors.Is(err, mongo.ErrNoDocuments) {
			return nil, nil
		}
		return nil, err
	}
	return &result, nil
}

// UpdateStatus 更新单环境任务状态
func (s *MongoStorage) UpdateStatus(req StatusRequest) (bool, error) {
	coll := s.db.Collection("deploy_queue")
	filter := bson.D{
		{"service", req.Service},
		{"version", req.Version},
		{"environment", req.Environment},
		{"user", req.User},
		{"status", "assigned"},
	}
	update := bson.D{{"$set", bson.D{{"status", req.Status}}}}
	result, err := coll.UpdateOne(s.ctx, filter, update)
	if err != nil {
		return false, err
	}
	return result.ModifiedCount > 0, nil
}

// GetDeployByFilter 获取任务（用于日志）
func (s *MongoStorage) GetDeployByFilter(service, version, environment string) ([]DeployRequest, error) {
	coll := s.db.Collection("deploy_queue")
	filter := bson.D{
		{"service", service},
		{"version", version},
		{"environment", environment},
	}
	cursor, err := coll.Find(s.ctx, filter)
	if err != nil {
		return nil, err
	}
	defer cursor.Close(s.ctx)

	var results []DeployRequest
	if err := cursor.All(s.ctx, &results); err != nil {
		return nil, err
	}
	return results, nil
}

// GetGroupEnvironments 获取同组所有环境
func (s *MongoStorage) GetGroupEnvironments(groupID string) ([]string, error) {
	coll := s.db.Collection("deploy_queue")
	filter := bson.D{{"task_group_id", groupID}}
	cursor, err := coll.Find(s.ctx, filter)
	if err != nil {
		return nil, err
	}
	defer cursor.Close(s.ctx)

	envs := make([]string, 0)
	seen := make(map[string]bool)
	for cursor.Next(s.ctx) {
		var t DeployRequest
		if err := cursor.Decode(&t); err != nil {
			continue
		}
		if !seen[t.Environment] {
			envs = append(envs, t.Environment)
			seen[t.Environment] = true
		}
	}
	return envs, nil
}