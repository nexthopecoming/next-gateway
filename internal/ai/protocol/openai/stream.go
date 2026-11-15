package openai

import (
	"github.com/nexthopecoming/next-gateway/internal/ai/protocol"
	"github.com/nexthopecoming/next-gateway/internal/core/engine"
)

type streamEncoder struct {
	id      string
	created int64
	model   string
}

type completionChunk struct {
	ID      string         `json:"id"`
	Object  string         `json:"object"`
	Created int64          `json:"created"`
	Model   string         `json:"model"`
	Choices []streamChoice `json:"choices"`
}

type streamChoice struct {
	Index        int         `json:"index"`
	Delta        streamDelta `json:"delta"`
	FinishReason *string     `json:"finish_reason"`
}

type streamDelta struct {
	Role    string  `json:"role,omitempty"`
	Content *string `json:"content,omitempty"`
}

func newStreamEncoder(id string, created int64, model string) *streamEncoder {
	return &streamEncoder{id: id, created: created, model: model}
}

// Start 固定先发送 assistant role chunk，空 content 也必须保留。
func (e *streamEncoder) Start() []protocol.StreamEvent {
	empty := ""
	return []protocol.StreamEvent{protocol.JSONEvent("", e.chunk(streamDelta{Role: "assistant", Content: &empty}, nil))}
}

func (e *streamEncoder) Content(content string) protocol.StreamEvent {
	return protocol.JSONEvent("", e.chunk(streamDelta{Content: &content}, nil))
}

// Finish 在正常 finish chunk 后发送 [DONE]，不伪造额外 usage chunk。
func (e *streamEncoder) Finish(response engine.Response) []protocol.StreamEvent {
	reason := finishReason(response.FinishReason)
	return []protocol.StreamEvent{
		protocol.JSONEvent("", e.chunk(streamDelta{}, &reason)),
		protocol.RawEvent("[DONE]"),
	}
}

// Error 在流内错误后发送 [DONE]，调用方不再发送正常 finish。
func (e *streamEncoder) Error(err *protocol.Error) []protocol.StreamEvent {
	return []protocol.StreamEvent{
		protocol.JSONEvent("", errorEnvelope{Error: errorBody{
			Message: err.Message, Type: err.Type, Param: err.Param, Code: err.Code,
		}}),
		protocol.RawEvent("[DONE]"),
	}
}

func (e *streamEncoder) chunk(delta streamDelta, finish *string) completionChunk {
	return completionChunk{
		ID: e.id, Object: "chat.completion.chunk", Created: e.created, Model: e.model,
		Choices: []streamChoice{{Index: 0, Delta: delta, FinishReason: finish}},
	}
}
