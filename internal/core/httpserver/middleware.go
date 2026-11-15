package httpserver

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"regexp"
	"time"

	"go.uber.org/zap"
)

const requestIDHeader = "X-Request-ID"

var validRequestID = regexp.MustCompile(`^[A-Za-z0-9._-]{1,128}$`)

type requestIDContextKey struct{}

type requestMetadataContextKey struct{}

// RequestMetadata 是访问日志允许写入的低基数请求元数据，禁止放入身份原文、模型或正文。
type RequestMetadata struct {
	Protocol       string
	Core           string
	DecisionReason string
	ConfigVersion  string
	ConfigRevision int64
	Upstream       string
	Stream         bool
	StreamResult   string
	InputTokens    int
	OutputTokens   int
	UsageSet       bool
}

// UpdateRequestMetadata 在 Handler goroutine 内补充观测字段，访问中间件在请求结束后统一读取。
func UpdateRequestMetadata(ctx context.Context, update func(*RequestMetadata)) {
	metadata, _ := ctx.Value(requestMetadataContextKey{}).(*RequestMetadata)
	if metadata != nil {
		update(metadata)
	}
}

type middleware func(http.Handler) http.Handler

// RequestID 返回中间件写入上下文的请求标识，供后续 biz/ai 链路复用。
func RequestID(ctx context.Context) string {
	value, _ := ctx.Value(requestIDContextKey{}).(string)
	return value
}

func withRequestID(logger *zap.Logger) middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			requestID := r.Header.Get(requestIDHeader)
			if !validRequestID.MatchString(requestID) {
				var err error
				requestID, err = generateRequestID()
				if err != nil {
					logger.Error("生成请求 ID 失败", zap.Error(err))
					writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal_error"})
					return
				}
			}
			w.Header().Set(requestIDHeader, requestID)
			ctx := context.WithValue(r.Context(), requestIDContextKey{}, requestID)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

func generateRequestID() (string, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", err
	}
	return "req_" + hex.EncodeToString(value[:]), nil
}

func withAccessLog(logger *zap.Logger) middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			started := time.Now()
			writer := &statusWriter{ResponseWriter: w, status: http.StatusOK}
			metadata := &RequestMetadata{}
			r = r.WithContext(context.WithValue(r.Context(), requestMetadataContextKey{}, metadata))
			defer func() {
				route := r.Pattern
				if route == "" {
					route = "unmatched"
				}
				fields := []zap.Field{
					zap.String("request_id", RequestID(r.Context())),
					zap.String("method", r.Method),
					zap.String("route", route),
					zap.Int("status", writer.status),
					zap.Int64("latency_ms", time.Since(started).Milliseconds()),
				}
				if metadata.Protocol != "" {
					fields = append(fields,
						zap.String("protocol", metadata.Protocol),
						zap.String("core", metadata.Core),
						zap.String("decision_reason", metadata.DecisionReason),
						zap.String("config_version", metadata.ConfigVersion),
						zap.Int64("config_revision", metadata.ConfigRevision),
						zap.String("upstream", metadata.Upstream),
						zap.Bool("stream", metadata.Stream),
					)
				}
				if metadata.StreamResult != "" {
					fields = append(fields, zap.String("stream_result", metadata.StreamResult))
				}
				if metadata.UsageSet {
					fields = append(fields,
						zap.Int("input_tokens", metadata.InputTokens),
						zap.Int("output_tokens", metadata.OutputTokens),
					)
				}
				logger.Info("HTTP 请求完成", fields...)
			}()
			next.ServeHTTP(writer, r)
		})
	}
}

func withRecovery(logger *zap.Logger) middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				recovered := recover()
				if recovered == nil {
					return
				}
				// ErrAbortHandler 表示 net/http 预期的连接中止，必须继续抛出且不能记录成应用 panic。
				if recovered == http.ErrAbortHandler {
					panic(http.ErrAbortHandler)
				}
				// 不记录 panic 值，避免其中意外包含请求正文或凭据；堆栈仍用于定位代码路径。
				logger.Error("HTTP 处理发生 panic",
					zap.String("request_id", RequestID(r.Context())),
					zap.Stack("stack"),
				)
				if writer, ok := w.(*statusWriter); !ok || !writer.wroteHeader {
					writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal_error"})
				}
			}()
			next.ServeHTTP(w, r)
		})
	}
}

type statusWriter struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
}

func (w *statusWriter) WriteHeader(status int) {
	// 1xx 是临时响应，可写多次；只有最终的 2xx..5xx 才进入访问日志并锁定状态。
	if status >= 100 && status < 200 {
		w.ResponseWriter.WriteHeader(status)
		return
	}
	if w.wroteHeader {
		return
	}
	w.status = status
	w.wroteHeader = true
	w.ResponseWriter.WriteHeader(status)
}

func (w *statusWriter) Write(data []byte) (int, error) {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	return w.ResponseWriter.Write(data)
}

// Unwrap 让 http.ResponseController 在后续 SSE 阶段仍能发现底层 Flusher 和写期限能力。
func (w *statusWriter) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}
