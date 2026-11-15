package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/pflag"

	"github.com/nexthopecoming/next-gateway/internal/core/role"
)

func TestLoadUsesYAMLAndDefaults(t *testing.T) {
	path := writeTestConfig(t, `
schema_version: 1
server:
  listen: "127.0.0.1:0"
log:
  level: debug
`)
	cfg, err := Load(WithPath(path), WithRole(role.AI))
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.Server.Listen != "127.0.0.1:0" {
		t.Fatalf("server.listen = %q", cfg.Server.Listen)
	}
	if cfg.Log.Level != "debug" {
		t.Fatalf("log.level = %q", cfg.Log.Level)
	}
	if cfg.Request.MaxBodyBytes != Default().Request.MaxBodyBytes {
		t.Fatalf("默认值未生效: max_body_bytes = %d", cfg.Request.MaxBodyBytes)
	}
	if cfg.AI.Mock.ChunkDelay != Default().AI.Mock.ChunkDelay {
		t.Fatalf("duration 默认值未生效: %s", cfg.AI.Mock.ChunkDelay)
	}
}

// TestLoadDoesNotCacheBetweenCalls 确保无状态入口不受前一次路径和结果影响。
func TestLoadDoesNotCacheBetweenCalls(t *testing.T) {
	unsetEnvironmentForTest(t, "NEXT_GATEWAY_SERVER_LISTEN")
	firstPath := writeTestConfig(t, "server:\n  listen: \"127.0.0.1:10001\"\n")
	secondPath := writeTestConfig(t, "server:\n  listen: \"127.0.0.1:10002\"\n")

	first, err := Load(WithPath(firstPath), WithRole(role.AI))
	if err != nil {
		t.Fatal(err)
	}
	second, err := Load(WithPath(secondPath), WithRole(role.AI))
	if err != nil {
		t.Fatal(err)
	}
	if first.Server.Listen != "127.0.0.1:10001" || second.Server.Listen != "127.0.0.1:10002" {
		t.Fatalf("独立 Load 共享了缓存: first=%q second=%q", first.Server.Listen, second.Server.Listen)
	}
}

func TestLoadPrecedence(t *testing.T) {
	path := writeTestConfig(t, `
server:
  listen: "127.0.0.1:10001"
  idle_timeout: 45s
log:
  level: warn
ai:
  mock:
    chunk_size: 7
`)
	t.Setenv("NEXT_GATEWAY_SERVER_LISTEN", "127.0.0.1:10002")
	t.Setenv("NEXT_GATEWAY_LOG_LEVEL", "error")
	t.Setenv("NEXT_GATEWAY_AI_MOCK_CHUNK_SIZE", "42")

	flags := pflag.NewFlagSet("test", pflag.ContinueOnError)
	BindFlags(flags)
	if err := flags.Set("listen", "127.0.0.1:10003"); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(WithPath(path), WithFlags(flags), WithRole(role.AI))
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.Server.Listen != "127.0.0.1:10003" {
		t.Errorf("CLI 未获得最高优先级: %q", cfg.Server.Listen)
	}
	if cfg.Log.Level != "error" {
		t.Errorf("环境变量未覆盖 YAML: %q", cfg.Log.Level)
	}
	if cfg.Server.IdleTimeout.String() != "45s" {
		t.Errorf("YAML 时长配置未生效: %s", cfg.Server.IdleTimeout)
	}
	if cfg.AI.Mock.ChunkSize != 42 {
		t.Errorf("嵌套环境变量映射失败: %d", cfg.AI.Mock.ChunkSize)
	}
}

// TestLoadUnchangedFlagsDoNotOverrideYAML 直接验证已绑定但未显式修改的 pflag 不参与覆盖。
func TestLoadUnchangedFlagsDoNotOverrideYAML(t *testing.T) {
	path := writeTestConfig(t, `
server:
  listen: "127.0.0.1:10001"
log:
  level: warn
`)
	flags := pflag.NewFlagSet("test", pflag.ContinueOnError)
	BindFlags(flags)
	cfg, err := Load(WithPath(path), WithFlags(flags), WithRole(role.AI))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Server.Listen != "127.0.0.1:10001" {
		t.Errorf("未修改的 listen flag 覆盖了 YAML: %q", cfg.Server.Listen)
	}
	if cfg.Log.Level != "warn" {
		t.Errorf("未修改的 log-level flag 覆盖了 YAML: %q", cfg.Log.Level)
	}
}

// TestLoadRejectsNumericDuration 防止裸数字按 time.Duration 的纳秒语义静默生效。
func TestLoadRejectsNumericDuration(t *testing.T) {
	tests := []struct {
		name    string
		content string
		key     string
	}{
		{name: "服务器时长", content: "server:\n  read_timeout: 30\n", key: "server.read_timeout"},
		{name: "Mock 时长", content: "ai:\n  mock:\n    chunk_delay: 20\n", key: "ai.mock.chunk_delay"},
		{name: "SSE 写期限", content: "request:\n  stream_write_timeout: 30\n", key: "request.stream_write_timeout"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := Load(WithPath(writeTestConfig(t, test.content)), WithRole(role.AI))
			if err == nil {
				t.Fatal("裸数字时长未返回错误")
			}
			for _, expected := range []string{test.key, "带单位的字符串"} {
				if !strings.Contains(err.Error(), expected) {
					t.Errorf("错误 %q 缺少 %q", err, expected)
				}
			}
		})
	}
}

func TestLoadRejectsUnknownField(t *testing.T) {
	path := writeTestConfig(t, "unknown_field: true\n")
	_, err := Load(WithPath(path), WithRole(role.AI))
	if err == nil || !strings.Contains(err.Error(), "unknown_field") {
		t.Fatalf("期望未知字段错误，实际为 %v", err)
	}
}

func TestLoadRejectsEtcdBeforeStageTwo(t *testing.T) {
	path := writeTestConfig(t, "config:\n  source: etcd\n")
	_, err := Load(WithPath(path), WithRole(role.AI))
	if err == nil || !strings.Contains(err.Error(), "阶段二") {
		t.Fatalf("期望明确的 etcd 未实现错误，实际为 %v", err)
	}
}

func TestLoadRejectsInvalidStreamTiming(t *testing.T) {
	path := writeTestConfig(t, `
request:
  stream_idle_timeout: 15s
  heartbeat_interval: 15s
`)
	_, err := Load(WithPath(path), WithRole(role.AI))
	if err == nil || !strings.Contains(err.Error(), "heartbeat_interval") {
		t.Fatalf("期望心跳配置错误，实际为 %v", err)
	}
}

func TestLoadMissingFile(t *testing.T) {
	_, err := Load(WithPath(filepath.Join(t.TempDir(), "missing.yaml")), WithRole(role.AI))
	if err == nil || !strings.Contains(err.Error(), "读取配置文件") {
		t.Fatalf("期望文件读取错误，实际为 %v", err)
	}
}

func TestDefaultConfigIsValid(t *testing.T) {
	for _, r := range []role.Role{role.Biz, role.AI} {
		if err := Default().Validate(r); err != nil {
			t.Fatalf("默认配置对角色 %s 无效: %v", r, err)
		}
	}
}

// TestValidateRoleSections 确保分段后仍聚合所有生效配置段，并忽略另一角色的专属字段。
func TestValidateRoleSections(t *testing.T) {
	newInvalidConfig := func() Config {
		cfg := Default()
		cfg.Log.Level = "invalid"
		cfg.Admin.Listen = ""
		cfg.Request.MaxBodyBytes = 0
		cfg.Canary.CanaryBasisPoints = -1
		cfg.AI.DefaultUpstream = "invalid"
		return cfg
	}
	tests := []struct {
		name      string
		role      role.Role
		want      []string
		doNotWant []string
	}{
		{
			name:      "biz 校验共享与管理面",
			role:      role.Biz,
			want:      []string{"log.level", "admin.listen"},
			doNotWant: []string{"request.max_body_bytes", "canary.canary_basis_points", "ai.default_upstream"},
		},
		{
			name:      "ai 聚合全部 AI 配置段",
			role:      role.AI,
			want:      []string{"log.level", "request.max_body_bytes", "canary.canary_basis_points", "ai.default_upstream"},
			doNotWant: []string{"admin.listen"},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := newInvalidConfig().Validate(test.role)
			if err == nil {
				t.Fatal("非法配置未返回错误")
			}
			for _, expected := range test.want {
				if !strings.Contains(err.Error(), expected) {
					t.Errorf("错误 %q 缺少配置段 %q", err, expected)
				}
			}
			for _, unexpected := range test.doNotWant {
				if strings.Contains(err.Error(), unexpected) {
					t.Errorf("错误 %q 包含其他角色配置段 %q", err, unexpected)
				}
			}
		})
	}
}

func TestStreamWriteTimeoutEnvironmentAndValidation(t *testing.T) {
	path := writeTestConfig(t, "")
	t.Setenv("NEXT_GATEWAY_REQUEST_STREAM_WRITE_TIMEOUT", "25ms")
	cfg, err := Load(WithPath(path), WithRole(role.AI))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Request.StreamWriteTimeout.String() != "25ms" {
		t.Fatalf("stream_write_timeout = %s", cfg.Request.StreamWriteTimeout)
	}
	cfg.Request.StreamWriteTimeout = 0
	if err := cfg.Validate(role.AI); err == nil || !strings.Contains(err.Error(), "stream_write_timeout") {
		t.Fatalf("非正写期限未拒绝: %v", err)
	}
}

func TestValidateCanaryTenantLists(t *testing.T) {
	cfg := Default()
	cfg.Canary.ForceStableTenants = []string{"bad\ntenant"}
	if err := cfg.Validate(role.AI); err == nil || !strings.Contains(err.Error(), "force_stable_tenants[0]") {
		t.Fatalf("非法强制名单未拒绝: %v", err)
	}
}

func TestLoadProcessProfiles(t *testing.T) {
	unsetEnvironmentForTest(t, "NEXT_GATEWAY_SERVER_LISTEN")
	root := filepath.Join("..", "..", "..")
	tests := []struct {
		name       string
		path       string
		role       role.Role
		wantListen string
	}{
		{name: "biz", path: "configs/biz-gateway.yaml", role: role.Biz, wantListen: ":8080"},
		{name: "ai", path: "configs/ai-gateway.yaml", role: role.AI, wantListen: ":8081"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cfg, err := Load(WithPath(filepath.Join(root, test.path)), WithRole(test.role))
			if err != nil {
				t.Fatal(err)
			}
			if cfg.Server.Listen != test.wantListen {
				t.Errorf("server.listen = %q，期望 %q", cfg.Server.Listen, test.wantListen)
			}
			if cfg.AI.Mock != Default().AI.Mock {
				t.Errorf("profile 未继承 Mock 默认值: %#v", cfg.AI.Mock)
			}
		})
	}

	// 角色 profile 只改变 YAML 来源，显式 CLI 仍必须保持最高优先级。
	flags := pflag.NewFlagSet("ai-test", pflag.ContinueOnError)
	BindFlags(flags)
	if err := flags.Set("listen", "127.0.0.1:0"); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(WithPath(filepath.Join(root, "configs/ai-gateway.yaml")), WithFlags(flags), WithRole(role.AI))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Server.Listen != "127.0.0.1:0" {
		t.Errorf("CLI 未覆盖 AI profile: %q", cfg.Server.Listen)
	}
	if ResolvePath("") != DefaultPath {
		t.Errorf("空路径兜底 = %q，期望 %q", ResolvePath(""), DefaultPath)
	}
	if _, err := os.Stat(filepath.Join(root, DefaultPath)); err != nil {
		t.Errorf("空路径兜底文件不存在: %v", err)
	}
}

func unsetEnvironmentForTest(t *testing.T, key string) {
	t.Helper()
	value, exists := os.LookupEnv(key)
	if err := os.Unsetenv(key); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if exists {
			_ = os.Setenv(key, value)
		} else {
			_ = os.Unsetenv(key)
		}
	})
}

func writeTestConfig(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}
