// 文件: internal/storage/stats.go
package storage

import (
	"context"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

// StatsStorage 封装统计 MongoDB 操作
type StatsStorage struct {
	client *mongo.Client
	db     *mongo.Database
	ctx    context.Context
}

// NewStatsStorage 初始化统计 MongoDB 存储
func NewStatsStorage(uri string) (*StatsStorage, error) {
	ctx := context.Background()
	clientOpts := options.Client().ApplyURI(uri)
	client, err := mongo.Connect(ctx, clientOpts)
	if err != nil {
		return nil, err
	}

	if err := client.Ping(ctx, nil); err != nil {
		return nil, err
	}

	db := client.Database("k8s_cicd_stats")
	s := &StatsStorage{
		client: client,
		db:     db,
		ctx:    ctx,
	}

	coll := db.Collection("deploys")
	_, err = coll.Indexes().CreateMany(ctx, []mongo.IndexModel{
		{Keys: bson.D{{"service", 1}}},
		{Keys: bson.D{{"environment", 1}}},
		{Keys: bson.D{{"version", 1}}},
		{Keys: bson.D{{"timestamp", 1}}},
	})
	if err != nil {
		return nil, err
	}

	return s, nil
}

// InsertDeploySuccess 插入成功部署记录
func (s *StatsStorage) InsertDeploySuccess(service, environment, version string) error {
	coll := s.db.Collection("deploys")
	doc := bson.D{
		{"service", service},
		{"environment", environment},
		{"version", version},
		{"timestamp", time.Now().UTC()},
	}
	_, err := coll.InsertOne(s.ctx, doc)
	return err
}

// GetStats 获取统计报告
func (s *StatsStorage) GetStats(match bson.D) ([]StatResult, error) {
	coll := s.db.Collection("deploys")
	pipeline := bson.A{
		{"$match", match},
		{"$group", bson.D{
			{"_id", bson.D{{"service", "$service"}, {"environment", "$environment"}}},
			{"versions", bson.D{{"$addToSet", "$version"}}},
		}},
		{"$project", bson.D{
			{"service", "$_id.service"},
			{"environment", "$_id.environment"},
			{"count", bson.D{{"$size", "$versions"}}},
		}},
	}
	cursor, err := coll.Aggregate(s.ctx, pipeline)
	if err != nil {
		return nil, err
	}
	defer cursor.Close(s.ctx)

	var results []StatResult
	if err := cursor.All(s.ctx, &results); err != nil {
		return nil, err
	}
	return results, nil
}

// StatResult 统计结果结构
type StatResult struct {
	Service     string `bson:"service"`
	Environment string `bson:"environment"`
	Count       int    `bson:"count"`
}

// DeleteMonthData 删除指定月份数据
func (s *StatsStorage) DeleteMonthData(start, end time.Time) error {
	coll := s.db.Collection("deploys")
	filter := bson.D{{"timestamp", bson.D{{"$gte", start}, {"$lt", end}}}}
	_, err := coll.DeleteMany(s.ctx, filter)
	return err
}