package ai

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/nexthopecoming/next-gateway/internal/core/canary"
	"github.com/nexthopecoming/next-gateway/internal/core/config"
	"github.com/nexthopecoming/next-gateway/internal/core/engine"
)

type identityExtractorFunc func(*http.Request) (canary.Identity, error)

func (f identityExtractorFunc) Extract(request *http.Request) (canary.Identity, error) {
	return f(request)
}

type recordingEngine struct {
	completeCalls int
	streamCalls   int
	complete      func(context.Context, engine.Request) (engine.Response, error)
}

func (e *recordingEngine) Complete(ctx context.Context, request engine.Request) (engine.Response, error) {
	e.completeCalls++
	if e.complete != nil {
		return e.complete(ctx, request)
	}
	return engine.Response{Text: "selected"}, nil
}

func (e *recordingEngine) Stream(context.Context, engine.Request) (<-chan engine.Event, error) {
	e.streamCalls++
	events := make(chan engine.Event, 1)
	events <- engine.Event{Kind: engine.EventDone, Response: engine.Response{Text: "selected"}}
	close(events)
	return events, nil
}

func TestInvocationDecidesOnceAndPinsEngineAndConfig(t *testing.T) {
	extractCalls := 0
	extractor := identityExtractorFunc(func(*http.Request) (canary.Identity, error) {
		extractCalls++
		return canary.NewIdentity(canary.IdentityTenant, "tenant-001"), nil
	})
	decider, err := canary.NewDecider(10000, nil, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	stable := &recordingEngine{}
	canaryEngine := &recordingEngine{}
	requestConfig := config.Default().Request
	executor := NewExecutor(extractor, decider, stable, canaryEngine, requestConfig)

	// Executor 和 Invocation 都按值保存启动配置，调用方后续改配置不能改变在途请求。
	requestConfig.MaxBodyBytes = 1
	requestConfig.StreamIdleTimeout = time.Hour
	if executor.RequestBodyLimit() != config.Default().Request.MaxBodyBytes {
		t.Fatalf("Executor 请求体上限未固定: %d", executor.RequestBodyLimit())
	}
	recorder := httptest.NewRecorder()
	invocation, err := executor.Begin(recorder, httptest.NewRequest(http.MethodPost, "/v1/messages", nil), ProtocolAnthropic)
	if err != nil {
		t.Fatal(err)
	}
	if extractCalls != 1 || invocation.Decision().Core != canary.CoreCanary || invocation.Decision().Bucket != 2023 {
		t.Fatalf("灰度决策错误: calls=%d decision=%#v", extractCalls, invocation.Decision())
	}
	if recorder.Header().Get("X-Gateway-Core") != "canary" {
		t.Fatalf("X-Gateway-Core = %q", recorder.Header().Get("X-Gateway-Core"))
	}
	if invocation.RequestConfig().StreamIdleTimeout != config.Default().Request.StreamIdleTimeout {
		t.Fatalf("Invocation 配置快照被修改: %s", invocation.RequestConfig().StreamIdleTimeout)
	}
	if _, err := invocation.Complete(context.Background(), engine.Request{}); err != nil {
		t.Fatal(err)
	}
	events, err := invocation.Stream(context.Background(), engine.Request{})
	if err != nil {
		t.Fatal(err)
	}
	for range events {
	}
	if stable.completeCalls != 0 || stable.streamCalls != 0 || canaryEngine.completeCalls != 1 || canaryEngine.streamCalls != 1 {
		t.Fatalf("请求未固定使用 canary: stable=%#v canary=%#v", stable, canaryEngine)
	}
	if extractCalls != 1 {
		t.Fatalf("同一 Invocation 重复提取身份: %d", extractCalls)
	}
}

func TestInvocationMapsUpstreamDeadline(t *testing.T) {
	decider, err := canary.NewDecider(0, nil, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	stable := &recordingEngine{complete: func(ctx context.Context, _ engine.Request) (engine.Response, error) {
		<-ctx.Done()
		return engine.Response{}, ctx.Err()
	}}
	requestConfig := config.Default().Request
	requestConfig.UpstreamTimeout = 20 * time.Millisecond
	executor := NewExecutor(
		identityExtractorFunc(func(*http.Request) (canary.Identity, error) { return canary.MissingIdentity(), nil }),
		decider,
		stable,
		&recordingEngine{},
		requestConfig,
	)
	invocation, err := executor.Begin(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil), ProtocolOpenAI)
	if err != nil {
		t.Fatal(err)
	}
	_, err = invocation.Complete(context.Background(), engine.Request{})
	var engineError *engine.Error
	if !errors.As(err, &engineError) || engineError.Kind != engine.ErrorTimeout {
		t.Fatalf("上游 deadline 未映射为 timeout: %T %v", err, err)
	}
}
