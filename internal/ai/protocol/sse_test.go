package protocol

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/nexthopecoming/next-gateway/internal/core/config"
	"github.com/nexthopecoming/next-gateway/internal/core/engine"
)

func TestServeSSEWritesFlushesAndDeadlines(t *testing.T) {
	events := make(chan engine.Event, 2)
	events <- engine.Event{Kind: engine.EventContent, Content: "A"}
	events <- engine.Event{Kind: engine.EventDone, Response: engine.Response{
		Text: "A", FinishReason: engine.FinishStop, Usage: engine.Usage{InputTokens: 2, OutputTokens: 1},
	}}
	close(events)
	writer := newDeadlineWriter()
	outcome := ServeSSE(context.Background(), writer, events, testStreamConfig(), testEncoder{})
	if outcome.Result != "success" || !outcome.UsageSet || outcome.Usage.OutputTokens != 1 {
		t.Fatalf("outcome = %+v", outcome)
	}
	want := "event: start\ndata: {}\n\nevent: content\ndata: A\n\nevent: finish\ndata: {}\n\nevent: done\ndata: [DONE]\n\n"
	if writer.body.String() != want {
		t.Fatalf("SSE body:\n%s\nwant:\n%s", writer.body.String(), want)
	}
	if writer.flushes != 4 {
		t.Fatalf("Flush 次数 = %d，期望每个事件一次", writer.flushes)
	}
	if len(writer.deadlines) != 8 {
		t.Fatalf("写期限设置次数 = %d，期望每次写入设置并清除", len(writer.deadlines))
	}
	for index, deadline := range writer.deadlines {
		if index%2 == 0 && deadline.IsZero() || index%2 == 1 && !deadline.IsZero() {
			t.Fatalf("写期限顺序错误: %v", writer.deadlines)
		}
	}
}

func TestServeSSEHeartbeatDoesNotResetFirstChunkTimer(t *testing.T) {
	configuration := testStreamConfig()
	configuration.FirstChunkTimeout = 30 * time.Millisecond
	configuration.HeartbeatInterval = 5 * time.Millisecond
	writer := newDeadlineWriter()
	outcome := ServeSSE(context.Background(), writer, make(chan engine.Event), configuration, testEncoder{})
	if outcome.Result != "first_chunk_timeout" {
		t.Fatalf("outcome = %+v", outcome)
	}
	if !strings.Contains(writer.body.String(), ": heartbeat\n\n") || strings.Contains(writer.body.String(), "data: : heartbeat") {
		t.Fatalf("heartbeat 帧错误: %s", writer.body.String())
	}
	if !strings.Contains(writer.body.String(), "first_chunk_timeout") {
		t.Fatalf("缺少首包超时错误: %s", writer.body.String())
	}
}

func TestServeSSEIdleTimeoutAfterContent(t *testing.T) {
	configuration := testStreamConfig()
	configuration.FirstChunkTimeout = 15 * time.Millisecond
	configuration.StreamIdleTimeout = 30 * time.Millisecond
	configuration.HeartbeatInterval = 5 * time.Millisecond
	events := make(chan engine.Event, 1)
	events <- engine.Event{Kind: engine.EventContent, Content: "A"}
	writer := newDeadlineWriter()
	outcome := ServeSSE(context.Background(), writer, events, configuration, testEncoder{})
	if outcome.Result != "stream_idle_timeout" {
		t.Fatalf("outcome = %+v", outcome)
	}
	if !strings.Contains(writer.body.String(), "event: content") || !strings.Contains(writer.body.String(), "stream_idle_timeout") {
		t.Fatalf("空闲超时输出错误: %s", writer.body.String())
	}
}

func TestServeSSEMaximumDurationWithActiveUpstream(t *testing.T) {
	configuration := testStreamConfig()
	configuration.StreamMaxDuration = 30 * time.Millisecond
	configuration.StreamIdleTimeout = 100 * time.Millisecond
	configuration.HeartbeatInterval = 10 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	events := make(chan engine.Event)
	producerDone := make(chan struct{})
	go func() {
		defer close(producerDone)
		ticker := time.NewTicker(5 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				select {
				case events <- engine.Event{Kind: engine.EventContent, Content: "A"}:
				case <-ctx.Done():
					return
				}
			case <-ctx.Done():
				return
			}
		}
	}()
	writer := newDeadlineWriter()
	outcome := ServeSSE(ctx, writer, events, configuration, testEncoder{})
	cancel()
	<-producerDone
	if outcome.Result != "stream_max_duration" || !strings.Contains(writer.body.String(), "stream_max_duration") {
		t.Fatalf("总流超时未生效: %+v %s", outcome, writer.body.String())
	}
}

func TestServeSSEClientCancellationDoesNotWriteError(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(10*time.Millisecond, cancel)
	writer := newDeadlineWriter()
	outcome := ServeSSE(ctx, writer, make(chan engine.Event), testStreamConfig(), testEncoder{})
	if outcome.Result != "canceled" {
		t.Fatalf("outcome = %+v", outcome)
	}
	if strings.Contains(writer.body.String(), "event: error") {
		t.Fatalf("客户端取消后仍写错误事件: %s", writer.body.String())
	}
}

func TestServeSSEWriteFailuresAndSlowWriter(t *testing.T) {
	writer := newDeadlineWriter()
	writer.writeErr = errors.New("network write secret")
	outcome := ServeSSE(context.Background(), writer, make(chan engine.Event), testStreamConfig(), testEncoder{})
	if outcome.Result != "write_error" {
		t.Fatalf("写错误 outcome = %+v", outcome)
	}

	slow := newDeadlineWriter()
	slow.blockWrite = true
	configuration := testStreamConfig()
	configuration.StreamWriteTimeout = 10 * time.Millisecond
	started := time.Now()
	outcome = ServeSSE(context.Background(), slow, make(chan engine.Event), configuration, testEncoder{})
	if outcome.Result != "write_error" || time.Since(started) > time.Second {
		t.Fatalf("慢客户端写期限未生效: %+v elapsed=%s", outcome, time.Since(started))
	}

	// 不支持连接写期限的 writer 不能静默伪装为已提供背压保护。
	outcome = ServeSSE(context.Background(), httptest.NewRecorder(), make(chan engine.Event), testStreamConfig(), testEncoder{})
	if outcome.Result != "write_error" {
		t.Fatalf("不支持写期限时 outcome = %+v", outcome)
	}
}

func TestServeSSEUnexpectedChannelClose(t *testing.T) {
	events := make(chan engine.Event)
	close(events)
	writer := newDeadlineWriter()
	outcome := ServeSSE(context.Background(), writer, events, testStreamConfig(), testEncoder{})
	if outcome.Result != "upstream_error" || !strings.Contains(writer.body.String(), "upstream_error") {
		t.Fatalf("异常关闭结果 = %+v %s", outcome, writer.body.String())
	}
}

func testStreamConfig() config.RequestConfig {
	return config.RequestConfig{
		FirstChunkTimeout:  time.Second,
		StreamIdleTimeout:  time.Second,
		StreamMaxDuration:  time.Second,
		StreamWriteTimeout: time.Second,
		HeartbeatInterval:  time.Second,
	}
}

type testEncoder struct{}

func (testEncoder) Start() []StreamEvent {
	return []StreamEvent{{Name: "start", Data: []byte("{}")}}
}
func (testEncoder) Content(content string) StreamEvent {
	return StreamEvent{Name: "content", Data: []byte(content)}
}
func (testEncoder) Finish(engine.Response) []StreamEvent {
	return []StreamEvent{{Name: "finish", Data: []byte("{}")}, {Name: "done", Data: []byte("[DONE]")}}
}
func (testEncoder) Error(err *Error) []StreamEvent {
	return []StreamEvent{{Name: "error", Data: []byte(err.Code)}}
}

type deadlineWriter struct {
	header       http.Header
	body         bytes.Buffer
	deadlines    []time.Time
	flushes      int
	writeErr     error
	blockWrite   bool
	lastDeadline time.Time
}

func newDeadlineWriter() *deadlineWriter {
	return &deadlineWriter{header: make(http.Header)}
}

func (w *deadlineWriter) Header() http.Header { return w.header }
func (w *deadlineWriter) WriteHeader(int)     {}
func (w *deadlineWriter) Write(data []byte) (int, error) {
	if w.blockWrite && !w.lastDeadline.IsZero() {
		timer := time.NewTimer(time.Until(w.lastDeadline))
		defer timer.Stop()
		<-timer.C
		return 0, context.DeadlineExceeded
	}
	if w.writeErr != nil {
		return 0, w.writeErr
	}
	return w.body.Write(data)
}
func (w *deadlineWriter) Flush() { w.flushes++ }
func (w *deadlineWriter) SetWriteDeadline(deadline time.Time) error {
	w.deadlines = append(w.deadlines, deadline)
	w.lastDeadline = deadline
	return nil
}
