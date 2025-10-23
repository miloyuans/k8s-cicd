// 文件: internal/storage/mongo.go
package storage

import (
	"context"
	"encoding/json"
	"errors"
	"k8s-cicd/internal/api"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

// MongoStorage 封装 MongoDB 操作，使用不同的集合隔离数据
// - services 集合：存储服务列表，仅 /push 接口写入，其他可读
// - environments 集合：存储环境列表，仅 /push 接口写入，其他可读
// - deploy_queue 集合：存储部署请求，/deploy 插入，/status 更新，/query 查询
type MongoStorage struct {
	client     *mongo.Client
	db         *mongo.Database
	ctx        context.Context
	dbName     string // 数据库名称
}

// NewMongoStorage 初始化 MongoDB 存储
func NewMongoStorage(uri string) (*MongoStorage, error) {
	ctx := context.Background()
	clientOpts := options.Client().ApplyURI(uri)
	client, err := mongo.Connect(ctx, clientOpts)
	if err != nil {
		return nil, err
	}

	if err := client.Ping(ctx, nil); err != nil {
		return nil, err
	}

	dbName := "k8s_cicd" // 默认数据库名称
	db := client.Database(dbName)

	// 初始化集合（如果不存在会自动创建）
	_, _ = db.Collection("services").Indexes().CreateOne(ctx, mongo.IndexModel{Keys: bson.D{{Key: "_id", Value: 1}}})
	_, _ = db.Collection("environments").Indexes().CreateOne(ctx, mongo.IndexModel{Keys: bson.D{{Key: "_id", Value: 1}}})
	_, _ = db.Collection("deploy_queue").Indexes().CreateOne(ctx, mongo.IndexModel{Keys: bson.D{{Key: "service", Value: 1}, {Key: "version", Value: 1}, {Key: "user", Value: 1}}})

	return &MongoStorage{
		client: client,
		db:     db,
		ctx:    ctx,
		dbName: dbName,
	}, nil
}

// StoreServices 存储或更新 services 列表（仅 /push 使用）
func (s *MongoStorage) StoreServices(services []string) error {
	collection := s.db.Collection("services")
	data, err := json.Marshal(services)
	if err != nil {
		return err
	}

	// 使用 upsert 更新或插入
	filter := bson.D{{Key: "_id", Value: "services"}}
	update := bson.D{{Key: "$set", Value: bson.D{{Key: "list", Value: string(data)}}}}
	opts := options.Update().SetUpsert(true)
	_, err = collection.UpdateOne(s.ctx, filter, update, opts)
	return err
}

// StoreEnvironments 存储或更新 environments 列表（仅 /push 使用）
func (s *MongoStorage) StoreEnvironments(environments []string) error {
	collection := s.db.Collection("environments")
	data, err := json.Marshal(environments)
	if err != nil {
		return err
	}

	// 使用 upsert 更新或插入
	filter := bson.D{{Key: "_id", Value: "environments"}}
	update := bson.D{{Key: "$set", Value: bson.D{{Key: "list", Value: string(data)}}}}
	opts := options.Update().SetUpsert(true)
	_, err = collection.UpdateOne(s.ctx, filter, update, opts)
	return err
}

// GetServices 获取 services 列表
func (s *MongoStorage) GetServices() ([]string, error) {
	collection := s.db.Collection("services")
	var result struct {
		List string `bson:"list"`
	}
	filter := bson.D{{Key: "_id", Value: "services"}}
	if err := collection.FindOne(s.ctx, filter).Decode(&result); err != nil {
		if errors.Is(err, mongo.ErrNoDocuments) {
			return []string{}, nil
		}
		return nil, err
	}

	var services []string
	if err := json.Unmarshal([]byte(result.List), &services); err != nil {
		return nil, err
	}
	return services, nil
}

// GetEnvironments 获取 environments 列表
func (s *MongoStorage) GetEnvironments() ([]string, error) {
	collection := s.db.Collection("environments")
	var result struct {
		List string `bson:"list"`
	}
	filter := bson.D{{Key: "_id", Value: "environments"}}
	if err := collection.FindOne(s.ctx, filter).Decode(&result); err != nil {
		if errors.Is(err, mongo.ErrNoDocuments) {
			return []string{}, nil
		}
		return nil, err
	}

	var envs []string
	if err := json.Unmarshal([]byte(result.List), &envs); err != nil {
		return nil, err
	}
	return envs, nil
}

// InsertDeployRequest 插入部署请求到 deploy_queue 集合
func (s *MongoStorage) InsertDeployRequest(req api.DeployRequest) error {
	collection := s.db.Collection("deploy_queue")
	_, err := collection.InsertOne(s.ctx, req)
	return err
}

// QueryDeployQueue 查询 deploy_queue 集合中的待处理任务
func (s *MongoStorage) QueryDeployQueue(environment, user string) ([]api.DeployRequest, error) {
	collection := s.db.Collection("deploy_queue")
	filter := bson.D{
		{Key: "environments", Value: bson.D{{Key: "$in", Value: []string{environment}}}},
		{Key: "user", Value: user},
		{Key: "status", Value: bson.D{{Key: "$nin", Value: []string{"success", "failure"}}}},
	}
	cursor, err := collection.Find(s.ctx, filter)
	if err != nil {
		return nil, err
	}
	defer cursor.Close(s.ctx)

	var results []api.DeployRequest
	if err := cursor.All(s.ctx, &results); err != nil {
		return nil, err
	}
	return results, nil
}

// UpdateStatus 更新 deploy_queue 集合中的任务状态
func (s *MongoStorage) UpdateStatus(req api.StatusRequest) (bool, error) {
	collection := s.db.Collection("deploy_queue")
	filter := bson.D{
		{Key: "service", Value: req.Service},
		{Key: "version", Value: req.Version},
		{Key: "user", Value: req.User},
		{Key: "environments", Value: bson.D{{Key: "$in", Value: []string{req.Environment}}}},
	}
	update := bson.D{{Key: "$set", Value: bson.D{{Key: "status", Value: req.Status}}}}
	result, err := collection.UpdateOne(s.ctx, filter, update)
	if err != nil {
		return false, err
	}
	return result.ModifiedCount > 0, nil
}