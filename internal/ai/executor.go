package ai

import (
	"context"
	"errors"
	"net/http"

	"github.com/nexthopecoming/next-gateway/internal/core/canary"
	"github.com/nexthopecoming/next-gateway/internal/core/config"
	"github.com/nexthopecoming/next-gateway/internal/core/engine"
	"github.com/nexthopecoming/next-gateway/internal/core/httpserver"
)

const (
	// ProtocolOpenAI 与 ProtocolAnthropic 是访问日志允许的固定协议枚举。
	ProtocolOpenAI    = "openai"
	ProtocolAnthropic = "anthropic"
	// LocalConfigVersion 明确表示 file 启动配置，不伪装成远程发布版本。
	LocalConfigVersion  = "local-file"
	LocalConfigRevision = int64(0)
)

// Executor 固定本地配置和两套 Engine，在每次协议请求中只执行一次灰度决策。
type Executor struct {
	identities canary.IdentityExtractor
	decider    *canary.Decider
	stable     engine.Engine
	canary     engine.Engine
	request    config.RequestConfig
}

// NewExecutor 由应用装配层注入身份、灰度和共享上游执行器。
func NewExecutor(
	identities canary.IdentityExtractor,
	decider *canary.Decider,
	stable, canaryEngine engine.Engine,
	requestConfig config.RequestConfig,
) *Executor {
	return &Executor{
		identities: identities,
		decider:    decider,
		stable:     stable,
		canary:     canaryEngine,
		request:    requestConfig,
	}
}

// RequestBodyLimit 返回启动配置中的请求体上限，协议解码在灰度决策前使用该固定值。
func (e *Executor) RequestBodyLimit() int64 {
	return e.request.MaxBodyBytes
}

// Invocation 是单请求不可变的执行视图；后续流式事件不能切换其中的 Engine 或配置。
type Invocation struct {
	decision canary.Decision
	engine   engine.Engine
	request  config.RequestConfig
}

// Begin 在协议解析成功后提取身份并固定 core；Header 和日志只包含有限枚举。
func (e *Executor) Begin(w http.ResponseWriter, r *http.Request, protocol string) (*Invocation, error) {
	identity, err := e.identities.Extract(r)
	if err != nil {
		return nil, err
	}
	decision := e.decider.Decide(identity, r.Header.Get("X-Gateway-Core"))
	selected := e.stable
	if decision.Core == canary.CoreCanary {
		selected = e.canary
	}
	w.Header().Set("X-Gateway-Core", string(decision.Core))
	httpserver.UpdateRequestMetadata(r.Context(), func(metadata *httpserver.RequestMetadata) {
		metadata.Protocol = protocol
		metadata.Core = string(decision.Core)
		metadata.DecisionReason = string(decision.Reason)
		metadata.ConfigVersion = LocalConfigVersion
		metadata.ConfigRevision = LocalConfigRevision
		metadata.Upstream = "mock"
	})
	return &Invocation{decision: decision, engine: selected, request: e.request}, nil
}

// Decision 返回请求开始时固定的结果，供测试和观测读取，不包含身份原文。
func (i *Invocation) Decision() canary.Decision {
	return i.decision
}

// Complete 对非流式调用应用单次上游总超时，并将超时映射为有限错误类别。
func (i *Invocation) Complete(ctx context.Context, request engine.Request) (engine.Response, error) {
	ctx, cancel := context.WithTimeout(ctx, i.request.UpstreamTimeout)
	defer cancel()
	response, err := i.engine.Complete(ctx, request)
	if errors.Is(err, context.DeadlineExceeded) {
		return engine.Response{}, &engine.Error{Kind: engine.ErrorTimeout}
	}
	return response, engine.ClassifyError(err)
}

// Stream 返回固定 Engine 的事件流；首包、空闲、总流和写回期限由协议共享 runner 管理。
func (i *Invocation) Stream(ctx context.Context, request engine.Request) (<-chan engine.Event, error) {
	events, err := i.engine.Stream(ctx, request)
	return events, engine.ClassifyError(err)
}

// RequestConfig 返回本次请求固定的不可变流式策略副本。
func (i *Invocation) RequestConfig() config.RequestConfig {
	return i.request
}
