// 文件: internal/storage/mongo.go
package storage

import (
	"context"
	"errors"
	"regexp"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

// DeployRequest 部署请求数据结构
type DeployRequest struct {
	Service      string    `json:"service" bson:"service"`
	Environments []string  `json:"environments" bson:"environments"`
	Version      string    `json:"version" bson:"version"`
	User         string    `json:"user" bson:"user"`
	Status       string    `json:"status,omitempty" bson:"status"`
	CreatedAt    time.Time `bson:"created_at"`
}

// StatusRequest 状态更新请求数据结构
type StatusRequest struct {
	Service     string `json:"service" bson:"service"`
	Version     string `json:"version" bson:"version"`
	Environment string `json:"environment" bson:"environment"`
	User        string `json:"user" bson:"user"`
	Status      string `json:"status" bson:"status"`
}

// MongoStorage 封装主 MongoDB 操作，支持并发查询
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

// createIndexes 创建所有集合的索引，确保TTL自动过期
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
		{Keys: bson.D{{"status", 1}}},
		{Keys: bson.D{{"user", 1}}},
		{Keys: bson.D{{"environments", 1}}},
		{Keys: bson.D{{"created_at", 1}}, Options: options.Index().SetExpireAfterSeconds(ttlSeconds)},
	})
	return err
}

// StoreServiceEnvironments 存储或合并服务环境，支持批量更新
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

// GetServices 获取所有服务列表
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

// GetServiceEnvironments 获取指定服务的环境列表
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

// InsertDeployRequest 插入部署请求，确保数据持久化避免丢失
func (s *MongoStorage) InsertDeployRequest(req DeployRequest) error {
	coll := s.db.Collection("deploy_queue")
	req.CreatedAt = time.Now().UTC()
	_, err := coll.InsertOne(s.ctx, req)
	return err
}

// QueryDeployQueueByServiceEnv 查询pending任务（重新设计：基于service模糊和environment精确）
func (s *MongoStorage) QueryDeployQueueByServiceEnv(service, environment string) ([]DeployRequest, error) {
	coll := s.db.Collection("deploy_queue")
	// 模糊查询service（使用正则），environment精确在environments数组中，status=pending
	filter := bson.D{
		{"service", bson.D{{"$regex", regexp.QuoteMeta(service)}}},
		{"environments", bson.D{{"$in", []string{environment}}}},
		{"status", "pending"},
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

// UpdateStatus 更新任务状态
func (s *MongoStorage) UpdateStatus(req StatusRequest) (bool, error) {
	coll := s.db.Collection("deploy_queue")
	filter := bson.D{
		{"service", req.Service},
		{"version", req.Version},
		{"user", req.User},
		{"environments", bson.D{{"$in", []string{req.Environment}}}},
	}
	update := bson.D{{"$set", bson.D{{"status", req.Status}}}}
	result, err := coll.UpdateOne(s.ctx, filter, update)
	if err != nil {
		return false, err
	}
	return result.ModifiedCount > 0, nil
}