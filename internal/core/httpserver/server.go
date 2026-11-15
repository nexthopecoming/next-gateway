package httpserver

import (
	"context"
	"errors"
	"net"
	"net/http"
	"sync/atomic"
	"time"

	"go.uber.org/zap"

	"github.com/nexthopecoming/next-gateway/internal/core/config"
)

// Server 管理 socket、就绪状态和带期限的优雅退出。
type Server struct {
	httpServer      *http.Server
	shutdownTimeout time.Duration
	ready           *atomic.Bool // 状态，服务是否就绪，是否接受新请求，对应/readyz 接口
	logger          *zap.Logger
}

// New 构造共享 HTTP Server；WriteTimeout 留零，避免后续长 SSE 被统一写超时截断。
func New(cfg config.ServerConfig, ready *atomic.Bool, logger *zap.Logger, handler http.Handler) *Server {
	return &Server{
		httpServer: &http.Server{
			Handler:           handler,
			ReadHeaderTimeout: cfg.ReadHeaderTimeout,
			ReadTimeout:       cfg.ReadTimeout,
			// 长 SSE 不能使用统一 WriteTimeout，后续由请求级写期限处理。
			WriteTimeout: 0,
			IdleTimeout:  cfg.IdleTimeout,
		},
		shutdownTimeout: cfg.ShutdownTimeout,
		ready:           ready,
		logger:          logger,
	}
}

// Run 在 listener 就绪后服务请求；取消时先拒绝新流量，再等待已有请求退出。
func (s *Server) Run(ctx context.Context, listener net.Listener) error {
	serveErrors := make(chan error, 1)
	go func() {
		serveErrors <- s.httpServer.Serve(listener)
	}()

	s.ready.Store(true)
	defer s.ready.Store(false)
	s.logger.Info("HTTP 服务已启动", zap.String("address", listener.Addr().String()))

	select {
	case err := <-serveErrors:
		// Serve 自身失败必须立即返回，不能等信号后才发现端口未监听。
		return normalizeServeError(err)
	case <-ctx.Done():
	}

	// 先关闭就绪状态，让探针停止分发新流量，再等待在途请求自然退出。
	s.ready.Store(false)
	shutdownErr := s.shutdown()
	serveErr := normalizeServeError(<-serveErrors)
	if shutdownErr == nil && serveErr == nil {
		s.logger.Info("HTTP 服务已优雅停止")
	}
	return errors.Join(shutdownErr, serveErr)
}

// shutdown 在独立期限下关闭 listener 并等待活跃请求结束。
// 期限必须从 Background 派生：此时调用方 ctx 已取消，继承会立即超时而不等待。
func (s *Server) shutdown() error {
	shutdownCtx, cancel := context.WithTimeout(context.Background(), s.shutdownTimeout)
	defer cancel()

	err := s.httpServer.Shutdown(shutdownCtx)
	if err == nil {
		return nil
	}
	// 超时后关闭剩余连接，避免进程被不响应取消的处理器永久阻塞。
	return errors.Join(err, s.httpServer.Close())
}

// normalizeServeError 把被动关闭视为正常结束，其余错误原样返回。
func normalizeServeError(err error) error {
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}
