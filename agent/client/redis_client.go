package client

import (
	"context"
	"fmt"
	"time"

	"k8s-cicd/agent/config"
	"k8s-cicd/agent/models"

	"github.com/redis/go-redis/v9"
	"github.com/sirupsen/logrus"
)

type RedisClient struct {
	client *redis.Client
	cfg    *config.RedisConfig
}

// NewRedisClient 创建Redis客户端
func NewRedisClient(cfg *config.RedisConfig) (*RedisClient, error) {
	// 步骤1：创建Redis客户端配置
	rdb := redis.NewClient(&redis.Options{
		Addr:         cfg.Addr,
		Password:     cfg.Password,
		DB:           cfg.DB,
		MaxRetries:   cfg.MaxRetries,
		PoolSize:     10,
		MinIdleConns: 2,
		DialTimeout:  5 * time.Second,
		ReadTimeout:  3 * time.Second,
		WriteTimeout: 3 * time.Second,
	})

	// 步骤2：测试连接
	ctx := context.Background()
	_, err := rdb.Ping(ctx).Result()
	if err != nil {
		return nil, fmt.Errorf("Redis连接失败: %v", err)
	}

	logrus.Info("Redis连接成功")
	return &RedisClient{
		client: rdb,
		cfg:    cfg,
	}, nil
}

// PushDeployments 推送部署任务到Redis（带TTL）
func (r *RedisClient) PushDeployments(deploys []models.DeployRequest) error {
	ctx := context.Background()
	
	for _, deploy := range deploys {
		// 步骤1：序列化部署任务
		data, err := models.MarshalDeploy(deploy)
		if err != nil {
			return err
		}

		// 步骤2：生成Redis Key
		key := fmt.Sprintf("deploy:pending:%s:%s:%s", deploy.Service, deploy.Environments[0], deploy.User)

		// 步骤3：设置带TTL的Key
		err = r.client.Set(ctx, key, data, r.cfg.TTL).Err()
		if err != nil {
			return fmt.Errorf("推送部署任务失败: %v", err)
		}
	}

	logrus.Infof("成功推送 %d 个部署任务到Redis", len(deploys))
	return nil
}

// QueryPendingTasks 查询用户的待处理任务
func (r *RedisClient) QueryPendingTasks(environment, user string) ([]models.DeployRequest, error) {
	ctx := context.Background()
	
	// 步骤1：使用SCAN命令查找匹配的keys
	var cursor uint64
	var keys []string
	var tasks []models.DeployRequest

	for {
		// 查找匹配的keys
		keys, cursor, _ = r.client.Scan(ctx, cursor, fmt.Sprintf("deploy:pending:%s:*:%s", environment, user), 10).Result()
		
		// 步骤2：获取每个key的值
		for _, key := range keys {
			data, err := r.client.Get(ctx, key).Result()
			if err != nil {
				continue
			}

			// 反序列化任务
			task, err := models.UnmarshalDeploy([]byte(data))
			if err == nil {
				tasks = append(tasks, task)
			}
		}

		// 步骤3：检查是否扫描完成
		if cursor == 0 {
			break
		}
	}

	logrus.Infof("查询到 %d 个待处理任务", len(tasks))
	return tasks, nil
}

// UpdateTaskStatus 更新任务状态并清理Redis数据
func (r *RedisClient) UpdateTaskStatus(service, version, environment, user, status string) error {
	ctx := context.Background()
	
	// 步骤1：生成key
	key := fmt.Sprintf("deploy:%s:%s:%s:%s", status, service, environment, user)

	// 步骤2：设置状态（带TTL）
	err := r.client.Set(ctx, key, version, r.cfg.TTL).Err()
	if err != nil {
		return err
	}

	// 步骤3：删除pending key
	pendingKey := fmt.Sprintf("deploy:pending:%s:%s:%s", service, environment, user)
	r.client.Del(ctx, pendingKey)

	logrus.Infof("任务状态更新成功: %s -> %s", service, status)
	return nil
}

// Close 关闭Redis连接
func (r *RedisClient) Close() error {
	return r.client.Close()
}