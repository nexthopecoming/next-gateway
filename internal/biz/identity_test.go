package biz

import (
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/nexthopecoming/next-gateway/internal/core/canary"
)

func TestIdentityExtractionPrecedenceAndNormalization(t *testing.T) {
	request := httptest.NewRequest("POST", "/v1/messages", nil)
	request.Header.Set("X-Tenant-ID", " tenant-001 ")
	request.Header.Set("Authorization", "Bearer should-not-be-selected")
	identity, err := (IdentityExtractor{}).Extract(request)
	if err != nil {
		t.Fatal(err)
	}
	if identity.Kind() != canary.IdentityTenant || identity.Bucket() != 2023 {
		t.Fatalf("租户优先级或规范化错误: kind=%s bucket=%d", identity.Kind(), identity.Bucket())
	}
}

func TestAPIKeyIdentityDoesNotRetainOriginal(t *testing.T) {
	const secret = "sk-test-example"
	for _, test := range []struct {
		name   string
		header string
		value  string
	}{
		{name: "Bearer", header: "Authorization", value: "Bearer " + secret},
		{name: "x-api-key", header: "x-api-key", value: secret},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest("POST", "/v1/chat/completions", nil)
			request.Header.Set(test.header, test.value)
			identity, err := (IdentityExtractor{}).Extract(request)
			if err != nil {
				t.Fatal(err)
			}
			if identity.Kind() != canary.IdentityAPIKey || identity.Bucket() != 1698 {
				t.Fatalf("API Key 身份错误: kind=%s bucket=%d", identity.Kind(), identity.Bucket())
			}
			if strings.Contains(fmt.Sprintf("%#v", identity), secret) {
				t.Fatal("身份对象仍包含原始 API Key")
			}
		})
	}
}

func TestInvalidAndDuplicateIdentity(t *testing.T) {
	tests := []struct {
		name    string
		headers [][2]string
	}{
		{name: "重复租户", headers: [][2]string{{"X-Tenant-ID", "a"}, {"X-Tenant-ID", "b"}}},
		{name: "非法租户", headers: [][2]string{{"X-Tenant-ID", "bad\nvalue"}}},
		{name: "空租户", headers: [][2]string{{"X-Tenant-ID", " "}}},
		{name: "非法 Bearer", headers: [][2]string{{"Authorization", "Basic value"}}},
		{name: "Bearer 含空白", headers: [][2]string{{"Authorization", "Bearer key with space"}}},
		{name: "重复 Authorization", headers: [][2]string{{"Authorization", "Bearer a"}, {"Authorization", "Bearer b"}}},
		{name: "非法 Bearer 不降级", headers: [][2]string{{"Authorization", "invalid"}, {"x-api-key", "valid"}}},
		{name: "空 x-api-key", headers: [][2]string{{"x-api-key", ""}}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest("POST", "/v1/messages", nil)
			for _, header := range test.headers {
				request.Header.Add(header[0], header[1])
			}
			if _, err := (IdentityExtractor{}).Extract(request); err == nil {
				t.Fatal("非法身份未拒绝")
			}
		})
	}
}

func TestMissingIdentity(t *testing.T) {
	identity, err := (IdentityExtractor{}).Extract(httptest.NewRequest("POST", "/v1/messages", nil))
	if err != nil || identity.Kind() != canary.IdentityMissing || identity.Bucket() != -1 {
		t.Fatalf("missing identity = %#v, %v", identity, err)
	}
}
