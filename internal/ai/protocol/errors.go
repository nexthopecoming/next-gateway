// Package protocol 提供双协议共享的严格 JSON 解码、错误分类和 SSE 生命周期。
package protocol

import (
	"context"
	"errors"
	"net/http"

	"github.com/nexthopecoming/next-gateway/internal/core/engine"
)

// Error 是可安全返回客户端的有限错误；Param 为 nil 时 OpenAI 输出 null。
type Error struct {
	Status  int
	Type    string
	Code    string
	Message string
	Param   *string
}

// UnsupportedField 固定未知字段错误格式，不返回字段值或请求正文。
func UnsupportedField(field string) *Error {
	return &Error{
		Status: http.StatusBadRequest, Type: "invalid_request_error", Code: "unsupported_field",
		Message: "unsupported field: " + field, Param: stringPointer(field),
	}
}

// InvalidField 返回有界字段名的校验错误。
func InvalidField(field string) *Error {
	return &Error{
		Status: http.StatusBadRequest, Type: "invalid_request_error", Code: "invalid_request",
		Message: "invalid field: " + field, Param: stringPointer(field),
	}
}

// InvalidIdentity 不区分具体凭据失败原因，避免身份原文进入响应。
func InvalidIdentity() *Error {
	return &Error{
		Status: http.StatusBadRequest, Type: "invalid_request_error", Code: "invalid_request",
		Message: "invalid gateway identity",
	}
}

// UpstreamError 将内部错误映射为稳定协议错误，绝不透传 err.Error()。
func UpstreamError(err error) *Error {
	if errors.Is(err, context.DeadlineExceeded) {
		return &Error{Status: http.StatusGatewayTimeout, Type: "timeout_error", Code: "upstream_timeout", Message: "mock upstream timeout"}
	}
	var engineError *engine.Error
	if errors.As(err, &engineError) {
		switch engineError.Kind {
		case engine.ErrorTimeout:
			return &Error{Status: http.StatusGatewayTimeout, Type: "timeout_error", Code: "upstream_timeout", Message: "mock upstream timeout"}
		case engine.ErrorInternal:
			return &Error{Status: http.StatusInternalServerError, Type: "api_error", Code: "internal_error", Message: "internal gateway error"}
		}
	}
	return &Error{Status: http.StatusBadGateway, Type: "api_error", Code: "upstream_error", Message: "mock upstream failed"}
}

// StreamTimeout 构造流内有限超时错误。
func StreamTimeout(code, message string) *Error {
	return &Error{Type: "timeout_error", Code: code, Message: message}
}

func stringPointer(value string) *string {
	return &value
}
