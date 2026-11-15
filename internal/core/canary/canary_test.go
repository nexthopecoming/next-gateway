package canary

import (
	"fmt"
	"testing"
)

func TestHashGoldenVectors(t *testing.T) {
	tests := []struct {
		kind  IdentityKind
		value string
		want  int
	}{
		{kind: IdentityTenant, value: "tenant-001", want: 2023},
		{kind: IdentityTenant, value: "Tenant-001", want: 3351},
		{kind: IdentityAPIKey, value: "sk-test-example", want: 1698},
	}
	for _, test := range tests {
		t.Run(string(test.kind)+"/"+test.value, func(t *testing.T) {
			identity := NewIdentity(test.kind, test.value)
			if got := identity.Bucket(); got != test.want {
				t.Fatalf("bucket = %d，期望 %d", got, test.want)
			}
		})
	}
}

func TestDecisionPrecedenceAndBoundaries(t *testing.T) {
	tenant := NewIdentity(IdentityTenant, "tenant-001")
	decider, err := NewDecider(2024, []string{"tenant-001"}, []string{"tenant-001"}, true)
	if err != nil {
		t.Fatal(err)
	}
	if got := decider.Decide(tenant, "canary"); got.Core != CoreStable || got.Reason != ReasonForceStable {
		t.Fatalf("强制 stable 优先级错误: %+v", got)
	}

	decider, err = NewDecider(0, nil, []string{"tenant-001"}, true)
	if err != nil {
		t.Fatal(err)
	}
	if got := decider.Decide(tenant, "stable"); got.Core != CoreCanary || got.Reason != ReasonForceCanary {
		t.Fatalf("强制 canary 优先级错误: %+v", got)
	}

	decider, err = NewDecider(0, nil, nil, true)
	if err != nil {
		t.Fatal(err)
	}
	if got := decider.Decide(MissingIdentity(), "canary"); got.Core != CoreCanary || got.Reason != ReasonOverride {
		t.Fatalf("显式覆盖应先于 missing: %+v", got)
	}
	if got := decider.Decide(MissingIdentity(), "invalid"); got.Core != CoreStable || got.Reason != ReasonMissing {
		t.Fatalf("missing 应固定 stable: %+v", got)
	}

	decider, err = NewDecider(2023, nil, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if got := decider.Decide(tenant, "canary"); got.Core != CoreStable {
		t.Fatalf("bucket 等于阈值时不应进入 canary: %+v", got)
	}
	decider, err = NewDecider(2024, nil, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if got := decider.Decide(tenant, "stable"); got.Core != CoreCanary || got.Reason != ReasonHash {
		t.Fatalf("bucket 小于阈值时应进入 canary: %+v", got)
	}
}

func TestWeightBoundariesAndMonotonicity(t *testing.T) {
	identities := make([]Identity, 200)
	for index := range identities {
		identities[index] = NewIdentity(IdentityTenant, fmt.Sprintf("tenant-%03d", index))
	}
	zero, err := NewDecider(0, nil, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	full, err := NewDecider(10000, nil, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	low, err := NewDecider(2500, nil, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	high, err := NewDecider(5000, nil, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, identity := range identities {
		if zero.Decide(identity, "").Core != CoreStable || full.Decide(identity, "").Core != CoreCanary {
			t.Fatalf("0/10000 权重边界错误，bucket=%d", identity.Bucket())
		}
		if low.Decide(identity, "").Core == CoreCanary && high.Decide(identity, "").Core != CoreCanary {
			t.Fatalf("权重增加丢失原 canary，bucket=%d", identity.Bucket())
		}
	}
	if _, err := NewDecider(-1, nil, nil, false); err == nil {
		t.Fatal("负权重未拒绝")
	}
	if _, err := NewDecider(10001, nil, nil, false); err == nil {
		t.Fatal("超过 10000 的权重未拒绝")
	}
}

func TestNormalizeTenant(t *testing.T) {
	tenant, err := NormalizeTenant(" tenant-001 ")
	if err != nil || tenant != "tenant-001" {
		t.Fatalf("规范化结果 = %q, %v", tenant, err)
	}
	for _, invalid := range []string{"", " ", "tenant\n001", string([]byte{0xff})} {
		if _, err := NormalizeTenant(invalid); err == nil {
			t.Errorf("非法租户未拒绝: %q", invalid)
		}
	}
	if _, err := NewDecider(0, []string{"bad\nvalue"}, nil, false); err == nil {
		t.Fatal("非法强制名单未拒绝")
	}
}
