package storage

import (
	"github.com/redis/go-redis/v9"
	"context"
)

// RedisStorage 封装 Redis 操作
type RedisStorage struct {
	client *redis.Client
	ctx    context.Context
}

// NewRedisStorage 初始化 Redis 存储
func NewRedisStorage(addr string) (*RedisStorage, error) {
	client := redis.NewClient(&redis.Options{
		Addr: addr,
	})
	_, err := client.Ping(context.Background()).Result()
	if err != nil {
		return nil, err
	}
	return &RedisStorage{
		client: client,
		ctx:    context.Background(),
	}, nil
}

// Set 设置键值对
func (s *RedisStorage) Set(key, value string) error {
	return s.client.Set(s.ctx, key, value, 0).Err()
}

// Get 获取键值
func (s *RedisStorage) Get(key string) (string, error) {
	return s.client.Get(s.ctx, key).Result()
}

// Delete 删除键
func (s *RedisStorage) Delete(key string) error {
	return s.client.Del(s.ctx, key).Err()
}

// Push 推入队列
func (s *RedisStorage) Push(queue, value string) error {
	return s.client.LPush(s.ctx, queue, value).Err()
}

// Pop 从队列弹出
func (s *RedisStorage) Pop(queue string) (string, error) {
	return s.client.RPop(s.ctx, queue).Result()
}

// List 获取队列中的所有元素
func (s *RedisStorage) List(queue string) ([]string, error) {
	return s.client.LRange(s.ctx, queue, 0, -1).Result()
}