package httpserver

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

func TestHealthAndRequestID(t *testing.T) {
	var ready atomic.Bool
	handler := NewHandler(&ready, zap.NewNop())
	request := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	request.Header.Set(requestIDHeader, "req_test-001")
	response := httptest.NewRecorder()

	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d", response.Code)
	}
	if got := response.Header().Get(requestIDHeader); got != "req_test-001" {
		t.Fatalf("request_id = %q", got)
	}
	var body map[string]string
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body["status"] != "ok" {
		t.Fatalf("body = %v", body)
	}
}

func TestInvalidRequestIDIsReplaced(t *testing.T) {
	var ready atomic.Bool
	handler := NewHandler(&ready, zap.NewNop())
	request := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	request.Header.Set(requestIDHeader, "invalid request id")
	response := httptest.NewRecorder()

	handler.ServeHTTP(response, request)
	got := response.Header().Get(requestIDHeader)
	if !validRequestID.MatchString(got) || got == "invalid request id" {
		t.Fatalf("非法请求 ID 未被替换: %q", got)
	}
}

func TestReadiness(t *testing.T) {
	var ready atomic.Bool
	handler := NewHandler(&ready, zap.NewNop())

	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("未就绪 status = %d", response.Code)
	}

	ready.Store(true)
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("已就绪 status = %d", response.Code)
	}
}

// TestAccessLog 固化访问日志字段，并确保未匹配路由不会把原始 URL 写入 route。
func TestAccessLog(t *testing.T) {
	tests := []struct {
		name       string
		path       string
		wantRoute  string
		wantStatus int
	}{
		{name: "已匹配路由", path: "/healthz", wantRoute: "GET /healthz", wantStatus: http.StatusOK},
		{name: "未匹配路由", path: "/missing", wantRoute: "unmatched", wantStatus: http.StatusNotFound},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			logger, logOutput := newJSONTestLogger()
			var ready atomic.Bool
			handler := NewHandler(&ready, logger)
			request := httptest.NewRequest(http.MethodGet, test.path, nil)
			request.Header.Set(requestIDHeader, "req_access-log")
			const secret = "access-log-test-secret"
			request.Header.Set("Authorization", "Bearer "+secret)
			response := httptest.NewRecorder()

			handler.ServeHTTP(response, request)
			if response.Code != test.wantStatus {
				t.Fatalf("status = %d，期望 %d", response.Code, test.wantStatus)
			}
			var fields map[string]any
			if err := json.Unmarshal(logOutput.Bytes(), &fields); err != nil {
				t.Fatalf("访问日志不是单个 JSON 对象: %v\n%s", err, logOutput.String())
			}
			if got := fields["msg"]; got != "HTTP 请求完成" {
				t.Errorf("msg = %#v", got)
			}
			for key, want := range map[string]any{
				"request_id": "req_access-log",
				"method":     http.MethodGet,
				"route":      test.wantRoute,
				"status":     float64(test.wantStatus),
			} {
				if got := fields[key]; got != want {
					t.Errorf("%s = %#v，期望 %#v", key, got, want)
				}
			}
			latency, ok := fields["latency_ms"].(float64)
			if !ok || latency < 0 {
				t.Errorf("latency_ms = %#v，期望非负整数", fields["latency_ms"])
			}
			// 检查完整序列化日志，防止 Secret 被改放进 headers 或其他嵌套字段。
			if strings.Contains(logOutput.String(), secret) {
				t.Error("访问日志包含 Authorization Secret")
			}
		})
	}
}

// TestRecovery 验证 panic 不泄露值，并区分响应提交前后的错误处理语义。
func TestRecovery(t *testing.T) {
	tests := []struct {
		name           string
		commitResponse bool
		wantStatus     int
		wantBody       string
	}{
		{
			name:       "响应开始前",
			wantStatus: http.StatusInternalServerError,
			wantBody:   "{\"error\":\"internal_error\"}\n",
		},
		{
			name:           "响应开始后",
			commitResponse: true,
			wantStatus:     http.StatusCreated,
			wantBody:       "partial",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			logger, logOutput := newJSONTestLogger()
			const secret = "recovery-test-secret"
			var handler http.Handler = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if test.commitResponse {
					w.WriteHeader(http.StatusCreated)
					_, _ = w.Write([]byte("partial"))
				}
				panic(secret)
			})
			handler = withRecovery(logger)(handler)
			handler = withAccessLog(logger)(handler)
			handler = withRequestID(logger)(handler)

			response := httptest.NewRecorder()
			handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/panic", nil))
			if response.Code != test.wantStatus {
				t.Errorf("panic 后 status = %d，期望 %d", response.Code, test.wantStatus)
			}
			if got := response.Body.String(); got != test.wantBody {
				t.Errorf("panic 后 body = %q，期望 %q", got, test.wantBody)
			}
			requestID := response.Header().Get(requestIDHeader)
			if requestID == "" {
				t.Fatal("panic 响应缺少 request_id")
			}
			if strings.Contains(logOutput.String(), secret) {
				t.Error("recovery 或访问日志包含 panic Secret")
			}

			entries := decodeJSONLogs(t, logOutput.Bytes())
			if len(entries) != 2 {
				t.Fatalf("日志条数 = %d，期望 recovery 和访问日志各一条", len(entries))
			}
			messages := make(map[string]map[string]any, len(entries))
			for _, entry := range entries {
				if got := entry["request_id"]; got != requestID {
					t.Errorf("日志 request_id = %#v，期望 %q", got, requestID)
				}
				message, _ := entry["msg"].(string)
				messages[message] = entry
			}
			for _, message := range []string{"HTTP 处理发生 panic", "HTTP 请求完成"} {
				if messages[message] == nil {
					t.Errorf("缺少日志 %q", message)
				}
			}
			if got := messages["HTTP 请求完成"]["status"]; got != float64(test.wantStatus) {
				t.Errorf("访问日志 status = %#v，期望 %d", got, test.wantStatus)
			}
		})
	}
}

func TestStatusWriterAllowsInformationalResponses(t *testing.T) {
	base := &statusRecordingWriter{header: make(http.Header)}
	writer := &statusWriter{ResponseWriter: base, status: http.StatusOK}
	writer.WriteHeader(http.StatusEarlyHints)
	writer.WriteHeader(http.StatusOK)
	writer.WriteHeader(http.StatusInternalServerError)
	if len(base.statuses) != 2 || base.statuses[0] != http.StatusEarlyHints || base.statuses[1] != http.StatusOK {
		t.Fatalf("底层状态序列 = %v", base.statuses)
	}
	if writer.status != http.StatusOK {
		t.Fatalf("访问日志最终状态 = %d", writer.status)
	}
}

type statusRecordingWriter struct {
	header   http.Header
	statuses []int
}

func (w *statusRecordingWriter) Header() http.Header { return w.header }
func (w *statusRecordingWriter) WriteHeader(status int) {
	w.statuses = append(w.statuses, status)
}
func (w *statusRecordingWriter) Write(data []byte) (int, error) { return len(data), nil }

func TestRecoveryRepanicsErrAbortHandler(t *testing.T) {
	logger, logOutput := newJSONTestLogger()
	var handler http.Handler = http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic(http.ErrAbortHandler)
	})
	handler = withRecovery(logger)(handler)
	handler = withAccessLog(logger)(handler)
	handler = withRequestID(logger)(handler)

	var recovered any
	func() {
		defer func() { recovered = recover() }()
		handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/abort", nil))
	}()
	if recovered != http.ErrAbortHandler {
		t.Fatalf("recover = %#v，期望 ErrAbortHandler", recovered)
	}
	entries := decodeJSONLogs(t, logOutput.Bytes())
	if len(entries) != 1 || entries[0]["msg"] != "HTTP 请求完成" {
		t.Fatalf("ErrAbortHandler 不应记录应用 panic: %s", logOutput.String())
	}
}

// newJSONTestLogger 使用真实 JSON 编码器捕获日志，避免仅检查内存字段而遗漏嵌套泄露。
func newJSONTestLogger() (*zap.Logger, *bytes.Buffer) {
	var output bytes.Buffer
	core := zapcore.NewCore(
		zapcore.NewJSONEncoder(zap.NewProductionEncoderConfig()),
		zapcore.AddSync(&output),
		zap.DebugLevel,
	)
	return zap.New(core), &output
}

// decodeJSONLogs 解码逐行 JSON 日志，供多条 recovery/访问日志统一断言。
func decodeJSONLogs(t *testing.T, content []byte) []map[string]any {
	t.Helper()
	decoder := json.NewDecoder(bytes.NewReader(content))
	var entries []map[string]any
	for {
		var entry map[string]any
		err := decoder.Decode(&entry)
		if err == io.EOF {
			return entries
		}
		if err != nil {
			t.Fatalf("解析 JSON 日志: %v\n%s", err, content)
		}
		entries = append(entries, entry)
	}
}
