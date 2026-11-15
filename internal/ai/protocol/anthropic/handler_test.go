package anthropic_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"

	"github.com/nexthopecoming/next-gateway/internal/ai"
	aimock "github.com/nexthopecoming/next-gateway/internal/ai/mock"
	"github.com/nexthopecoming/next-gateway/internal/ai/protocol/anthropic"
	"github.com/nexthopecoming/next-gateway/internal/core/canary"
	"github.com/nexthopecoming/next-gateway/internal/core/config"
	"github.com/nexthopecoming/next-gateway/internal/core/engine"
	"github.com/nexthopecoming/next-gateway/internal/core/httpserver"
)

func TestMessageGoldenContractAcrossCores(t *testing.T) {
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
			server, logs := newAnthropicServer(t, cfg, nil)
			response := anthropicRequest(t, server, `{"model":"mock-model","max_tokens":10,"messages":[{"role":"user","content":"Hello"}]}`, "2023-06-01")
			want := "{\"id\":\"msg_mock_req_001\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"mock-model\",\"content\":[{\"type\":\"text\",\"text\":\"Hello\"}],\"stop_reason\":\"end_turn\",\"stop_sequence\":null,\"usage\":{\"input_tokens\":5,\"output_tokens\":5}}\n"
			if response.status != http.StatusOK || response.body != want {
				t.Fatalf("响应不匹配:\nstatus=%d\nbody=%s\nwant=%s", response.status, response.body, want)
			}
			if response.header.Get("X-Gateway-Core") != test.core || response.header.Get("X-Request-ID") != "req_001" {
				t.Fatalf("响应 Header = %v", response.header)
			}
			assertNoSensitiveAnthropicLog(t, logs.String())
		})
	}
}

func TestMessageSSEGoldenContract(t *testing.T) {
	cfg := config.Default()
	cfg.AI.Mock = config.MockConfig{ResponseText: "Hello", ChunkSize: 5}
	server, logs := newAnthropicServer(t, cfg, nil)
	response := anthropicRequest(t, server, `{"model":"mock-model","max_tokens":10,"messages":[{"role":"user","content":"Hello"}],"stream":true}`, "2023-06-01")
	want := "" +
		"event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_mock_req_001\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"mock-model\",\"content\":[],\"stop_reason\":null,\"stop_sequence\":null,\"usage\":{\"input_tokens\":5,\"output_tokens\":0}}}\n\n" +
		"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n" +
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"Hello\"}}\n\n" +
		"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n" +
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\",\"stop_sequence\":null},\"usage\":{\"output_tokens\":5}}\n\n" +
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
	if response.status != http.StatusOK || response.body != want {
		t.Fatalf("SSE 不匹配:\nstatus=%d\nbody:\n%s\nwant:\n%s", response.status, response.body, want)
	}
	if !strings.Contains(logs.String(), `"stream_result":"success"`) || !strings.Contains(logs.String(), `"output_tokens":5`) {
		t.Fatalf("缺少流式结果或 usage 日志: %s", logs.String())
	}
	assertNoSensitiveAnthropicLog(t, logs.String())
}

func TestMessageContentBlocksAndStopSemantics(t *testing.T) {
	for _, test := range []struct {
		name         string
		text         string
		maxTokens    int
		stops        string
		wantText     string
		wantReason   string
		wantSequence string
	}{
		{name: "max tokens", text: "Hello", maxTokens: 2, wantText: "He", wantReason: "max_tokens"},
		{name: "stop", text: "Hello", maxTokens: 10, stops: `,"stop_sequences":["ll"]`, wantText: "He", wantReason: "stop_sequence", wantSequence: `"stop_sequence":"ll"`},
	} {
		t.Run(test.name, func(t *testing.T) {
			cfg := config.Default()
			cfg.AI.Mock = config.MockConfig{ResponseText: test.text, ChunkSize: 2}
			server, _ := newAnthropicServer(t, cfg, nil)
			body := `{"model":"mock-model","max_tokens":` + strconv.Itoa(test.maxTokens) + `,"messages":[{"role":"user","content":[{"type":"text","text":"Hel"},{"type":"text","text":"lo"}]}]` + test.stops + `}`
			response := anthropicRequest(t, server, body, "")
			if response.status != http.StatusOK || !strings.Contains(response.body, `"text":"`+test.wantText+`"`) || !strings.Contains(response.body, `"stop_reason":"`+test.wantReason+`"`) {
				t.Fatalf("截断响应 = %d %s", response.status, response.body)
			}
			if test.wantSequence != "" && !strings.Contains(response.body, test.wantSequence) {
				t.Fatalf("缺少 stop_sequence: %s", response.body)
			}
			if !strings.Contains(response.body, `"input_tokens":5`) {
				t.Fatalf("content block usage 错误: %s", response.body)
			}
		})
	}
}

func TestMessageStrictValidation(t *testing.T) {
	tests := []struct {
		name    string
		body    string
		version string
	}{
		{name: "unknown tools", body: `{"model":"m","max_tokens":5,"messages":[{"role":"user","content":"x"}],"tools":[]}`},
		{name: "missing max tokens", body: `{"model":"m","messages":[{"role":"user","content":"x"}]}`},
		{name: "zero max tokens", body: `{"model":"m","max_tokens":0,"messages":[{"role":"user","content":"x"}]}`},
		{name: "bad role", body: `{"model":"m","max_tokens":5,"messages":[{"role":"system","content":"x"}]}`},
		{name: "null content", body: `{"model":"m","max_tokens":5,"messages":[{"role":"user","content":null}]}`},
		{name: "image block", body: `{"model":"m","max_tokens":5,"messages":[{"role":"user","content":[{"type":"image","source":{}}]}]}`},
		{name: "unknown block field", body: `{"model":"m","max_tokens":5,"messages":[{"role":"user","content":[{"type":"text","text":"x","extra":true}]}]}`},
		{name: "null system", body: `{"model":"m","max_tokens":5,"system":null,"messages":[{"role":"user","content":"x"}]}`},
		{name: "temperature", body: `{"model":"m","max_tokens":5,"temperature":1.1,"messages":[{"role":"user","content":"x"}]}`},
		{name: "top k", body: `{"model":"m","max_tokens":5,"top_k":0,"messages":[{"role":"user","content":"x"}]}`},
		{name: "too many stops", body: `{"model":"m","max_tokens":5,"stop_sequences":["a","b","c","d","e"],"messages":[{"role":"user","content":"x"}]}`},
		{name: "empty stop", body: `{"model":"m","max_tokens":5,"stop_sequences":[""],"messages":[{"role":"user","content":"x"}]}`},
		{name: "version", body: `{"model":"m","max_tokens":5,"messages":[{"role":"user","content":"x"}]}`, version: "2024-01-01"},
		{name: "trailing", body: `{"model":"m","max_tokens":5,"messages":[{"role":"user","content":"x"}]} true`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server, _ := newAnthropicServer(t, config.Default(), nil)
			response := anthropicRequest(t, server, test.body, test.version)
			if response.status != http.StatusBadRequest {
				t.Fatalf("status=%d body=%s", response.status, response.body)
			}
			if strings.Contains(test.body, `"tools"`) && response.body != "{\"type\":\"error\",\"error\":{\"type\":\"invalid_request_error\",\"message\":\"unsupported field: tools\"}}\n" {
				t.Fatalf("未知字段错误不符合黄金契约: %s", response.body)
			}
		})
	}
}

func TestMessageUpstreamErrorsAreSafe(t *testing.T) {
	upstream := anthropicFailingUpstream{err: errors.New("provider-secret-must-not-escape")}
	server, logs := newAnthropicServer(t, config.Default(), upstream)
	nonStream := anthropicRequest(t, server, `{"model":"m","max_tokens":5,"messages":[{"role":"user","content":"secret prompt"}]}`, "")
	if nonStream.status != http.StatusBadGateway || !strings.Contains(nonStream.body, `"message":"mock upstream failed"`) {
		t.Fatalf("非流式错误 = %d %s", nonStream.status, nonStream.body)
	}
	stream := anthropicRequest(t, server, `{"model":"m","max_tokens":5,"messages":[{"role":"user","content":"secret prompt"}],"stream":true}`, "")
	if stream.status != http.StatusOK || !strings.Contains(stream.body, "event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"api_error\",\"message\":\"mock upstream failed\"}}\n\n") || strings.Contains(stream.body, "message_stop") {
		t.Fatalf("流式错误 = %d %s", stream.status, stream.body)
	}
	if strings.Contains(logs.String()+nonStream.body+stream.body, "provider-secret") || !strings.Contains(logs.String(), `"stream_result":"upstream_error"`) {
		t.Fatalf("错误或日志不安全: %s", logs.String())
	}
}

// anthropicIdentityExtractor 隔离协议与身份实现，避免 ai 测试包反向依赖 biz。
type anthropicIdentityExtractor struct{}

func (anthropicIdentityExtractor) Extract(request *http.Request) (canary.Identity, error) {
	if request.Header.Get("Authorization") == "" && request.Header.Get("X-Tenant-ID") == "" && request.Header.Get("x-api-key") == "" {
		return canary.MissingIdentity(), nil
	}
	return canary.NewIdentity(canary.IdentityAPIKey, "protocol-test-identity"), nil
}

type anthropicFailingUpstream struct{ err error }

func (f anthropicFailingUpstream) Complete(context.Context, engine.Request) (engine.Response, error) {
	return engine.Response{}, f.err
}
func (f anthropicFailingUpstream) Stream(context.Context, engine.Request) (<-chan engine.Event, error) {
	events := make(chan engine.Event, 1)
	events <- engine.Event{Kind: engine.EventError, Err: f.err}
	close(events)
	return events, nil
}

type anthropicResponse struct {
	status int
	header http.Header
	body   string
}

func newAnthropicServer(t *testing.T, cfg config.Config, upstream engine.Upstream) (*httptest.Server, *bytes.Buffer) {
	t.Helper()
	if upstream == nil {
		upstream = aimock.New(cfg.AI.Mock)
	}
	decider, err := canary.NewDecider(cfg.Canary.CanaryBasisPoints, cfg.Canary.ForceStableTenants, cfg.Canary.ForceCanaryTenants, cfg.Canary.AllowOverrideHeader)
	if err != nil {
		t.Fatal(err)
	}
	executor := ai.NewExecutor(anthropicIdentityExtractor{}, decider, engine.NewStable(upstream), engine.NewCanary(upstream, engine.Hooks{}), cfg.Request)
	handler := anthropic.NewHandler(executor)
	var ready atomic.Bool
	ready.Store(true)
	var logs bytes.Buffer
	logger := zap.New(zapcore.NewCore(zapcore.NewJSONEncoder(zap.NewProductionEncoderConfig()), zapcore.AddSync(&logs), zap.DebugLevel))
	root := httpserver.NewHandler(&ready, logger, func(mux *http.ServeMux) {
		mux.Handle("POST /v1/messages", handler)
	})
	server := httptest.NewServer(root)
	t.Cleanup(server.Close)
	return server, &logs
}

func anthropicRequest(t *testing.T, server *httptest.Server, body, version string) anthropicResponse {
	t.Helper()
	request, err := http.NewRequest(http.MethodPost, server.URL+"/v1/messages", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Request-ID", "req_001")
	request.Header.Set("Authorization", "Bearer sk-log-secret")
	if version != "" {
		request.Header.Set("anthropic-version", version)
	}
	response, err := server.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	content, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	return anthropicResponse{status: response.StatusCode, header: response.Header.Clone(), body: string(content)}
}

func assertNoSensitiveAnthropicLog(t *testing.T, logs string) {
	t.Helper()
	for _, forbidden := range []string{"sk-log-secret", "mock-model", "Hello", "secret prompt"} {
		if strings.Contains(logs, forbidden) {
			t.Errorf("日志包含敏感或无界内容 %q: %s", forbidden, logs)
		}
	}
}
