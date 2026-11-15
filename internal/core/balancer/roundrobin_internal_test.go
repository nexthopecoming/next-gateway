package balancer

import (
	"fmt"
	"sync"
	"testing"
)

func TestRoundRobinPreservesSequenceAcrossAlignedReset(t *testing.T) {
	tests := []struct {
		name  string
		count int
		want  []string
	}{
		{name: "one endpoint", count: 1, want: []string{"a", "a", "a", "a", "a"}},
		{name: "two endpoints", count: 2, want: []string{"a", "b", "a", "b", "a"}},
		{name: "three endpoints", count: 3, want: []string{"b", "c", "a", "b", "c"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			pool := newInternalPool(t, test.count)
			pool.next.Store(pool.roundRobinReset - 2)
			for index, want := range test.want {
				endpoint, err := pool.Select()
				if err != nil {
					t.Fatal(err)
				}
				if endpoint.Name() != want {
					t.Fatalf("跨重置第 %d 次选择 = %q，期望 %q", index, endpoint.Name(), want)
				}
			}
		})
	}
}

func TestRoundRobinConcurrentDistributionAcrossReset(t *testing.T) {
	pool := newInternalPool(t, 3)
	pool.next.Store(pool.roundRobinReset - 1200)

	const workers = 8
	const selections = 300
	results := make(chan string, workers*selections)
	errs := make(chan error, workers)
	var waitGroup sync.WaitGroup
	for range workers {
		waitGroup.Add(1)
		go func() {
			defer waitGroup.Done()
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
	// 结果 channel 容量足够，等待所有 worker 后统一收集，不依赖调度或 sleeps。
	waitGroup.Wait()
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
	if counts["a"] != 800 || counts["b"] != 800 || counts["c"] != 800 {
		t.Fatalf("跨重置并发 RR 分布错误: %+v", counts)
	}
}

func newInternalPool(t *testing.T, count int) *Pool {
	t.Helper()
	specs := make([]EndpointSpec, count)
	for index := range specs {
		specs[index] = EndpointSpec{
			Name:   fmt.Sprintf("%c", 'a'+index),
			URL:    fmt.Sprintf("http://127.0.0.1:%d", 18081+index),
			Weight: 1,
		}
	}
	pool, err := NewPool(PoolSpec{Name: "test-pool", Policy: PolicyRoundRobin, Endpoints: specs})
	if err != nil {
		t.Fatal(err)
	}
	return pool
}
