// Package openai 实现 OpenAI 文本对话子集的 HTTP 与 SSE 适配。
package openai

import (
	"bytes"
	"encoding/json"

	"github.com/nexthopecoming/next-gateway/internal/ai/protocol"
	"github.com/nexthopecoming/next-gateway/internal/core/engine"
)

type chatRequest struct {
	Model       protocol.Text              `json:"model"`
	Messages    []chatMessage              `json:"messages"`
	MaxTokens   protocol.Optional[int]     `json:"max_tokens"`
	Temperature protocol.Optional[float64] `json:"temperature"`
	TopP        protocol.Optional[float64] `json:"top_p"`
	Stop        json.RawMessage            `json:"stop"`
	Stream      bool                       `json:"stream"`
}

type chatMessage struct {
	Role    protocol.Text                    `json:"role"`
	Content protocol.Optional[protocol.Text] `json:"content"`
}

// toEngine 将已严格解码的协议字段转换为统一请求，校验错误只包含有界字段名。
func (r chatRequest) toEngine() (engine.Request, *protocol.Error) {
	if err := protocol.ValidateModel(string(r.Model)); err != nil {
		return engine.Request{}, err
	}
	if len(r.Messages) == 0 {
		return engine.Request{}, protocol.InvalidField("messages")
	}
	request := engine.Request{Model: string(r.Model), Stream: r.Stream}
	for _, message := range r.Messages {
		switch string(message.Role) {
		case "system", "user", "assistant":
		default:
			return engine.Request{}, protocol.InvalidField("messages.role")
		}
		if !message.Content.Set {
			return engine.Request{}, protocol.InvalidField("messages.content")
		}
		request.Messages = append(request.Messages, engine.Message{
			Role: string(message.Role), Content: string(message.Content.Value),
		})
	}
	if r.MaxTokens.Set {
		if r.MaxTokens.Value <= 0 {
			return engine.Request{}, protocol.InvalidField("max_tokens")
		}
		request.MaxTokens = r.MaxTokens.Value
	}
	if r.Temperature.Set {
		if r.Temperature.Value < 0 || r.Temperature.Value > 2 {
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
	stops, err := parseStops(r.Stop)
	if err != nil {
		return engine.Request{}, err
	}
	request.Stop = stops
	return request, nil
}

func parseStops(raw json.RawMessage) ([]string, *protocol.Error) {
	if len(raw) == 0 {
		return nil, nil
	}
	trimmed := bytes.TrimSpace(raw)
	var values []string
	if len(trimmed) > 0 && trimmed[0] == '"' {
		var value protocol.Text
		if err := protocol.DecodeStrict(trimmed, &value); err != nil {
			return nil, protocol.InvalidField("stop")
		}
		values = []string{string(value)}
	} else {
		var entries []protocol.Text
		if err := protocol.DecodeStrict(trimmed, &entries); err != nil {
			return nil, protocol.InvalidField("stop")
		}
		for _, entry := range entries {
			values = append(values, string(entry))
		}
	}
	if err := protocol.ValidateStops(values); err != nil {
		return nil, err
	}
	return values, nil
}
