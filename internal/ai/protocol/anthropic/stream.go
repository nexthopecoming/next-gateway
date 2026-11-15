package anthropic

import (
	"github.com/nexthopecoming/next-gateway/internal/ai/protocol"
	"github.com/nexthopecoming/next-gateway/internal/core/engine"
)

type streamEncoder struct {
	id          string
	model       string
	inputTokens int
}

type messageStartEvent struct {
	Type    string       `json:"type"`
	Message startMessage `json:"message"`
}

type startMessage struct {
	ID           string        `json:"id"`
	Type         string        `json:"type"`
	Role         string        `json:"role"`
	Model        string        `json:"model"`
	Content      []textContent `json:"content"`
	StopReason   *string       `json:"stop_reason"`
	StopSequence *string       `json:"stop_sequence"`
	Usage        messageUsage  `json:"usage"`
}

type indexedEvent struct {
	Type         string       `json:"type"`
	Index        int          `json:"index"`
	ContentBlock *textContent `json:"content_block,omitempty"`
	Delta        *textDelta   `json:"delta,omitempty"`
}

type textDelta struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type messageDeltaEvent struct {
	Type  string       `json:"type"`
	Delta messageDelta `json:"delta"`
	Usage deltaUsage   `json:"usage"`
}

type messageDelta struct {
	StopReason   string  `json:"stop_reason"`
	StopSequence *string `json:"stop_sequence"`
}

type deltaUsage struct {
	OutputTokens int `json:"output_tokens"`
}

func newStreamEncoder(id, model string, inputTokens int) *streamEncoder {
	return &streamEncoder{id: id, model: model, inputTokens: inputTokens}
}

// Start 先发送 message_start，再打开唯一的文本 content block。
func (e *streamEncoder) Start() []protocol.StreamEvent {
	start := messageStartEvent{
		Type: "message_start",
		Message: startMessage{
			ID: e.id, Type: "message", Role: "assistant", Model: e.model,
			Content: []textContent{}, Usage: messageUsage{InputTokens: e.inputTokens, OutputTokens: 0},
		},
	}
	block := textContent{Type: "text", Text: ""}
	return []protocol.StreamEvent{
		protocol.JSONEvent("message_start", start),
		protocol.JSONEvent("content_block_start", indexedEvent{Type: "content_block_start", Index: 0, ContentBlock: &block}),
	}
}

func (e *streamEncoder) Content(content string) protocol.StreamEvent {
	delta := textDelta{Type: "text_delta", Text: content}
	return protocol.JSONEvent("content_block_delta", indexedEvent{Type: "content_block_delta", Index: 0, Delta: &delta})
}

// Finish 关闭 block，发送最终停止原因和 usage，最后发送 message_stop。
func (e *streamEncoder) Finish(response engine.Response) []protocol.StreamEvent {
	return []protocol.StreamEvent{
		protocol.JSONEvent("content_block_stop", indexedEvent{Type: "content_block_stop", Index: 0}),
		protocol.JSONEvent("message_delta", messageDeltaEvent{
			Type:  "message_delta",
			Delta: messageDelta{StopReason: stopReason(response.FinishReason), StopSequence: stopSequence(response)},
			Usage: deltaUsage{OutputTokens: response.Usage.OutputTokens},
		}),
		protocol.JSONEvent("message_stop", struct {
			Type string `json:"type"`
		}{Type: "message_stop"}),
	}
}

func (e *streamEncoder) Error(err *protocol.Error) []protocol.StreamEvent {
	return []protocol.StreamEvent{protocol.JSONEvent("error", errorEnvelope{
		Type: "error", Error: errorBody{Type: err.Type, Message: err.Message},
	})}
}
