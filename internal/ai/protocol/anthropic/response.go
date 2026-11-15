package anthropic

import (
	"net/http"

	"github.com/nexthopecoming/next-gateway/internal/ai/protocol"
	"github.com/nexthopecoming/next-gateway/internal/core/engine"
)

type messageResponse struct {
	ID           string        `json:"id"`
	Type         string        `json:"type"`
	Role         string        `json:"role"`
	Model        string        `json:"model"`
	Content      []textContent `json:"content"`
	StopReason   string        `json:"stop_reason"`
	StopSequence *string       `json:"stop_sequence"`
	Usage        messageUsage  `json:"usage"`
}

type textContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type messageUsage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

type errorEnvelope struct {
	Type  string    `json:"type"`
	Error errorBody `json:"error"`
}

type errorBody struct {
	Type    string `json:"type"`
	Message string `json:"message"`
}

func newMessage(id, model string, response engine.Response) messageResponse {
	return messageResponse{
		ID: id, Type: "message", Role: "assistant", Model: model,
		Content:    []textContent{{Type: "text", Text: response.Text}},
		StopReason: stopReason(response.FinishReason), StopSequence: stopSequence(response),
		Usage: messageUsage{InputTokens: response.Usage.InputTokens, OutputTokens: response.Usage.OutputTokens},
	}
}

func writeError(w http.ResponseWriter, err *protocol.Error) {
	status := err.Status
	if status == 0 {
		status = http.StatusInternalServerError
	}
	protocol.WriteJSON(w, status, errorEnvelope{
		Type: "error", Error: errorBody{Type: err.Type, Message: err.Message},
	})
}

func stopReason(reason engine.FinishReason) string {
	switch reason {
	case engine.FinishLength:
		return "max_tokens"
	case engine.FinishStopSequence:
		return "stop_sequence"
	default:
		return "end_turn"
	}
}

func stopSequence(response engine.Response) *string {
	if response.FinishReason != engine.FinishStopSequence {
		return nil
	}
	value := response.StopSequence
	return &value
}
