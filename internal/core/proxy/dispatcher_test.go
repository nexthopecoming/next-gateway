package proxy_test

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nexthopecoming/next-gateway/internal/core/balancer"
	"github.com/nexthopecoming/next-gateway/internal/core/proxy"
	"github.com/nexthopecoming/next-gateway/internal/core/router"
)

type receivedRequest struct {
	method        string
	requestURI    string
	host          string
	header        http.Header
	body          string
	contentLength int64
}

type testBackend struct {
	name     string
	server   *httptest.Server
	requests atomic.Int64
	received chan receivedRequest
}

func TestDispatcherOpenAIRoundRobinOverRealTCP(t *testing.T) {
	backendA := newBackend(t, "a", nil)
	backendB := newBackend(t, "b", nil)
	dispatcher := newDispatcher(t, []router.RouteSpec{openAIRoute("openai", "pool")},
		[]*balancer.Pool{newPool(t, "pool", balancer.PolicyRoundRobin, []balancer.EndpointSpec{
			{Name: "a", URL: backendA.server.URL, Weight: 1},
			{Name: "b", URL: backendB.server.URL, Weight: 1},
		})}, proxy.DefaultTransportConfig())

	for index, expected := range []string{"a", "b", "a", "b"} {
		body := fmt.Sprintf(`{"request":%d}`, index)
		selection, response, err := dispatcher.Do(context.Background(), proxy.Request{
			Match:  openAIMatch("model-a"),
			Header: http.Header{"Content-Type": []string{"application/json"}},
			Body:   []byte(body),
		})
		if err != nil {
			t.Fatal(err)
		}
		if selection != (proxy.Selection{Route: "openai", Pool: "pool", Endpoint: expected}) {
			t.Fatalf("第 %d 次 Selection = %+v，期望 Endpoint %q", index, selection, expected)
		}
		if response.Header.Get("X-Backend") != expected {
			t.Fatalf("实际后端与 Selection 不一致: %+v", selection)
		}
		closeResponse(t, response)

		request := receiveRequest(t, backendA, backendB)
		if request.method != http.MethodPost || request.requestURI != router.OpenAIChatCompletionsPath {
			t.Fatalf("下游方法或路径错误: method=%q uri=%q", request.method, request.requestURI)
		}
		expectedHost := strings.TrimPrefix(map[string]string{"a": backendA.server.URL, "b": backendB.server.URL}[expected], "http://")
		if request.host != expectedHost {
			t.Fatalf("下游 Host = %q，期望 %q", request.host, expectedHost)
		}
		if request.body != body || request.contentLength != int64(len(body)) {
			t.Fatalf("下游 Body/Content-Length 错误: body=%q length=%d", request.body, request.contentLength)
		}
	}
	if backendA.requests.Load() != 2 || backendB.requests.Load() != 2 {
		t.Fatalf("RR 实际网络分布错误: a=%d b=%d", backendA.requests.Load(), backendB.requests.Load())
	}
}

func TestDispatcherAnthropicWeightedRoundRobin(t *testing.T) {
	backendA := newBackend(t, "a", nil)
	backendB := newBackend(t, "b", nil)
	dispatcher := newDispatcher(t, []router.RouteSpec{anthropicRoute("anthropic", "weighted-pool")},
		[]*balancer.Pool{newPool(t, "weighted-pool", balancer.PolicyWeightedRoundRobin, []balancer.EndpointSpec{
			{Name: "a", URL: backendA.server.URL, Weight: 2},
			{Name: "b", URL: backendB.server.URL, Weight: 1},
		})}, proxy.DefaultTransportConfig())

	for index, expected := range []string{"a", "b", "a", "a", "b", "a"} {
		selection, response, err := dispatcher.Do(context.Background(), proxy.Request{
			Match: anthropicMatch("claude-test"),
			Body:  []byte(`{"messages":[]}`),
		})
		if err != nil {
			t.Fatal(err)
		}
		if selection.Endpoint != expected || response.Header.Get("X-Backend") != expected {
			t.Fatalf("第 %d 次 WRR 命中错误: selection=%+v backend=%q", index, selection, response.Header.Get("X-Backend"))
		}
		closeResponse(t, response)
		request := receiveRequest(t, backendA, backendB)
		if request.requestURI != router.AnthropicMessagesPath || request.method != http.MethodPost {
			t.Fatalf("Anthropic 静态路径错误: %+v", request)
		}
	}
	if backendA.requests.Load() != 4 || backendB.requests.Load() != 2 {
		t.Fatalf("WRR 实际网络分布错误: a=%d b=%d", backendA.requests.Load(), backendB.requests.Load())
	}
}

func TestDispatcherRouteMissDoesNotCallBackend(t *testing.T) {
	backend := newBackend(t, "a", nil)
	table := compileTable(t, router.RouteSpec{
		Name: "exact", Protocol: router.ProtocolOpenAI, Path: router.OpenAIChatCompletionsPath, Model: "known", Pool: "pool",
	})
	dispatcher := newDispatcherWithTable(t, table, []*balancer.Pool{
		newPool(t, "pool", balancer.PolicyRoundRobin, []balancer.EndpointSpec{{Name: "a", URL: backend.server.URL, Weight: 1}}),
	}, proxy.DefaultTransportConfig())

	selection, response, err := dispatcher.Do(context.Background(), proxy.Request{Match: openAIMatch("secret-model")})
	if !errors.Is(err, router.ErrRouteNotFound) || response != nil || selection != (proxy.Selection{}) {
		t.Fatalf("Route 未命中结果错误: selection=%+v response=%v err=%v", selection, response, err)
	}
	if strings.Contains(err.Error(), "secret-model") || backend.requests.Load() != 0 {
		t.Fatalf("Route 未命中泄露模型或访问后端: err=%v count=%d", err, backend.requests.Load())
	}
}

func TestDispatcherDoesNotRetryOrFollowRedirect(t *testing.T) {
	t.Run("500 is not retried", func(t *testing.T) {
		secretBody := "secret-downstream-body"
		backendA := newBackend(t, "a", func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = io.WriteString(w, secretBody)
		})
		backendB := newBackend(t, "b", nil)
		dispatcher := twoBackendDispatcher(t, backendA, backendB)

		selection, response, err := dispatcher.Do(context.Background(), proxy.Request{Match: openAIMatch("secret-model")})
		if !errors.Is(err, proxy.ErrUpstream) || response != nil || selection.Endpoint != "a" {
			t.Fatalf("500 处理错误: selection=%+v response=%v err=%v", selection, response, err)
		}
		if backendA.requests.Load() != 1 || backendB.requests.Load() != 0 {
			t.Fatalf("500 发生重试: a=%d b=%d", backendA.requests.Load(), backendB.requests.Load())
		}
		for _, leaked := range []string{secretBody, backendA.server.URL, "secret-model"} {
			if strings.Contains(err.Error(), leaked) {
				t.Fatalf("有限错误泄露下游信息: %v", err)
			}
		}
	})

	t.Run("302 does not visit location", func(t *testing.T) {
		backendB := newBackend(t, "b", nil)
		var backendA *testBackend
		backendA = newBackend(t, "a", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Location", backendB.server.URL+"/redirect-target")
			w.WriteHeader(http.StatusFound)
		})
		dispatcher := twoBackendDispatcher(t, backendA, backendB)

		selection, response, err := dispatcher.Do(context.Background(), proxy.Request{Match: openAIMatch("model")})
		if !errors.Is(err, proxy.ErrUpstream) || response != nil || selection.Endpoint != "a" {
			t.Fatalf("302 处理错误: selection=%+v response=%v err=%v", selection, response, err)
		}
		if backendA.requests.Load() != 1 || backendB.requests.Load() != 0 {
			t.Fatalf("302 被跟随或重试: a=%d b=%d", backendA.requests.Load(), backendB.requests.Load())
		}
	})
}

func TestDispatcherDoesNotReplayPostBodyOnReusedConnectionFailure(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	type observation struct {
		connection int64
		body       string
	}
	observations := make(chan observation, 4)
	var connectionCount atomic.Int64
	var requestCount atomic.Int64
	serverDone := make(chan struct{})
	go func() {
		defer close(serverDone)
		for {
			connection, err := listener.Accept()
			if err != nil {
				return
			}
			connectionID := connectionCount.Add(1)
			reader := bufio.NewReader(connection)
			for {
				request, err := http.ReadRequest(reader)
				if err != nil {
					_ = connection.Close()
					break
				}
				body, _ := io.ReadAll(request.Body)
				_ = request.Body.Close()
				observations <- observation{connection: connectionID, body: string(body)}
				if requestCount.Add(1) == 1 {
					// 第一个响应保留 keep-alive，第二个请求必定复用此连接。
					_, _ = io.WriteString(connection, "HTTP/1.1 200 OK\r\nContent-Length: 0\r\nConnection: keep-alive\r\n\r\n")
					continue
				}
				// 已完整收到第二个 POST 后断开；若 GetBody 未清除，Transport 会再次发送它。
				_ = connection.Close()
				break
			}
		}
	}()
	t.Cleanup(func() {
		_ = listener.Close()
		<-serverDone
	})

	dispatcher := newDispatcher(t, []router.RouteSpec{openAIRoute("openai", "pool")}, []*balancer.Pool{
		newPool(t, "pool", balancer.PolicyRoundRobin, []balancer.EndpointSpec{{
			Name: "a", URL: "http://" + listener.Addr().String(), Weight: 1,
		}}),
	}, proxy.DefaultTransportConfig())
	receive := func() observation {
		t.Helper()
		select {
		case observed := <-observations:
			return observed
		case <-time.After(5 * time.Second):
			t.Fatal("等待原始 TCP 请求超时")
			return observation{}
		}
	}

	_, response, err := dispatcher.Do(context.Background(), proxy.Request{
		Match: openAIMatch("model"), Body: []byte(`{"request":1}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	closeResponse(t, response)
	first := receive()
	_, response, err = dispatcher.Do(context.Background(), proxy.Request{
		Match: openAIMatch("model"), Body: []byte(`{"request":2}`),
	})
	if !errors.Is(err, proxy.ErrUpstream) || response != nil {
		t.Fatalf("复用连接断开后的结果错误: response=%v err=%v", response, err)
	}
	second := receive()
	if first.connection != second.connection || first.body != `{"request":1}` || second.body != `{"request":2}` {
		t.Fatalf("测试未按预期复用连接或 Body 错误: first=%+v second=%+v", first, second)
	}
	if requestCount.Load() != 2 {
		t.Fatalf("连接故障后重放了 POST: request_count=%d", requestCount.Load())
	}
}

func TestStalledErrorResponseReturnsWithoutDraining(t *testing.T) {
	started := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		w.Header().Set("Content-Length", "8192")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, "partial")
		w.(http.Flusher).Flush()
		close(started)
		// 响应尚未结束时，Do 不应等待剩余错误正文。
		<-request.Context().Done()
	}))
	t.Cleanup(server.Close)
	dispatcher := newDispatcher(t, []router.RouteSpec{openAIRoute("openai", "pool")}, []*balancer.Pool{
		newPool(t, "pool", balancer.PolicyRoundRobin, []balancer.EndpointSpec{{Name: "a", URL: server.URL, Weight: 1}}),
	}, proxy.DefaultTransportConfig())

	type result struct {
		response *http.Response
		err      error
	}
	completed := make(chan result, 1)
	go func() {
		_, response, err := dispatcher.Do(context.Background(), proxy.Request{
			Match: openAIMatch("model"), Body: []byte(`{"model":"model"}`),
		})
		completed <- result{response: response, err: err}
	}()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("等待部分错误响应超时")
	}
	select {
	case result := <-completed:
		if !errors.Is(result.err, proxy.ErrUpstream) || result.response != nil {
			t.Fatalf("停滞错误响应结果错误: response=%v err=%v", result.response, result.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Do 同步等待了停滞的错误 Body")
	}
}

type connectionIDKey struct{}

func TestErrorResponseIsNotDrainedForConnectionReuse(t *testing.T) {
	var nextConnectionID atomic.Int64
	connectionIDs := make(chan int64, 2)
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		connectionID, _ := request.Context().Value(connectionIDKey{}).(int64)
		connectionIDs <- connectionID
		w.Header().Set("Content-Length", fmt.Sprint(8192))
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write(bytes.Repeat([]byte("x"), 8192))
	}))
	server.Config.ConnContext = func(ctx context.Context, _ net.Conn) context.Context {
		return context.WithValue(ctx, connectionIDKey{}, nextConnectionID.Add(1))
	}
	server.Start()
	t.Cleanup(server.Close)
	dispatcher := newDispatcher(t, []router.RouteSpec{openAIRoute("openai", "pool")}, []*balancer.Pool{
		newPool(t, "pool", balancer.PolicyRoundRobin, []balancer.EndpointSpec{{Name: "a", URL: server.URL, Weight: 1}}),
	}, proxy.DefaultTransportConfig())

	firstID := int64(0)
	for index := range 2 {
		_, response, err := dispatcher.Do(context.Background(), proxy.Request{Match: openAIMatch("model")})
		if !errors.Is(err, proxy.ErrUpstream) || response != nil {
			t.Fatalf("第 %d 次大错误响应处理错误: response=%v err=%v", index, response, err)
		}
		select {
		case id := <-connectionIDs:
			if index == 0 {
				firstID = id
			} else if id == firstID {
				t.Fatal("错误 Body 被继续 drain 并复用了连接")
			}
		case <-time.After(5 * time.Second):
			t.Fatal("等待下游连接标识超时")
		}
	}
}

func TestDispatcherHeaderWhitelistAndConnectionNomination(t *testing.T) {
	t.Run("fixed whitelist", func(t *testing.T) {
		backend := newBackend(t, "a", nil)
		dispatcher := oneBackendDispatcher(t, backend)
		header := http.Header{}
		header.Add("Accept", "application/json")
		header.Add("Accept", "text/event-stream")
		header.Set("Content-Type", "application/json")
		header.Set("Anthropic-Version", "2023-06-01")
		header.Set("X-Request-ID", "req-safe")
		header.Set("Authorization", "Bearer secret-authorization")
		header.Set("X-Api-Key", "secret-api-key")
		header.Set("X-Tenant-ID", "secret-tenant")
		header.Set("X-Gateway-Core", "canary")
		header.Set("Forwarded", "for=secret")
		header.Set("X-Forwarded-Host", "untrusted")
		header.Set("X-Unknown", "secret-extension")
		header.Set("Idempotency-Key", "secret-idempotency")
		header.Set("Proxy-Authorization", "secret-proxy")
		header.Set("TE", "trailers")
		header.Set("Upgrade", "websocket")

		_, response, err := dispatcher.Do(context.Background(), proxy.Request{Match: openAIMatch("model"), Header: header})
		if err != nil {
			t.Fatal(err)
		}
		closeResponse(t, response)
		request := receiveRequest(t, backend)
		if got := request.header.Values("Accept"); len(got) != 2 || got[0] != "application/json" || got[1] != "text/event-stream" {
			t.Fatalf("白名单多值 Header 错误: %+v", got)
		}
		for name, expected := range map[string]string{
			"Content-Type": "application/json", "Anthropic-Version": "2023-06-01", "X-Request-ID": "req-safe",
		} {
			if request.header.Get(name) != expected {
				t.Fatalf("%s = %q，期望 %q", name, request.header.Get(name), expected)
			}
		}
		for _, name := range []string{
			"Authorization", "X-Api-Key", "X-Tenant-ID", "X-Gateway-Core", "Forwarded", "X-Forwarded-Host",
			"X-Unknown", "Idempotency-Key", "Proxy-Authorization", "TE", "Upgrade", "Connection",
		} {
			if _, exists := request.header[http.CanonicalHeaderKey(name)]; exists {
				t.Fatalf("禁止转发的 Header 到达下游: %s=%+v", name, request.header.Values(name))
			}
		}
	})

	t.Run("connection nominates whitelisted headers", func(t *testing.T) {
		backend := newBackend(t, "a", nil)
		dispatcher := oneBackendDispatcher(t, backend)
		header := http.Header{}
		header.Set("Connection", "Content-Type, X-Request-ID")
		header.Set("Content-Type", "application/json")
		header.Set("X-Request-ID", "must-be-removed")
		header.Set("Anthropic-Version", "2023-06-01")
		header.Set("Accept", "application/json")

		_, response, err := dispatcher.Do(context.Background(), proxy.Request{Match: openAIMatch("model"), Header: header})
		if err != nil {
			t.Fatal(err)
		}
		closeResponse(t, response)
		request := receiveRequest(t, backend)
		if request.header.Get("Content-Type") != "" || request.header.Get("X-Request-ID") != "" || request.header.Get("Connection") != "" {
			t.Fatalf("Connection 提名 Header 未删除: %+v", request.header)
		}
		if request.header.Get("Anthropic-Version") != "2023-06-01" || request.header.Get("Accept") != "application/json" {
			t.Fatalf("未提名白名单 Header 被误删: %+v", request.header)
		}
	})

	t.Run("non-canonical connection keys cannot bypass nomination", func(t *testing.T) {
		backend := newBackend(t, "a", nil)
		dispatcher := oneBackendDispatcher(t, backend)
		header := http.Header{
			"connection":   {"Content-Type"},
			"cOnNeCtIoN":   {"X-Request-ID"},
			"Content-Type": {"application/json"},
			"x-request-id": {"must-be-removed"},
			"aCcEpT":       {"application/json"},
		}
		_, response, err := dispatcher.Do(context.Background(), proxy.Request{Match: openAIMatch("model"), Header: header})
		if err != nil {
			t.Fatal(err)
		}
		closeResponse(t, response)
		request := receiveRequest(t, backend)
		if request.header.Get("Content-Type") != "" || request.header.Get("X-Request-ID") != "" {
			t.Fatalf("非规范 Connection 键绕过了提名过滤: %+v", request.header)
		}
		if request.header.Get("Accept") != "application/json" {
			t.Fatalf("大小写混合的合法 Header 未保留: %+v", request.header)
		}
	})
}

func TestNewDispatcherValidatesCompleteConfiguration(t *testing.T) {
	empty := compileTable(t)
	validPool := func(name string) *balancer.Pool {
		return newPool(t, name, balancer.PolicyRoundRobin, []balancer.EndpointSpec{{Name: "a", URL: "http://127.0.0.1:18081", Weight: 1}})
	}
	config := proxy.DefaultTransportConfig()

	if _, err := proxy.NewDispatcher(nil, nil, config); err == nil {
		t.Fatal("nil Route Table 未拒绝")
	}
	if _, err := proxy.NewDispatcher(empty, []*balancer.Pool{nil}, config); err == nil {
		t.Fatal("nil Pool 未拒绝")
	}
	duplicate := validPool("duplicate")
	if _, err := proxy.NewDispatcher(empty, []*balancer.Pool{duplicate, duplicate}, config); err == nil {
		t.Fatal("重复 Pool 名未拒绝")
	}
	table := compileTable(t, openAIRoute("route", "missing"))
	if _, err := proxy.NewDispatcher(table, []*balancer.Pool{validPool("present")}, config); err == nil {
		t.Fatal("悬空 Pool 引用未拒绝")
	}

	manyPools := make([]*balancer.Pool, proxy.MaxPools+1)
	for index := range manyPools {
		manyPools[index] = validPool(fmt.Sprintf("pool-%03d", index))
	}
	if _, err := proxy.NewDispatcher(empty, manyPools, config); err == nil {
		t.Fatal("Pool 总数超限未拒绝")
	}

	endpointSpecs := make([]balancer.EndpointSpec, balancer.MaxEndpointsPerPool)
	for index := range endpointSpecs {
		endpointSpecs[index] = balancer.EndpointSpec{Name: fmt.Sprintf("e-%03d", index), URL: fmt.Sprintf("http://127.0.0.1:%d", 10001+index), Weight: 1}
	}
	tooManyEndpoints := make([]*balancer.Pool, 0, 5)
	for index := range 5 {
		tooManyEndpoints = append(tooManyEndpoints, newPool(t, fmt.Sprintf("pool-%d", index), balancer.PolicyRoundRobin, endpointSpecs))
	}
	if _, err := proxy.NewDispatcher(empty, tooManyEndpoints, config); err == nil {
		t.Fatal("Endpoint 总数超限未拒绝")
	}

	exactLimit := make([]*balancer.Pool, 4)
	copy(exactLimit, tooManyEndpoints[:4])
	dispatcher, err := proxy.NewDispatcher(empty, exactLimit, config)
	if err != nil {
		t.Fatalf("合法 Endpoint 总上限被拒绝: %v", err)
	}
	dispatcher.Close()
}

type doResult struct {
	selection proxy.Selection
	response  *http.Response
	err       error
}

func newBackend(t *testing.T, name string, override http.HandlerFunc) *testBackend {
	t.Helper()
	backend := &testBackend{name: name, received: make(chan receivedRequest, 32)}
	handler := override
	if handler == nil {
		handler = func(w http.ResponseWriter, r *http.Request) {
			recordBackendRequest(backend, w, r)
		}
	} else {
		handler = func(w http.ResponseWriter, r *http.Request) {
			backend.requests.Add(1)
			override(w, r)
		}
	}
	backend.server = httptest.NewServer(http.HandlerFunc(handler))
	t.Cleanup(backend.server.Close)
	return backend
}

func recordBackendRequest(backend *testBackend, w http.ResponseWriter, r *http.Request) {
	backend.requests.Add(1)
	body, _ := io.ReadAll(r.Body)
	backend.received <- receivedRequest{
		method:        r.Method,
		requestURI:    r.URL.RequestURI(),
		host:          r.Host,
		header:        r.Header.Clone(),
		body:          string(body),
		contentLength: r.ContentLength,
	}
	w.Header().Set("X-Backend", backend.name)
	w.Header().Set("Content-Type", "application/json")
	_, _ = io.WriteString(w, `{}`)
}

func receiveRequest(t *testing.T, backends ...*testBackend) receivedRequest {
	t.Helper()
	for _, backend := range backends {
		select {
		case request := <-backend.received:
			return request
		default:
		}
	}
	t.Fatal("没有收到预期的下游请求")
	return receivedRequest{}
}

func newDispatcher(t *testing.T, routes []router.RouteSpec, pools []*balancer.Pool, config proxy.TransportConfig) *proxy.Dispatcher {
	t.Helper()
	return newDispatcherWithTable(t, compileTable(t, routes...), pools, config)
}

func newDispatcherWithTable(t *testing.T, table *router.Table, pools []*balancer.Pool, config proxy.TransportConfig) *proxy.Dispatcher {
	t.Helper()
	dispatcher, err := proxy.NewDispatcher(table, pools, config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(dispatcher.Close)
	return dispatcher
}

func oneBackendDispatcher(t *testing.T, backend *testBackend) *proxy.Dispatcher {
	t.Helper()
	return newDispatcher(t, []router.RouteSpec{openAIRoute("openai", "pool")}, []*balancer.Pool{
		newPool(t, "pool", balancer.PolicyRoundRobin, []balancer.EndpointSpec{{Name: "a", URL: backend.server.URL, Weight: 1}}),
	}, proxy.DefaultTransportConfig())
}

func twoBackendDispatcher(t *testing.T, backendA, backendB *testBackend) *proxy.Dispatcher {
	t.Helper()
	return newDispatcher(t, []router.RouteSpec{openAIRoute("openai", "pool")}, []*balancer.Pool{
		newPool(t, "pool", balancer.PolicyRoundRobin, []balancer.EndpointSpec{
			{Name: "a", URL: backendA.server.URL, Weight: 1},
			{Name: "b", URL: backendB.server.URL, Weight: 1},
		}),
	}, proxy.DefaultTransportConfig())
}

func newPool(t *testing.T, name string, policy balancer.Policy, endpoints []balancer.EndpointSpec) *balancer.Pool {
	t.Helper()
	pool, err := balancer.NewPool(balancer.PoolSpec{Name: name, Policy: policy, Endpoints: endpoints})
	if err != nil {
		t.Fatal(err)
	}
	return pool
}

func compileTable(t *testing.T, specs ...router.RouteSpec) *router.Table {
	t.Helper()
	table, err := router.Compile(specs)
	if err != nil {
		t.Fatal(err)
	}
	return table
}

func openAIRoute(name, pool string) router.RouteSpec {
	return router.RouteSpec{Name: name, Protocol: router.ProtocolOpenAI, Path: router.OpenAIChatCompletionsPath, Model: router.WildcardModel, Pool: pool}
}

func anthropicRoute(name, pool string) router.RouteSpec {
	return router.RouteSpec{Name: name, Protocol: router.ProtocolAnthropic, Path: router.AnthropicMessagesPath, Model: router.WildcardModel, Pool: pool}
}

func openAIMatch(model string) router.Match {
	return router.Match{Protocol: router.ProtocolOpenAI, Path: router.OpenAIChatCompletionsPath, Model: model}
}

func anthropicMatch(model string) router.Match {
	return router.Match{Protocol: router.ProtocolAnthropic, Path: router.AnthropicMessagesPath, Model: model}
}

func closeResponse(t *testing.T, response *http.Response) {
	t.Helper()
	if response == nil || response.Body == nil {
		t.Fatal("成功响应缺少 Body")
	}
	_, _ = io.Copy(io.Discard, response.Body)
	if err := response.Body.Close(); err != nil {
		t.Fatal(err)
	}
}

func waitSignal(t *testing.T, signal <-chan struct{}) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(5 * time.Second):
		t.Fatal("等待测试协调信号超时")
	}
}

func waitResult(t *testing.T, results <-chan doResult) doResult {
	t.Helper()
	select {
	case result := <-results:
		return result
	case <-time.After(5 * time.Second):
		t.Fatal("等待下游请求结果超时")
		return doResult{}
	}
}
