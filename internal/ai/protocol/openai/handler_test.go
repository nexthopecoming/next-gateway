package openai_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"

	"github.com/nexthopecoming/next-gateway/internal/ai"
	aimock "github.com/nexthopecoming/next-gateway/internal/ai/mock"
	"github.com/nexthopecoming/next-gateway/internal/ai/protocol/openai"
	"github.com/nexthopecoming/next-gateway/internal/core/canary"
	"github.com/nexthopecoming/next-gateway/internal/core/config"
	"github.com/nexthopecoming/next-gateway/internal/core/engine"
	"github.com/nexthopecoming/next-gateway/internal/core/httpserver"
)

func TestCompletionGoldenContractAcrossCores(t *testing.T) {
	for _, test := range []struct {
		name        string
		basisPoints int
		core        string
	}{
		{name: "stable", basisPoints: 0, core: "stable"},
		{name: "canary", basisPoints: 10000, core: "canary"},
	} {
		t.Run(test.name, func(t *testing.T) {
			cfg := config.Default()
			cfg.Canary.CanaryBasisPoints = test.basisPoints
			cfg.AI.Mock = config.MockConfig{ResponseText: "Hello", ChunkSize: 5}
			server, logs := newOpenAIServer(t, cfg, nil)
			response := openAIRequest(t, server, `{"model":"mock-model","messages":[{"role":"user","content":"Hello"}]}`)
			want := "{\"id\":\"chatcmpl_mock_req_001\",\"object\":\"chat.completion\",\"created\":1767225600,\"model\":\"mock-model\",\"choices\":[{\"index\":0,\"message\":{\"role\":\"assistant\",\"content\":\"Hello\"},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":5,\"completion_tokens\":5,\"total_tokens\":10}}\n"
			if response.status != http.StatusOK || response.body != want {
				t.Fatalf("响应不匹配:\nstatus=%d\nbody=%s\nwant=%s", response.status, response.body, want)
			}
			if response.header.Get("X-Gateway-Core") != test.core || response.header.Get("X-Request-ID") != "req_001" {
				t.Fatalf("响应 Header = %v", response.header)
			}
			assertNoSensitiveLog(t, logs.String())
			for _, field := range []string{`"core":"` + test.core + `"`, `"input_tokens":5`, `"output_tokens":5`, `"config_version":"local-file"`} {
				if !strings.Contains(logs.String(), field) {
					t.Errorf("访问日志缺少 %s: %s", field, logs.String())
				}
			}
		})
	}
}

func TestCompletionSSEGoldenContract(t *testing.T) {
	cfg := config.Default()
	cfg.AI.Mock = config.MockConfig{ResponseText: "Hello", ChunkSize: 5}
	server, logs := newOpenAIServer(t, cfg, nil)
	response := openAIRequest(t, server, `{"model":"mock-model","messages":[{"role":"user","content":"Hello"}],"stream":true}`)
	want := "" +
		"data: {\"id\":\"chatcmpl_mock_req_001\",\"object\":\"chat.completion.chunk\",\"created\":1767225600,\"model\":\"mock-model\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"\"},\"finish_reason\":null}]}\n\n" +
		"data: {\"id\":\"chatcmpl_mock_req_001\",\"object\":\"chat.completion.chunk\",\"created\":1767225600,\"model\":\"mock-model\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"Hello\"},\"finish_reason\":null}]}\n\n" +
		"data: {\"id\":\"chatcmpl_mock_req_001\",\"object\":\"chat.completion.chunk\",\"created\":1767225600,\"model\":\"mock-model\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n" +
		"data: [DONE]\n\n"
	if response.status != http.StatusOK || response.body != want {
		t.Fatalf("SSE 不匹配:\nstatus=%d\nbody:\n%s\nwant:\n%s", response.status, response.body, want)
	}
	if got := response.header.Get("Content-Type"); got != "text/event-stream; charset=utf-8" {
		t.Fatalf("Content-Type = %q", got)
	}
	if !strings.Contains(logs.String(), `"stream_result":"success"`) {
		t.Fatalf("缺少流式成功日志: %s", logs.String())
	}
	assertNoSensitiveLog(t, logs.String())
}

func TestCompletionStopAndMaxTokens(t *testing.T) {
	for _, test := range []struct {
		name       string
		text       string
		request    string
		wantText   string
		wantReason string
	}{
		{name: "max tokens", text: "Hello", request: `,"max_tokens":2`, wantText: "He", wantReason: "length"},
		{name: "stop string", text: "Hello", request: `,"stop":"ll"`, wantText: "He", wantReason: "stop"},
		{name: "stop list", text: "Hello", request: `,"stop":["xyz","llo"]`, wantText: "He", wantReason: "stop"},
	} {
		t.Run(test.name, func(t *testing.T) {
			cfg := config.Default()
			cfg.AI.Mock = config.MockConfig{ResponseText: test.text, ChunkSize: 2}
			server, _ := newOpenAIServer(t, cfg, nil)
			body := `{"model":"mock-model","messages":[{"role":"user","content":"Hello"}]` + test.request + `}`
			response := openAIRequest(t, server, body)
			if response.status != http.StatusOK || !strings.Contains(response.body, `"content":"`+test.wantText+`"`) || !strings.Contains(response.body, `"finish_reason":"`+test.wantReason+`"`) {
				t.Fatalf("截断响应 = %d %s", response.status, response.body)
			}
		})
	}
}

func TestCompletionStrictValidation(t *testing.T) {
	tests := []struct {
		name       string
		body       string
		wantStatus int
	}{
		{name: "unknown tools", body: `{"model":"m","messages":[{"role":"user","content":"x"}],"tools":[]}`, wantStatus: 400},
		{name: "unknown n", body: `{"model":"m","messages":[{"role":"user","content":"x"}],"n":2}`, wantStatus: 400},
		{name: "multimodal", body: `{"model":"m","messages":[{"role":"user","content":[{"type":"text","text":"x"}]}]}`, wantStatus: 400},
		{name: "missing model", body: `{"messages":[{"role":"user","content":"x"}]}`, wantStatus: 400},
		{name: "empty messages", body: `{"model":"m","messages":[]}`, wantStatus: 400},
		{name: "bad role", body: `{"model":"m","messages":[{"role":"tool","content":"x"}]}`, wantStatus: 400},
		{name: "null content", body: `{"model":"m","messages":[{"role":"user","content":null}]}`, wantStatus: 400},
		{name: "zero max tokens", body: `{"model":"m","messages":[{"role":"user","content":"x"}],"max_tokens":0}`, wantStatus: 400},
		{name: "temperature", body: `{"model":"m","messages":[{"role":"user","content":"x"}],"temperature":2.1}`, wantStatus: 400},
		{name: "top p", body: `{"model":"m","messages":[{"role":"user","content":"x"}],"top_p":-0.1}`, wantStatus: 400},
		{name: "too many stops", body: `{"model":"m","messages":[{"role":"user","content":"x"}],"stop":["a","b","c","d","e"]}`, wantStatus: 400},
		{name: "empty stop", body: `{"model":"m","messages":[{"role":"user","content":"x"}],"stop":""}`, wantStatus: 400},
		{name: "null optional", body: `{"model":"m","messages":[{"role":"user","content":"x"}],"top_p":null}`, wantStatus: 400},
		{name: "trailing JSON", body: `{"model":"m","messages":[{"role":"user","content":"x"}]} {}`, wantStatus: 400},
		{name: "array top level", body: `[]`, wantStatus: 400},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server, _ := newOpenAIServer(t, config.Default(), nil)
			response := openAIRequest(t, server, test.body)
			if response.status != test.wantStatus {
				t.Fatalf("status=%d body=%s", response.status, response.body)
			}
			if strings.Contains(test.body, "tools") && response.body != "{\"error\":{\"message\":\"unsupported field: tools\",\"type\":\"invalid_request_error\",\"param\":\"tools\",\"code\":\"unsupported_field\"}}\n" {
				t.Fatalf("未知字段错误不符合黄金契约: %s", response.body)
			}
		})
	}
}

func TestCompletionMediaTypeAndBodyLimit(t *testing.T) {
	server, _ := newOpenAIServer(t, config.Default(), nil)
	request, _ := http.NewRequest(http.MethodPost, server.URL+"/v1/chat/completions", strings.NewReader(`{}`))
	request.Header.Set("Content-Type", "text/plain")
	response, err := server.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusUnsupportedMediaType {
		t.Fatalf("媒体类型 status=%d", response.StatusCode)
	}

	cfg := config.Default()
	cfg.Request.MaxBodyBytes = 16
	limited, _ := newOpenAIServer(t, cfg, nil)
	result := openAIRequest(t, limited, `{"model":"mock-model","messages":[]}`)
	if result.status != http.StatusRequestEntityTooLarge || !strings.Contains(result.body, `"code":"request_too_large"`) {
		t.Fatalf("请求体限制响应 = %d %s", result.status, result.body)
	}
}

func TestCompletionUpstreamErrorsAreSafe(t *testing.T) {
	upstream := failingUpstream{err: errors.New("provider-secret-must-not-escape")}
	server, logs := newOpenAIServer(t, config.Default(), upstream)
	nonStream := openAIRequest(t, server, `{"model":"m","messages":[{"role":"user","content":"secret prompt"}]}`)
	if nonStream.status != http.StatusBadGateway || !strings.Contains(nonStream.body, `"message":"mock upstream failed"`) {
		t.Fatalf("非流式错误 = %d %s", nonStream.status, nonStream.body)
	}
	stream := openAIRequest(t, server, `{"model":"m","messages":[{"role":"user","content":"secret prompt"}],"stream":true}`)
	if stream.status != http.StatusOK || !strings.Contains(stream.body, `data: {"error":{"message":"mock upstream failed","type":"api_error","param":null,"code":"upstream_error"}}`) || !strings.HasSuffix(stream.body, "data: [DONE]\n\n") {
		t.Fatalf("流式错误 = %d %s", stream.status, stream.body)
	}
	if strings.Contains(stream.body, `"finish_reason":"stop"`) || strings.Contains(logs.String()+nonStream.body+stream.body, "provider-secret") {
		t.Fatalf("流内错误或日志不安全: %s %s", stream.body, logs.String())
	}
	if !strings.Contains(logs.String(), `"stream_result":"upstream_error"`) {
		t.Fatalf("缺少流内错误日志: %s", logs.String())
	}
}

// protocolIdentityExtractor 只提供协议测试所需身份；真实 Header 校验由 biz 单测和进程测试覆盖。
type protocolIdentityExtractor struct{}

func (protocolIdentityExtractor) Extract(request *http.Request) (canary.Identity, error) {
	if request.Header.Get("Authorization") == "" && request.Header.Get("X-Tenant-ID") == "" && request.Header.Get("x-api-key") == "" {
		return canary.MissingIdentity(), nil
	}
	return canary.NewIdentity(canary.IdentityAPIKey, "protocol-test-identity"), nil
}

type failingUpstream struct{ err error }

func (f failingUpstream) Complete(context.Context, engine.Request) (engine.Response, error) {
	return engine.Response{}, f.err
}
func (f failingUpstream) Stream(context.Context, engine.Request) (<-chan engine.Event, error) {
	events := make(chan engine.Event, 1)
	events <- engine.Event{Kind: engine.EventError, Err: f.err}
	close(events)
	return events, nil
}

type openAIResponse struct {
	status int
	header http.Header
	body   string
}

func newOpenAIServer(t *testing.T, cfg config.Config, upstream engine.Upstream) (*httptest.Server, *bytes.Buffer) {
	t.Helper()
	root, logs := newOpenAIRoot(t, cfg, upstream)
	server := httptest.NewServer(root)
	t.Cleanup(server.Close)
	return server, logs
}

// newOpenAIRoot 让黄金测试和真实 Server 生命周期测试共享完全相同的中间件与协议装配。
func newOpenAIRoot(t *testing.T, cfg config.Config, upstream engine.Upstream) (http.Handler, *bytes.Buffer) {
	t.Helper()
	if upstream == nil {
		upstream = aimock.New(cfg.AI.Mock)
	}
	decider, err := canary.NewDecider(cfg.Canary.CanaryBasisPoints, cfg.Canary.ForceStableTenants, cfg.Canary.ForceCanaryTenants, cfg.Canary.AllowOverrideHeader)
	if err != nil {
		t.Fatal(err)
	}
	executor := ai.NewExecutor(protocolIdentityExtractor{}, decider, engine.NewStable(upstream), engine.NewCanary(upstream, engine.Hooks{}), cfg.Request)
	handler := openai.NewHandler(executor, func() time.Time { return time.Unix(1767225600, 0) })
	var ready atomic.Bool
	ready.Store(true)
	var logs bytes.Buffer
	logger := zap.New(zapcore.NewCore(zapcore.NewJSONEncoder(zap.NewProductionEncoderConfig()), zapcore.AddSync(&logs), zap.DebugLevel))
	root := httpserver.NewHandler(&ready, logger, func(mux *http.ServeMux) {
		mux.Handle("POST /v1/chat/completions", handler)
	})
	return root, &logs
}

func openAIRequest(t *testing.T, server *httptest.Server, body string) openAIResponse {
	t.Helper()
	request, err := http.NewRequest(http.MethodPost, server.URL+"/v1/chat/completions", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Request-ID", "req_001")
	request.Header.Set("Authorization", "Bearer sk-log-secret")
	response, err := server.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	content, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	return openAIResponse{status: response.StatusCode, header: response.Header.Clone(), body: string(content)}
}

func assertNoSensitiveLog(t *testing.T, logs string) {
	t.Helper()
	for _, forbidden := range []string{"sk-log-secret", "mock-model", "Hello", "secret prompt"} {
		if strings.Contains(logs, forbidden) {
			t.Errorf("日志包含敏感或无界内容 %q: %s", forbidden, logs)
		}
	}
}
