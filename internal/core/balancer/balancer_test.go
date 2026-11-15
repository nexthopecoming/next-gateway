package balancer_test

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/nexthopecoming/next-gateway/internal/core/balancer"
)

func TestRoundRobinGoldenSequence(t *testing.T) {
	pool := newPool(t, balancer.PolicyRoundRobin, []balancer.EndpointSpec{
		{Name: "a", URL: "http://127.0.0.1:18081", Weight: 1},
		{Name: "b", URL: "http://127.0.0.1:18082", Weight: 1},
	})
	if pool.Name() != "test-pool" || pool.Policy() != balancer.PolicyRoundRobin || pool.EndpointCount() != 2 {
		t.Fatalf("Pool 元数据错误: name=%q policy=%q count=%d", pool.Name(), pool.Policy(), pool.EndpointCount())
	}
	assertSequence(t, pool, []string{"a", "b", "a", "b"})
}

func TestWeightedRoundRobinGoldenSequenceAndRatio(t *testing.T) {
	pool := newPool(t, balancer.PolicyWeightedRoundRobin, []balancer.EndpointSpec{
		{Name: "a", URL: "http://127.0.0.1:18081", Weight: 2},
		{Name: "b", URL: "http://127.0.0.1:18082", Weight: 1},
	})
	assertSequence(t, pool, []string{"a", "b", "a", "a", "b", "a"})

	counts := map[string]int{}
	for range 3000 {
		endpoint, err := pool.Select()
		if err != nil {
			t.Fatal(err)
		}
		counts[endpoint.Name()]++
	}
	// 前面的 6 次调用也是 2:1，因此完整序列仍应精确保持长期比例。
	if counts["a"] != 2000 || counts["b"] != 1000 {
		t.Fatalf("平滑 WRR 长期比例错误: %+v", counts)
	}
}

func TestWeightedRoundRobinBreaksTiesByConfigurationOrder(t *testing.T) {
	pool := newPool(t, balancer.PolicyWeightedRoundRobin, []balancer.EndpointSpec{
		{Name: "a", URL: "http://127.0.0.1:18081", Weight: 1},
		{Name: "b", URL: "http://127.0.0.1:18082", Weight: 1},
		{Name: "c", URL: "http://127.0.0.1:18083", Weight: 1},
	})
	assertSequence(t, pool, []string{"a", "b", "c", "a", "b", "c"})
}

func TestNewPoolCopiesAndNormalizesConfiguration(t *testing.T) {
	spec := balancer.PoolSpec{
		Name:   "copy-pool",
		Policy: balancer.PolicyWeightedRoundRobin,
		Endpoints: []balancer.EndpointSpec{
			{Name: "a", URL: "HTTP://Example.COM:80/", Weight: balancer.MaxWeight},
			{Name: "b", URL: "https://Example.COM:443/", Weight: 1},
		},
	}
	pool, err := balancer.NewPool(spec)
	if err != nil {
		t.Fatal(err)
	}
	spec.Name = "changed"
	spec.Endpoints[0] = balancer.EndpointSpec{Name: "changed", URL: "http://changed.local", Weight: 1}

	endpoint, err := pool.Select()
	if err != nil {
		t.Fatal(err)
	}
	if pool.Name() != "copy-pool" || endpoint.Name() != "a" || endpoint.Origin() != "http://example.com" || endpoint.Weight() != balancer.MaxWeight {
		t.Fatalf("Pool 未保留深拷贝或规范化值: pool=%q endpoint=%+v origin=%q", pool.Name(), endpoint, endpoint.Origin())
	}
}

func TestNormalizedOriginCanConstructRequest(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want string
	}{
		{name: "IPv6 zone", raw: "http://[FE80::1%25Lo0]", want: "http://[fe80::1%25Lo0]"},
		{name: "expanded IPv6", raw: "http://[0:0:0:0:0:0:0:1]", want: "http://[::1]"},
		{name: "IPv6 default port", raw: "http://[::1]:080/", want: "http://[::1]"},
		{name: "non default port leading zero", raw: "http://Example.com:08080/", want: "http://example.com:8080"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			pool := newPool(t, balancer.PolicyRoundRobin, []balancer.EndpointSpec{{Name: "a", URL: test.raw, Weight: 1}})
			endpoint, err := pool.Select()
			if err != nil {
				t.Fatal(err)
			}
			if endpoint.Origin() != test.want {
				t.Fatalf("规范化 origin = %q，期望 %q", endpoint.Origin(), test.want)
			}
			if _, err := http.NewRequest(http.MethodPost, endpoint.Origin()+"/v1/messages", nil); err != nil {
				t.Fatalf("规范化 origin 无法构造下游请求: %v", err)
			}
		})
	}
}

func TestNewPoolRejectsInvalidOriginsWithoutLeakingURL(t *testing.T) {
	invalid := []string{
		"",
		" http://example.com",
		"ftp://example.com",
		"http:example.com",
		"http:///missing-host",
		"http://example.com/path",
		"http://example.com/%2f",
		"http://example.com?query=secret",
		"http://example.com?",
		"http://example.com#fragment",
		"http://example.com#",
		"http://example.com:",
		"http://secret-user:secret-password@example.com",
		"http://example.com:0",
		"http://example.com:65536",
		"http://example.com:not-a-port",
		"http://[fe80::1%25%5B]",
	}
	for _, raw := range invalid {
		t.Run(raw, func(t *testing.T) {
			_, err := balancer.NewPool(balancer.PoolSpec{
				Name:      "pool",
				Policy:    balancer.PolicyRoundRobin,
				Endpoints: []balancer.EndpointSpec{{Name: "a", URL: raw, Weight: 1}},
			})
			if err == nil {
				t.Fatalf("非法 origin 未拒绝: %q", raw)
			}
			for _, secret := range []string{"secret-user", "secret-password", "query=secret", "fe80::1", "%5B"} {
				if strings.Contains(err.Error(), secret) {
					t.Fatalf("配置错误泄露原始 URL 或 zone: %v", err)
				}
			}
			if raw != "" && strings.Contains(err.Error(), raw) {
				t.Fatalf("配置错误包含完整原始 URL: %v", err)
			}
		})
	}
}

func TestNewPoolRejectsDuplicatesAndInvalidConfiguration(t *testing.T) {
	tests := []struct {
		name string
		spec balancer.PoolSpec
	}{
		{
			name: "empty pool",
			spec: balancer.PoolSpec{Name: "pool", Policy: balancer.PolicyRoundRobin},
		},
		{
			name: "invalid pool name",
			spec: balancer.PoolSpec{Name: "Pool", Policy: balancer.PolicyRoundRobin, Endpoints: endpoints(1, 1)},
		},
		{
			name: "unknown policy",
			spec: balancer.PoolSpec{Name: "pool", Policy: "random", Endpoints: endpoints(1, 1)},
		},
		{
			name: "duplicate endpoint name",
			spec: balancer.PoolSpec{Name: "pool", Policy: balancer.PolicyRoundRobin, Endpoints: []balancer.EndpointSpec{
				{Name: "same", URL: "http://127.0.0.1:18081", Weight: 1},
				{Name: "same", URL: "http://127.0.0.1:18082", Weight: 1},
			}},
		},
		{
			name: "equivalent origins",
			spec: balancer.PoolSpec{Name: "pool", Policy: balancer.PolicyRoundRobin, Endpoints: []balancer.EndpointSpec{
				{Name: "a", URL: "http://EXAMPLE.com", Weight: 1},
				{Name: "b", URL: "HTTP://example.COM:80/", Weight: 1},
			}},
		},
		{
			name: "equivalent expanded IPv6",
			spec: balancer.PoolSpec{Name: "pool", Policy: balancer.PolicyRoundRobin, Endpoints: []balancer.EndpointSpec{
				{Name: "a", URL: "http://[::1]", Weight: 1},
				{Name: "b", URL: "http://[0:0:0:0:0:0:0:1]/", Weight: 1},
			}},
		},
		{
			name: "equivalent default port with leading zero",
			spec: balancer.PoolSpec{Name: "pool", Policy: balancer.PolicyRoundRobin, Endpoints: []balancer.EndpointSpec{
				{Name: "a", URL: "http://example.com", Weight: 1},
				{Name: "b", URL: "http://example.com:080/", Weight: 1},
			}},
		},
		{
			name: "equivalent non default port with leading zero",
			spec: balancer.PoolSpec{Name: "pool", Policy: balancer.PolicyRoundRobin, Endpoints: []balancer.EndpointSpec{
				{Name: "a", URL: "http://example.com:8080", Weight: 1},
				{Name: "b", URL: "http://example.com:08080/", Weight: 1},
			}},
		},
		{
			name: "round robin ignores no weight",
			spec: balancer.PoolSpec{Name: "pool", Policy: balancer.PolicyRoundRobin, Endpoints: []balancer.EndpointSpec{
				{Name: "a", URL: "http://127.0.0.1:18081", Weight: 2},
			}},
		},
		{
			name: "zero weight",
			spec: balancer.PoolSpec{Name: "pool", Policy: balancer.PolicyWeightedRoundRobin, Endpoints: []balancer.EndpointSpec{
				{Name: "a", URL: "http://127.0.0.1:18081", Weight: 0},
			}},
		},
		{
			name: "excessive weight",
			spec: balancer.PoolSpec{Name: "pool", Policy: balancer.PolicyWeightedRoundRobin, Endpoints: []balancer.EndpointSpec{
				{Name: "a", URL: "http://127.0.0.1:18081", Weight: balancer.MaxWeight + 1},
			}},
		},
		{
			name: "too many endpoints",
			spec: balancer.PoolSpec{Name: "pool", Policy: balancer.PolicyRoundRobin, Endpoints: endpoints(balancer.MaxEndpointsPerPool+1, 1)},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if pool, err := balancer.NewPool(test.spec); err == nil || pool != nil {
				t.Fatalf("非法 Pool 未拒绝: pool=%v err=%v", pool, err)
			}
		})
	}

	if _, err := balancer.NewPool(balancer.PoolSpec{
		Name: "pool", Policy: balancer.PolicyRoundRobin, Endpoints: endpoints(balancer.MaxEndpointsPerPool, 1),
	}); err != nil {
		t.Fatalf("最大合法 Endpoint 数量被拒绝: %v", err)
	}
}

func TestConcurrentSelectionDistribution(t *testing.T) {
	t.Run("round robin", func(t *testing.T) {
		pool := newPool(t, balancer.PolicyRoundRobin, []balancer.EndpointSpec{
			{Name: "a", URL: "http://127.0.0.1:18081", Weight: 1},
			{Name: "b", URL: "http://127.0.0.1:18082", Weight: 1},
		})
		counts := selectConcurrently(t, pool, 16, 500)
		if counts["a"] != 4000 || counts["b"] != 4000 {
			t.Fatalf("并发 RR 分布错误: %+v", counts)
		}
	})

	t.Run("weighted round robin", func(t *testing.T) {
		pool := newPool(t, balancer.PolicyWeightedRoundRobin, []balancer.EndpointSpec{
			{Name: "a", URL: "http://127.0.0.1:18081", Weight: 2},
			{Name: "b", URL: "http://127.0.0.1:18082", Weight: 1},
		})
		counts := selectConcurrently(t, pool, 16, 300)
		if counts["a"] != 3200 || counts["b"] != 1600 {
			t.Fatalf("并发 WRR 分布错误: %+v", counts)
		}
	})
}

func TestNilPoolReturnsUnavailable(t *testing.T) {
	var pool *balancer.Pool
	if _, err := pool.Select(); !errors.Is(err, balancer.ErrUnavailable) {
		t.Fatalf("nil Pool 错误 = %v", err)
	}
	if pool.Name() != "" || pool.Policy() != "" || pool.EndpointCount() != 0 {
		t.Fatal("nil Pool 只读访问器应返回零值")
	}
}

func newPool(t *testing.T, policy balancer.Policy, specs []balancer.EndpointSpec) *balancer.Pool {
	t.Helper()
	pool, err := balancer.NewPool(balancer.PoolSpec{Name: "test-pool", Policy: policy, Endpoints: specs})
	if err != nil {
		t.Fatal(err)
	}
	return pool
}

func assertSequence(t *testing.T, pool *balancer.Pool, want []string) {
	t.Helper()
	for index, name := range want {
		endpoint, err := pool.Select()
		if err != nil {
			t.Fatal(err)
		}
		if endpoint.Name() != name {
			t.Fatalf("第 %d 次选择 = %q，期望 %q", index, endpoint.Name(), name)
		}
	}
}

func selectConcurrently(t *testing.T, pool *balancer.Pool, workers, selections int) map[string]int {
	t.Helper()
	results := make(chan string, workers*selections)
	errs := make(chan error, workers)
	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range selections {
				endpoint, err := pool.Select()
				if err != nil {
					errs <- err
					return
				}
				results <- endpoint.Name()
			}
		}()
	}
	wg.Wait()
	close(results)
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	counts := map[string]int{}
	for name := range results {
		counts[name]++
	}
	return counts
}

func endpoints(count, weight int) []balancer.EndpointSpec {
	specs := make([]balancer.EndpointSpec, count)
	for index := range specs {
		specs[index] = balancer.EndpointSpec{
			Name:   fmt.Sprintf("endpoint-%03d", index),
			URL:    fmt.Sprintf("http://127.0.0.1:%d", 10001+index),
			Weight: weight,
		}
	}
	return specs
}
