// Package engine 定义 AI 执行器与上游之间的统一契约。
// 本包只依赖标准库，协议解码和身份提取分别留在 ai、biz，避免 core 反向依赖领域包。
package engine

import (
	"context"
	"errors"
)

// Message 是已严格校验的单条文本消息，不包含工具或多模态内容。
type Message struct {
	Role    string
	Content string
}

// Request 是两种入站协议共享的内部请求；采样参数保留用于后续真实上游。
type Request struct {
	Model       string
	System      string
	Messages    []Message
	MaxTokens   int
	Temperature *float64
	TopP        *float64
	TopK        int
	Stop        []string
	Stream      bool
}

// FinishReason 使用协议无关的有限枚举，由适配器映射为各自结束原因。
type FinishReason string

const (
	FinishStop         FinishReason = "stop"
	FinishLength       FinishReason = "length"
	FinishStopSequence FinishReason = "stop_sequence"
)

// Usage 中一个 Mock token 固定等于一个 Unicode code point，不代表真实模型 token。
type Usage struct {
	InputTokens  int
	OutputTokens int
}

// Response 是上游的完整逻辑结果；流式完成事件复用同一结构保证文本和 usage 一致。
type Response struct {
	Text         string
	FinishReason FinishReason
	StopSequence string
	Usage        Usage
}

// ErrorKind 限制进入协议和日志的错误类别，避免透传上游原文或敏感细节。
type ErrorKind string

const (
	ErrorUpstream ErrorKind = "upstream"
	ErrorTimeout  ErrorKind = "timeout"
	ErrorInternal ErrorKind = "internal"
)

// Error 是上游链路的安全错误，只携带有限分类，不保存原始错误消息。
type Error struct {
	Kind ErrorKind
}

func (e *Error) Error() string {
	return "gateway engine error: " + string(e.Kind)
}

// ClassifyError 将任意上游错误收敛为安全分类；上下文取消保留原语义供 SSE 停止写回。
func ClassifyError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	var engineError *Error
	if errors.As(err, &engineError) {
		return engineError
	}
	return &Error{Kind: ErrorUpstream}
}

// EventKind 标识统一流式事件。完成事件之后实现必须关闭 channel。
type EventKind uint8

const (
	EventContent EventKind = iota + 1
	EventDone
	EventError
)

// Event 每次只表达一种语义：内容、完成或错误。
type Event struct {
	Kind     EventKind
	Content  string
	Response Response
	Err      error
}

// Upstream 是 Mock 和后续真实 Provider 的共同能力。
// Stream 的实现必须在 ctx 取消后停止发送并关闭返回 channel。
type Upstream interface {
	Complete(ctx context.Context, request Request) (Response, error)
	Stream(ctx context.Context, request Request) (<-chan Event, error)
}

// Engine 是 stable/canary 共有的请求级执行契约，请求开始后不得切换实现。
type Engine interface {
	Complete(ctx context.Context, request Request) (Response, error)
	Stream(ctx context.Context, request Request) (<-chan Event, error)
}
