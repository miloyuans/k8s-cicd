package config

import (
	"os"
	"time"

	"github.com/sirupsen/logrus"
	"gopkg.in/yaml.v3"
)

// TelegramBot 配置单个Telegram机器人的配置
type TelegramBot struct {
	Name        string              `yaml:"name"`          // 机器人名称
	Token       string              `yaml:"token"`         // Bot Token
	GroupID     string              `yaml:"group_id"`      // 群组ID
	Services    map[string][]string `yaml:"services"`      // 服务匹配规则: prefix -> 服务列表
	RegexMatch  bool                `yaml:"regex_match"`   // 是否使用正则匹配
	IsEnabled   bool                `yaml:"enabled"`       // 是否启用该机器人
}

// TelegramConfig Telegram多机器人配置
type TelegramConfig struct {
	Bots []TelegramBot `yaml:"bots"`
}

// APIConfig API服务配置
type APIConfig struct {
	BaseURL      string        `yaml:"base_url"`        // API基础地址
	PushInterval time.Duration `yaml:"push_interval"`   // 推送间隔，默认5s
	MaxRetries   int           `yaml:"max_retries"`     // 最大重试次数，0=无限重试
}

// K8sAuthConfig K8s认证方式配置（支持kubeconfig和ServiceAccount双重认证）
type K8sAuthConfig struct {
	AuthType    string `yaml:"auth_type"`    // "kubeconfig" 或 "serviceaccount"
	Kubeconfig  string `yaml:"kubeconfig"`   // kubeconfig文件路径
	Namespace   string `yaml:"namespace"`    // 默认命名空间
	ServiceName string `yaml:"service_name"` // ServiceAccount名称（集群内运行时）
}

// RedisConfig Redis配置（增加TTL支持）
type RedisConfig struct {
	Addr        string        `yaml:"addr"`
	Password    string        `yaml:"password"`
	DB          int           `yaml:"db"`
	TTL         time.Duration `yaml:"ttl"`           // 数据过期时间，默认24小时
	MaxRetries  int           `yaml:"max_retries"`   // 连接重试次数
	IdleTimeout time.Duration `yaml:"idle_timeout"`  // 空闲连接超时
}

// TaskConfig 任务队列配置
type TaskConfig struct {
	MaxRetries     int `yaml:"max_retries"`
	RetryDelay     int `yaml:"retry_delay_seconds"`
	QueueWorkers   int `yaml:"queue_workers"`
	PollInterval   int `yaml:"poll_interval_seconds"`
	MaxQueueSize   int `yaml:"max_queue_size"`
}

// DeployConfig 部署相关配置
type DeployConfig struct {
	WaitTimeout     time.Duration `yaml:"wait_timeout"`        // 等待Deployment就绪超时，默认5分钟
	RollbackTimeout time.Duration `yaml:"rollback_timeout"`    // 回滚等待超时，默认3分钟
	PollInterval    time.Duration `yaml:"poll_interval"`       // 健康检查轮询间隔，默认5秒
}

// UserConfig 用户配置
type UserConfig struct {
	Default string `yaml:"default"`  // 默认用户
}

// EnvMappingConfig 环境到命名空间的映射配置
type EnvMappingConfig struct {
	Mappings map[string]string `yaml:"mappings"`  // env -> namespace
}

// Config 全局配置结构
type Config struct {
	Telegram    TelegramConfig    `yaml:"telegram"`
	API         APIConfig         `yaml:"api"`
	Kubernetes  K8sAuthConfig     `yaml:"kubernetes"`
	Redis       RedisConfig       `yaml:"redis"`
	Task        TaskConfig        `yaml:"task"`
	Deploy      DeployConfig      `yaml:"deploy"`
	User        UserConfig        `yaml:"user"`
	EnvMapping  EnvMappingConfig  `yaml:"env_mapping"`
	LogLevel    string            `yaml:"log_level"`
}

// LoadConfig 从YAML文件加载配置，并合并环境变量
func LoadConfig(filePath string) (*Config, error) {
	// 步骤1：读取配置文件
	data, err := os.ReadFile(filePath)
	if err != nil {
		return nil, err
	}

	// 步骤2：解析YAML配置
	var cfg Config
	err = yaml.Unmarshal(data, &cfg)
	if err != nil {
		return nil, err
	}

	// 步骤3：设置默认值
	cfg.setDefaults()

	// 步骤4：合并环境变量
	cfg.mergeEnvVars()

	// 步骤5：配置日志级别
	logrus.SetLevel(logrus.DebugLevel)
	if cfg.LogLevel != "" {
		level, err := logrus.ParseLevel(cfg.LogLevel)
		if err == nil {
			logrus.SetLevel(level)
		}
	}

	logrus.Info("配置加载成功")
	return &cfg, nil
}

// setDefaults 设置配置默认值
func (c *Config) setDefaults() {
	// API推送默认配置
	if c.API.PushInterval == 0 {
		c.API.PushInterval = 5 * time.Second
	}
	if c.API.MaxRetries == 0 {
		c.API.MaxRetries = 0 // 无限重试
	}

	// 部署配置默认值
	if c.Deploy.WaitTimeout == 0 {
		c.Deploy.WaitTimeout = 5 * time.Minute
	}
	if c.Deploy.RollbackTimeout == 0 {
		c.Deploy.RollbackTimeout = 3 * time.Minute
	}
	if c.Deploy.PollInterval == 0 {
		c.Deploy.PollInterval = 5 * time.Second
	}

	// Redis默认TTL为24小时
	if c.Redis.TTL == 0 {
		c.Redis.TTL = 24 * time.Hour
	}
	// Redis默认重试次数
	if c.Redis.MaxRetries == 0 {
		c.Redis.MaxRetries = 3
	}
	// Redis默认空闲超时
	if c.Redis.IdleTimeout == 0 {
		c.Redis.IdleTimeout = 5 * time.Minute
	}
	// 任务默认配置
	if c.Task.MaxRetries == 0 {
		c.Task.MaxRetries = 3
	}
	if c.Task.RetryDelay == 0 {
		c.Task.RetryDelay = 30
	}
	if c.Task.QueueWorkers == 0 {
		c.Task.QueueWorkers = 5
	}
	if c.Task.PollInterval == 0 {
		c.Task.PollInterval = 30
	}
	if c.Task.MaxQueueSize == 0 {
		c.Task.MaxQueueSize = 100
	}
	// K8s默认认证方式
	if c.Kubernetes.AuthType == "" {
		c.Kubernetes.AuthType = "kubeconfig"
	}
	if c.Kubernetes.Namespace == "" {
		c.Kubernetes.Namespace = "default"
	}

	// 用户默认配置
	if c.User.Default == "" {
		c.User.Default = "deployer"
	}
	
	// 环境映射默认配置
	if len(c.EnvMapping.Mappings) == 0 {
		c.EnvMapping.Mappings = map[string]string{
			"eks":     "ns",
			"eks-pro": "bs",
		}
	}
}

// mergeEnvVars 合并环境变量覆盖配置
func (c *Config) mergeEnvVars() {
	if token := os.Getenv("TELEGRAM_TOKEN"); token != "" {
		for i := range c.Telegram.Bots {
			c.Telegram.Bots[i].Token = token
		}
	}
	if groupID := os.Getenv("TELEGRAM_GROUP_ID"); groupID != "" {
		for i := range c.Telegram.Bots {
			c.Telegram.Bots[i].GroupID = groupID
		}
	}
	if redisAddr := os.Getenv("REDIS_ADDR"); redisAddr != "" {
		c.Redis.Addr = redisAddr
	}
}