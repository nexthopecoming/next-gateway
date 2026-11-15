package command

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nexthopecoming/next-gateway/internal/core/config"
	"github.com/nexthopecoming/next-gateway/internal/core/role"
)

func TestRootCommandRoleMetadata(t *testing.T) {
	tests := []struct {
		role          role.Role
		use           string
		defaultConfig string
	}{
		{role: role.Biz, use: "biz-gateway", defaultConfig: "configs/biz-gateway.yaml"},
		{role: role.AI, use: "ai-gateway", defaultConfig: "configs/ai-gateway.yaml"},
	}
	for _, test := range tests {
		t.Run(test.use, func(t *testing.T) {
			root, err := newRootCommand(test.role, config.NewManager())
			if err != nil {
				t.Fatal(err)
			}
			if root.Use != test.use {
				t.Errorf("Use = %q，期望 %q", root.Use, test.use)
			}
			flag := root.PersistentFlags().Lookup("config")
			if flag == nil || flag.DefValue != test.defaultConfig {
				t.Errorf("默认配置 = %#v，期望 %q", flag, test.defaultConfig)
			}
		})
	}
}

func TestVersionCommand(t *testing.T) {
	for _, r := range []role.Role{role.Biz, role.AI} {
		t.Run(r.String(), func(t *testing.T) {
			output, err := executeCommand(t, r, "version")
			if err != nil {
				t.Fatal(err)
			}
			for _, expected := range []string{"version:", "commit:", "build_time:"} {
				if !strings.Contains(output, expected) {
					t.Errorf("version 输出缺少 %q: %s", expected, output)
				}
			}
		})
	}
}

func TestConfigValidateCommand(t *testing.T) {
	for _, r := range []role.Role{role.Biz, role.AI} {
		t.Run(r.String(), func(t *testing.T) {
			path := writeCommandConfig(t, "schema_version: 1\n")
			output, err := executeCommand(t, r, "config", "validate", "--config", path)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(output, "配置有效") {
				t.Fatalf("输出 = %s", output)
			}
		})
	}
}

func TestConfigValidateCommandReturnsError(t *testing.T) {
	for _, r := range []role.Role{role.Biz, role.AI} {
		t.Run(r.String(), func(t *testing.T) {
			path := writeCommandConfig(t, "unknown: true\n")
			if _, err := executeCommand(t, r, "config", "validate", "--config", path); err == nil {
				t.Fatal("非法配置未返回错误")
			}
		})
	}
}

// TestConfigValidateDoesNotUseServeManagerCache 防止校验命令误用 serve 的一次性缓存。
func TestConfigValidateDoesNotUseServeManagerCache(t *testing.T) {
	manager := config.NewManager()
	validPath := writeCommandConfig(t, "schema_version: 1\n")
	if _, err := manager.Load(config.WithPath(validPath), config.WithRole(role.AI)); err != nil {
		t.Fatalf("预置 Manager 缓存失败: %v", err)
	}

	root, err := newRootCommand(role.AI, manager)
	if err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	root.SetOut(&output)
	root.SetErr(&output)
	root.SetArgs([]string{"config", "validate", "--config", writeCommandConfig(t, "unknown: true\n")})
	if err := root.Execute(); err == nil || !strings.Contains(err.Error(), "unknown") {
		t.Fatalf("validate 未独立拒绝非法配置: %v", err)
	}
}

func TestRootCommandRejectsUnknownRole(t *testing.T) {
	if _, err := newRootCommand(role.Role("unknown"), config.NewManager()); err == nil {
		t.Fatal("未知角色未返回错误")
	}
}

func executeCommand(t *testing.T, r role.Role, args ...string) (string, error) {
	t.Helper()
	root, err := newRootCommand(r, config.NewManager())
	if err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	root.SetOut(&output)
	root.SetErr(&output)
	root.SetArgs(args)
	err = root.Execute()
	return output.String(), err
}

func writeCommandConfig(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}
