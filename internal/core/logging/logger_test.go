package logging

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"

	"github.com/nexthopecoming/next-gateway/internal/core/config"
)

// TestLoggerEncodingAndSetLevel 验证两种编码、文件输出及原子级别调整的实际效果。
func TestLoggerEncodingAndSetLevel(t *testing.T) {
	for _, encoding := range []string{"json", "console"} {
		t.Run(encoding, func(t *testing.T) {
			cfg := config.Default().Log
			cfg.Encoding = encoding
			cfg.Output = filepath.Join(t.TempDir(), "gateway.log")
			logger, err := New(cfg)
			if err != nil {
				t.Fatal(err)
			}
			logger.Debug("初始隐藏")
			logger.Info("应用启动", zap.String("module", "gateway"))
			logger.SetLevel(zapcore.DebugLevel)
			logger.Debug("调试可见", zap.Int("attempt", 1))
			if err := logger.Close(); err != nil {
				t.Fatalf("关闭日志: %v", err)
			}

			data, err := os.ReadFile(cfg.Output)
			if err != nil {
				t.Fatal(err)
			}
			text := string(data)
			if strings.Contains(text, "初始隐藏") {
				t.Error("info 级别输出了 debug 日志")
			}
			for _, expected := range []string{"应用启动", "调试可见", "gateway"} {
				if !strings.Contains(text, expected) {
					t.Errorf("日志缺少 %q: %s", expected, text)
				}
			}
			if encoding == "json" {
				lines := strings.Split(strings.TrimSpace(text), "\n")
				if len(lines) != 2 {
					t.Fatalf("JSON 日志行数 = %d，期望 2", len(lines))
				}
				for _, line := range lines {
					var entry map[string]any
					if err := json.Unmarshal([]byte(line), &entry); err != nil {
						t.Fatalf("JSON 日志无效: %v\n%s", err, line)
					}
				}
			}
		})
	}
}

// TestLoggerRejectsInvalidConfig 确保日志初始化错误能在服务监听前明确返回。
func TestLoggerRejectsInvalidConfig(t *testing.T) {
	tests := []struct {
		name     string
		level    string
		encoding string
	}{
		{name: "非法级别", level: "verbose", encoding: "json"},
		{name: "非法编码", level: "info", encoding: "xml"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			logger, err := New(config.LogConfig{
				Level:    test.level,
				Encoding: test.encoding,
				Output:   filepath.Join(t.TempDir(), "gateway.log"),
			})
			if err == nil {
				_ = logger.Close()
				t.Fatal("非法日志配置未返回错误")
			}
		})
	}
}

// TestLoggerRejectsInvalidOutput 验证文件 sink 创建失败不会被静默忽略。
func TestLoggerRejectsInvalidOutput(t *testing.T) {
	cfg := config.Default().Log
	cfg.Output = filepath.Join(t.TempDir(), "missing", "gateway.log")
	logger, err := New(cfg)
	if err == nil {
		_ = logger.Close()
		t.Fatal("无法创建日志文件时未返回错误")
	}
}
