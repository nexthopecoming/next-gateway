package proxy_test

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nexthopecoming/next-gateway/internal/core/balancer"
	"github.com/nexthopecoming/next-gateway/internal/core/proxy"
	"github.com/nexthopecoming/next-gateway/internal/core/router"
)

func TestTransportConfigDefaultsAndValidation(t *testing.T) {
	defaults := proxy.DefaultTransportConfig()
	if defaults.DialTimeout != 5*time.Second ||
		defaults.TLSHandshakeTimeout != 5*time.Second ||
		defaults.ResponseHeaderTimeout != 30*time.Second ||
		defaults.RequestHeaderTimeout != 35*time.Second ||
		defaults.IdleConnectionTimeout != 90*time.Second ||
		defaults.MaxIdleConnections != 100 ||
		defaults.MaxIdleConnectionsPerHost != 20 ||
		defaults.MaxInFlightRequests != 1024 {
		t.Fatalf("Transport 默认值偏离设计: %+v", defaults)
	}
	empty := compileTable(t)
	dispatcher, err := proxy.NewDispatcher(empty, nil, defaults)
	if err != nil {
		t.Fatal(err)
	}
	dispatcher.Close()

	for _, config := range []proxy.TransportConfig{
		{
			DialTimeout: time.Millisecond, TLSHandshakeTimeout: time.Millisecond, ResponseHeaderTimeout: time.Millisecond,
			RequestHeaderTimeout: time.Millisecond, IdleConnectionTimeout: time.Second,
			MaxIdleConnections: 1, MaxIdleConnectionsPerHost: 1, MaxInFlightRequests: 1,
		},
		{
			DialTimeout: time.Minute, TLSHandshakeTimeout: time.Minute, ResponseHeaderTimeout: 5 * time.Minute,
			RequestHeaderTimeout: 5 * time.Minute, IdleConnectionTimeout: 10 * time.Minute,
			MaxIdleConnections: 10000, MaxIdleConnectionsPerHost: 1000, MaxInFlightRequests: 100000,
		},
	} {
		dispatcher, err := proxy.NewDispatcher(empty, nil, config)
		if err != nil {
			t.Fatalf("合法 Transport 边界被拒绝: %+v: %v", config, err)
		}
		dispatcher.Close()
	}

	tests := []struct {
		name   string
		mutate func(*proxy.TransportConfig)
	}{
		{name: "zero dial", mutate: func(config *proxy.TransportConfig) { config.DialTimeout = 0 }},
		{name: "excessive dial", mutate: func(config *proxy.TransportConfig) { config.DialTimeout = time.Minute + time.Millisecond }},
		{name: "zero TLS", mutate: func(config *proxy.TransportConfig) { config.TLSHandshakeTimeout = 0 }},
		{name: "zero response header", mutate: func(config *proxy.TransportConfig) { config.ResponseHeaderTimeout = 0 }},
		{name: "excessive response header", mutate: func(config *proxy.TransportConfig) { config.ResponseHeaderTimeout = 5*time.Minute + time.Millisecond }},
		{name: "zero request header", mutate: func(config *proxy.TransportConfig) { config.RequestHeaderTimeout = 0 }},
		{name: "excessive request header", mutate: func(config *proxy.TransportConfig) { config.RequestHeaderTimeout = 5*time.Minute + time.Millisecond }},
		{name: "short idle", mutate: func(config *proxy.TransportConfig) { config.IdleConnectionTimeout = 999 * time.Millisecond }},
		{name: "excessive idle", mutate: func(config *proxy.TransportConfig) { config.IdleConnectionTimeout = 10*time.Minute + time.Second }},
		{name: "zero idle connections", mutate: func(config *proxy.TransportConfig) { config.MaxIdleConnections = 0 }},
		{name: "excessive idle connections", mutate: func(config *proxy.TransportConfig) { config.MaxIdleConnections = 10001 }},
		{name: "zero per host", mutate: func(config *proxy.TransportConfig) { config.MaxIdleConnectionsPerHost = 0 }},
		{name: "excessive per host", mutate: func(config *proxy.TransportConfig) { config.MaxIdleConnectionsPerHost = 1001 }},
		{name: "zero in-flight requests", mutate: func(config *proxy.TransportConfig) { config.MaxInFlightRequests = 0 }},
		{name: "excessive in-flight requests", mutate: func(config *proxy.TransportConfig) { config.MaxInFlightRequests = 100001 }},
		{name: "per host exceeds total", mutate: func(config *proxy.TransportConfig) {
			config.MaxIdleConnections = 10
			config.MaxIdleConnectionsPerHost = 11
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			config := proxy.DefaultTransportConfig()
			test.mutate(&config)
			if dispatcher, err := proxy.NewDispatcher(empty, nil, config); err == nil || dispatcher != nil {
				t.Fatalf("非法 Transport 配置未拒绝: dispatcher=%v err=%v", dispatcher, err)
			}
		})
	}
}

func TestResponseHeaderTimeoutClassification(t *testing.T) {
	started := make(chan struct{})
	canceled := make(chan struct{})
	backend := newBackend(t, "a", func(_ http.ResponseWriter, request *http.Request) {
		close(started)
		<-request.Context().Done()
		close(canceled)
	})
	config := proxy.DefaultTransportConfig()
	config.ResponseHeaderTimeout = 40 * time.Millisecond
	dispatcher := newDispatcher(t, openAIRoutes(), []*balancer.Pool{
		newPool(t, "pool", balancer.PolicyRoundRobin, []balancer.EndpointSpec{{Name: "a", URL: backend.server.URL, Weight: 1}}),
	}, config)

	selection, response, err := dispatcher.Do(context.Background(), proxy.Request{Match: openAIMatch("model")})
	if !errors.Is(err, proxy.ErrUpstreamTimeout) || response != nil || selection.Endpoint != "a" {
		t.Fatalf("响应头超时分类错误: selection=%+v response=%v err=%v", selection, response, err)
	}
	waitSignal(t, started)
	waitSignal(t, canceled)
	if strings.Contains(err.Error(), backend.server.URL) {
		t.Fatalf("超时错误泄露 URL: %v", err)
	}
}

func TestCallerCancellationPrecedesTransportClassification(t *testing.T) {
	started := make(chan struct{})
	canceled := make(chan struct{})
	backend := newBackend(t, "a", func(_ http.ResponseWriter, request *http.Request) {
		close(started)
		<-request.Context().Done()
		close(canceled)
	})
	config := proxy.DefaultTransportConfig()
	config.ResponseHeaderTimeout = 5 * time.Second
	dispatcher := newDispatcher(t, openAIRoutes(), []*balancer.Pool{
		newPool(t, "pool", balancer.PolicyRoundRobin, []balancer.EndpointSpec{{Name: "a", URL: backend.server.URL, Weight: 1}}),
	}, config)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	results := make(chan doResult, 1)
	go func() {
		selection, response, err := dispatcher.Do(ctx, proxy.Request{Match: openAIMatch("model")})
		results <- doResult{selection: selection, response: response, err: err}
	}()
	waitSignal(t, started)
	cancel()
	result := waitResult(t, results)
	if result.err != context.Canceled || result.response != nil || result.selection.Endpoint != "a" {
		t.Fatalf("调用方取消未保留原错误: %+v", result)
	}
	waitSignal(t, canceled)
}

func TestCallerDeadlinePrecedesTransportClassification(t *testing.T) {
	canceled := make(chan struct{})
	backend := newBackend(t, "a", func(_ http.ResponseWriter, request *http.Request) {
		<-request.Context().Done()
		close(canceled)
	})
	config := proxy.DefaultTransportConfig()
	config.ResponseHeaderTimeout = 5 * time.Second
	dispatcher := newDispatcher(t, openAIRoutes(), []*balancer.Pool{
		newPool(t, "pool", balancer.PolicyRoundRobin, []balancer.EndpointSpec{{Name: "a", URL: backend.server.URL, Weight: 1}}),
	}, config)
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancel()

	selection, response, err := dispatcher.Do(ctx, proxy.Request{Match: openAIMatch("model")})
	if err != context.DeadlineExceeded || response != nil || selection.Endpoint != "a" {
		t.Fatalf("调用方 deadline 未保留原错误: selection=%+v response=%v err=%v", selection, response, err)
	}
	waitSignal(t, canceled)
}

func TestConnectionAndTLSErrorsAreBounded(t *testing.T) {
	t.Run("connection failure", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
		origin := server.URL
		server.Close()
		dispatcher := newDispatcher(t, openAIRoutes(), []*balancer.Pool{
			newPool(t, "pool", balancer.PolicyRoundRobin, []balancer.EndpointSpec{{Name: "a", URL: origin, Weight: 1}}),
		}, proxy.DefaultTransportConfig())
		selection, response, err := dispatcher.Do(context.Background(), proxy.Request{Match: openAIMatch("model")})
		if !errors.Is(err, proxy.ErrUpstream) || response != nil || selection.Endpoint != "a" {
			t.Fatalf("连接错误分类错误: selection=%+v response=%v err=%v", selection, response, err)
		}
		if strings.Contains(err.Error(), origin) {
			t.Fatalf("连接错误泄露 origin: %v", err)
		}
	})

	t.Run("self signed TLS is rejected", func(t *testing.T) {
		var requests atomic.Int64
		server := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
			requests.Add(1)
		}))
		t.Cleanup(server.Close)
		dispatcher := newDispatcher(t, openAIRoutes(), []*balancer.Pool{
			newPool(t, "pool", balancer.PolicyRoundRobin, []balancer.EndpointSpec{{Name: "a", URL: server.URL, Weight: 1}}),
		}, proxy.DefaultTransportConfig())
		selection, response, err := dispatcher.Do(context.Background(), proxy.Request{Match: openAIMatch("model")})
		if !errors.Is(err, proxy.ErrUpstream) || response != nil || selection.Endpoint != "a" {
			t.Fatalf("自签名 TLS 未按有限错误拒绝: selection=%+v response=%v err=%v", selection, response, err)
		}
		if requests.Load() != 0 || strings.Contains(err.Error(), server.URL) {
			t.Fatalf("TLS 验证或错误脱敏失败: requests=%d err=%v", requests.Load(), err)
		}
	})
}

func TestHeaderTimeoutsDoNotTruncateStreamBody(t *testing.T) {
	release := make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(unblock)
	backend := newBackend(t, "a", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		<-release
		_, _ = io.WriteString(w, "data: late\n\n")
	})
	config := proxy.DefaultTransportConfig()
	config.ResponseHeaderTimeout = 40 * time.Millisecond
	config.RequestHeaderTimeout = 40 * time.Millisecond
	dispatcher := newDispatcher(t, openAIRoutes(), []*balancer.Pool{
		newPool(t, "pool", balancer.PolicyRoundRobin, []balancer.EndpointSpec{{Name: "a", URL: backend.server.URL, Weight: 1}}),
	}, config)

	selection, response, err := dispatcher.Do(context.Background(), proxy.Request{Match: openAIMatch("model")})
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if selection.Endpoint != "a" || response.Header.Get("Content-Type") != "text/event-stream" {
		t.Fatalf("流式响应头错误: selection=%+v headers=%+v", selection, response.Header)
	}
	timer := time.NewTimer(3 * config.ResponseHeaderTimeout)
	select {
	case <-timer.C:
	case <-time.After(5 * time.Second):
		t.Fatal("等待越过响应头阶段超时时超时")
	}
	unblock()
	body, err := io.ReadAll(response.Body)
	if err != nil || string(body) != "data: late\n\n" {
		t.Fatalf("响应头阶段超时截断了流式 Body: body=%q err=%v", body, err)
	}
}

func TestCancellationAfterHeadersStopsBodyAndDownstreamHandler(t *testing.T) {
	canceled := make(chan struct{})
	backend := newBackend(t, "a", func(w http.ResponseWriter, request *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		<-request.Context().Done()
		close(canceled)
	})
	dispatcher := oneBackendDispatcher(t, backend)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	selection, response, err := dispatcher.Do(ctx, proxy.Request{Match: openAIMatch("model")})
	if err != nil {
		t.Fatal(err)
	}
	if selection.Endpoint != "a" {
		t.Fatalf("Selection 错误: %+v", selection)
	}
	cancel()
	_, _ = io.ReadAll(response.Body)
	_ = response.Body.Close()
	waitSignal(t, canceled)
}

func TestCloseDoesNotInterruptInFlightStream(t *testing.T) {
	firstSent := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(unblock)
	backend := newBackend(t, "a", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "data: first\n\n")
		w.(http.Flusher).Flush()
		close(firstSent)
		<-release
		_, _ = io.WriteString(w, "data: second\n\n")
	})
	dispatcher := oneBackendDispatcher(t, backend)
	_, response, err := dispatcher.Do(context.Background(), proxy.Request{Match: openAIMatch("model")})
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	waitSignal(t, firstSent)
	reader := bufio.NewReader(response.Body)
	first, err := reader.ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	blank, err := reader.ReadString('\n')
	if err != nil || first != "data: first\n" || blank != "\n" {
		t.Fatalf("首个 SSE 帧错误: first=%q blank=%q err=%v", first, blank, err)
	}

	dispatcher.Close()
	unblock()
	remaining, err := io.ReadAll(reader)
	if err != nil || string(remaining) != "data: second\n\n" {
		t.Fatalf("Close 中断在途 SSE: body=%q err=%v", remaining, err)
	}
}

func openAIRoutes() []router.RouteSpec {
	return []router.RouteSpec{openAIRoute("openai", "pool")}
}
