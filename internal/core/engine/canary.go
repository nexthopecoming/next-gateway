package engine

import "context"

// Hooks 是 canary 的静态扩展点；本阶段只允许进程启动时注册，不支持动态插件。
type Hooks struct {
	BeforeRequest func(context.Context, Request) error
	OnContent     func(string)
	OnComplete    func(Response)
}

// Canary 通过独立钩子编排调用同一个上游，不改变正常协议结果。
type Canary struct {
	upstream Upstream
	hooks    Hooks
}

// NewCanary 创建 canary 执行器；空钩子使用无操作实现，保证两条链路行为一致。
func NewCanary(upstream Upstream, hooks Hooks) *Canary {
	if hooks.BeforeRequest == nil {
		hooks.BeforeRequest = func(context.Context, Request) error { return nil }
	}
	if hooks.OnContent == nil {
		hooks.OnContent = func(string) {}
	}
	if hooks.OnComplete == nil {
		hooks.OnComplete = func(Response) {}
	}
	return &Canary{upstream: upstream, hooks: hooks}
}

// Complete 按前置、上游、完成钩子的固定顺序执行。
func (c *Canary) Complete(ctx context.Context, request Request) (Response, error) {
	if err := c.hooks.BeforeRequest(ctx, request); err != nil {
		return Response{}, ClassifyError(err)
	}
	response, err := c.upstream.Complete(ctx, request)
	if err != nil {
		return Response{}, ClassifyError(err)
	}
	c.hooks.OnComplete(response)
	return response, nil
}

// Stream 使用单一转发 goroutine 调用内容钩子；所有发送都监听 ctx，取消时不会滞留。
func (c *Canary) Stream(ctx context.Context, request Request) (<-chan Event, error) {
	if err := c.hooks.BeforeRequest(ctx, request); err != nil {
		return nil, ClassifyError(err)
	}
	upstream, err := c.upstream.Stream(ctx, request)
	if err != nil {
		return nil, ClassifyError(err)
	}

	events := make(chan Event)
	go func() {
		defer close(events)
		for {
			select {
			case <-ctx.Done():
				return
			case event, ok := <-upstream:
				if !ok {
					return
				}
				switch event.Kind {
				case EventContent:
					c.hooks.OnContent(event.Content)
				case EventDone:
					c.hooks.OnComplete(event.Response)
				}
				select {
				case events <- event:
				case <-ctx.Done():
					return
				}
			}
		}
	}()
	return events, nil
}
