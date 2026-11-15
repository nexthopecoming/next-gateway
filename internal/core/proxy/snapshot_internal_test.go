package proxy

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/nexthopecoming/next-gateway/internal/core/balancer"
	"github.com/nexthopecoming/next-gateway/internal/core/router"
)

func TestFilterHeadersHandlesNonCanonicalKeysWithoutMutatingInput(t *testing.T) {
	input := http.Header{
		"connection":        {"Content-Type, X-Request-ID"},
		"cOnNeCtIoN":        {"Accept-Encoding"},
		"aCcEpT":            {"application/json"},
		"Content-Type":      {"application/json"},
		"x-request-id":      {"request-123"},
		"anthropic-version": {"2023-06-01"},
		"Authorization":     {"secret-key"},
		"X-Unknown":         {"unknown"},
	}
	before := input.Clone()
	output := filterHeaders(input)
	if !reflect.DeepEqual(input, before) {
		t.Fatalf("Header 过滤修改了输入: before=%+v after=%+v", before, input)
	}
	if len(output["Content-Type"]) != 0 || len(output["X-Request-Id"]) != 0 {
		t.Fatalf("Connection 提名的 Header 未删除: %+v", output)
	}
	if got := output.Values("Accept"); !reflect.DeepEqual(got, []string{"application/json"}) {
		t.Fatalf("大小写混合的 Accept 未保留: %+v", got)
	}
	if got := output.Values("Anthropic-Version"); !reflect.DeepEqual(got, []string{"2023-06-01"}) {
		t.Fatalf("白名单 Header 未保留: %+v", got)
	}
	if len(output.Values("Authorization")) != 0 || len(output.Values("X-Unknown")) != 0 {
		t.Fatalf("Secret 或未知 Header 被透传: %+v", output)
	}

	// 输出值切片也必须独立，调用方后续修改不能反向影响输入。
	output["Accept"][0] = "changed"
	if input["aCcEpT"][0] != "application/json" {
		t.Fatal("输出与输入共享 Header 值切片")
	}
}

func TestDispatcherSnapshotsInputBeforeNetworkWait(t *testing.T) {
	type receivedRequest struct {
		contentType string
		requestID   string
		body        string
	}
	received := make(chan receivedRequest, 1)
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		body, err := io.ReadAll(request.Body)
		if err != nil {
			return
		}
		received <- receivedRequest{
			contentType: request.Header.Get("Content-Type"),
			requestID:   request.Header.Get("X-Request-ID"),
			body:        string(body),
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer backend.Close()

	pool, err := balancer.NewPool(balancer.PoolSpec{
		Name: "pool", Policy: balancer.PolicyRoundRobin,
		Endpoints: []balancer.EndpointSpec{{Name: "a", URL: backend.URL, Weight: 1}},
	})
	if err != nil {
		t.Fatal(err)
	}
	table, err := router.Compile([]router.RouteSpec{{
		Name: "openai", Protocol: router.ProtocolOpenAI, Path: router.OpenAIChatCompletionsPath,
		Model: router.WildcardModel, Pool: "pool",
	}})
	if err != nil {
		t.Fatal(err)
	}
	dispatcher, err := NewDispatcher(table, []*balancer.Pool{pool}, DefaultTransportConfig())
	if err != nil {
		t.Fatal(err)
	}
	defer dispatcher.Close()

	// 在真正拨号前暂停 RoundTrip；此时 Dispatcher 必须已经完成输入快照。
	originalDial := dispatcher.transport.DialContext
	dialStarted := make(chan struct{})
	releaseDial := make(chan struct{})
	var startOnce sync.Once
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(releaseDial) }) }
	defer unblock()
	dispatcher.transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		startOnce.Do(func() { close(dialStarted) })
		select {
		case <-releaseDial:
			return originalDial(ctx, network, address)
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}

	body := []byte(`{"model":"model"}`)
	header := http.Header{
		"content-type": {"application/json"},
		"x-request-id": {"original-request"},
	}
	type snapshotResult struct {
		response *http.Response
		err      error
	}
	results := make(chan snapshotResult, 1)
	go func() {
		_, response, err := dispatcher.Do(context.Background(), Request{
			Match:  router.Match{Protocol: router.ProtocolOpenAI, Path: router.OpenAIChatCompletionsPath, Model: "model"},
			Header: header, Body: body,
		})
		results <- snapshotResult{response: response, err: err}
	}()
	waitInternalSignal(t, dialStarted)

	// 只在 DialContext 已阻塞后修改，删除 bytes.Clone 或值复制必然被后端观测到。
	body[0] = 'X'
	header["content-type"][0] = "text/plain"
	header["x-request-id"][0] = "changed-request"
	unblock()
	result := waitInternalResult(t, results)
	if result.err != nil {
		t.Fatal(result.err)
	}
	_ = result.response.Body.Close()
	got := <-received
	if got.contentType != "application/json" || got.requestID != "original-request" || got.body != `{"model":"model"}` {
		t.Fatalf("后端收到的不是网络等待前快照: %+v", got)
	}
}

func waitInternalSignal(t *testing.T, signal <-chan struct{}) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(5 * time.Second):
		t.Fatal("等待内部测试同步信号超时")
	}
}

func waitInternalResult[T any](t *testing.T, results <-chan T) T {
	t.Helper()
	select {
	case result := <-results:
		return result
	case <-time.After(5 * time.Second):
		t.Fatal("等待内部测试请求结果超时")
		var zero T
		return zero
	}
}
