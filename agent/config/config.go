// 修改后的 config.go：添加 TelegramConfig 到 Config 结构，并从 YAML 加载。
// 在 LoadConfig 中处理默认值和合并（如果需要环境变量覆盖，可选）。

package config

import (
	"os"
	"strconv"
	"time"

	"github.com/sirupsen/logrus"
	"gopkg.in/yaml.v3"
)

// APIConfig API 服务配置
type APIConfig struct {
	BaseURL       string        `yaml:"base_url"`       // API 基础地址
	PushInterval  time.Duration `yaml:"push_interval"`  // 推送间隔
	QueryInterval time.Duration `yaml:"query_interval"` // 任务轮询间隔（从 Mongo）
	MaxRetries    int           `yaml:"max_retries"`    // 最大重试次数
}

// K8sAuthConfig Kubernetes 认证配置
type K8sAuthConfig struct {
	AuthType   string `yaml:"auth_type"`    // 认证类型: kubeconfig / serviceaccount
	Kubeconfig string `yaml:"kubeconfig"`   // kubeconfig 路径（完整路径）
	Namespace  string `yaml:"namespace"`    // 默认命名空间
}

// MongoConfig MongoDB 配置
type MongoConfig struct {
	URI        string           `yaml:"uri"`         // MongoDB 连接 URI
	TTL        time.Duration    `yaml:"ttl"`         // 数据过期时间
	MaxRetries int              `yaml:"max_retries"` // 连接重试次数
	EnvMapping EnvMappingConfig `yaml:"env_mapping"` // 环境映射
}

// TaskConfig 任务队列配置
type TaskConfig struct {
	MaxRetries     int `yaml:"max_retries"`         // 最大重试次数
	RetryDelay     int `yaml:"retry_delay_seconds"` // 重试延迟
	QueueWorkers   int `yaml:"queue_workers"`       // 工作线程数
	MaxQueueSize   int `yaml:"max_queue_size"`      // 最大队列大小
}

// DeployConfig 部署配置
type DeployConfig struct {
	WaitTimeout     time.Duration `yaml:"wait_timeout"`     // 部署等待超时
	RollbackTimeout time.Duration `yaml:"rollback_timeout"` // 回滚等待超时
	PollInterval    time.Duration `yaml:"poll_interval"`    // 健康检查间隔
}

// EnvMappingConfig 环境到命名空间的映射
type EnvMappingConfig struct {
	Mappings map[string]string `yaml:"mappings"` // env -> namespace
}

// TelegramConfig Telegram 配置（新增）
type TelegramConfig struct {
	Token     string `yaml:"token"`     // Bot Token
	GroupID   string `yaml:"group_id"`  // 群组ID
	Enabled   bool   `yaml:"enabled"`   // 是否启用（默认true）
}

// Config 完整配置结构（添加 Telegram）
type Config struct {
	API        APIConfig        `yaml:"api"`
	Kubernetes K8sAuthConfig    `yaml:"kubernetes"`
	Mongo      MongoConfig      `yaml:"mongo"`
	Task       TaskConfig       `yaml:"task"`
	Deploy     DeployConfig     `yaml:"deploy"`
	EnvMapping EnvMappingConfig `yaml:"env_mapping"`
	Telegram   TelegramConfig   `yaml:"telegram"` // 新增
	LogLevel   string           `yaml:"log_level"` // 日志级别
}

// LoadConfig 加载配置文件（不变，但会加载 Telegram）
func LoadConfig(filename string) (*Config, error) {
	startTime := time.Now()

	// 步骤1：读取文件
	data, err := os.ReadFile(filename)
	if err != nil {
		logrus.WithFields(logrus.Fields{
			"time":   time.Now().Format("2006-01-02 15:04:05"),
			"method": "LoadConfig",
			"took":   time.Since(startTime),
		}).Errorf("读取配置文件失败: %v", err)
		return nil, err
	}

	// 步骤2：解析 YAML
	var cfg Config
	if err = yaml.Unmarshal(data, &cfg); err != nil {
		logrus.WithFields(logrus.Fields{
			"time":   time.Now().Format("2006-01-02 15:04:05"),
			"method": "LoadConfig",
			"took":   time.Since(startTime),
		}).Errorf("解析 YAML 失败: %v", err)
		return nil, err
	}

	// 步骤3：设置默认值
	cfg.setDefaults()

	// 步骤4：合并环境变量（可选覆盖 Telegram 等）
	cfg.mergeEnvVars()

	// 步骤5：设置日志级别
	logrus.SetLevel(logrus.InfoLevel)
	if cfg.LogLevel != "" {
		level, err := logrus.ParseLevel(cfg.LogLevel)
		if err == nil {
			logrus.SetLevel(level)
		} else {
			logrus.Warnf("无效日志级别: %s，使用默认 info", cfg.LogLevel)
		}
	}

	logrus.WithFields(logrus.Fields{
		"time":   time.Now().Format("2006-01-02 15:04:05"),
		"method": "LoadConfig",
		"took":   time.Since(startTime),
	}).Info("配置加载成功")
	return &cfg, nil
}

// setDefaults 设置默认值（添加 Telegram 默认）
// setDefaults 移除所有硬编码默认值
// setDefaults 设置默认值（强制 MaxRetries=1，发布失败不重试）
func (c *Config) setDefaults() {
	// API 默认值
	if c.API.PushInterval == 0 {
		c.API.PushInterval = 30 * time.Second
	}
	if c.API.QueryInterval == 0 {
		c.API.QueryInterval = 15 * time.Second
	}
	if c.API.MaxRetries == 0 {
		c.API.MaxRetries = 3
	}

	// MongoDB 默认值
	if c.Mongo.TTL == 0 {
		c.Mongo.TTL = 30 * 24 * time.Hour
	}
	if c.Mongo.MaxRetries == 0 {
		c.Mongo.MaxRetries = 3
	}
	if c.Mongo.URI == "" {
		c.Mongo.URI = "mongodb://localhost:27017"
	}

	// 任务队列：强制 MaxRetries=1，失败后直接进入失败处理
	if c.Task.MaxRetries == 0 {
		c.Task.MaxRetries = 1 // 关键：不再重试
	}
	if c.Task.RetryDelay == 0 {
		c.Task.RetryDelay = 30
	}
	if c.Task.QueueWorkers == 0 {
		c.Task.QueueWorkers = 5
	}
	if c.Task.MaxQueueSize == 0 {
		c.Task.MaxQueueSize = 100
	}

	// 部署默认值
	if c.Deploy.WaitTimeout == 0 {
		c.Deploy.WaitTimeout = 5 * time.Minute
	}
	if c.Deploy.RollbackTimeout == 0 {
		c.Deploy.RollbackTimeout = 3 * time.Minute
	}
	if c.Deploy.PollInterval == 0 {
		c.Deploy.PollInterval = 5 * time.Second
	}

	// Kubernetes 默认值
	if c.Kubernetes.AuthType == "" {
		c.Kubernetes.AuthType = "kubeconfig"
	}
	if c.Kubernetes.Namespace == "" {
		c.Kubernetes.Namespace = "default"
	}

	// Telegram：配置文件优先
	if c.Telegram.Token == "" {
		c.Telegram.Token = os.Getenv("TELEGRAM_TOKEN")
	}
	if c.Telegram.GroupID == "" {
		c.Telegram.GroupID = os.Getenv("TELEGRAM_GROUP_ID")
	}
	if c.Telegram.Token != "" && c.Telegram.GroupID != "" {
		c.Telegram.Enabled = true
	}
}

// mergeEnvVars 合并环境变量（添加 Telegram 覆盖）
func (c *Config) mergeEnvVars() {
	// MongoDB 环境变量
	if uri := os.Getenv("MONGO_URI"); uri != "" {
		c.Mongo.URI = uri
	}
	if ttl := os.Getenv("MONGO_TTL"); ttl != "" {
		if t, err := strconv.Atoi(ttl); err == nil {
			c.Mongo.TTL = time.Duration(t) * time.Hour
		}
	}

	// API 环境变量
	if url := os.Getenv("API_BASE_URL"); url != "" {
		c.API.BaseURL = url
	}

	// Telegram 环境变量覆盖（可选）
	if token := os.Getenv("TELEGRAM_TOKEN"); token != "" {
		c.Telegram.Token = token
	}
	if groupID := os.Getenv("TELEGRAM_GROUP_ID"); groupID != "" {
		c.Telegram.GroupID = groupID
	}
	if enabledStr := os.Getenv("TELEGRAM_ENABLED"); enabledStr != "" {
		if e, err := strconv.ParseBool(enabledStr); err == nil {
			c.Telegram.Enabled = e
		}
	}
}