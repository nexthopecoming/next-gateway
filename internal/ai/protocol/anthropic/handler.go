package anthropic

import (
	"context"
	"net/http"

	"github.com/nexthopecoming/next-gateway/internal/ai"
	"github.com/nexthopecoming/next-gateway/internal/ai/protocol"
	"github.com/nexthopecoming/next-gateway/internal/core/engine"
	"github.com/nexthopecoming/next-gateway/internal/core/httpserver"
)

// Handler 只负责 Anthropic 协议编排，不复制灰度、上游或 SSE 状态机。
type Handler struct {
	executor *ai.Executor
}

// NewHandler 创建 Anthropic 协议 Handler。
func NewHandler(executor *ai.Executor) *Handler {
	return &Handler{executor: executor}
}

// ServeHTTP 校验 anthropic-version 和严格文本子集，再固定一次 Engine 决策。
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	httpserver.UpdateRequestMetadata(r.Context(), func(metadata *httpserver.RequestMetadata) {
		metadata.Protocol = ai.ProtocolAnthropic
	})
	if values := r.Header.Values("anthropic-version"); len(values) > 0 && (len(values) != 1 || values[0] != "2023-06-01") {
		writeError(w, protocol.InvalidField("anthropic-version"))
		return
	}
	var input messagesRequest
	if err := protocol.DecodeRequest(w, r, h.executor.RequestBodyLimit(), &input); err != nil {
		writeError(w, err)
		return
	}
	request, requestErr := input.toEngine()
	if requestErr != nil {
		writeError(w, requestErr)
		return
	}
	invocation, err := h.executor.Begin(w, r, ai.ProtocolAnthropic)
	if err != nil {
		writeError(w, protocol.InvalidIdentity())
		return
	}
	httpserver.UpdateRequestMetadata(r.Context(), func(metadata *httpserver.RequestMetadata) {
		metadata.Stream = request.Stream
	})

	id := "msg_mock_" + httpserver.RequestID(r.Context())
	if !request.Stream {
		response, err := invocation.Complete(r.Context(), request)
		if err != nil {
			writeError(w, protocol.UpstreamError(err))
			return
		}
		recordUsage(r, response.Usage)
		protocol.WriteJSON(w, http.StatusOK, newMessage(id, request.Model, response))
		return
	}

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	events, err := invocation.Stream(ctx, request)
	if err != nil {
		writeError(w, protocol.UpstreamError(err))
		return
	}
	// message_start 的输入 usage 与最终完成事件一致；先计算输入即可避免等待输出。
	inputTokens := inputCodePoints(request)
	outcome := protocol.ServeSSE(ctx, w, events, invocation.RequestConfig(), newStreamEncoder(id, request.Model, inputTokens))
	httpserver.UpdateRequestMetadata(r.Context(), func(metadata *httpserver.RequestMetadata) {
		metadata.StreamResult = outcome.Result
		if outcome.UsageSet {
			metadata.UsageSet = true
			metadata.InputTokens = outcome.Usage.InputTokens
			metadata.OutputTokens = outcome.Usage.OutputTokens
		}
	})
}

func inputCodePoints(request engine.Request) int {
	total := len([]rune(request.System))
	for _, message := range request.Messages {
		total += len([]rune(message.Content))
	}
	return total
}

func recordUsage(r *http.Request, usage engine.Usage) {
	httpserver.UpdateRequestMetadata(r.Context(), func(metadata *httpserver.RequestMetadata) {
		metadata.UsageSet = true
		metadata.InputTokens = usage.InputTokens
		metadata.OutputTokens = usage.OutputTokens
	})
}
