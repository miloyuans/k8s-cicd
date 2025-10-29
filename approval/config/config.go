package config

import (
	"os"
	"strconv"
	"time"

	"github.com/sirupsen/logrus"
	"gopkg.in/yaml.v3"
)

// TelegramBot 单个机器人配置
type TelegramBot struct {
	Name         string              `yaml:"name"`          // 机器人名称
	Token        string              `yaml:"token"`         // Bot Token
	GroupID      string              `yaml:"group_id"`      // 群组ID
	Services     map[string][]string `yaml:"services"`      // 服务匹配规则
	RegexMatch   bool                `yaml:"regex_match"`   // 是否使用正则匹配
	IsEnabled    bool                `yaml:"enabled"`       // 是否启用
	AllowedUsers []string            `yaml:"allowed_users"` // 允许操作的用户
}

// TelegramConfig Telegram 配置
type TelegramConfig struct {
	Bots           []TelegramBot `yaml:"bots"`            // 机器人列表
	AllowedUsers   []string      `yaml:"allowed_users"`   // 全局允许用户
	ConfirmTimeout time.Duration `yaml:"confirm_timeout"` // 弹窗超时时间
}

// APIConfig API 配置（用于查询任务）
type APIConfig struct {
	BaseURL       string        `yaml:"base_url"`       // API 基础地址
	QueryInterval time.Duration `yaml:"query_interval"` // 查询任务间隔
	MaxRetries    int           `yaml:"max_retries"`    // 最大重试次数
}

// QueryConfig 弹窗确认环境配置
type QueryConfig struct {
	ConfirmEnvs []string `yaml:"confirm_envs"` // 需要弹窗确认的环境
}

// MongoConfig MongoDB 配置（与 k8s-cd 共享）
type MongoConfig struct {
	URI        string           `yaml:"uri"`         // MongoDB 连接 URI
	TTL        time.Duration    `yaml:"ttl"`         // 数据过期时间
	MaxRetries int              `yaml:"max_retries"` // 连接重试次数
	EnvMapping EnvMappingConfig `yaml:"env_mapping"` // 环境映射
}

// PopupConfig 弹窗防重配置
type PopupConfig struct {
	MaxRetries     int `yaml:"max_retries"`         // 最大弹窗重试次数
	RetryDelay     int `yaml:"retry_delay_seconds"` // 重试延迟
	ConcurrentLimit int `yaml:"concurrent_limit"`   // 并发弹窗限制
}

// EnvMappingConfig 环境到命名空间的映射
type EnvMappingConfig struct {
	Mappings map[string]string `yaml:"mappings"` // env -> namespace
}

// Config 完整配置结构
type Config struct {
	Telegram   TelegramConfig   `yaml:"telegram"`
	API        APIConfig        `yaml:"api"`
	Query      QueryConfig      `yaml:"query"`
	Mongo      MongoConfig      `yaml:"mongo"`
	Popup      PopupConfig      `yaml:"popup"`
	EnvMapping EnvMappingConfig `yaml:"env_mapping"`
	LogLevel   string           `yaml:"log_level"` // 日志级别
}

// LoadConfig 加载配置文件
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

	// 步骤4：合并环境变量
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
	}).Info("k8s-approval 配置加载成功")
	return &cfg, nil
}

// setDefaults 设置默认值
func (c *Config) setDefaults() {
	// API 默认值
	if c.API.QueryInterval == 0 {
		c.API.QueryInterval = 15 * time.Second
	}
	if c.API.MaxRetries == 0 {
		c.API.MaxRetries = 3
	}

	// Telegram 默认值
	if c.Telegram.ConfirmTimeout == 0 {
		c.Telegram.ConfirmTimeout = 24 * time.Hour
	}

	// 查询默认值
	if len(c.Query.ConfirmEnvs) == 0 {
		c.Query.ConfirmEnvs = []string{"eks", "eks-pro"}
	}

	// MongoDB 默认值
	if c.Mongo.TTL == 0 {
		c.Mongo.TTL = 30 * 24 * time.Hour // 30 天
	}
	if c.Mongo.MaxRetries == 0 {
		c.Mongo.MaxRetries = 3
	}
	if c.Mongo.URI == "" {
		c.Mongo.URI = "mongodb://localhost:27017"
	}

	// 弹窗防重默认值
	if c.Popup.MaxRetries == 0 {
		c.Popup.MaxRetries = 3
	}
	if c.Popup.RetryDelay == 0 {
		c.Popup.RetryDelay = 300 // 5 分钟
	}
	if c.Popup.ConcurrentLimit == 0 {
		c.Popup.ConcurrentLimit = 10
	}

	// 环境映射默认值
	if len(c.EnvMapping.Mappings) == 0 {
		c.EnvMapping.Mappings = map[string]string{
			"eks":     "ns-eks",
			"eks-pro": "ns-pro",
			"prod":    "international",
		}
	}
}

// mergeEnvVars 合并环境变量
func (c *Config) mergeEnvVars() {
	// Telegram 环境变量
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
}