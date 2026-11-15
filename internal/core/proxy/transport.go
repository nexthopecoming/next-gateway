package proxy

import (
	"context"
	"errors"
	"net"
	"net/http"
	"time"
)

// TransportConfig 控制通用 HTTP 连接和 Dispatcher 资源预算；协议响应大小限制由 AI Adapter 负责。
type TransportConfig struct {
	DialTimeout               time.Duration
	TLSHandshakeTimeout       time.Duration
	ResponseHeaderTimeout     time.Duration
	RequestHeaderTimeout      time.Duration
	IdleConnectionTimeout     time.Duration
	MaxIdleConnections        int
	MaxIdleConnectionsPerHost int
	MaxInFlightRequests       int
}

// DefaultTransportConfig 返回文档约定的安全开发默认值。
func DefaultTransportConfig() TransportConfig {
	return TransportConfig{
		DialTimeout:               5 * time.Second,
		TLSHandshakeTimeout:       5 * time.Second,
		ResponseHeaderTimeout:     30 * time.Second,
		RequestHeaderTimeout:      35 * time.Second,
		IdleConnectionTimeout:     90 * time.Second,
		MaxIdleConnections:        100,
		MaxIdleConnectionsPerHost: 20,
		MaxInFlightRequests:       1024,
	}
}

// newTransport 为整组不可变代理资源创建一个共享连接池，禁止环境代理和跳过 TLS 验证。
func newTransport(config TransportConfig) (*http.Transport, error) {
	if err := validateTransportConfig(config); err != nil {
		return nil, err
	}
	dialer := &net.Dialer{
		Timeout:   config.DialTimeout,
		KeepAlive: 30 * time.Second,
	}
	return &http.Transport{
		Proxy:                 nil,
		DialContext:           dialer.DialContext,
		ForceAttemptHTTP2:     true,
		TLSHandshakeTimeout:   config.TLSHandshakeTimeout,
		ResponseHeaderTimeout: config.ResponseHeaderTimeout,
		IdleConnTimeout:       config.IdleConnectionTimeout,
		MaxIdleConns:          config.MaxIdleConnections,
		MaxIdleConnsPerHost:   config.MaxIdleConnectionsPerHost,
	}, nil
}

// validateTransportConfig 限制所有连接及在途参数，避免零值意外关闭保护或创建无界资源。
func validateTransportConfig(config TransportConfig) error {
	if !durationInRange(config.DialTimeout, time.Millisecond, time.Minute) {
		return errors.New("proxy: invalid dial timeout")
	}
	if !durationInRange(config.TLSHandshakeTimeout, time.Millisecond, time.Minute) {
		return errors.New("proxy: invalid TLS handshake timeout")
	}
	if !durationInRange(config.ResponseHeaderTimeout, time.Millisecond, 5*time.Minute) {
		return errors.New("proxy: invalid response header timeout")
	}
	if !durationInRange(config.RequestHeaderTimeout, time.Millisecond, 5*time.Minute) {
		return errors.New("proxy: invalid request header timeout")
	}
	if !durationInRange(config.IdleConnectionTimeout, time.Second, 10*time.Minute) {
		return errors.New("proxy: invalid idle connection timeout")
	}
	if config.MaxIdleConnections < 1 || config.MaxIdleConnections > 10000 {
		return errors.New("proxy: invalid max idle connections")
	}
	if config.MaxIdleConnectionsPerHost < 1 ||
		config.MaxIdleConnectionsPerHost > 1000 ||
		config.MaxIdleConnectionsPerHost > config.MaxIdleConnections {
		return errors.New("proxy: invalid max idle connections per host")
	}
	if config.MaxInFlightRequests < 1 || config.MaxInFlightRequests > 100000 {
		return errors.New("proxy: invalid max in-flight requests")
	}
	return nil
}

// durationInRange 使用闭区间统一校验带单位的时长。
func durationInRange(value, minimum, maximum time.Duration) bool {
	return value >= minimum && value <= maximum
}

// classifyRoundTripError 不保留原始 url.Error，避免目标 URL 或网络细节越过安全边界。
func classifyRoundTripError(ctx context.Context, err error) error {
	if ctx != nil {
		if contextErr := ctx.Err(); contextErr != nil {
			return contextErr
		}
	}
	var networkError net.Error
	if errors.As(err, &networkError) && networkError.Timeout() {
		return ErrUpstreamTimeout
	}
	return ErrUpstream
}
