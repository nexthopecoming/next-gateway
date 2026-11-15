package biz

import (
	"errors"
	"net/http"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/nexthopecoming/next-gateway/internal/core/canary"
)

// ErrInvalidIdentity 只表达有限校验类别，错误链中不拼接原始身份或凭据。
var ErrInvalidIdentity = errors.New("invalid gateway identity")

// IdentityExtractor 从 HTTP Header 提取身份，并立即转换为不可逆灰度摘要。
type IdentityExtractor struct{}

// Extract 按租户、Bearer、x-api-key 的固定顺序提取；存在非法高优先级身份时不静默降级。
func (IdentityExtractor) Extract(request *http.Request) (canary.Identity, error) {
	if values := request.Header.Values("X-Tenant-ID"); len(values) > 0 {
		if len(values) != 1 {
			return canary.Identity{}, ErrInvalidIdentity
		}
		tenant, err := canary.NormalizeTenant(values[0])
		if err != nil {
			return canary.Identity{}, ErrInvalidIdentity
		}
		return canary.NewIdentity(canary.IdentityTenant, tenant), nil
	}

	if values := request.Header.Values("Authorization"); len(values) > 0 {
		if len(values) != 1 {
			return canary.Identity{}, ErrInvalidIdentity
		}
		scheme, key, found := strings.Cut(values[0], " ")
		if !found || !strings.EqualFold(scheme, "Bearer") || !validAPIKey(key) {
			return canary.Identity{}, ErrInvalidIdentity
		}
		return canary.NewIdentity(canary.IdentityAPIKey, key), nil
	}

	if values := request.Header.Values("x-api-key"); len(values) > 0 {
		if len(values) != 1 || !validAPIKey(values[0]) {
			return canary.Identity{}, ErrInvalidIdentity
		}
		return canary.NewIdentity(canary.IdentityAPIKey, values[0]), nil
	}
	return canary.MissingIdentity(), nil
}

// validAPIKey 拒绝任意空白、控制字符和非法 UTF-8；不会把 key 写入错误或日志。
func validAPIKey(key string) bool {
	if key == "" || strings.TrimSpace(key) != key || !utf8.ValidString(key) {
		return false
	}
	for _, value := range key {
		if unicode.IsSpace(value) || unicode.IsControl(value) {
			return false
		}
	}
	return true
}
