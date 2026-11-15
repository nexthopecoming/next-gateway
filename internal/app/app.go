// Package app 负责组装配置、日志、HTTP Handler 和进程生命周期。
package app

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"sync/atomic"

	"go.uber.org/zap"

	"github.com/nexthopecoming/next-gateway/internal/ai"
	aimock "github.com/nexthopecoming/next-gateway/internal/ai/mock"
	"github.com/nexthopecoming/next-gateway/internal/ai/protocol/anthropic"
	"github.com/nexthopecoming/next-gateway/internal/ai/protocol/openai"
	"github.com/nexthopecoming/next-gateway/internal/biz"
	"github.com/nexthopecoming/next-gateway/internal/core/canary"
	"github.com/nexthopecoming/next-gateway/internal/core/config"
	"github.com/nexthopecoming/next-gateway/internal/core/engine"
	"github.com/nexthopecoming/next-gateway/internal/core/httpserver"
	"github.com/nexthopecoming/next-gateway/internal/core/logging"
	"github.com/nexthopecoming/next-gateway/internal/core/role"
	"github.com/nexthopecoming/next-gateway/internal/core/version"
)

// Run 是两个 serve 命令共用的应用入口；role 只能取固定枚举，保证日志标签低基数。
// AI 依赖只在这里组装，core 通过路由注册回调保持对 biz/ai 的零依赖。
func Run(ctx context.Context, cfg config.Config, processRole role.Role) (returnErr error) {
	logger, err := logging.New(cfg.Log)
	if err != nil {
		return err
	}
	defer func() {
		// 部分平台对 stdout/stderr 执行 Sync 会返回不支持错误，不影响服务退出结果。
		if err := logger.Close(); err != nil && cfg.Log.Output != "stdout" && cfg.Log.Output != "stderr" {
			returnErr = errors.Join(returnErr, fmt.Errorf("关闭日志: %w", err))
		}
	}()
	serviceLogger := logger.With(zap.String("service", processRole.String()))

	var ready atomic.Bool
	var registrars []func(*http.ServeMux)
	if processRole == role.AI {
		decider, err := canary.NewDecider(
			cfg.Canary.CanaryBasisPoints,
			cfg.Canary.ForceStableTenants,
			cfg.Canary.ForceCanaryTenants,
			cfg.Canary.AllowOverrideHeader,
		)
		if err != nil {
			return fmt.Errorf("构建灰度决策器: %w", err)
		}
		// 两套 Engine 共享同一个 Mock；差异仅在执行编排，不人为改变协议文本。
		upstream := aimock.New(cfg.AI.Mock)
		stableEngine := engine.NewStable(upstream)
		canaryEngine := engine.NewCanary(upstream, engine.Hooks{})
		executor := ai.NewExecutor(biz.IdentityExtractor{}, decider, stableEngine, canaryEngine, cfg.Request)
		openAIHandler := openai.NewHandler(executor, nil)
		anthropicHandler := anthropic.NewHandler(executor)
		registrars = append(registrars, func(mux *http.ServeMux) {
			mux.Handle("POST /v1/chat/completions", openAIHandler)
			mux.Handle("POST /v1/messages", anthropicHandler)
		})
	}
	handler := httpserver.NewHandler(&ready, serviceLogger, registrars...)
	server := httpserver.New(cfg.Server, &ready, serviceLogger, handler)

	listener, err := net.Listen("tcp", cfg.Server.Listen)
	if err != nil {
		return fmt.Errorf("监听 %s: %w", cfg.Server.Listen, err)
	}
	serviceLogger.Info("应用配置加载完成",
		zap.String("version", version.Current().Version),
		zap.String("config_source", cfg.Source.Source),
		zap.String("listen", listener.Addr().String()),
		zap.String("log_level", cfg.Log.Level),
	)
	return server.Run(ctx, listener)
}
