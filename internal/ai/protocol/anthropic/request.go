// Package anthropic 实现 Anthropic 文本消息子集的 HTTP 与 SSE 适配。
package anthropic

import (
	"bytes"
	"encoding/json"

	"github.com/nexthopecoming/next-gateway/internal/ai/protocol"
	"github.com/nexthopecoming/next-gateway/internal/core/engine"
)

type messagesRequest struct {
	Model         protocol.Text                      `json:"model"`
	Messages      []message                          `json:"messages"`
	System        protocol.Optional[protocol.Text]   `json:"system"`
	MaxTokens     protocol.Optional[int]             `json:"max_tokens"`
	Temperature   protocol.Optional[float64]         `json:"temperature"`
	TopP          protocol.Optional[float64]         `json:"top_p"`
	TopK          protocol.Optional[int]             `json:"top_k"`
	StopSequences protocol.Optional[[]protocol.Text] `json:"stop_sequences"`
	Stream        bool                               `json:"stream"`
}

type message struct {
	Role    protocol.Text   `json:"role"`
	Content json.RawMessage `json:"content"`
}

type contentBlock struct {
	Type protocol.Text                    `json:"type"`
	Text protocol.Optional[protocol.Text] `json:"text"`
}

func (r messagesRequest) toEngine() (engine.Request, *protocol.Error) {
	if err := protocol.ValidateModel(string(r.Model)); err != nil {
		return engine.Request{}, err
	}
	if len(r.Messages) == 0 {
		return engine.Request{}, protocol.InvalidField("messages")
	}
	if !r.MaxTokens.Set || r.MaxTokens.Value <= 0 {
		return engine.Request{}, protocol.InvalidField("max_tokens")
	}
	request := engine.Request{
		Model: string(r.Model), MaxTokens: r.MaxTokens.Value, Stream: r.Stream,
		System: string(r.System.Value),
	}
	for _, item := range r.Messages {
		switch string(item.Role) {
		case "user", "assistant":
		default:
			return engine.Request{}, protocol.InvalidField("messages.role")
		}
		content, err := parseContent(item.Content)
		if err != nil {
			return engine.Request{}, err
		}
		request.Messages = append(request.Messages, engine.Message{Role: string(item.Role), Content: content})
	}
	if r.Temperature.Set {
		if r.Temperature.Value < 0 || r.Temperature.Value > 1 {
			return engine.Request{}, protocol.InvalidField("temperature")
		}
		request.Temperature = &r.Temperature.Value
	}
	if r.TopP.Set {
		if r.TopP.Value < 0 || r.TopP.Value > 1 {
			return engine.Request{}, protocol.InvalidField("top_p")
		}
		request.TopP = &r.TopP.Value
	}
	if r.TopK.Set {
		if r.TopK.Value <= 0 {
			return engine.Request{}, protocol.InvalidField("top_k")
		}
		request.TopK = r.TopK.Value
	}
	if r.StopSequences.Set {
		if len(r.StopSequences.Value) > 4 {
			return engine.Request{}, protocol.InvalidField("stop_sequences")
		}
		for _, stop := range r.StopSequences.Value {
			if stop == "" {
				return engine.Request{}, protocol.InvalidField("stop_sequences")
			}
			request.Stop = append(request.Stop, string(stop))
		}
	}
	return request, nil
}

// parseContent 接受字符串或仅含 text block 的数组，并严格拒绝 block 未知字段。
func parseContent(raw json.RawMessage) (string, *protocol.Error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return "", protocol.InvalidField("messages.content")
	}
	if trimmed[0] == '"' {
		var text protocol.Text
		if err := protocol.DecodeStrict(trimmed, &text); err != nil {
			return "", protocol.InvalidField("messages.content")
		}
		return string(text), nil
	}
	if trimmed[0] != '[' {
		return "", protocol.InvalidField("messages.content")
	}
	var blocks []contentBlock
	if err := protocol.DecodeStrict(trimmed, &blocks); err != nil {
		return "", protocol.ValidationError("messages.content", err)
	}
	content := ""
	for _, block := range blocks {
		if block.Type != "text" || !block.Text.Set {
			return "", protocol.InvalidField("messages.content")
		}
		content += string(block.Text.Value)
	}
	return content, nil
}
