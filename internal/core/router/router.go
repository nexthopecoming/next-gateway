// Package router 将静态路由配置编译为请求期只读的路由表。
// 本包只匹配协议、固定路径和模型键，不读取 AI 请求正文，也不决定下游 URL。
package router

import (
	"errors"
	"fmt"
	"net/http"
	"regexp"

	coremodel "github.com/nexthopecoming/next-gateway/internal/core/model"
)

const (
	// ProtocolOpenAI 和 ProtocolAnthropic 是当前支持的有限协议标识。
	ProtocolOpenAI    = "openai"
	ProtocolAnthropic = "anthropic"

	// OpenAIChatCompletionsPath 和 AnthropicMessagesPath 是协议对应的唯一静态路径。
	OpenAIChatCompletionsPath = "/v1/chat/completions"
	AnthropicMessagesPath     = "/v1/messages"

	// WildcardModel 只作为完整模型匹配键使用，不表示前缀或正则表达式。
	WildcardModel = "*"

	// MaxRoutes 限制单张不可变路由表的配置规模。
	MaxRoutes = 1024
)

var (
	// ErrRouteNotFound 是统一的安全未命中错误，不携带模型或其他请求值。
	ErrRouteNotFound = errors.New("route_not_found")
	logicalName      = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)
)

// RouteSpec 是一条尚未编译的静态路由配置。
type RouteSpec struct {
	Name     string
	Protocol string
	Path     string
	Model    string
	Pool     string
}

// Match 是 AI 适配层提供的有限路由元数据，不包含正文或凭据。
type Match struct {
	Protocol string
	Path     string
	Model    string
}

// Route 是编译后的不可变路由；私有字段防止调用方绕过完整替换语义。
type Route struct {
	name     string
	protocol string
	path     string
	model    string
	pool     string
	method   string
}

// Name 返回用于安全观测的 Route 逻辑名称。
func (r Route) Name() string { return r.name }

// Protocol 返回 Route 的有限协议标识。
func (r Route) Protocol() string { return r.protocol }

// Path 返回静态下游路径，而不是客户端原始 URL。
func (r Route) Path() string { return r.path }

// Model 返回配置匹配键；调用方不得把它写入默认日志或指标。
func (r Route) Model() string { return r.model }

// Pool 返回 Route 引用的 Pool 逻辑名称。
func (r Route) Pool() string { return r.pool }

// Method 返回由静态协议路由推导的下游方法。
func (r Route) Method() string { return r.method }

type routeKey struct {
	protocol string
	path     string
}

type routeGroup struct {
	exact       map[string]Route
	wildcard    Route
	hasWildcard bool
}

// Table 在编译后不再修改，可被多个请求并发读取。
type Table struct {
	groups map[routeKey]routeGroup
	routes []Route
}

// Compile 完整校验并复制输入，精确模型与通配模型分别建立确定性索引。
func Compile(specs []RouteSpec) (*Table, error) {
	if len(specs) > MaxRoutes {
		return nil, fmt.Errorf("router: route count exceeds %d", MaxRoutes)
	}

	// 配置切片只在本函数读取，编译结果不保留任何调用方可变引用。
	copied := append([]RouteSpec(nil), specs...)
	table := &Table{
		groups: make(map[routeKey]routeGroup, len(copied)),
		routes: make([]Route, 0, len(copied)),
	}
	names := make(map[string]struct{}, len(copied))

	for index, spec := range copied {
		if !logicalName.MatchString(spec.Name) || !logicalName.MatchString(spec.Pool) {
			return nil, fmt.Errorf("router: route %d has invalid logical name", index)
		}
		if _, exists := names[spec.Name]; exists {
			return nil, fmt.Errorf("router: route %d has duplicate name", index)
		}
		names[spec.Name] = struct{}{}

		expectedPath, ok := protocolPath(spec.Protocol)
		if !ok || spec.Path != expectedPath {
			return nil, fmt.Errorf("router: route %d has invalid protocol or path", index)
		}
		if !coremodel.Valid(spec.Model) {
			return nil, fmt.Errorf("router: route %d has invalid model", index)
		}

		route := Route{
			name:     spec.Name,
			protocol: spec.Protocol,
			path:     spec.Path,
			model:    spec.Model,
			pool:     spec.Pool,
			method:   http.MethodPost,
		}
		key := routeKey{protocol: spec.Protocol, path: spec.Path}
		group := table.groups[key]
		if group.exact == nil {
			group.exact = make(map[string]Route)
		}

		if spec.Model == WildcardModel {
			if group.hasWildcard {
				return nil, fmt.Errorf("router: route %d duplicates wildcard route", index)
			}
			group.wildcard = route
			group.hasWildcard = true
		} else {
			if _, exists := group.exact[spec.Model]; exists {
				return nil, fmt.Errorf("router: route %d duplicates exact model route", index)
			}
			group.exact[spec.Model] = route
		}
		table.groups[key] = group
		table.routes = append(table.routes, route)
	}

	return table, nil
}

// Lookup 优先选择精确模型，再回退到相同协议和路径下的通配路由。
func (t *Table) Lookup(match Match) (Route, error) {
	if t == nil {
		return Route{}, ErrRouteNotFound
	}
	group, ok := t.groups[routeKey{protocol: match.Protocol, path: match.Path}]
	if !ok {
		return Route{}, ErrRouteNotFound
	}
	if route, ok := group.exact[match.Model]; ok {
		return route, nil
	}
	if group.hasWildcard {
		return group.wildcard, nil
	}
	return Route{}, ErrRouteNotFound
}

// Routes 返回配置顺序的副本，供装配层校验全部 Pool 引用。
func (t *Table) Routes() []Route {
	if t == nil {
		return nil
	}
	return append([]Route(nil), t.routes...)
}

// Len 返回路由数量；空表是合法的 core 数据结构，运行模式约束由配置层负责。
func (t *Table) Len() int {
	if t == nil {
		return 0
	}
	return len(t.routes)
}

// protocolPath 将有限协议映射到唯一静态下游路径，拒绝客户端自定义目标。
func protocolPath(protocol string) (string, bool) {
	switch protocol {
	case ProtocolOpenAI:
		return OpenAIChatCompletionsPath, true
	case ProtocolAnthropic:
		return AnthropicMessagesPath, true
	default:
		return "", false
	}
}
