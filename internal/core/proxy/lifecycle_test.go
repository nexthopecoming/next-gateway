package proxy_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/nexthopecoming/next-gateway/internal/core/balancer"
	"github.com/nexthopecoming/next-gateway/internal/core/proxy"
	"github.com/nexthopecoming/next-gateway/internal/core/router"
)

func TestFinishedCallerContextDoesNotAdvanceBalancer(t *testing.T) {
	for _, test := range []struct {
		name    string
		policy  balancer.Policy
		weights []int
		expired bool
	}{
		{name: "round robin canceled", policy: balancer.PolicyRoundRobin, weights: []int{1, 1}},
		{name: "weighted round robin canceled", policy: balancer.PolicyWeightedRoundRobin, weights: []int{2, 1}},
		{name: "round robin deadline", policy: balancer.PolicyRoundRobin, weights: []int{1, 1}, expired: true},
		{name: "weighted round robin deadline", policy: balancer.PolicyWeightedRoundRobin, weights: []int{2, 1}, expired: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			backendA := newBackend(t, "a", nil)
			backendB := newBackend(t, "b", nil)
			dispatcher := newDispatcher(t, openAIRoutes(), []*balancer.Pool{
				newPool(t, "pool", test.policy, []balancer.EndpointSpec{
					{Name: "a", URL: backendA.server.URL, Weight: test.weights[0]},
					{Name: "b", URL: backendB.server.URL, Weight: test.weights[1]},
				}),
			}, proxy.DefaultTransportConfig())
			ctx, cancel := context.WithCancel(context.Background())
			if test.expired {
				var cancelDeadline context.CancelFunc
				ctx, cancelDeadline = context.WithDeadline(ctx, time.Now().Add(-time.Second))
				defer cancelDeadline()
			}
			cancel()
			selection, response, err := dispatcher.Do(ctx, proxy.Request{Match: openAIMatch("model")})
			wantError := context.Canceled
			if test.expired {
				wantError = context.DeadlineExceeded
			}
			if err != wantError || response != nil || selection != (proxy.Selection{}) {
				t.Fatalf("已结束调用方上下文未最早短路: selection=%+v response=%v err=%v", selection, response, err)
			}
			if backendA.requests.Load() != 0 || backendB.requests.Load() != 0 {
				t.Fatal("已结束请求访问了后端")
			}

			selection, response, err = dispatcher.Do(context.Background(), proxy.Request{Match: openAIMatch("model")})
			if err != nil {
				t.Fatal(err)
			}
			closeResponse(t, response)
			if selection.Endpoint != "a" {
				t.Fatalf("已结束请求推进了负载均衡游标: %+v", selection)
			}
		})
	}
}

func TestOverloadedRequestDoesNotAdvanceBalancerAndCloseReleasesBudget(t *testing.T) {
	for _, test := range []struct {
		name    string
		policy  balancer.Policy
		weights []int
	}{
		{name: "round robin", policy: balancer.PolicyRoundRobin, weights: []int{1, 1}},
		{name: "weighted round robin", policy: balancer.PolicyWeightedRoundRobin, weights: []int{2, 1}},
	} {
		t.Run(test.name, func(t *testing.T) {
			backendA := newBackend(t, "a", func(w http.ResponseWriter, request *http.Request) {
				w.WriteHeader(http.StatusOK)
				w.(http.Flusher).Flush()
				<-request.Context().Done()
			})
			backendB := newBackend(t, "b", nil)
			config := proxy.DefaultTransportConfig()
			config.MaxInFlightRequests = 1
			dispatcher := newDispatcher(t, openAIRoutes(), []*balancer.Pool{
				newPool(t, "pool", test.policy, []balancer.EndpointSpec{
					{Name: "a", URL: backendA.server.URL, Weight: test.weights[0]},
					{Name: "b", URL: backendB.server.URL, Weight: test.weights[1]},
				}),
			}, config)

			firstSelection, first, err := dispatcher.Do(context.Background(), proxy.Request{Match: openAIMatch("model")})
			if err != nil || firstSelection.Endpoint != "a" {
				t.Fatalf("首个请求失败: selection=%+v err=%v", firstSelection, err)
			}
			selection, response, err := dispatcher.Do(context.Background(), proxy.Request{Match: openAIMatch("model")})
			if !errors.Is(err, proxy.ErrOverloaded) || response != nil || selection != (proxy.Selection{}) {
				t.Fatalf("在途预算拒绝结果错误: selection=%+v response=%v err=%v", selection, response, err)
			}
			if backendA.requests.Load() != 1 || backendB.requests.Load() != 0 {
				t.Fatalf("过载请求访问了后端: a=%d b=%d", backendA.requests.Load(), backendB.requests.Load())
			}
			if err := first.Body.Close(); err != nil {
				t.Fatal(err)
			}

			// 过载请求不消耗轮次，因此 Close 释放额度后应命中原本的第二顺位 B。
			selection, response, err = dispatcher.Do(context.Background(), proxy.Request{Match: openAIMatch("model")})
			if err != nil {
				t.Fatal(err)
			}
			closeResponse(t, response)
			if selection.Endpoint != "b" {
				t.Fatalf("过载请求推进了负载均衡游标: %+v", selection)
			}
		})
	}
}

func TestInFlightBudgetReleasedOnFailurePaths(t *testing.T) {
	t.Run("route miss", func(t *testing.T) {
		backend := newBackend(t, "a", nil)
		table := compileTable(t, router.RouteSpec{
			Name: "exact", Protocol: router.ProtocolOpenAI, Path: router.OpenAIChatCompletionsPath,
			Model: "known", Pool: "pool",
		})
		config := proxy.DefaultTransportConfig()
		config.MaxInFlightRequests = 1
		dispatcher := newDispatcherWithTable(t, table, []*balancer.Pool{
			newPool(t, "pool", balancer.PolicyRoundRobin, []balancer.EndpointSpec{{Name: "a", URL: backend.server.URL, Weight: 1}}),
		}, config)
		for range 2 {
			_, response, err := dispatcher.Do(context.Background(), proxy.Request{Match: openAIMatch("other")})
			if !errors.Is(err, router.ErrRouteNotFound) || response != nil {
				t.Fatalf("Route 未命中后额度未释放: response=%v err=%v", response, err)
			}
		}
	})

	t.Run("non-2xx response", func(t *testing.T) {
		var backend *testBackend
		backend = newBackend(t, "a", func(w http.ResponseWriter, _ *http.Request) {
			if backend.requests.Load() == 1 {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			_, _ = io.WriteString(w, `{}`)
		})
		config := proxy.DefaultTransportConfig()
		config.MaxInFlightRequests = 1
		dispatcher := newDispatcher(t, openAIRoutes(), []*balancer.Pool{
			newPool(t, "pool", balancer.PolicyRoundRobin, []balancer.EndpointSpec{{Name: "a", URL: backend.server.URL, Weight: 1}}),
		}, config)
		if _, response, err := dispatcher.Do(context.Background(), proxy.Request{Match: openAIMatch("model")}); !errors.Is(err, proxy.ErrUpstream) || response != nil {
			t.Fatalf("非 2xx 结果错误: response=%v err=%v", response, err)
		}
		_, response, err := dispatcher.Do(context.Background(), proxy.Request{Match: openAIMatch("model")})
		if err != nil {
			t.Fatalf("非 2xx 后额度未释放: %v", err)
		}
		closeResponse(t, response)
	})

	t.Run("round trip error", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
		origin := server.URL
		server.Close()
		config := proxy.DefaultTransportConfig()
		config.MaxInFlightRequests = 1
		dispatcher := newDispatcher(t, openAIRoutes(), []*balancer.Pool{
			newPool(t, "pool", balancer.PolicyRoundRobin, []balancer.EndpointSpec{{Name: "a", URL: origin, Weight: 1}}),
		}, config)
		for range 2 {
			_, response, err := dispatcher.Do(context.Background(), proxy.Request{Match: openAIMatch("model")})
			if !errors.Is(err, proxy.ErrUpstream) || response != nil {
				t.Fatalf("RoundTrip 错误后额度未释放: response=%v err=%v", response, err)
			}
		}
	})

	t.Run("caller cancellation", func(t *testing.T) {
		started := make(chan struct{})
		var backend *testBackend
		backend = newBackend(t, "a", func(w http.ResponseWriter, request *http.Request) {
			if backend.requests.Load() == 1 {
				close(started)
				<-request.Context().Done()
				return
			}
			_, _ = io.WriteString(w, `{}`)
		})
		config := proxy.DefaultTransportConfig()
		config.MaxInFlightRequests = 1
		dispatcher := newDispatcher(t, openAIRoutes(), []*balancer.Pool{
			newPool(t, "pool", balancer.PolicyRoundRobin, []balancer.EndpointSpec{{Name: "a", URL: backend.server.URL, Weight: 1}}),
		}, config)
		ctx, cancel := context.WithCancel(context.Background())
		results := make(chan doResult, 1)
		go func() {
			selection, response, err := dispatcher.Do(ctx, proxy.Request{Match: openAIMatch("model")})
			results <- doResult{selection: selection, response: response, err: err}
		}()
		waitSignal(t, started)
		cancel()
		result := waitResult(t, results)
		if result.err != context.Canceled || result.response != nil {
			t.Fatalf("调用方取消结果错误: %+v", result)
		}
		_, response, err := dispatcher.Do(context.Background(), proxy.Request{Match: openAIMatch("model")})
		if err != nil {
			t.Fatalf("调用方取消后额度未释放: %v", err)
		}
		closeResponse(t, response)
	})
}
