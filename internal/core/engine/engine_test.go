package engine

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"
)

type fakeUpstream struct {
	response Response
	err      error
	events   []Event
	calls    int
}

func (f *fakeUpstream) Complete(context.Context, Request) (Response, error) {
	f.calls++
	return f.response, f.err
}

func (f *fakeUpstream) Stream(ctx context.Context, _ Request) (<-chan Event, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	events := make(chan Event, len(f.events))
	for _, event := range f.events {
		events <- event
	}
	close(events)
	return events, nil
}

func TestStableAndCanaryShareGoldenContract(t *testing.T) {
	response := Response{
		Text: "你好", FinishReason: FinishStop,
		Usage: Usage{InputTokens: 2, OutputTokens: 2},
	}
	for _, test := range []struct {
		name string
		new  func(Upstream) Engine
	}{
		{name: "stable", new: func(upstream Upstream) Engine { return NewStable(upstream) }},
		{name: "canary", new: func(upstream Upstream) Engine { return NewCanary(upstream, Hooks{}) }},
	} {
		t.Run(test.name, func(t *testing.T) {
			upstream := &fakeUpstream{
				response: response,
				events: []Event{
					{Kind: EventContent, Content: "你"},
					{Kind: EventContent, Content: "好"},
					{Kind: EventDone, Response: response},
				},
			}
			executor := test.new(upstream)
			completed, err := executor.Complete(context.Background(), Request{Model: "mock-model"})
			if err != nil || !reflect.DeepEqual(completed, response) {
				t.Fatalf("Complete = %+v, %v", completed, err)
			}
			events, err := executor.Stream(context.Background(), Request{Stream: true})
			if err != nil {
				t.Fatal(err)
			}
			var got []Event
			for event := range events {
				got = append(got, event)
			}
			if !reflect.DeepEqual(got, upstream.events) {
				t.Fatalf("流事件不一致:\n got %+v\nwant %+v", got, upstream.events)
			}
			if upstream.calls != 2 {
				t.Fatalf("上游调用次数 = %d，期望 2", upstream.calls)
			}
		})
	}
}

func TestCanaryHooksAndCancellation(t *testing.T) {
	var content []string
	var completions int
	upstream := &fakeUpstream{events: []Event{
		{Kind: EventContent, Content: "A"},
		{Kind: EventDone, Response: Response{Text: "A"}},
	}}
	executor := NewCanary(upstream, Hooks{
		OnContent:  func(value string) { content = append(content, value) },
		OnComplete: func(Response) { completions++ },
	})
	events, err := executor.Stream(context.Background(), Request{})
	if err != nil {
		t.Fatal(err)
	}
	for range events {
	}
	if !reflect.DeepEqual(content, []string{"A"}) || completions != 1 {
		t.Fatalf("钩子调用结果 content=%v completions=%d", content, completions)
	}

	// 无事件上游只随 ctx 退出，可验证 canary 转发 goroutine 不泄漏。
	blocking := blockingUpstream{started: make(chan struct{})}
	executor = NewCanary(blocking, Hooks{})
	ctx, cancel := context.WithCancel(context.Background())
	events, err = executor.Stream(ctx, Request{})
	if err != nil {
		t.Fatal(err)
	}
	<-blocking.started
	cancel()
	select {
	case _, ok := <-events:
		if ok {
			t.Fatal("取消后仍收到事件")
		}
	case <-time.After(time.Second):
		t.Fatal("取消后 canary channel 未关闭")
	}
}

type blockingUpstream struct {
	started chan struct{}
}

func (b blockingUpstream) Complete(context.Context, Request) (Response, error) {
	return Response{}, nil
}

func (b blockingUpstream) Stream(ctx context.Context, _ Request) (<-chan Event, error) {
	events := make(chan Event)
	go func() {
		defer close(events)
		close(b.started)
		<-ctx.Done()
	}()
	return events, nil
}

func TestErrorsAreClassifiedWithoutOriginalMessage(t *testing.T) {
	secret := errors.New("provider failed with secret")
	executor := NewStable(&fakeUpstream{err: secret})
	_, err := executor.Complete(context.Background(), Request{})
	var engineError *Error
	if !errors.As(err, &engineError) || engineError.Kind != ErrorUpstream {
		t.Fatalf("错误分类 = %v", err)
	}
	if err.Error() == secret.Error() {
		t.Fatal("原始上游错误被透传")
	}
}
