// 文件: internal/storage/redis.go
package storage

import (
	"context"
	"encoding/json"
	"sync"

	"github.com/redis/go-redis/v9"
)

// RedisStorage 封装 Redis 操作（支持并发安全）
type RedisStorage struct {
	client *redis.Client
	ctx    context.Context
	mu     sync.RWMutex // 并发读写锁
}

// NewRedisStorage 初始化 Redis 存储
func NewRedisStorage(addr string) (*RedisStorage, error) {
	client := redis.NewClient(&redis.Options{
		Addr: addr,
	})
	if _, err := client.Ping(context.Background()).Result(); err != nil {
		return nil, err
	}
	return &RedisStorage{
		client: client,
		ctx:    context.Background(),
	}, nil
}

// Set 设置键值对（线程安全）
func (s *RedisStorage) Set(key, value string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.client.Set(s.ctx, key, value, 0).Err()
}

// Get 获取键值（线程安全）
func (s *RedisStorage) Get(key string) (string, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.client.Get(s.ctx, key).Result()
}

// Delete 删除键（线程安全）
func (s *RedisStorage) Delete(key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.client.Del(s.ctx, key).Err()
}

// Push 推入队列（线程安全）
func (s *RedisStorage) Push(queue, value string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.client.LPush(s.ctx, queue, value).Err()
}

// List 获取队列中的所有元素（线程安全）
func (s *RedisStorage) List(queue string) ([]string, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.client.LRange(s.ctx, queue, 0, -1).Result()
}

// GetServices 获取服务列表（线程安全）
func (s *RedisStorage) GetServices() ([]string, error) {
	data, err := s.Get("services")
	if err != nil || data == "" {
		return []string{}, nil
	}
	var services []string
	if err := json.Unmarshal([]byte(data), &services); err != nil {
		return nil, err
	}
	return services, nil
}

// GetEnvironments 获取环境列表（线程安全）
func (s *RedisStorage) GetEnvironments() ([]string, error) {
	data, err := s.Get("environments")
	if err != nil || data == "" {
		return []string{}, nil
	}
	var envs []string
	if err := json.Unmarshal([]byte(data), &envs); err != nil {
		return nil, err
	}
	return envs, nil
}

// *** 新增：asyncStoreServices 异步存储服务列表（保留原始大小写） ***
func (s *RedisStorage) asyncStoreServices(services []string) error {
	if len(services) == 0 {
		return nil
	}
	
	data, err := json.Marshal(services)
	if err != nil {
		return err
	}
	return s.Set("services", string(data))
}

// *** 新增：asyncStoreEnvironments 异步存储环境列表（保留原始大小写） ***
func (s *RedisStorage) asyncStoreEnvironments(environments []string) error {
	if len(environments) == 0 {
		return nil
	}
	
	data, err := json.Marshal(environments)
	if err != nil {
		return err
	}
	return s.Set("environments", string(data))
}