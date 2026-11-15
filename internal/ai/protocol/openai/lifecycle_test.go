package openai_test

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/nexthopecoming/next-gateway/internal/core/config"
	"github.com/nexthopecoming/next-gateway/internal/core/engine"
	"github.com/nexthopecoming/next-gateway/internal/core/httpserver"
)

// controlledUpstream 由测试逐事件推进流，并暴露真实请求 context 的取消信号。
type controlledUpstream struct {
	events     chan engine.Event
	started    chan struct{}
	canceled   chan struct{}
	startOnce  sync.Once
	cancelOnce sync.Once
}

func newControlledUpstream() *controlledUpstream {
	return &controlledUpstream{
		events:   make(chan engine.Event),
		started:  make(chan struct{}),
		canceled: make(chan struct{}),
	}
}

func (u *controlledUpstream) Complete(ctx context.Context, _ engine.Request) (engine.Response, error) {
	u.startOnce.Do(func() { close(u.started) })
	<-ctx.Done()
	u.cancelOnce.Do(func() { close(u.canceled) })
	return engine.Response{}, ctx.Err()
}

func (u *controlledUpstream) Stream(ctx context.Context, _ engine.Request) (<-chan engine.Event, error) {
	u.startOnce.Do(func() { close(u.started) })
	go func() {
		<-ctx.Done()
		u.cancelOnce.Do(func() { close(u.canceled) })
	}()
	return u.events, nil
}

func TestNetworkFlushPrecedesUpstreamCompletion(t *testing.T) {
	upstream := newControlledUpstream()
	server, _ := newOpenAIServer(t, config.Default(), upstream)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	response := startOpenAIStream(t, ctx, server.Client(), server.URL)
	defer response.Body.Close()
	reader := bufio.NewReader(response.Body)

	// 上游尚未发送内容或 Done，协议初始 role 事件仍必须已经到达真实客户端。
	role, err := readSSEFrame(reader)
	if err != nil {
		t.Fatalf("读取首个 role 事件: %v", err)
	}
	if !strings.Contains(role, `"role":"assistant"`) {
		t.Fatalf("首帧不是 role chunk: %q", role)
	}
	sendUpstreamEvent(t, upstream.events, engine.Event{Kind: engine.EventContent, Content: "A"})
	content, err := readSSEFrame(reader)
	if err != nil {
		t.Fatalf("读取首个内容事件: %v", err)
	}
	if !strings.Contains(content, `"content":"A"`) {
		t.Fatalf("内容 chunk 未及时 Flush: %q", content)
	}

	sendUpstreamEvent(t, upstream.events, engine.Event{Kind: engine.EventDone, Response: engine.Response{
		Text: "A", FinishReason: engine.FinishStop, Usage: engine.Usage{InputTokens: 1, OutputTokens: 1},
	}})
	remaining, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("读取结束事件: %v", err)
	}
	if !strings.Contains(string(remaining), "data: [DONE]\n\n") {
		t.Fatalf("缺少正常结束标记: %q", remaining)
	}
	waitForSignal(t, upstream.canceled, "流结束后取消上游")
}

func TestNetworkFirstChunkTimeoutCancelsUpstream(t *testing.T) {
	cfg := config.Default()
	cfg.Request.FirstChunkTimeout = 30 * time.Millisecond
	upstream := newControlledUpstream()
	server, _ := newOpenAIServer(t, cfg, upstream)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	response := startOpenAIStream(t, ctx, server.Client(), server.URL)
	defer response.Body.Close()

	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("读取首包超时响应: %v", err)
	}
	if !strings.Contains(string(body), `"code":"first_chunk_timeout"`) || !strings.Contains(string(body), "data: [DONE]") {
		t.Fatalf("首包超时协议响应错误: %s", body)
	}
	waitForSignal(t, upstream.canceled, "首包超时取消上游")
}

func TestNetworkClientCancellationPropagatesUpstream(t *testing.T) {
	upstream := newControlledUpstream()
	server, _ := newOpenAIServer(t, config.Default(), upstream)
	ctx, cancel := context.WithCancel(context.Background())
	response := startOpenAIStream(t, ctx, server.Client(), server.URL)
	reader := bufio.NewReader(response.Body)
	if _, err := readSSEFrame(reader); err != nil {
		t.Fatalf("读取初始事件: %v", err)
	}

	// 客户端在收到首帧后主动断开，Handler 返回时必须取消共享上游 context。
	cancel()
	_ = response.Body.Close()
	waitForSignal(t, upstream.canceled, "客户端断开传播到上游")
}

func TestNetworkNonStreamingUpstreamTimeout(t *testing.T) {
	cfg := config.Default()
	cfg.Request.UpstreamTimeout = 30 * time.Millisecond
	upstream := newControlledUpstream()
	server, _ := newOpenAIServer(t, cfg, upstream)
	response := openAIRequest(t, server, `{"model":"mock-model","messages":[{"role":"user","content":"hello"}]}`)
	if response.status != http.StatusGatewayTimeout || !strings.Contains(response.body, `"code":"upstream_timeout"`) {
		t.Fatalf("非流式上游超时 = %d %s", response.status, response.body)
	}
	waitForSignal(t, upstream.canceled, "非流式总超时取消上游")
}

func TestServerShutdownDrainsActiveSSE(t *testing.T) {
	cfg := config.Default()
	cfg.Server.ShutdownTimeout = time.Second
	upstream := newControlledUpstream()
	gateway := startRealOpenAIServer(t, cfg, upstream)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	response := startOpenAIStream(t, ctx, gateway.client(), gateway.url)
	defer response.Body.Close()
	reader := bufio.NewReader(response.Body)
	if _, err := readSSEFrame(reader); err != nil {
		t.Fatalf("读取初始事件: %v", err)
	}

	gateway.stop()
	select {
	case <-gateway.done:
		t.Fatalf("活跃 SSE 尚未完成，Shutdown 却提前返回: %v", gateway.err)
	case <-time.After(50 * time.Millisecond):
	}
	sendUpstreamEvent(t, upstream.events, engine.Event{Kind: engine.EventContent, Content: "A"})
	sendUpstreamEvent(t, upstream.events, engine.Event{Kind: engine.EventDone, Response: engine.Response{
		Text: "A", FinishReason: engine.FinishStop, Usage: engine.Usage{InputTokens: 1, OutputTokens: 1},
	}})
	body, err := io.ReadAll(reader)
	if err != nil || !strings.Contains(string(body), "data: [DONE]") {
		t.Fatalf("draining 中响应未正常结束: %v %q", err, body)
	}
	waitForSignal(t, gateway.done, "活跃 SSE 完成后的优雅退出")
	if gateway.err != nil {
		t.Fatalf("正常 draining 返回错误: %v", gateway.err)
	}
}

func TestServerShutdownDeadlineClosesActiveSSE(t *testing.T) {
	cfg := config.Default()
	cfg.Server.ShutdownTimeout = 40 * time.Millisecond
	upstream := newControlledUpstream()
	gateway := startRealOpenAIServer(t, cfg, upstream)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	response := startOpenAIStream(t, ctx, gateway.client(), gateway.url)
	defer response.Body.Close()
	reader := bufio.NewReader(response.Body)
	if _, err := readSSEFrame(reader); err != nil {
		t.Fatalf("读取初始事件: %v", err)
	}

	gateway.stop()
	body, readErr := io.ReadAll(reader)
	if readErr == nil && strings.Contains(string(body), "data: [DONE]") {
		t.Fatalf("强制关闭后不应出现完整正常流: %q", body)
	}
	waitForSignal(t, gateway.done, "Shutdown 超时后的强制退出")
	if !errors.Is(gateway.err, context.DeadlineExceeded) {
		t.Fatalf("强制退出错误 = %v，期望包含 context deadline", gateway.err)
	}
	waitForSignal(t, upstream.canceled, "强制关闭连接后取消上游")
}

type realOpenAIServer struct {
	url    string
	cancel context.CancelFunc
	done   chan struct{}
	err    error
}

func startRealOpenAIServer(t *testing.T, cfg config.Config, upstream engine.Upstream) *realOpenAIServer {
	t.Helper()
	root, _ := newOpenAIRoot(t, cfg, upstream)
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	gateway := &realOpenAIServer{url: "http://" + listener.Addr().String(), cancel: cancel, done: make(chan struct{})}
	var ready atomic.Bool
	server := httpserver.New(cfg.Server, &ready, zap.NewNop(), root)
	go func() {
		gateway.err = server.Run(ctx, listener)
		close(gateway.done)
	}()
	t.Cleanup(func() {
		gateway.stop()
		select {
		case <-gateway.done:
		case <-time.After(3 * time.Second):
			t.Error("清理真实 HTTP Server 超时")
		}
	})
	return gateway
}

func (s *realOpenAIServer) stop() { s.cancel() }
func (s *realOpenAIServer) client() *http.Client {
	return &http.Client{}
}

func startOpenAIStream(t *testing.T, ctx context.Context, client *http.Client, baseURL string) *http.Response {
	t.Helper()
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"/v1/chat/completions", strings.NewReader(
		`{"model":"mock-model","messages":[{"role":"user","content":"hello"}],"stream":true}`,
	))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := client.Do(request)
	if err != nil {
		t.Fatalf("启动真实 SSE 请求: %v", err)
	}
	if response.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(response.Body)
		_ = response.Body.Close()
		t.Fatalf("SSE 状态码 = %d: %s", response.StatusCode, body)
	}
	return response
}

func readSSEFrame(reader *bufio.Reader) (string, error) {
	var frame strings.Builder
	for {
		line, err := reader.ReadString('\n')
		frame.WriteString(line)
		if err != nil {
			return frame.String(), err
		}
		if line == "\n" {
			return frame.String(), nil
		}
	}
}

func sendUpstreamEvent(t *testing.T, events chan<- engine.Event, event engine.Event) {
	t.Helper()
	select {
	case events <- event:
	case <-time.After(2 * time.Second):
		t.Fatal("发送测试上游事件超时")
	}
}

func waitForSignal(t *testing.T, signal <-chan struct{}, description string) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(2 * time.Second):
		t.Fatal(description + "超时")
	}
}
