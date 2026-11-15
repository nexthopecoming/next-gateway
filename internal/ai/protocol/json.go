package protocol

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"strconv"
	"strings"

	coremodel "github.com/nexthopecoming/next-gateway/internal/core/model"
)

// Optional 区分可选字段缺失与显式 null；null 不属于本项目的严格字段类型。
type Optional[T any] struct {
	Value T
	Set   bool
}

// UnmarshalJSON 解码一个显式提供的非 null 值。
func (o *Optional[T]) UnmarshalJSON(data []byte) error {
	if bytes.Equal(bytes.TrimSpace(data), []byte("null")) {
		return errors.New("null is not supported")
	}
	if err := json.Unmarshal(data, &o.Value); err != nil {
		return err
	}
	o.Set = true
	return nil
}

// Text 拒绝 JSON null，避免 null 被 encoding/json 静默解释为空字符串。
type Text string

// UnmarshalJSON 只接受 JSON 字符串。
func (t *Text) UnmarshalJSON(data []byte) error {
	if bytes.Equal(bytes.TrimSpace(data), []byte("null")) {
		return errors.New("null is not supported")
	}
	var value string
	if err := json.Unmarshal(data, &value); err != nil {
		return err
	}
	*t = Text(value)
	return nil
}

// DecodeRequest 校验媒体类型和请求体上限，并严格拒绝未知字段及尾随 JSON。
func DecodeRequest(w http.ResponseWriter, r *http.Request, maximumBytes int64, destination any) *Error {
	if !validJSONContentType(r.Header.Values("Content-Type")) {
		return &Error{
			Status: http.StatusUnsupportedMediaType, Type: "invalid_request_error", Code: "unsupported_content_type",
			Message: "Content-Type must be application/json",
		}
	}

	reader := http.MaxBytesReader(w, r.Body, maximumBytes)
	decoder := json.NewDecoder(reader)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return decodeError(err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err != nil {
			var maxBytesError *http.MaxBytesError
			if errors.As(err, &maxBytesError) {
				return requestTooLarge()
			}
		}
		return &Error{
			Status: http.StatusBadRequest, Type: "invalid_request_error", Code: "invalid_request",
			Message: "request body must contain exactly one JSON object",
		}
	}
	return nil
}

// DecodeStrict 供 Anthropic content block 等自定义嵌套结构复用同一未知字段规则。
func DecodeStrict(data []byte, destination any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return errors.New("trailing JSON data")
	}
	return nil
}

func decodeError(err error) *Error {
	var maxBytesError *http.MaxBytesError
	if errors.As(err, &maxBytesError) {
		return requestTooLarge()
	}
	const unknownPrefix = "json: unknown field "
	if strings.HasPrefix(err.Error(), unknownPrefix) {
		quoted := strings.TrimPrefix(err.Error(), unknownPrefix)
		if field, unquoteErr := strconv.Unquote(quoted); unquoteErr == nil && safeFieldName(field) {
			return UnsupportedField(field)
		}
	}
	var typeError *json.UnmarshalTypeError
	if errors.As(err, &typeError) && safeFieldName(typeError.Field) {
		return InvalidField(typeError.Field)
	}
	return &Error{
		Status: http.StatusBadRequest, Type: "invalid_request_error", Code: "invalid_request",
		Message: "invalid JSON request body",
	}
}

func requestTooLarge() *Error {
	return &Error{
		Status: http.StatusRequestEntityTooLarge, Type: "invalid_request_error", Code: "request_too_large",
		Message: "request body too large",
	}
}

func validJSONContentType(values []string) bool {
	if len(values) != 1 {
		return false
	}
	mediaType, parameters, err := mime.ParseMediaType(values[0])
	if err != nil || mediaType != "application/json" {
		return false
	}
	if charset := parameters["charset"]; charset != "" && !strings.EqualFold(charset, "utf-8") {
		return false
	}
	return true
}

// safeFieldName 防止异常大的 JSON key 被原样反射到响应，限制客户端错误大小。
func safeFieldName(field string) bool {
	return field != "" && len(field) <= 128 && !strings.ContainsAny(field, "\r\n")
}

// ValidateModel 复用 core 模型键约束，并保持协议字段错误语义不变。
func ValidateModel(model string) *Error {
	if !coremodel.Valid(model) {
		return InvalidField("model")
	}
	return nil
}

// ValidateStops 统一 stop 数量及空字符串约束，不改变有效字符串中的空白。
func ValidateStops(stops []string) *Error {
	if len(stops) > 4 {
		return InvalidField("stop")
	}
	for _, stop := range stops {
		if stop == "" {
			return InvalidField("stop")
		}
	}
	return nil
}

// ValidationError 将自定义嵌套解码错误收敛为固定字段，不透传 JSON 原文。
func ValidationError(field string, err error) *Error {
	if err == nil {
		return nil
	}
	const unknownPrefix = "json: unknown field "
	if strings.HasPrefix(err.Error(), unknownPrefix) {
		quoted := strings.TrimPrefix(err.Error(), unknownPrefix)
		if unknown, unquoteErr := strconv.Unquote(quoted); unquoteErr == nil && safeFieldName(unknown) {
			return UnsupportedField(unknown)
		}
	}
	return InvalidField(field)
}

// WriteJSON 使用一致 UTF-8 媒体类型写出有限响应模型，序列化错误不会包含请求正文。
func WriteJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	encoder := json.NewEncoder(w)
	encoder.SetEscapeHTML(false)
	_ = encoder.Encode(value)
}
