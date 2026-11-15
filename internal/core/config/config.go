// Package config 定义网关配置模型、默认值、加载和启动校验。
package config

import (
	"errors"
	"fmt"
	"net"
	"slices"
	"strings"
	"time"

	"github.com/nexthopecoming/next-gateway/internal/core/canary"
	"github.com/nexthopecoming/next-gateway/internal/core/role"
)

const (
	// CurrentSchemaVersion 是当前唯一可读取的配置结构版本。
	CurrentSchemaVersion = 1
	// DefaultPath 是库级空路径兜底；各二进制的角色默认 profile 由 command 包显式指定。
	DefaultPath = "configs/biz-gateway.yaml"
)

// Config 包含完整配置模型；当前启用本地 file、共享服务和 Mock 数据面，其余字段保留后续能力。
type Config struct {
	// SchemaVersion 标识配置结构版本，当前只接受 1，用于后续兼容与迁移。
	SchemaVersion int `mapstructure:"schema_version"`
	// Server 定义当前进程的业务 HTTP 监听和连接生命周期。
	Server ServerConfig `mapstructure:"server"`
	// Admin 定义管理面监听与实例注册参数；管理面当前尚未启动。
	Admin AdminConfig `mapstructure:"admin"`
	// Source 定义配置来源、etcd 访问参数和最后有效快照位置。
	Source SourceConfig `mapstructure:"config"`
	// Log 定义 zap 日志级别、编码和输出目标。
	Log LogConfig `mapstructure:"log"`
	// Request 定义请求体上限、上游调用和流式请求的时间约束。
	Request RequestConfig `mapstructure:"request"`
	// Canary 定义 stable/canary 的确定性灰度规则。
	Canary CanaryConfig `mapstructure:"canary"`
	// AI 定义 AI 数据面的默认上游和 Mock 行为。
	AI AIConfig `mapstructure:"ai"`
}

// clone 返回与当前配置不共享引用字段的副本，保护启动 Manager 的缓存。
// 新增 slice、map 或指针字段时，必须在这里同步补充复制规则。
func (c Config) clone() Config {
	c.Source.Endpoints = slices.Clone(c.Source.Endpoints)
	c.Canary.ForceStableTenants = slices.Clone(c.Canary.ForceStableTenants)
	c.Canary.ForceCanaryTenants = slices.Clone(c.Canary.ForceCanaryTenants)
	return c
}

// ServerConfig 定义当前进程数据面 listener 及连接生命周期参数；监听地址只支持重启生效。
type ServerConfig struct {
	// Listen 是业务 HTTP 监听地址，例如 :8080；修改后必须重启进程。
	Listen string `mapstructure:"listen"`
	// ShutdownTimeout 是优雅退出等待进行中请求的最长时间，超时后强制关闭剩余连接。
	ShutdownTimeout time.Duration `mapstructure:"shutdown_timeout"`
	// ReadHeaderTimeout 是读取 HTTP 请求头允许的最长时间，用于限制慢速连接占用。
	ReadHeaderTimeout time.Duration `mapstructure:"read_header_timeout"`
	// ReadTimeout 是读取完整 HTTP 请求允许的最长时间，包含请求头读取阶段。
	ReadTimeout time.Duration `mapstructure:"read_timeout"`
	// IdleTimeout 是 Keep-Alive 连接等待下一个请求的最大空闲时间。
	IdleTimeout time.Duration `mapstructure:"idle_timeout"`
}

// AdminConfig 定义 biz-gateway 后续管理面监听和实例注册时间参数。
type AdminConfig struct {
	// Listen 是管理 HTTP 监听地址，当前仅预留；启用和修改均需重启。
	Listen string `mapstructure:"listen"`
	// InstanceHeartbeatInterval 是实例向 etcd 续租并上报状态的间隔，管理面实现后使用。
	InstanceHeartbeatInterval time.Duration `mapstructure:"instance_heartbeat_interval"`
	// InstanceLeaseTTL 是实例注册租约的有效期，必须大于实例心跳间隔。
	InstanceLeaseTTL time.Duration `mapstructure:"instance_lease_ttl"`
}

// SourceConfig 定义配置中心、动态键和最后有效快照；当前仅启用 file 来源。
type SourceConfig struct {
	// Source 指定配置来源，当前仅支持 file；etcd 模式将在阶段二启用。
	Source string `mapstructure:"source"`
	// Endpoints 是 etcd 服务地址列表，仅在 etcd 模式使用，修改后需要重启。
	Endpoints []string `mapstructure:"endpoints"`
	// Key 是 etcd 中保存完整动态配置文档的键。
	Key string `mapstructure:"key"`
	// SnapshotPath 是最后有效远程配置快照的本地路径，快照不得包含 Secret。
	SnapshotPath string `mapstructure:"snapshot_path"`
	// ConnectTimeout 是 etcd 建连及初始读取操作允许的最长时间。
	ConnectTimeout time.Duration `mapstructure:"connect_timeout"`
}

// LogConfig 定义 zap 的初始级别、编码和输出位置，不包含任何凭据或正文。
type LogConfig struct {
	// Level 是初始日志级别，支持 debug/info/warn/error；后续仅该字段允许热更新。
	Level string `mapstructure:"level"`
	// Encoding 是日志编码，支持 json 或 console，修改后需要重启。
	Encoding string `mapstructure:"encoding"`
	// Output 是日志输出目标，支持 stdout、stderr 或文件路径，修改后需要重启。
	Output string `mapstructure:"output"`
}

// RequestConfig 定义请求体、上游与后续 SSE 的超时约束，旧请求固定使用开始时的值。
type RequestConfig struct {
	// MaxBodyBytes 是 HTTP 请求体允许的最大字节数，超出限制时拒绝请求。
	MaxBodyBytes int64 `mapstructure:"max_body_bytes"`
	// UpstreamTimeout 是单次上游调用的总超时，覆盖连接和响应处理过程。
	UpstreamTimeout time.Duration `mapstructure:"upstream_timeout"`
	// FirstChunkTimeout 是流式请求等待首个内容事件允许的最长时间。
	FirstChunkTimeout time.Duration `mapstructure:"first_chunk_timeout"`
	// StreamIdleTimeout 是 SSE 相邻事件之间允许的最大空闲时间。
	StreamIdleTimeout time.Duration `mapstructure:"stream_idle_timeout"`
	// StreamMaxDuration 是单个流式请求从开始到结束允许的最长总时长。
	StreamMaxDuration time.Duration `mapstructure:"stream_max_duration"`
	// StreamWriteTimeout 是每次 SSE 写入和 Flush 允许阻塞的最长时间，用于限制慢客户端。
	StreamWriteTimeout time.Duration `mapstructure:"stream_write_timeout"`
	// HeartbeatInterval 是 SSE 注释心跳的发送间隔，必须小于流空闲超时。
	HeartbeatInterval time.Duration `mapstructure:"heartbeat_interval"`
}

// CanaryConfig 定义确定性分桶和显式名单，万分比避免跨实例浮点解释差异。
type CanaryConfig struct {
	// CanaryBasisPoints 是进入 canary 的万分比阈值：0 全走 stable，10000 全走 canary。
	CanaryBasisPoints int `mapstructure:"canary_basis_points"`
	// ForceStableTenants 是强制使用 stable 执行器的租户 ID 精确匹配列表。
	ForceStableTenants []string `mapstructure:"force_stable_tenants"`
	// ForceCanaryTenants 是强制使用 canary 执行器的租户 ID 精确匹配列表。
	ForceCanaryTenants []string `mapstructure:"force_canary_tenants"`
	// AllowOverrideHeader 控制是否允许调试 Header 覆盖灰度结果，默认关闭以避免任意选链。
	AllowOverrideHeader bool `mapstructure:"allow_override_header"`
}

// AIConfig 定义 AI 数据面的上游选择和 Mock 行为，当前不包含真实 Provider 凭据。
type AIConfig struct {
	// DefaultUpstream 是默认上游名称，当前仅支持 mock，后续可扩展真实 Provider。
	DefaultUpstream string `mapstructure:"default_upstream"`
	// Mock 定义 Mock 上游的响应文本和流式分块参数。
	Mock MockConfig `mapstructure:"mock"`
}

// MockConfig 定义确定性文本及后续流式分块方式，chunk 以 Unicode code point 为单位。
type MockConfig struct {
	// ResponseText 是 Mock 返回的完整文本，非流式 JSON 和 SSE 响应共用。
	ResponseText string `mapstructure:"response_text"`
	// ChunkSize 是每个 SSE 内容块包含的 Unicode code point 数量，必须大于 0。
	ChunkSize int `mapstructure:"chunk_size"`
	// ChunkDelay 是相邻 Mock 内容块之间的发送延迟，0 表示不主动等待。
	ChunkDelay time.Duration `mapstructure:"chunk_delay"`
}

// Default 返回不依赖外部服务即可进行本地开发的安全默认配置。
func Default() Config {
	return Config{
		SchemaVersion: CurrentSchemaVersion,
		Server: ServerConfig{
			Listen:            ":8080",
			ShutdownTimeout:   30 * time.Second,
			ReadHeaderTimeout: 10 * time.Second,
			ReadTimeout:       30 * time.Second,
			IdleTimeout:       60 * time.Second,
		},
		Admin: AdminConfig{
			Listen:                    ":9091",
			InstanceHeartbeatInterval: 5 * time.Second,
			InstanceLeaseTTL:          15 * time.Second,
		},
		Source: SourceConfig{
			Source:         "file",
			Endpoints:      []string{"127.0.0.1:2379"},
			Key:            "/next-gateway/v1/runtime",
			SnapshotPath:   "data/runtime-snapshot.yaml",
			ConnectTimeout: 5 * time.Second,
		},
		Log: LogConfig{Level: "info", Encoding: "json", Output: "stdout"},
		Request: RequestConfig{
			MaxBodyBytes:       1 << 20,
			UpstreamTimeout:    60 * time.Second,
			FirstChunkTimeout:  30 * time.Second,
			StreamIdleTimeout:  60 * time.Second,
			StreamMaxDuration:  10 * time.Minute,
			StreamWriteTimeout: 30 * time.Second,
			HeartbeatInterval:  15 * time.Second,
		},
		Canary: CanaryConfig{
			CanaryBasisPoints:   0,
			ForceStableTenants:  []string{},
			ForceCanaryTenants:  []string{},
			AllowOverrideHeader: false,
		},
		AI: AIConfig{
			DefaultUpstream: "mock",
			Mock: MockConfig{
				ResponseText: "Hello from Next Gateway Mock",
				ChunkSize:    8,
				ChunkDelay:   20 * time.Millisecond,
			},
		},
	}
}

// Validate 在进程创建 socket、文件或日志资源前按角色检查配置，避免带病启动。
// 这里只组合共享配置和角色专属配置段；biz 不要求 ai.* 合法，ai 不要求管理面参数合法。
func (c Config) Validate(r role.Role) error {
	var errs []error
	if r != role.Biz && r != role.AI {
		errs = append(errs, fmt.Errorf("未知进程角色 %q", r))
	}

	errs = append(errs, c.validateCommon()...)
	switch r {
	case role.Biz:
		errs = append(errs, c.Admin.validate()...)
	case role.AI:
		errs = append(errs, c.Request.validate()...)
		errs = append(errs, c.Canary.validate()...)
		errs = append(errs, c.AI.validate()...)
	}
	return errors.Join(errs...)
}

// validateCommon 校验两个进程共同依赖的 schema、配置源、HTTP 生命周期和日志配置。
func (c Config) validateCommon() []error {
	var errs []error
	if c.SchemaVersion != CurrentSchemaVersion {
		errs = append(errs, fmt.Errorf("schema_version 必须为 %d", CurrentSchemaVersion))
	}
	if c.Source.Source != "file" {
		errs = append(errs, fmt.Errorf("config.source 当前仅支持 file，etcd 将在阶段二实现"))
	}
	if err := validateListenAddress(c.Server.Listen); err != nil {
		errs = append(errs, fmt.Errorf("server.listen 无效: %w", err))
	}
	validatePositiveDuration(&errs, "server.shutdown_timeout", c.Server.ShutdownTimeout)
	validatePositiveDuration(&errs, "server.read_header_timeout", c.Server.ReadHeaderTimeout)
	validatePositiveDuration(&errs, "server.read_timeout", c.Server.ReadTimeout)
	validatePositiveDuration(&errs, "server.idle_timeout", c.Server.IdleTimeout)
	validatePositiveDuration(&errs, "config.connect_timeout", c.Source.ConnectTimeout)

	if strings.TrimSpace(c.Source.Key) == "" {
		errs = append(errs, errors.New("config.key 不能为空"))
	}
	if strings.TrimSpace(c.Source.SnapshotPath) == "" {
		errs = append(errs, errors.New("config.snapshot_path 不能为空"))
	}
	for i, endpoint := range c.Source.Endpoints {
		if strings.TrimSpace(endpoint) == "" {
			errs = append(errs, fmt.Errorf("config.endpoints[%d] 不能为空", i))
		}
	}

	switch c.Log.Level {
	case "debug", "info", "warn", "error":
	default:
		errs = append(errs, errors.New("log.level 必须为 debug/info/warn/error"))
	}
	switch c.Log.Encoding {
	case "json", "console":
	default:
		errs = append(errs, errors.New("log.encoding 必须为 json 或 console"))
	}
	if strings.TrimSpace(c.Log.Output) == "" {
		errs = append(errs, errors.New("log.output 不能为空"))
	}
	return errs
}

// validate 校验 biz 管理面的监听地址、续租时间及租约跨字段约束。
func (c AdminConfig) validate() []error {
	var errs []error
	if err := validateListenAddress(c.Listen); err != nil {
		errs = append(errs, fmt.Errorf("admin.listen 无效: %w", err))
	}
	validatePositiveDuration(&errs, "admin.instance_heartbeat_interval", c.InstanceHeartbeatInterval)
	validatePositiveDuration(&errs, "admin.instance_lease_ttl", c.InstanceLeaseTTL)
	if c.InstanceLeaseTTL <= c.InstanceHeartbeatInterval {
		errs = append(errs, errors.New("admin.instance_lease_ttl 必须大于 instance_heartbeat_interval"))
	}
	return errs
}

// validate 校验 AI 请求限制与流式超时，并集中维护心跳和空闲超时的关系。
func (c RequestConfig) validate() []error {
	var errs []error
	if c.MaxBodyBytes <= 0 {
		errs = append(errs, errors.New("request.max_body_bytes 必须大于 0"))
	}
	validatePositiveDuration(&errs, "request.upstream_timeout", c.UpstreamTimeout)
	validatePositiveDuration(&errs, "request.first_chunk_timeout", c.FirstChunkTimeout)
	validatePositiveDuration(&errs, "request.stream_idle_timeout", c.StreamIdleTimeout)
	validatePositiveDuration(&errs, "request.stream_max_duration", c.StreamMaxDuration)
	validatePositiveDuration(&errs, "request.stream_write_timeout", c.StreamWriteTimeout)
	validatePositiveDuration(&errs, "request.heartbeat_interval", c.HeartbeatInterval)
	if c.HeartbeatInterval >= c.StreamIdleTimeout {
		errs = append(errs, errors.New("request.heartbeat_interval 必须小于 stream_idle_timeout"))
	}
	return errs
}

// validate 校验确定性灰度权重及两个强制租户名单。
func (c CanaryConfig) validate() []error {
	var errs []error
	if c.CanaryBasisPoints < 0 || c.CanaryBasisPoints > 10000 {
		errs = append(errs, errors.New("canary.canary_basis_points 必须在 0..10000 之间"))
	}
	for _, rule := range []struct {
		field   string
		tenants []string
	}{
		{field: "canary.force_stable_tenants", tenants: c.ForceStableTenants},
		{field: "canary.force_canary_tenants", tenants: c.ForceCanaryTenants},
	} {
		for index, tenant := range rule.tenants {
			if _, err := canary.NormalizeTenant(tenant); err != nil {
				errs = append(errs, fmt.Errorf("%s[%d] 包含非法租户", rule.field, index))
			}
		}
	}
	return errs
}

// validate 校验 AI 上游选择及当前 Mock 数据面的分块参数。
func (c AIConfig) validate() []error {
	var errs []error
	if c.DefaultUpstream != "mock" {
		errs = append(errs, errors.New("ai.default_upstream 当前仅支持 mock"))
	}
	if c.Mock.ChunkSize <= 0 {
		errs = append(errs, errors.New("ai.mock.chunk_size 必须大于 0"))
	}
	if c.Mock.ChunkDelay < 0 {
		errs = append(errs, errors.New("ai.mock.chunk_delay 不能为负数"))
	}
	return errs
}

func validateListenAddress(address string) error {
	if strings.TrimSpace(address) == "" {
		return errors.New("地址不能为空")
	}
	_, err := net.ResolveTCPAddr("tcp", address)
	return err
}

func validatePositiveDuration(errs *[]error, field string, value time.Duration) {
	if value <= 0 {
		*errs = append(*errs, fmt.Errorf("%s 必须大于 0", field))
	}
}
