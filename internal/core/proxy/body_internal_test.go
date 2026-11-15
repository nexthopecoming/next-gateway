package proxy

import (
	"context"
	"errors"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/nexthopecoming/next-gateway/internal/core/balancer"
	"github.com/nexthopecoming/next-gateway/internal/core/router"
)

type trackingBody struct {
	read                bool
	closed              bool
	canceledBeforeClose bool
	canceled            *bool
}

func (b *trackingBody) Read([]byte) (int, error) {
	b.read = true
	return 0, errors.New("unexpected read")
}

func (b *trackingBody) Close() error {
	b.closed = true
	b.canceledBeforeClose = *b.canceled
	return nil
}

func TestCancelAndCloseDoesNotReadErrorBody(t *testing.T) {
	canceled := false
	body := &trackingBody{canceled: &canceled}
	cancelAndClose(body, func() { canceled = true })
	if body.read || !body.closed || !body.canceledBeforeClose {
		t.Fatalf("错误 Body 释放顺序错误: read=%v closed=%v canceled_before_close=%v", body.read, body.closed, body.canceledBeforeClose)
	}
}

func TestResponseBodyReleasesResourcesExactlyOnce(t *testing.T) {
	for _, test := range []struct {
		name    string
		readErr error
		read    bool
	}{
		{name: "EOF", readErr: io.EOF, read: true},
		{name: "read error", readErr: errors.New("controlled read failure"), read: true},
		{name: "close before read", readErr: io.EOF},
	} {
		t.Run(test.name, func(t *testing.T) {
			canceled := 0
			released := 0
			body := &cancelOnCloseBody{
				ReadCloser: &endingBody{readErr: test.readErr},
				cancel:     func() { canceled++ },
				release:    func() { released++ },
			}
			if test.read {
				if _, err := body.Read(make([]byte, 1)); !errors.Is(err, test.readErr) {
					t.Fatalf("读取错误 = %v，期望 %v", err, test.readErr)
				}
			}
			// EOF/Read error 后 Close 以及重复 Close 都不得重复释放 token。
			_ = body.Close()
			_ = body.Close()
			if canceled != 1 || released != 1 {
				t.Fatalf("资源释放次数错误: canceled=%d released=%d", canceled, released)
			}
		})
	}
}

type endingBody struct {
	readErr error
}

func (b *endingBody) Read([]byte) (int, error) { return 0, b.readErr }
func (b *endingBody) Close() error             { return nil }

func TestPoolFailureRetainsRouteAndPoolSelection(t *testing.T) {
	table, err := router.Compile([]router.RouteSpec{{
		Name: "openai", Protocol: router.ProtocolOpenAI, Path: router.OpenAIChatCompletionsPath, Model: router.WildcardModel, Pool: "missing",
	}})
	if err != nil {
		t.Fatal(err)
	}
	dispatcher := &Dispatcher{
		table: table, pools: map[string]*balancer.Pool{}, transport: &http.Transport{},
		requestHeaderTimeout: time.Second, inFlight: make(chan struct{}, 1),
	}
	selection, response, err := dispatcher.Do(context.Background(), Request{
		Match: router.Match{Protocol: router.ProtocolOpenAI, Path: router.OpenAIChatCompletionsPath, Model: "model"},
	})
	if !errors.Is(err, balancer.ErrUnavailable) || response != nil {
		t.Fatalf("Pool 失败结果错误: response=%v err=%v", response, err)
	}
	if selection != (Selection{Route: "openai", Pool: "missing"}) {
		t.Fatalf("Pool 失败未保留 Route/Pool: %+v", selection)
	}
}
