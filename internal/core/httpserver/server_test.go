package httpserver

import (
	"context"
	"errors"
	"net"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/nexthopecoming/next-gateway/internal/core/config"
)

func TestServerWaitsForInFlightRequestDuringShutdown(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	mux := http.NewServeMux()
	mux.HandleFunc("GET /wait", func(w http.ResponseWriter, _ *http.Request) {
		close(started)
		<-release
		w.WriteHeader(http.StatusNoContent)
	})

	cfg := config.Default().Server
	cfg.ShutdownTimeout = time.Second
	var ready atomic.Bool
	server := New(cfg, &ready, zap.NewNop(), mux)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	runResult := make(chan error, 1)
	go func() { runResult <- server.Run(ctx, listener) }()

	client := http.Client{Timeout: 2 * time.Second}
	requestResult := make(chan error, 1)
	go func() {
		response, err := client.Get("http://" + listener.Addr().String() + "/wait")
		if err == nil {
			_ = response.Body.Close()
		}
		requestResult <- err
	}()

	select {
	case <-started:
	case <-time.After(time.Second):
		cancel()
		t.Fatal("请求未进入处理器")
	}
	cancel()

	// Shutdown 期间就绪状态必须立即关闭，但已有请求尚未被强制中断。
	select {
	case err := <-runResult:
		t.Fatalf("进行中请求尚未释放，服务却提前退出: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	if ready.Load() {
		t.Fatal("开始退出后服务仍处于 ready")
	}

	close(release)
	select {
	case err := <-requestResult:
		if err != nil {
			t.Fatalf("进行中请求失败: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("请求未正常结束")
	}
	select {
	case err := <-runResult:
		if err != nil {
			t.Fatalf("优雅退出失败: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("服务未退出")
	}
}

// TestServerForcesCloseAfterShutdownTimeout 验证不响应 Shutdown 的连接会在期限后被强制取消。
func TestServerForcesCloseAfterShutdownTimeout(t *testing.T) {
	started := make(chan struct{})
	handlerDone := make(chan struct{})
	mux := http.NewServeMux()
	mux.HandleFunc("GET /hang", func(_ http.ResponseWriter, r *http.Request) {
		close(started)
		defer close(handlerDone)
		<-r.Context().Done()
	})

	cfg := config.Default().Server
	cfg.ShutdownTimeout = 50 * time.Millisecond
	var ready atomic.Bool
	server := New(cfg, &ready, zap.NewNop(), mux)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() {
		cancel()
		_ = server.httpServer.Close()
	})

	runResult := make(chan error, 1)
	go func() { runResult <- server.Run(ctx, listener) }()
	client := http.Client{Timeout: 2 * time.Second}
	requestResult := make(chan error, 1)
	go func() {
		response, err := client.Get("http://" + listener.Addr().String() + "/hang")
		if err == nil {
			_ = response.Body.Close()
		}
		requestResult <- err
	}()

	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("请求未进入阻塞处理器")
	}
	cancel()

	// 处理器只随请求上下文退出，因此 DeadlineExceeded 后的取消可证明 Close 已执行。
	select {
	case err := <-runResult:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("强制退出错误 = %v，期望包含 context.DeadlineExceeded", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Shutdown 超时后服务仍未退出")
	}
	if ready.Load() {
		t.Fatal("强制退出后服务仍处于 ready")
	}
	select {
	case err := <-requestResult:
		if err == nil {
			t.Fatal("被强制关闭的请求仍返回成功")
		}
	case <-time.After(time.Second):
		t.Fatal("被强制关闭的客户端请求未退出")
	}
	select {
	case <-handlerDone:
	case <-time.After(time.Second):
		t.Fatal("强制关闭后处理器上下文未取消")
	}
}
