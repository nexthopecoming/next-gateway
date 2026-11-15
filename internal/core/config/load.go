package config

import (
	"fmt"
	"reflect"
	"time"

	"github.com/go-viper/mapstructure/v2"
	"github.com/spf13/pflag"
	"github.com/spf13/viper"

	"github.com/nexthopecoming/next-gateway/internal/core/role"
)

// Option 配置一次独立加载；私有载体确保调用方只能通过受控选项组合参数。
type Option func(*loadOptions)

// loadOptions 保存加载参数；未显式修改的 pflag 不会覆盖 YAML。
type loadOptions struct {
	path  string
	flags *pflag.FlagSet
	role  role.Role
}

// WithPath 指定 YAML 路径；空路径仍由 ResolvePath 归一到默认配置文件。
func WithPath(path string) Option {
	return func(options *loadOptions) {
		options.path = path
	}
}

// WithFlags 指定当前命令的 pflag 参数集；nil 表示不绑定 CLI 覆盖项。
func WithFlags(flags *pflag.FlagSet) Option {
	return func(options *loadOptions) {
		options.flags = flags
	}
}

// WithRole 指定目标进程角色，Load 仍按该角色执行完整语义校验。
func WithRole(r role.Role) Option {
	return func(options *loadOptions) {
		options.role = r
	}
}

// BindFlags 为 serve 和 config validate 提供完全一致的启动参数定义。
func BindFlags(flags *pflag.FlagSet) {
	defaults := Default()
	flags.String("listen", defaults.Server.Listen, "覆盖当前进程的 server.listen；未显式指定时由 YAML 和默认值决定")
	flags.String("admin-listen", defaults.Admin.Listen, "管理 HTTP 监听地址（阶段二启用）")
	flags.String("config-source", defaults.Source.Source, "配置来源，当前仅支持 file")
	flags.StringSlice("etcd-endpoints", defaults.Source.Endpoints, "etcd 地址列表（阶段二启用）")
	flags.String("etcd-key", defaults.Source.Key, "etcd 动态配置键（阶段二启用）")

	flags.String("snapshot-path", defaults.Source.SnapshotPath, "有效远程配置快照路径（阶段二启用）")
	flags.String("log-level", defaults.Log.Level, "日志级别：debug/info/warn/error")
}

// configDecodeHook 保留 Viper 默认解码能力，并在数字被转换成 Duration 前拒绝它。
// 严格检查必须放在字符串时长解析之前，否则合法的 "30s" 会被转换后误判为数字。
func configDecodeHook() mapstructure.DecodeHookFunc {
	durationType := reflect.TypeOf(time.Duration(0))
	strictDurations := mapstructure.DecodeHookFuncType(func(from, to reflect.Type, value any) (any, error) {
		if to == durationType && from != nil && from.Kind() != reflect.String {
			return nil, fmt.Errorf("时长必须使用带单位的字符串，例如 30s，实际类型为 %s", from)
		}
		return value, nil
	})
	return mapstructure.ComposeDecodeHookFunc(
		strictDurations,
		mapstructure.StringToTimeDurationHookFunc(),
		mapstructure.StringToSliceHookFunc(","),
	)
}

// ResolvePath 将空路径归一到默认配置文件，便于错误信息和命令输出保持一致。
func ResolvePath(path string) string {
	if path == "" {
		return DefaultPath
	}
	return path
}

// Load 按 CLI > 环境变量 > YAML > 默认值的优先级生成并校验配置。
// 每次调用都创建独立 Viper，不缓存文件、环境或选项结果；重复选项按后出现者覆盖。
func Load(opts ...Option) (Config, error) {
	var options loadOptions
	for _, apply := range opts {
		apply(&options)
	}

	v := viper.New()
	// 同一注册表驱动默认值和环境变量绑定，避免新增字段只登记在其中一处。
	defaults := defaultValues()
	for key, value := range defaults {
		v.SetDefault(key, value)
	}

	v.SetEnvPrefix("NEXT_GATEWAY")
	v.SetEnvKeyReplacer(newEnvKeyReplacer())
	// 显式绑定确保 UnmarshalExact 也能看到环境变量，而不只依赖 Get 的隐式查询。
	for key := range defaults {
		if err := v.BindEnv(key); err != nil {
			return Config{}, fmt.Errorf("绑定环境变量 %s: %w", key, err)
		}
	}
	if options.flags != nil {
		if err := bindPFlags(v, options.flags); err != nil {
			return Config{}, err
		}
	}

	path := ResolvePath(options.path)
	v.SetConfigFile(path)
	if err := v.ReadInConfig(); err != nil {
		return Config{}, fmt.Errorf("读取配置文件 %s: %w", path, err)
	}

	var cfg Config
	// Exact 解码让配置拼写错误在启动时暴露，而不是静默使用默认值。
	if err := v.UnmarshalExact(&cfg, viper.DecodeHook(configDecodeHook())); err != nil {
		return Config{}, fmt.Errorf("解析配置文件 %s: %w", path, err)
	}
	if err := cfg.Validate(options.role); err != nil {
		return Config{}, fmt.Errorf("配置校验失败: %w", err)
	}
	return cfg, nil
}

// defaultValues 是默认值与环境变量绑定共用的唯一配置键注册表。
func defaultValues() map[string]any {
	defaults := Default()
	return map[string]any{
		"schema_version":                    defaults.SchemaVersion,
		"server.listen":                     defaults.Server.Listen,
		"server.shutdown_timeout":           defaults.Server.ShutdownTimeout.String(),
		"server.read_header_timeout":        defaults.Server.ReadHeaderTimeout.String(),
		"server.read_timeout":               defaults.Server.ReadTimeout.String(),
		"server.idle_timeout":               defaults.Server.IdleTimeout.String(),
		"admin.listen":                      defaults.Admin.Listen,
		"admin.instance_heartbeat_interval": defaults.Admin.InstanceHeartbeatInterval.String(),
		"admin.instance_lease_ttl":          defaults.Admin.InstanceLeaseTTL.String(),
		"config.source":                     defaults.Source.Source,
		"config.endpoints":                  defaults.Source.Endpoints,
		"config.key":                        defaults.Source.Key,
		"config.snapshot_path":              defaults.Source.SnapshotPath,
		"config.connect_timeout":            defaults.Source.ConnectTimeout.String(),
		"log.level":                         defaults.Log.Level,
		"log.encoding":                      defaults.Log.Encoding,
		"log.output":                        defaults.Log.Output,
		"request.max_body_bytes":            defaults.Request.MaxBodyBytes,
		"request.upstream_timeout":          defaults.Request.UpstreamTimeout.String(),
		"request.first_chunk_timeout":       defaults.Request.FirstChunkTimeout.String(),
		"request.stream_idle_timeout":       defaults.Request.StreamIdleTimeout.String(),
		"request.stream_max_duration":       defaults.Request.StreamMaxDuration.String(),
		"request.stream_write_timeout":      defaults.Request.StreamWriteTimeout.String(),
		"request.heartbeat_interval":        defaults.Request.HeartbeatInterval.String(),
		"canary.canary_basis_points":        defaults.Canary.CanaryBasisPoints,
		"canary.force_stable_tenants":       defaults.Canary.ForceStableTenants,
		"canary.force_canary_tenants":       defaults.Canary.ForceCanaryTenants,
		"canary.allow_override_header":      defaults.Canary.AllowOverrideHeader,
		"ai.default_upstream":               defaults.AI.DefaultUpstream,
		"ai.mock.response_text":             defaults.AI.Mock.ResponseText,
		"ai.mock.chunk_size":                defaults.AI.Mock.ChunkSize,
		"ai.mock.chunk_delay":               defaults.AI.Mock.ChunkDelay.String(),
	}
}

func bindPFlags(v *viper.Viper, flags *pflag.FlagSet) error {
	bindings := map[string]string{
		"server.listen":        "listen",
		"admin.listen":         "admin-listen",
		"config.source":        "config-source",
		"config.endpoints":     "etcd-endpoints",
		"config.key":           "etcd-key",
		"config.snapshot_path": "snapshot-path",
		"log.level":            "log-level",
	}
	for key, name := range bindings {
		flag := flags.Lookup(name)
		if flag == nil {
			continue
		}
		if err := v.BindPFlag(key, flag); err != nil {
			return fmt.Errorf("绑定命令行参数 --%s: %w", name, err)
		}
	}
	return nil
}
