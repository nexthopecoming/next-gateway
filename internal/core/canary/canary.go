// Package canary 实现不依赖实例和随机数的确定性灰度决策。
package canary

import (
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"net/http"
	"strings"
	"unicode"
	"unicode/utf8"
)

// IdentityKind 是哈希 canonical key 中固定、低基数的身份类型。
type IdentityKind string

const (
	IdentityMissing IdentityKind = "missing"
	IdentityTenant  IdentityKind = "tenant"
	IdentityAPIKey  IdentityKind = "apikey"
)

// Identity 只保留不可逆摘要和 bucket，原始租户/API Key 不进入请求上下文。
type Identity struct {
	kind    IdentityKind
	digest  [sha256.Size]byte
	bucket  int
	present bool
}

// NewIdentity 立即散列原始值并计算黄金算法 bucket，调用方无需继续保存原文。
func NewIdentity(kind IdentityKind, value string) Identity {
	digest := sha256.Sum256([]byte(value))
	canonical := "v1|" + string(kind) + "|" + value
	canonicalDigest := sha256.Sum256([]byte(canonical))
	bucket := int(binary.BigEndian.Uint64(canonicalDigest[:8]) % 10000)
	return Identity{kind: kind, digest: digest, bucket: bucket, present: true}
}

// MissingIdentity 构造无身份请求；未显式覆盖时固定 stable。
func MissingIdentity() Identity {
	return Identity{kind: IdentityMissing, bucket: -1}
}

// IdentityExtractor 是 core 定义的身份提取契约，由 biz 实现并注入 ai。
type IdentityExtractor interface {
	Extract(request *http.Request) (Identity, error)
}

// Kind 返回有限身份类型，不包含原始身份值。
func (i Identity) Kind() IdentityKind {
	return i.kind
}

// Bucket 返回 0..9999；missing 身份返回 -1。
func (i Identity) Bucket() int {
	return i.bucket
}

// Core 是 stable/canary 执行器的对外固定名称。
type Core string

const (
	CoreStable Core = "stable"
	CoreCanary Core = "canary"
)

// DecisionReason 是访问日志允许的有限决策原因。
type DecisionReason string

const (
	ReasonForceStable DecisionReason = "force_stable"
	ReasonForceCanary DecisionReason = "force_canary"
	ReasonOverride    DecisionReason = "override"
	ReasonMissing     DecisionReason = "missing"
	ReasonHash        DecisionReason = "hash"
)

// Decision 在一次请求开始时生成，之后不得随配置或重试改变。
type Decision struct {
	Core   Core
	Reason DecisionReason
	Bucket int
}

// Decider 是不可变灰度规则，可安全用于并发请求。
type Decider struct {
	basisPoints   int
	forceStable   map[[sha256.Size]byte]struct{}
	forceCanary   map[[sha256.Size]byte]struct{}
	allowOverride bool
}

// NewDecider 预散列强制租户名单，运行期不会保存或比较原始租户值。
func NewDecider(basisPoints int, stableTenants, canaryTenants []string, allowOverride bool) (*Decider, error) {
	if basisPoints < 0 || basisPoints > 10000 {
		return nil, errors.New("canary basis points 必须在 0..10000 之间")
	}
	decider := &Decider{
		basisPoints:   basisPoints,
		forceStable:   make(map[[sha256.Size]byte]struct{}, len(stableTenants)),
		forceCanary:   make(map[[sha256.Size]byte]struct{}, len(canaryTenants)),
		allowOverride: allowOverride,
	}
	for _, raw := range stableTenants {
		tenant, err := NormalizeTenant(raw)
		if err != nil {
			return nil, errors.New("force stable 租户名单包含非法值")
		}
		decider.forceStable[sha256.Sum256([]byte(tenant))] = struct{}{}
	}
	for _, raw := range canaryTenants {
		tenant, err := NormalizeTenant(raw)
		if err != nil {
			return nil, errors.New("force canary 租户名单包含非法值")
		}
		decider.forceCanary[sha256.Sum256([]byte(tenant))] = struct{}{}
	}
	return decider, nil
}

// Decide 严格按强制 stable、强制 canary、调试覆盖、missing、哈希的顺序决策一次。
func (d *Decider) Decide(identity Identity, override string) Decision {
	if identity.kind == IdentityTenant && identity.present {
		if _, ok := d.forceStable[identity.digest]; ok {
			return Decision{Core: CoreStable, Reason: ReasonForceStable, Bucket: identity.bucket}
		}
		if _, ok := d.forceCanary[identity.digest]; ok {
			return Decision{Core: CoreCanary, Reason: ReasonForceCanary, Bucket: identity.bucket}
		}
	}
	if d.allowOverride {
		switch Core(override) {
		case CoreStable:
			return Decision{Core: CoreStable, Reason: ReasonOverride, Bucket: identity.bucket}
		case CoreCanary:
			return Decision{Core: CoreCanary, Reason: ReasonOverride, Bucket: identity.bucket}
		}
	}
	if !identity.present {
		return Decision{Core: CoreStable, Reason: ReasonMissing, Bucket: -1}
	}
	if identity.bucket < d.basisPoints {
		return Decision{Core: CoreCanary, Reason: ReasonHash, Bucket: identity.bucket}
	}
	return Decision{Core: CoreStable, Reason: ReasonHash, Bucket: identity.bucket}
}

// NormalizeTenant 统一 TrimSpace、UTF-8、字节长度和控制字符规则，保留大小写。
func NormalizeTenant(raw string) (string, error) {
	tenant := strings.TrimSpace(raw)
	if len(tenant) == 0 || len(tenant) > 128 || !utf8.ValidString(tenant) {
		return "", errors.New("tenant 必须为 1..128 字节有效 UTF-8")
	}
	for _, value := range tenant {
		if unicode.IsControl(value) {
			return "", errors.New("tenant 不得包含控制字符")
		}
	}
	return tenant, nil
}
