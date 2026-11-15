package mock

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/nexthopecoming/next-gateway/internal/core/config"
	"github.com/nexthopecoming/next-gateway/internal/core/engine"
)

func TestUnicodeChunksAndJSONStreamConsistency(t *testing.T) {
	upstream := New(config.MockConfig{ResponseText: "A你🙂B好", ChunkSize: 2})
	request := engine.Request{
		System: "系统",
		Messages: []engine.Message{
			{Role: "user", Content: "Hi🙂"},
		},
	}
	completed, err := upstream.Complete(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if completed.Text != "A你🙂B好" || completed.Usage.InputTokens != 5 || completed.Usage.OutputTokens != 5 {
		t.Fatalf("非流式结果错误: %+v", completed)
	}

	events, err := upstream.Stream(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	var chunks []string
	var done engine.Response
	for event := range events {
		switch event.Kind {
		case engine.EventContent:
			chunks = append(chunks, event.Content)
		case engine.EventDone:
			done = event.Response
		}
	}
	if !reflect.DeepEqual(chunks, []string{"A你", "🙂B", "好"}) {
		t.Fatalf("Unicode chunks = %#v", chunks)
	}
	if !reflect.DeepEqual(done, completed) {
		t.Fatalf("流式完成结果 = %+v，非流式 = %+v", done, completed)
	}
}

func TestMaxTokensAndStopSemantics(t *testing.T) {
	tests := []struct {
		name         string
		text         string
		maxTokens    int
		stops        []string
		wantText     string
		wantReason   engine.FinishReason
		wantSequence string
	}{
		{name: "max tokens", text: "A你🙂B好", maxTokens: 3, wantText: "A你🙂", wantReason: engine.FinishLength},
		{name: "stop", text: "A你🙂B好", stops: []string{"🙂B"}, wantText: "A你", wantReason: engine.FinishStopSequence, wantSequence: "🙂B"},
		{name: "最早 stop", text: "hello world", stops: []string{"world", "lo w"}, wantText: "hel", wantReason: engine.FinishStopSequence, wantSequence: "lo w"},
		{name: "max 先于 stop", text: "hello world", maxTokens: 3, stops: []string{"world"}, wantText: "hel", wantReason: engine.FinishLength},
		{name: "stop 先于 max", text: "hello world", maxTokens: 8, stops: []string{"world"}, wantText: "hello ", wantReason: engine.FinishStopSequence, wantSequence: "world"},
		{name: "开头 stop", text: "hello", stops: []string{"he"}, wantText: "", wantReason: engine.FinishStopSequence, wantSequence: "he"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			upstream := New(config.MockConfig{ResponseText: test.text, ChunkSize: 2})
			response, err := upstream.Complete(context.Background(), engine.Request{MaxTokens: test.maxTokens, Stop: test.stops})
			if err != nil {
				t.Fatal(err)
			}
			if response.Text != test.wantText || response.FinishReason != test.wantReason || response.StopSequence != test.wantSequence {
				t.Fatalf("结果 = %+v", response)
			}
			if response.Usage.OutputTokens != len([]rune(test.wantText)) {
				t.Fatalf("输出 usage = %d", response.Usage.OutputTokens)
			}
		})
	}
}

func TestStreamDelayHonorsCancellation(t *testing.T) {
	upstream := New(config.MockConfig{ResponseText: "abcd", ChunkSize: 1, ChunkDelay: time.Hour})
	ctx, cancel := context.WithCancel(context.Background())
	events, err := upstream.Stream(ctx, engine.Request{})
	if err != nil {
		t.Fatal(err)
	}
	if event := <-events; event.Content != "a" {
		t.Fatalf("首 chunk = %+v", event)
	}
	cancel()
	select {
	case _, ok := <-events:
		if ok {
			t.Fatal("取消后仍收到事件")
		}
	case <-time.After(time.Second):
		t.Fatal("Mock 延迟未随取消退出")
	}
}
