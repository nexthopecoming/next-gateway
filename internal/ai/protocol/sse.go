package protocol

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/nexthopecoming/next-gateway/internal/core/config"
	"github.com/nexthopecoming/next-gateway/internal/core/engine"
)

// StreamEvent 是一帧完整 SSE；Name 为空时省略 event 行。
type StreamEvent struct {
	Name    string
	Data    []byte
	Comment bool
}

// JSONEvent 序列化协议响应模型。模型仅含 JSON 原生字段，错误时生成有限内部错误对象而非正文。
func JSONEvent(name string, value any) StreamEvent {
	data, err := json.Marshal(value)
	if err != nil {
		data = []byte(`{"type":"error","error":{"type":"api_error","message":"internal gateway error"}}`)
		name = "error"
	}
	return StreamEvent{Name: name, Data: data}
}

// RawEvent 用于 OpenAI 的 [DONE] 等固定非 JSON 标记。
func RawEvent(data string) StreamEvent {
	return StreamEvent{Data: []byte(data)}
}

// HeartbeatEvent 生成 SSE 注释帧，不添加 data 或 event 行。
func HeartbeatEvent() StreamEvent {
	return StreamEvent{Data: []byte(": heartbeat"), Comment: true}
}

// StreamEncoder 由每个请求独立创建，只负责协议事件，不负责计时或网络写回。
type StreamEncoder interface {
	Start() []StreamEvent
	Content(content string) StreamEvent
	Finish(response engine.Response) []StreamEvent
	Error(err *Error) []StreamEvent
}

// StreamOutcome 提供访问日志所需的有限结果与 usage，不包含响应正文。
type StreamOutcome struct {
	Result   string
	Usage    engine.Usage
	UsageSet bool
}

// ServeSSE 串行处理上游事件、首包/空闲/总时长、heartbeat 和每次写回期限。
// 所有网络写入都留在当前 Handler goroutine，避免并发写 ResponseWriter。
func ServeSSE(
	ctx context.Context,
	w http.ResponseWriter,
	events <-chan engine.Event,
	configuration config.RequestConfig,
	encoder StreamEncoder,
) StreamOutcome {
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	for _, event := range encoder.Start() {
		if err := writeStreamEvent(ctx, w, event, configuration.StreamWriteTimeout); err != nil {
			return StreamOutcome{Result: streamFailureResult(ctx, "write_error")}
		}
	}

	firstChunk := time.NewTimer(configuration.FirstChunkTimeout)
	idle := time.NewTimer(configuration.StreamIdleTimeout)
	maximum := time.NewTimer(configuration.StreamMaxDuration)
	heartbeat := time.NewTicker(configuration.HeartbeatInterval)
	defer func() {
		stopTimer(firstChunk)
		stopTimer(idle)
		stopTimer(maximum)
		heartbeat.Stop()
	}()

	seenContent := false
	for {
		select {
		case <-ctx.Done():
			return StreamOutcome{Result: "canceled"}
		case <-firstChunk.C:
			return writeStreamFailure(ctx, w, encoder, StreamTimeout("first_chunk_timeout", "first upstream chunk timeout"), "first_chunk_timeout", configuration.StreamWriteTimeout)
		case <-idle.C:
			return writeStreamFailure(ctx, w, encoder, StreamTimeout("stream_idle_timeout", "upstream stream idle timeout"), "stream_idle_timeout", configuration.StreamWriteTimeout)
		case <-maximum.C:
			return writeStreamFailure(ctx, w, encoder, StreamTimeout("stream_max_duration", "upstream stream maximum duration exceeded"), "stream_max_duration", configuration.StreamWriteTimeout)
		case <-heartbeat.C:
			// heartbeat 只维持连接，不停止或重置首包、idle 计时器。
			if err := writeStreamEvent(ctx, w, HeartbeatEvent(), configuration.StreamWriteTimeout); err != nil {
				return StreamOutcome{Result: streamFailureResult(ctx, "write_error")}
			}
		case event, ok := <-events:
			if !ok {
				return writeStreamFailure(ctx, w, encoder, UpstreamError(nil), "upstream_error", configuration.StreamWriteTimeout)
			}
			switch event.Kind {
			case engine.EventContent:
				if !seenContent {
					seenContent = true
					stopTimer(firstChunk)
				}
				resetTimer(idle, configuration.StreamIdleTimeout)
				if err := writeStreamEvent(ctx, w, encoder.Content(event.Content), configuration.StreamWriteTimeout); err != nil {
					return StreamOutcome{Result: streamFailureResult(ctx, "write_error")}
				}
			case engine.EventDone:
				stopTimer(firstChunk)
				for _, finished := range encoder.Finish(event.Response) {
					if err := writeStreamEvent(ctx, w, finished, configuration.StreamWriteTimeout); err != nil {
						return StreamOutcome{Result: streamFailureResult(ctx, "write_error")}
					}
				}
				return StreamOutcome{Result: "success", Usage: event.Response.Usage, UsageSet: true}
			case engine.EventError:
				protocolError := UpstreamError(event.Err)
				result := protocolError.Code
				return writeStreamFailure(ctx, w, encoder, protocolError, result, configuration.StreamWriteTimeout)
			default:
				return writeStreamFailure(ctx, w, encoder, UpstreamError(nil), "upstream_error", configuration.StreamWriteTimeout)
			}
		}
	}
}

func writeStreamFailure(
	ctx context.Context,
	w http.ResponseWriter,
	encoder StreamEncoder,
	protocolError *Error,
	result string,
	writeTimeout time.Duration,
) StreamOutcome {
	if ctx.Err() != nil {
		return StreamOutcome{Result: "canceled"}
	}
	for _, event := range encoder.Error(protocolError) {
		if err := writeStreamEvent(ctx, w, event, writeTimeout); err != nil {
			return StreamOutcome{Result: streamFailureResult(ctx, "write_error")}
		}
	}
	return StreamOutcome{Result: result}
}

func writeStreamEvent(ctx context.Context, w http.ResponseWriter, event StreamEvent, timeout time.Duration) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	controller := http.NewResponseController(w)
	if err := controller.SetWriteDeadline(time.Now().Add(timeout)); err != nil {
		return err
	}
	frame := make([]byte, 0, len(event.Name)+len(event.Data)+16)
	if event.Comment {
		frame = append(frame, event.Data...)
		frame = append(frame, '\n', '\n')
	} else {
		if event.Name != "" {
			frame = append(frame, "event: "...)
			frame = append(frame, event.Name...)
			frame = append(frame, '\n')
		}
		frame = append(frame, "data: "...)
		frame = append(frame, event.Data...)
		frame = append(frame, '\n', '\n')
	}

	_, writeErr := w.Write(frame)
	flushErr := controller.Flush()
	clearErr := controller.SetWriteDeadline(time.Time{})
	return errors.Join(writeErr, flushErr, clearErr)
}

func streamFailureResult(ctx context.Context, result string) string {
	if ctx.Err() != nil {
		return "canceled"
	}
	return result
}

func stopTimer(timer *time.Timer) {
	if !timer.Stop() {
		select {
		case <-timer.C:
		default:
		}
	}
}

func resetTimer(timer *time.Timer, duration time.Duration) {
	stopTimer(timer)
	timer.Reset(duration)
}
