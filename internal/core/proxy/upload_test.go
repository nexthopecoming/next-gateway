package proxy_test

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nexthopecoming/next-gateway/internal/core/balancer"
	"github.com/nexthopecoming/next-gateway/internal/core/proxy"
)

func TestRequestHeaderTimeoutBoundsStalledBodyUpload(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	headersRead := make(chan error, 1)
	releaseConnection := make(chan struct{})
	serverDone := make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(releaseConnection) }) }
	go func() {
		defer close(serverDone)
		connection, err := listener.Accept()
		if err != nil {
			headersRead <- err
			return
		}
		defer connection.Close()
		// 只消费请求头，刻意不读大 Body，使客户端停留在上传阶段。
		_, err = http.ReadRequest(bufio.NewReader(connection))
		headersRead <- err
		if err == nil {
			<-releaseConnection
		}
	}()
	t.Cleanup(func() {
		unblock()
		_ = listener.Close()
		<-serverDone
	})

	config := proxy.DefaultTransportConfig()
	config.RequestHeaderTimeout = 40 * time.Millisecond
	config.ResponseHeaderTimeout = 3 * time.Second
	origin := "http://" + listener.Addr().String()
	dispatcher := newDispatcher(t, openAIRoutes(), []*balancer.Pool{
		newPool(t, "pool", balancer.PolicyRoundRobin, []balancer.EndpointSpec{{Name: "a", URL: origin, Weight: 1}}),
	}, config)
	results := make(chan doResult, 1)
	started := time.Now()
	go func() {
		selection, response, err := dispatcher.Do(context.Background(), proxy.Request{
			Match: openAIMatch("model"), Body: bytes.Repeat([]byte("x"), 16<<20),
		})
		results <- doResult{selection: selection, response: response, err: err}
	}()
	select {
	case err := <-headersRead:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("原始 TCP 后端未收到请求头")
	}
	result := waitResult(t, results)
	if !errors.Is(result.err, proxy.ErrUpstreamTimeout) || result.response != nil || result.selection.Endpoint != "a" {
		t.Fatalf("上传阶段超时分类错误: %+v", result)
	}
	if elapsed := time.Since(started); elapsed >= time.Second {
		t.Fatalf("上传超时未在 ResponseHeaderTimeout 前有界返回: %v", elapsed)
	}
	if strings.Contains(result.err.Error(), origin) {
		t.Fatalf("上传超时错误泄露 origin: %v", result.err)
	}
	unblock()
}
