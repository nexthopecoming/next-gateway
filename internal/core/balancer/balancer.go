// Package balancer 在不可变 Endpoint 配置上进行进程内负载均衡。
// 本包只保存逻辑名称和已校验 origin，不提供健康检查、重试或跨实例状态。
package balancer

import (
	"errors"
	"fmt"
	"math"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
)

// Policy 是 Pool 支持的有限负载均衡算法。
type Policy string

const (
	PolicyRoundRobin         Policy = "round_robin"
	PolicyWeightedRoundRobin Policy = "weighted_round_robin"

	// MaxEndpointsPerPool 限制单个 Pool 的 Endpoint 数量和观测标签集合。
	MaxEndpointsPerPool = 256
	// MaxWeight 限制平滑加权轮询权重，避免无界状态增长。
	MaxWeight = 1000
)

var (
	// ErrUnavailable 表示运行期无法从 Pool 取得 Endpoint。
	ErrUnavailable = errors.New("upstream_unavailable")
	logicalName    = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)
)

// EndpointSpec 是一个静态下游 origin 配置；URL 不允许携带路径、查询或凭据。
type EndpointSpec struct {
	Name   string
	URL    string
	Weight int
}

// PoolSpec 是尚未编译的 Pool 配置。
type PoolSpec struct {
	Name      string
	Policy    Policy
	Endpoints []EndpointSpec
}

// Endpoint 是不可变的已校验 Endpoint，origin 已规范化且不含尾斜杠。
type Endpoint struct {
	name   string
	origin string
	weight int
}

// Name 返回用于安全观测的逻辑名称。
func (e Endpoint) Name() string { return e.name }

// Origin 返回已校验的 HTTP(S) origin，不包含路径、查询、片段或 userinfo。
func (e Endpoint) Origin() string { return e.origin }

// Weight 返回该 Endpoint 的静态有效权重。
func (e Endpoint) Weight() int { return e.weight }

// Pool 的 Endpoint 配置只读，轮询游标或平滑权重状态独立支持并发选择。
type Pool struct {
	name      string
	policy    Policy
	endpoints []Endpoint

	next            atomic.Uint64
	roundRobinReset uint64
	weightedMu      sync.Mutex
	current         []int64
	totalWeight     int64
}

// NewPool 深拷贝并完整校验配置，调用方之后修改输入不会改变 Pool。
func NewPool(spec PoolSpec) (*Pool, error) {
	if !logicalName.MatchString(spec.Name) {
		return nil, errors.New("balancer: invalid pool name")
	}
	if spec.Policy != PolicyRoundRobin && spec.Policy != PolicyWeightedRoundRobin {
		return nil, errors.New("balancer: invalid policy")
	}
	if len(spec.Endpoints) == 0 || len(spec.Endpoints) > MaxEndpointsPerPool {
		return nil, fmt.Errorf("balancer: endpoint count must be 1..%d", MaxEndpointsPerPool)
	}

	copied := append([]EndpointSpec(nil), spec.Endpoints...)
	endpointCount := uint64(len(copied))
	pool := &Pool{
		name:      spec.Name,
		policy:    spec.Policy,
		endpoints: make([]Endpoint, 0, len(copied)),
		// 游标只在 [0, reset) 内移动，reset 是 Endpoint 数量的倍数。
		roundRobinReset: math.MaxUint64 - math.MaxUint64%endpointCount,
	}
	if spec.Policy == PolicyWeightedRoundRobin {
		pool.current = make([]int64, len(copied))
	}

	names := make(map[string]struct{}, len(copied))
	origins := make(map[string]struct{}, len(copied))
	for index, endpoint := range copied {
		if !logicalName.MatchString(endpoint.Name) {
			return nil, fmt.Errorf("balancer: endpoint %d has invalid name", index)
		}
		if _, exists := names[endpoint.Name]; exists {
			return nil, fmt.Errorf("balancer: endpoint %d has duplicate name", index)
		}
		names[endpoint.Name] = struct{}{}

		if endpoint.Weight < 1 || endpoint.Weight > MaxWeight {
			return nil, fmt.Errorf("balancer: endpoint %d has invalid weight", index)
		}
		if spec.Policy == PolicyRoundRobin && endpoint.Weight != 1 {
			return nil, fmt.Errorf("balancer: endpoint %d must have weight 1 for round_robin", index)
		}

		// 原始 URL 只用于构造规范化值，校验错误一律按索引报告，避免泄露 userinfo。
		origin, err := normalizeOrigin(endpoint.URL)
		if err != nil {
			return nil, fmt.Errorf("balancer: endpoint %d has invalid origin", index)
		}
		if _, exists := origins[origin]; exists {
			return nil, fmt.Errorf("balancer: endpoint %d has duplicate origin", index)
		}
		origins[origin] = struct{}{}

		pool.endpoints = append(pool.endpoints, Endpoint{
			name:   endpoint.Name,
			origin: origin,
			weight: endpoint.Weight,
		})
		pool.totalWeight += int64(endpoint.Weight)
	}
	return pool, nil
}

// Name 返回 Pool 逻辑名称。
func (p *Pool) Name() string {
	if p == nil {
		return ""
	}
	return p.name
}

// Policy 返回 Pool 的有限负载均衡策略。
func (p *Pool) Policy() Policy {
	if p == nil {
		return ""
	}
	return p.policy
}

// EndpointCount 返回静态 Endpoint 数量。
func (p *Pool) EndpointCount() int {
	if p == nil {
		return 0
	}
	return len(p.endpoints)
}

// Select 每次只选择一个 Endpoint；调用方不得据此实现隐式重试。
func (p *Pool) Select() (Endpoint, error) {
	if p == nil || len(p.endpoints) == 0 {
		return Endpoint{}, ErrUnavailable
	}

	if p.policy == PolicyRoundRobin {
		count := uint64(len(p.endpoints))
		for {
			current := p.next.Load()
			next := current + 1
			// 在最大的 count 倍数处归零，避免 uint64 回绕破坏模序列。
			if next >= p.roundRobinReset {
				next = 0
			}
			if p.next.CompareAndSwap(current, next) {
				return p.endpoints[current%count], nil
			}
		}
	}
	if p.policy != PolicyWeightedRoundRobin {
		return Endpoint{}, ErrUnavailable
	}

	// 平滑 WRR 在同一临界区内完成累加、选择和扣减，确保并发下总量比例准确。
	p.weightedMu.Lock()
	defer p.weightedMu.Unlock()
	selected := 0
	for index := range p.endpoints {
		p.current[index] += int64(p.endpoints[index].weight)
		if p.current[index] > p.current[selected] {
			selected = index
		}
	}
	p.current[selected] -= p.totalWeight
	return p.endpoints[selected], nil
}

// normalizeOrigin 将等价 origin 收敛为同一形式，并丢弃默认端口和根路径尾斜杠。
func normalizeOrigin(raw string) (string, error) {
	if raw == "" || strings.TrimSpace(raw) != raw {
		return "", errors.New("invalid origin")
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return "", errors.New("invalid origin")
	}
	scheme := strings.ToLower(parsed.Scheme)
	if scheme != "http" && scheme != "https" {
		return "", errors.New("invalid origin")
	}
	if parsed.Opaque != "" || parsed.User != nil || parsed.Host == "" || strings.HasSuffix(parsed.Host, ":") {
		return "", errors.New("invalid origin")
	}
	if strings.Contains(raw, "#") {
		return "", errors.New("invalid origin")
	}
	if (parsed.Path != "" && parsed.Path != "/") || parsed.RawPath != "" {
		return "", errors.New("invalid origin")
	}
	if parsed.ForceQuery || parsed.RawQuery != "" || parsed.Fragment != "" || parsed.RawFragment != "" {
		return "", errors.New("invalid origin")
	}

	hostname := parsed.Hostname()
	if address, err := netip.ParseAddr(hostname); err == nil {
		// netip 统一 IPv4/IPv6 字面量，且保留 IPv6 zone 及其大小写。
		hostname = address.String()
	} else {
		hostname = strings.ToLower(hostname)
	}
	if hostname == "" {
		return "", errors.New("invalid origin")
	}
	port := parsed.Port()
	if port != "" {
		number, err := strconv.Atoi(port)
		if err != nil || number < 1 || number > 65535 {
			return "", errors.New("invalid origin")
		}
		if (scheme == "http" && number == 80) || (scheme == "https" && number == 443) {
			port = ""
		} else {
			// 非默认端口也使用十进制规范形式，避免前导零绕过重复 origin 校验。
			port = strconv.Itoa(number)
		}
	}

	var host string
	if strings.Contains(hostname, ":") {
		host = "[" + hostname + "]"
	} else {
		host = hostname
	}
	if port != "" {
		host = net.JoinHostPort(hostname, port)
	}
	// URL.String 会重新转义 IPv6 zone 中的 '%'，保证返回值可再次构造请求。
	origin := (&url.URL{Scheme: scheme, Host: host}).String()
	// 最终值必须通过实际消费者的构造校验，拒绝编码后仍非法的 IP zone 等边界。
	if _, err := http.NewRequest(http.MethodPost, origin+"/", nil); err != nil {
		return "", errors.New("invalid origin")
	}
	return origin, nil
}
