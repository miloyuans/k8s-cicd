//config.go
package config

import (
	"os"
	"time"
	"strconv"

	"github.com/sirupsen/logrus"
	"gopkg.in/yaml.v3"
)

// TelegramBot 配置单个Telegram机器人
type TelegramBot struct {
	Name        string              `yaml:"name"`          // 机器人名称
	Token       string              `yaml:"token"`         // Bot Token
	GroupID     string              `yaml:"group_id"`      // 群组ID
	Services    map[string][]string `yaml:"services"`      // 服务匹配规则
	RegexMatch  bool                `yaml:"regex_match"`   // 是否使用正则匹配
	IsEnabled   bool                `yaml:"enabled"`       // 是否启用
}

// TelegramConfig Telegram多机器人配置
type TelegramConfig struct {
	Bots           []TelegramBot    `yaml:"bots"`            // 机器人列表
	AllowedUsers   []int64          `yaml:"allowed_users"`   // 有效用户ID
	ConfirmTimeout time.Duration    `yaml:"confirm_timeout"` // 弹窗超时
}

// APIConfig API服务配置
type APIConfig struct {
	BaseURL        string        `yaml:"base_url"`       // API基础地址
	PushInterval   time.Duration `yaml:"push_interval"`  // 推送间隔
	QueryInterval  time.Duration `yaml:"query_interval"` // 查询间隔
	MaxRetries     int           `yaml:"max_retries"`    // 最大重试次数
}

// QueryConfig 查询弹窗过滤配置
type QueryConfig struct {
	ConfirmEnvs []string `yaml:"confirm_envs"` // 需要弹窗的环境
}

// K8sAuthConfig Kubernetes认证配置
type K8sAuthConfig struct {
	AuthType    string `yaml:"auth_type"`     // 认证类型
	Kubeconfig  string `yaml:"kubeconfig"`    // kubeconfig路径
	Namespace   string `yaml:"namespace"`     // 默认命名空间
	ServiceName string `yaml:"service_name"`  // ServiceAccount名称
}

// MongoConfig MongoDB配置
type MongoConfig struct {
	URI         string              `yaml:"uri"`          // MongoDB连接URI
	TTL         time.Duration       `yaml:"ttl"`          // 数据过期时间
	MaxRetries  int                 `yaml:"max_retries"`  // 连接重试次数
	IdleTimeout time.Duration       `yaml:"idle_timeout"` // 空闲连接超时
	EnvMapping  EnvMappingConfig    `yaml:"env_mapping"`  // 环境映射
}

// TaskConfig 任务队列配置
type TaskConfig struct {
	MaxRetries     int `yaml:"max_retries"`         // 最大重试次数
	RetryDelay     int `yaml:"retry_delay_seconds"` // 重试延迟
	QueueWorkers   int `yaml:"queue_workers"`       // 工作线程数
	PollInterval   int `yaml:"poll_interval_seconds"` // 轮询间隔
	MaxQueueSize   int `yaml:"max_queue_size"`      // 最大队列大小
}

// DeployConfig 部署配置
type DeployConfig struct {
	WaitTimeout     time.Duration `yaml:"wait_timeout"`     // 部署等待超时
	RollbackTimeout time.Duration `yaml:"rollback_timeout"` // 回滚等待超时
	PollInterval    time.Duration `yaml:"poll_interval"`    // 健康检查间隔
}

// UserConfig 用户配置
type UserConfig struct {
	Default string `yaml:"default"` // 默认用户
}

// EnvMappingConfig 环境到命名空间的映射
type EnvMappingConfig struct {
	Mappings map[string]string `yaml:"mappings"` // env -> namespace
}

// Config 完整配置结构
type Config struct {
	Telegram    TelegramConfig      `yaml:"telegram"`    // Telegram配置
	API         APIConfig           `yaml:"api"`         // API配置
	Query       QueryConfig         `yaml:"query"`       // 查询配置
	Kubernetes  K8sAuthConfig       `yaml:"kubernetes"`  // Kubernetes配置
	Mongo       MongoConfig         `yaml:"mongo"`       // MongoDB配置
	Task        TaskConfig          `yaml:"task"`        // 任务配置
	Deploy      DeployConfig        `yaml:"deploy"`      // 部署配置
	User        UserConfig          `yaml:"user"`        // 用户配置
	EnvMapping  EnvMappingConfig    `yaml:"env_mapping"` // 环境映射
	LogLevel    string              `yaml:"log_level"`   // 日志级别
}

// LoadConfig 从YAML文件加载配置
func LoadConfig(filePath string) (*Config, error) {
	startTime := time.Now()
	// 步骤1：读取配置文件
	data, err := os.ReadFile(filePath)
	if err != nil {
		logrus.WithFields(logrus.Fields{
			"time":   time.Now().Format("2006-01-02 15:04:05"),
			"method": "LoadConfig",
			"took":   time.Since(startTime),
		}).Errorf("读取配置文件失败: %v", err)
		return nil, err
	}

	// 步骤2：解析YAML
	var cfg Config
	err = yaml.Unmarshal(data, &cfg)
	if err != nil {
		logrus.WithFields(logrus.Fields{
			"time":   time.Now().Format("2006-01-02 15:04:05"),
			"method": "LoadConfig",
			"took":   time.Since(startTime),
		}).Errorf("解析YAML失败: %v", err)
		return nil, err
	}

	// 步骤3：设置默认值
	cfg.setDefaults()

	// 步骤4：合并环境变量
	cfg.mergeEnvVars()

	// 步骤5：设置日志级别
	logrus.SetLevel(logrus.DebugLevel)
	if cfg.LogLevel != "" {
		level, err := logrus.ParseLevel(cfg.LogLevel)
		if err == nil {
			logrus.SetLevel(level)
		}
	}

	logrus.WithFields(logrus.Fields{
		"time":   time.Now().Format("2006-01-02 15:04:05"),
		"method": "LoadConfig",
		"took":   time.Since(startTime),
	}).Info("配置加载成功")
	return &cfg, nil
}

// setDefaults 设置配置默认值
func (c *Config) setDefaults() {
	// 步骤1：设置API默认值
	if c.API.PushInterval == 0 {
		c.API.PushInterval = 30 * time.Second
	}
	if c.API.QueryInterval == 0 {
		c.API.QueryInterval = 15 * time.Second
	}
	if c.API.MaxRetries == 0 {
		c.API.MaxRetries = 0
	}

	// 步骤2：设置Telegram默认值
	if c.Telegram.ConfirmTimeout == 0 {
		c.Telegram.ConfirmTimeout = 24 * time.Hour
	}

	// 步骤3：设置查询默认值
	if len(c.Query.ConfirmEnvs) == 0 {
		c.Query.ConfirmEnvs = []string{"eks", "eks-pro"}
	}

	// 步骤4：设置部署默认值
	if c.Deploy.WaitTimeout == 0 {
		c.Deploy.WaitTimeout = 5 * time.Minute
	}
	if c.Deploy.RollbackTimeout == 0 {
		c.Deploy.RollbackTimeout = 3 * time.Minute
	}
	if c.Deploy.PollInterval == 0 {
		c.Deploy.PollInterval = 5 * time.Second
	}

	// 步骤5：设置MongoDB默认值
	if c.Mongo.TTL == 0 {
		c.Mongo.TTL = 30 * 24 * time.Hour // 默认30天
	}
	if c.Mongo.MaxRetries == 0 {
		c.Mongo.MaxRetries = 3
	}
	if c.Mongo.IdleTimeout == 0 {
		c.Mongo.IdleTimeout = 5 * time.Minute
	}
	if c.Mongo.URI == "" {
		c.Mongo.URI = "mongodb://localhost:27017"
	}

	// 步骤6：设置任务默认值
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

	// 步骤7：设置Kubernetes默认值
	if c.Kubernetes.AuthType == "" {
		c.Kubernetes.AuthType = "kubeconfig"
	}
	if c.Kubernetes.Namespace == "" {
		c.Kubernetes.Namespace = "default"
	}

	// 步骤8：设置用户默认值
	if c.User.Default == "" {
		c.User.Default = "deployer"
	}

	// 步骤9：设置环境映射默认值
	if len(c.EnvMapping.Mappings) == 0 {
		c.EnvMapping.Mappings = map[string]string{
			"eks":     "ns",
			"eks-pro": "bs",
		}
	}
}

// mergeEnvVars 合并环境变量
func (c *Config) mergeEnvVars() {
	// 步骤1：合并Telegram环境变量
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

	// 步骤2：合并MongoDB环境变量
	if mongoURI := os.Getenv("MONGO_URI"); mongoURI != "" {
		c.Mongo.URI = mongoURI
	}
	if mongoTTL := os.Getenv("MONGO_TTL"); mongoTTL != "" {
		if ttl, err := strconv.Atoi(mongoTTL); err == nil {
			c.Mongo.TTL = time.Duration(ttl) * time.Hour
		}
	}

	// 步骤3：合并API环境变量
	if apiURL := os.Getenv("API_BASE_URL"); apiURL != "" {
		c.API.BaseURL = apiURL
	}
}