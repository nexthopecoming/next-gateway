// Package proxy 执行固定 Route -> Pool -> Endpoint 的通用下游 HTTP 调用。
// 本包不解析 AI 协议，不自动重试或跟随重定向，也不在流式响应中途更换 Endpoint。
package proxy

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/nexthopecoming/next-gateway/internal/core/balancer"
	"github.com/nexthopecoming/next-gateway/internal/core/router"
)

const (
	// MaxPools 和 MaxTotalEndpoints 限制一份完整代理配置的资源规模。
	MaxPools          = 256
	MaxTotalEndpoints = 1024
)

var (
	// ErrUpstream、ErrUpstreamTimeout 和 ErrOverloaded 是不含 URL、正文及原始网络错误的有限错误。
	ErrUpstream        = errors.New("upstream_error")
	ErrUpstreamTimeout = errors.New("upstream_timeout")
	ErrOverloaded      = errors.New("upstream_overloaded")
)

// Request 是 AI Adapter 提交的通用下游请求，不包含客户端可控制的目标 URL。
type Request struct {
	Match  router.Match
	Header http.Header
	Body   []byte
}

// Selection 只包含经过名称约束的配置逻辑标识，可直接用于受限观测字段。
type Selection struct {
	Route    string
	Pool     string
	Endpoint string
}

// Dispatcher 持有只读路由/池索引、共享 Transport 和本实例的在途预算。
type Dispatcher struct {
	table                *router.Table
	pools                map[string]*balancer.Pool
	transport            *http.Transport
	requestHeaderTimeout time.Duration
	inFlight             chan struct{}
}

// NewDispatcher 先校验 Pool 切片再建立索引，避免重复名称被 map 构造静默覆盖。
func NewDispatcher(
	table *router.Table,
	pools []*balancer.Pool,
	config TransportConfig,
) (*Dispatcher, error) {
	if table == nil {
		return nil, errors.New("proxy: nil route table")
	}
	if len(pools) > MaxPools {
		return nil, errors.New("proxy: too many pools")
	}

	poolIndex := make(map[string]*balancer.Pool, len(pools))
	totalEndpoints := 0
	for _, pool := range pools {
		if pool == nil {
			return nil, errors.New("proxy: nil pool")
		}
		if _, exists := poolIndex[pool.Name()]; exists {
			return nil, errors.New("proxy: duplicate pool name")
		}
		if pool.EndpointCount() == 0 {
			return nil, errors.New("proxy: empty pool")
		}
		totalEndpoints += pool.EndpointCount()
		if totalEndpoints > MaxTotalEndpoints {
			return nil, errors.New("proxy: too many endpoints")
		}
		poolIndex[pool.Name()] = pool
	}
	for _, route := range table.Routes() {
		if _, exists := poolIndex[route.Pool()]; !exists {
			return nil, errors.New("proxy: route references unknown pool")
		}
	}

	transport, err := newTransport(config)
	if err != nil {
		return nil, err
	}
	return &Dispatcher{
		table:                table,
		pools:                poolIndex,
		transport:            transport,
		requestHeaderTimeout: config.RequestHeaderTimeout,
		inFlight:             make(chan struct{}, config.MaxInFlightRequests),
	}, nil
}

// Do 固定本次选择并只执行一次 RoundTrip；2xx Body 接管派生 context 和在途额度。
func (d *Dispatcher) Do(ctx context.Context, request Request) (Selection, *http.Response, error) {
	if d == nil || d.table == nil || d.transport == nil || d.inFlight == nil || d.requestHeaderTimeout <= 0 {
		return Selection{}, nil, ErrUpstream
	}
	if ctx == nil {
		return Selection{}, nil, ErrUpstream
	}
	// 已结束的调用不得复制大 Body、消耗负载均衡轮次或占用新的在途额度。
	if err := ctx.Err(); err != nil {
		return Selection{}, nil, err
	}
	select {
	case d.inFlight <- struct{}{}:
	default:
		return Selection{}, nil, ErrOverloaded
	}
	handedOver := false
	defer func() {
		if !handedOver {
			d.releaseInFlight()
		}
	}()

	// Header/Body 快照必须在任何网络等待之前完成，调用方随后修改原输入不会影响本次请求。
	header := filterHeaders(request.Header)
	body := bytes.Clone(request.Body)
	route, err := d.table.Lookup(request.Match)
	if err != nil {
		return Selection{}, nil, err
	}
	selection := Selection{Route: route.Name(), Pool: route.Pool()}

	pool, exists := d.pools[route.Pool()]
	if !exists || pool == nil {
		return selection, nil, balancer.ErrUnavailable
	}
	endpoint, err := pool.Select()
	if err != nil {
		return selection, nil, err
	}
	selection.Endpoint = endpoint.Name()

	// 派生取消只管理本次下游资源，不会反向取消调用方 context。
	downstreamContext, cancel := context.WithCancel(ctx)
	// Endpoint 仅提供已校验 origin，路径和方法只来自静态 Route。
	downstreamRequest, err := http.NewRequestWithContext(
		downstreamContext,
		route.Method(),
		endpoint.Origin()+route.Path(),
		bytes.NewReader(body),
	)
	if err != nil {
		cancel()
		return selection, nil, classifyRoundTripError(ctx, err)
	}
	// NewRequest 会为 bytes.Reader 自动生成 GetBody；清除它以禁止 Transport 重放 AI POST Body。
	downstreamRequest.GetBody = nil
	downstreamRequest.Header = header

	// 阶段计时覆盖上传和等待响应头；Stop 返回 false 时等待回调结束，避免与返回路径竞争。
	requestHeaderTimedOut := false
	timerDone := make(chan struct{})
	timer := time.AfterFunc(d.requestHeaderTimeout, func() {
		requestHeaderTimedOut = true
		cancel()
		close(timerDone)
	})
	response, err := d.transport.RoundTrip(downstreamRequest)
	if !timer.Stop() {
		<-timerDone
	}
	if requestHeaderTimedOut {
		cancelAndClose(responseBody(response), cancel)
		if contextErr := ctx.Err(); contextErr != nil {
			return selection, nil, contextErr
		}
		return selection, nil, ErrUpstreamTimeout
	}
	if err != nil {
		cancel()
		if response != nil && response.Body != nil {
			_ = response.Body.Close()
		}
		return selection, nil, classifyRoundTripError(ctx, err)
	}
	if response == nil || response.Body == nil {
		cancel()
		return selection, nil, ErrUpstream
	}
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		cancelAndClose(response.Body, cancel)
		if err := ctx.Err(); err != nil {
			return selection, nil, err
		}
		return selection, nil, ErrUpstream
	}
	// 响应头成功后只停止阶段计时；Body 的结束才释放派生 context 和在途额度。
	response.Body = &cancelOnCloseBody{
		ReadCloser: response.Body,
		cancel:     cancel,
		release:    d.releaseInFlight,
	}
	handedOver = true
	return selection, response, nil
}

// responseBody 安全处理 RoundTrip 在异常情况下同时返回的响应。
func responseBody(response *http.Response) io.ReadCloser {
	if response == nil {
		return nil
	}
	return response.Body
}

// releaseInFlight 只由已经取得 token 的路径调用；channel 永不关闭。
func (d *Dispatcher) releaseInFlight() {
	<-d.inFlight
}

// Close 只释放空闲连接；进行中请求及其 Body 仍由原调用方和 context 管理。
func (d *Dispatcher) Close() {
	if d == nil || d.transport == nil {
		return
	}
	d.transport.CloseIdleConnections()
}

// allowedHeaders 的键用于大小写无关匹配，值是下游请求的规范 Header 名。
var allowedHeaders = map[string]string{
	"accept":            "Accept",
	"content-type":      "Content-Type",
	"anthropic-version": "Anthropic-Version",
	"x-request-id":      "X-Request-Id",
}

// hopByHopHeaders 使用小写键，防止非规范 map 键绕过连接级 Header 过滤。
var hopByHopHeaders = map[string]struct{}{
	"connection":          {},
	"keep-alive":          {},
	"proxy-authenticate":  {},
	"proxy-authorization": {},
	"proxy-connection":    {},
	"te":                  {},
	"trailer":             {},
	"transfer-encoding":   {},
	"upgrade":             {},
}

// filterHeaders 不修改输入，只生成独立白名单快照，并统一处理 Connection 提名。
func filterHeaders(input http.Header) http.Header {
	// map 本身不保留不同大小写键的插入顺序，排序使重复键的值顺序可重复。
	names := make([]string, 0, len(input))
	for name := range input {
		names = append(names, name)
	}
	sort.Strings(names)

	nominated := make(map[string]struct{})
	for _, name := range names {
		if !strings.EqualFold(name, "Connection") {
			continue
		}
		for _, line := range input[name] {
			for _, token := range strings.Split(line, ",") {
				if value := strings.TrimSpace(token); value != "" {
					nominated[strings.ToLower(value)] = struct{}{}
				}
			}
		}
	}

	output := make(http.Header, len(allowedHeaders))
	for _, name := range names {
		folded := strings.ToLower(name)
		canonical, allowed := allowedHeaders[folded]
		if !allowed {
			continue
		}
		if _, excluded := nominated[folded]; excluded {
			continue
		}
		if _, excluded := hopByHopHeaders[folded]; excluded {
			continue
		}
		// append 从 nil 或已有输出切片增长，不与任何输入值切片共享底层数组。
		output[canonical] = append(output[canonical], input[name]...)
	}
	return output
}

// cancelOnCloseBody 将下游响应 Body 的结束与派生 context、在途额度绑定。
type cancelOnCloseBody struct {
	io.ReadCloser
	cancel  context.CancelFunc
	release func()
	once    sync.Once
}

// finish 保证 EOF、读取错误和 Close 交错时也只释放一次请求资源。
func (b *cancelOnCloseBody) finish() {
	b.once.Do(func() {
		b.cancel()
		if b.release != nil {
			b.release()
		}
	})
}

// Read 在 EOF 或网络错误时立即释放连接关联的派生 context 和在途额度。
func (b *cancelOnCloseBody) Read(buffer []byte) (int, error) {
	count, err := b.ReadCloser.Read(buffer)
	if err != nil {
		b.finish()
	}
	return count, err
}

// Close 允许调用方提前结束响应，同时不反向取消父 context。
func (b *cancelOnCloseBody) Close() error {
	// 先取消再 Close，防止 HTTP/1 Transport 为复用连接而继续 drain 大型响应。
	b.finish()
	return b.ReadCloser.Close()
}

// cancelAndClose 不读取错误正文，避免部分响应停滞；先取消可阻止 Transport 二次 drain。
func cancelAndClose(body io.ReadCloser, cancel context.CancelFunc) {
	cancel()
	if body != nil {
		_ = body.Close()
	}
}
