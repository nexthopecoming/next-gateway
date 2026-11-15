package openai

import (
	"net/http"

	"github.com/nexthopecoming/next-gateway/internal/ai/protocol"
	"github.com/nexthopecoming/next-gateway/internal/core/engine"
)

type completionResponse struct {
	ID      string             `json:"id"`
	Object  string             `json:"object"`
	Created int64              `json:"created"`
	Model   string             `json:"model"`
	Choices []completionChoice `json:"choices"`
	Usage   completionUsage    `json:"usage"`
}

type completionChoice struct {
	Index        int               `json:"index"`
	Message      completionMessage `json:"message"`
	FinishReason string            `json:"finish_reason"`
}

type completionMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type completionUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

type errorEnvelope struct {
	Error errorBody `json:"error"`
}

type errorBody struct {
	Message string  `json:"message"`
	Type    string  `json:"type"`
	Param   *string `json:"param"`
	Code    string  `json:"code"`
}

func newCompletion(id string, created int64, model string, response engine.Response) completionResponse {
	return completionResponse{
		ID: id, Object: "chat.completion", Created: created, Model: model,
		Choices: []completionChoice{{
			Index:        0,
			Message:      completionMessage{Role: "assistant", Content: response.Text},
			FinishReason: finishReason(response.FinishReason),
		}},
		Usage: completionUsage{
			PromptTokens: response.Usage.InputTokens, CompletionTokens: response.Usage.OutputTokens,
			TotalTokens: response.Usage.InputTokens + response.Usage.OutputTokens,
		},
	}
}

func writeError(w http.ResponseWriter, err *protocol.Error) {
	status := err.Status
	if status == 0 {
		status = http.StatusInternalServerError
	}
	protocol.WriteJSON(w, status, errorEnvelope{Error: errorBody{
		Message: err.Message, Type: err.Type, Param: err.Param, Code: err.Code,
	}})
}

func finishReason(reason engine.FinishReason) string {
	if reason == engine.FinishLength {
		return "length"
	}
	return "stop"
}
