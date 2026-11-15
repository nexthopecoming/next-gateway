package engine

import "context"

// Stable 是不附加编排钩子的基础顺序执行链。
type Stable struct {
	upstream Upstream
}

// NewStable 创建 stable 执行器；上游由应用装配层唯一注入。
func NewStable(upstream Upstream) *Stable {
	return &Stable{upstream: upstream}
}

// Complete 直接调用上游，保持 stable 主链路简单可预测。
func (s *Stable) Complete(ctx context.Context, request Request) (Response, error) {
	response, err := s.upstream.Complete(ctx, request)
	return response, ClassifyError(err)
}

// Stream 直接转发上游事件，不额外启动 goroutine。
func (s *Stable) Stream(ctx context.Context, request Request) (<-chan Event, error) {
	events, err := s.upstream.Stream(ctx, request)
	return events, ClassifyError(err)
}
