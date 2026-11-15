// Package logging 提供统一的 zap 日志初始化和后续热更新所需的原子级别。
package logging

import (
	"fmt"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"

	"github.com/nexthopecoming/next-gateway/internal/core/config"
)

// Logger 保留原子级别，后续配置热更新无需替换请求正在使用的 Logger 实例。
type Logger struct {
	*zap.Logger
	level zap.AtomicLevel
	close func()
}

// New 按配置创建 zap Logger，并保留可并发调整级别的 AtomicLevel。
func New(cfg config.LogConfig) (*Logger, error) {
	var level zapcore.Level
	if err := level.Set(cfg.Level); err != nil {
		return nil, fmt.Errorf("解析日志级别: %w", err)
	}
	atomicLevel := zap.NewAtomicLevelAt(level)

	encoderConfig := zap.NewProductionEncoderConfig()
	encoderConfig.EncodeTime = zapcore.ISO8601TimeEncoder
	encoderConfig.EncodeLevel = zapcore.LowercaseLevelEncoder

	var encoder zapcore.Encoder
	switch cfg.Encoding {
	case "json":
		encoder = zapcore.NewJSONEncoder(encoderConfig)
	case "console":
		encoder = zapcore.NewConsoleEncoder(encoderConfig)
	default:
		return nil, fmt.Errorf("不支持的日志编码 %q", cfg.Encoding)
	}

	writeSyncer, closeSink, err := zap.Open(cfg.Output)
	if err != nil {
		return nil, fmt.Errorf("打开日志输出 %s: %w", cfg.Output, err)
	}
	core := zapcore.NewCore(encoder, writeSyncer, atomicLevel)
	logger := zap.New(core, zap.AddCaller(), zap.AddStacktrace(zapcore.ErrorLevel))
	return &Logger{Logger: logger, level: atomicLevel, close: closeSink}, nil
}

// SetLevel 是后续动态配置应用器使用的并发安全日志级别入口。
func (l *Logger) SetLevel(level zapcore.Level) {
	l.level.SetLevel(level)
}

// Close 先刷新缓冲，再关闭文件 sink；stdout/stderr 的 Sync 错误由调用方决定是否忽略。
func (l *Logger) Close() error {
	err := l.Sync()
	l.close()
	return err
}
