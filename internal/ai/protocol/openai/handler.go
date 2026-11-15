package openai

import (
	"context"
	"net/http"
	"time"

	"github.com/nexthopecoming/next-gateway/internal/ai"
	"github.com/nexthopecoming/next-gateway/internal/ai/protocol"
	"github.com/nexthopecoming/next-gateway/internal/core/engine"
	"github.com/nexthopecoming/next-gateway/internal/core/httpserver"
)

// Handler 只负责 OpenAI 协议；身份、灰度和执行链复用根 ai Executor。
type Handler struct {
	executor *ai.Executor
	now      func() time.Time
}

// NewHandler 创建协议 Handler；now 可注入固定时钟以稳定黄金测试。
func NewHandler(executor *ai.Executor, now func() time.Time) *Handler {
	if now == nil {
		now = time.Now
	}
	return &Handler{executor: executor, now: now}
}

// ServeHTTP 严格按解析、单次灰度决策、固定 Engine、协议编码的顺序处理请求。
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	httpserver.UpdateRequestMetadata(r.Context(), func(metadata *httpserver.RequestMetadata) {
		metadata.Protocol = ai.ProtocolOpenAI
	})
	var input chatRequest
	if err := protocol.DecodeRequest(w, r, h.executor.RequestBodyLimit(), &input); err != nil {
		writeError(w, err)
		return
	}
	request, requestErr := input.toEngine()
	if requestErr != nil {
		writeError(w, requestErr)
		return
	}
	invocation, err := h.executor.Begin(w, r, ai.ProtocolOpenAI)
	if err != nil {
		writeError(w, protocol.InvalidIdentity())
		return
	}
	httpserver.UpdateRequestMetadata(r.Context(), func(metadata *httpserver.RequestMetadata) {
		metadata.Stream = request.Stream
	})

	requestID := httpserver.RequestID(r.Context())
	id := "chatcmpl_mock_" + requestID
	created := h.now().Unix()
	if !request.Stream {
		response, err := invocation.Complete(r.Context(), request)
		if err != nil {
			writeError(w, protocol.UpstreamError(err))
			return
		}
		recordUsage(r, response.Usage)
		protocol.WriteJSON(w, http.StatusOK, newCompletion(id, created, request.Model, response))
		return
	}

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	events, err := invocation.Stream(ctx, request)
	if err != nil {
		writeError(w, protocol.UpstreamError(err))
		return
	}
	outcome := protocol.ServeSSE(ctx, w, events, invocation.RequestConfig(), newStreamEncoder(id, created, request.Model))
	httpserver.UpdateRequestMetadata(r.Context(), func(metadata *httpserver.RequestMetadata) {
		metadata.StreamResult = outcome.Result
		if outcome.UsageSet {
			metadata.UsageSet = true
			metadata.InputTokens = outcome.Usage.InputTokens
			metadata.OutputTokens = outcome.Usage.OutputTokens
		}
	})
}

func recordUsage(r *http.Request, usage engine.Usage) {
	httpserver.UpdateRequestMetadata(r.Context(), func(metadata *httpserver.RequestMetadata) {
		metadata.UsageSet = true
		metadata.InputTokens = usage.InputTokens
		metadata.OutputTokens = usage.OutputTokens
	})
}
