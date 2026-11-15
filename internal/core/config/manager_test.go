package config

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"

	"github.com/nexthopecoming/next-gateway/internal/core/role"
)

func TestManagerCachesSuccessfulLoad(t *testing.T) {
	unsetEnvironmentForTest(t, "NEXT_GATEWAY_SERVER_LISTEN")
	path := writeTestConfig(t, "server:\n  listen: \"127.0.0.1:10001\"\n")
	otherPath := writeTestConfig(t, "server:\n  listen: \"127.0.0.1:10002\"\n")
	manager := NewManager()

	first, err := manager.Load(WithPath(path), WithRole(role.AI))
	if err != nil {
		t.Fatal(err)
	}
	// 首次加载后同时改变文件、环境和后续选项，结果仍必须来自首次快照。
	if err := os.WriteFile(path, []byte("server:\n  listen: \"127.0.0.1:10003\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("NEXT_GATEWAY_SERVER_LISTEN", "127.0.0.1:10004")

	for _, loadPath := range []string{path, otherPath} {
		cfg, err := manager.Load(WithPath(loadPath), WithRole(role.AI))
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(cfg, first) {
			t.Fatalf("后续加载未复用首次配置:\n首次 = %#v\n后续 = %#v", first, cfg)
		}
	}
}

func TestManagerCachesError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing.yaml")
	manager := NewManager()
	_, firstErr := manager.Load(WithPath(path), WithRole(role.AI))
	if firstErr == nil {
		t.Fatal("缺失文件未返回错误")
	}

	// 修复文件不会在同一启动 Manager 上触发隐式重试。
	if err := os.WriteFile(path, []byte("schema_version: 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, secondErr := manager.Load(WithPath(path), WithRole(role.AI))
	if secondErr == nil || secondErr.Error() != firstErr.Error() {
		t.Fatalf("首次错误未被缓存: first=%v second=%v", firstErr, secondErr)
	}
	if !reflect.DeepEqual(cfg, Config{}) {
		t.Fatalf("失败时返回了非零配置: %#v", cfg)
	}

	// 无状态 Load 不受某个 Manager 的失败缓存影响。
	if _, err := Load(WithPath(path), WithRole(role.AI)); err != nil {
		t.Fatalf("独立 Load 未读取修复后的文件: %v", err)
	}
}

func TestManagerReturnsIndependentCopies(t *testing.T) {
	path := writeTestConfig(t, `
config:
  endpoints: ["etcd-a:2379", "etcd-b:2379"]
canary:
  force_stable_tenants: ["stable-a"]
  force_canary_tenants: ["canary-a"]
`)
	manager := NewManager()
	first, err := manager.Load(WithPath(path), WithRole(role.AI))
	if err != nil {
		t.Fatal(err)
	}
	second, err := manager.Load(WithPath(path), WithRole(role.AI))
	if err != nil {
		t.Fatal(err)
	}

	first.Source.Endpoints[0] = "changed"
	first.Canary.ForceStableTenants[0] = "changed"
	first.Canary.ForceCanaryTenants[0] = "changed"
	if second.Source.Endpoints[0] != "etcd-a:2379" ||
		second.Canary.ForceStableTenants[0] != "stable-a" ||
		second.Canary.ForceCanaryTenants[0] != "canary-a" {
		t.Fatalf("两次返回值共享 slice 底层数组: %#v", second)
	}

	third, err := manager.Load(WithPath(path), WithRole(role.AI))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(third, second) {
		t.Fatalf("修改返回值污染了 Manager 缓存:\n缓存副本 = %#v\n修改后读取 = %#v", second, third)
	}
}

func TestManagerConcurrentLoad(t *testing.T) {
	path := writeTestConfig(t, `
server:
  listen: "127.0.0.1:10001"
canary:
  force_stable_tenants: ["stable-a"]
`)
	manager := NewManager()
	type result struct {
		cfg Config
		err error
	}

	const workers = 32
	start := make(chan struct{})
	results := make(chan result, workers)
	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			cfg, err := manager.Load(WithPath(path), WithRole(role.AI))
			results <- result{cfg: cfg, err: err}
		}()
	}
	close(start)
	wg.Wait()
	close(results)

	var expected *Config
	for result := range results {
		if result.err != nil {
			t.Fatal(result.err)
		}
		if expected == nil {
			expected = &result.cfg
			continue
		}
		if !reflect.DeepEqual(result.cfg, *expected) {
			t.Fatalf("并发加载结果不一致:\n期望 = %#v\n实际 = %#v", *expected, result.cfg)
		}
	}
}

func TestDefaultManagerReturnsSameInstance(t *testing.T) {
	expected := DefaultManager()
	const workers = 16
	managers := make(chan *Manager, workers)
	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			managers <- DefaultManager()
		}()
	}
	wg.Wait()
	close(managers)
	for manager := range managers {
		if manager != expected {
			t.Fatal("DefaultManager 返回了不同实例")
		}
	}
}

func TestManagersAreIndependent(t *testing.T) {
	firstManager := NewManager()
	secondManager := NewManager()
	if firstManager == secondManager {
		t.Fatal("NewManager 返回了共享实例")
	}

	firstPath := writeTestConfig(t, "server:\n  listen: \"127.0.0.1:10001\"\n")
	secondPath := writeTestConfig(t, "server:\n  listen: \"127.0.0.1:10002\"\n")
	first, err := firstManager.Load(WithPath(firstPath), WithRole(role.AI))
	if err != nil {
		t.Fatal(err)
	}
	second, err := secondManager.Load(WithPath(secondPath), WithRole(role.AI))
	if err != nil {
		t.Fatal(err)
	}
	if first.Server.Listen != "127.0.0.1:10001" || second.Server.Listen != "127.0.0.1:10002" {
		t.Fatalf("独立 Manager 的配置相互污染: first=%q second=%q", first.Server.Listen, second.Server.Listen)
	}
}

func TestManagerConcurrentLoadResultsRemainIndependent(t *testing.T) {
	path := writeTestConfig(t, "config:\n  endpoints: [\"etcd-a:2379\"]\n")
	manager := NewManager()
	const workers = 16
	start := make(chan struct{})
	errs := make(chan error, workers)
	var wg sync.WaitGroup
	for index := range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			cfg, err := manager.Load(WithPath(path), WithRole(role.AI))
			if err != nil {
				errs <- err
				return
			}
			if cfg.Source.Endpoints[0] != "etcd-a:2379" {
				errs <- fmt.Errorf("第 %d 个副本在修改前已被污染: %q", index, cfg.Source.Endpoints[0])
				return
			}
			// 并发修改各自副本；若共享底层数组，race 检测和后续检查都会暴露。
			cfg.Source.Endpoints[0] = fmt.Sprintf("worker-%d", index)
		}()
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}
