package router_test

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/nexthopecoming/next-gateway/internal/core/router"
)

func TestLookupExactModelPrecedesWildcard(t *testing.T) {
	table, err := router.Compile([]router.RouteSpec{
		routeSpec("wildcard", router.WildcardModel, "wildcard-pool"),
		routeSpec("exact", "Model-A", "exact-pool"),
		{
			Name:     "anthropic",
			Protocol: router.ProtocolAnthropic,
			Path:     router.AnthropicMessagesPath,
			Model:    router.WildcardModel,
			Pool:     "anthropic-pool",
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	route, err := table.Lookup(router.Match{
		Protocol: router.ProtocolOpenAI,
		Path:     router.OpenAIChatCompletionsPath,
		Model:    "Model-A",
	})
	if err != nil {
		t.Fatal(err)
	}
	if route.Name() != "exact" || route.Pool() != "exact-pool" {
		t.Fatalf("精确模型未优先匹配: %+v", route)
	}
	if route.Method() != http.MethodPost || route.Path() != router.OpenAIChatCompletionsPath {
		t.Fatalf("静态方法或路径错误: method=%q path=%q", route.Method(), route.Path())
	}

	// 模型比较区分大小写；精确键未命中时才允许回退到通配 Route。
	route, err = table.Lookup(router.Match{
		Protocol: router.ProtocolOpenAI,
		Path:     router.OpenAIChatCompletionsPath,
		Model:    "model-a",
	})
	if err != nil || route.Name() != "wildcard" {
		t.Fatalf("大小写不同的模型不应命中精确 Route: route=%q err=%v", route.Name(), err)
	}

	route, err = table.Lookup(router.Match{
		Protocol: router.ProtocolAnthropic,
		Path:     router.AnthropicMessagesPath,
		Model:    "claude-test",
	})
	if err != nil || route.Protocol() != router.ProtocolAnthropic || route.Name() != "anthropic" {
		t.Fatalf("Anthropic 路由错误: route=%q err=%v", route.Name(), err)
	}
}

func TestLookupMissesUseBoundedError(t *testing.T) {
	table, err := router.Compile([]router.RouteSpec{
		routeSpec("exact", "sensitive-model-value", "pool"),
	})
	if err != nil {
		t.Fatal(err)
	}

	for _, match := range []router.Match{
		{Protocol: router.ProtocolAnthropic, Path: router.OpenAIChatCompletionsPath, Model: "sensitive-model-value"},
		{Protocol: router.ProtocolOpenAI, Path: router.AnthropicMessagesPath, Model: "sensitive-model-value"},
		{Protocol: router.ProtocolOpenAI, Path: router.OpenAIChatCompletionsPath, Model: "other-model"},
	} {
		route, lookupErr := table.Lookup(match)
		if !errors.Is(lookupErr, router.ErrRouteNotFound) || route.Name() != "" {
			t.Fatalf("未命中结果错误: route=%+v err=%v", route, lookupErr)
		}
		if strings.Contains(lookupErr.Error(), "sensitive-model-value") {
			t.Fatalf("未命中错误泄露模型: %v", lookupErr)
		}
	}

	var nilTable *router.Table
	if _, err := nilTable.Lookup(router.Match{}); !errors.Is(err, router.ErrRouteNotFound) {
		t.Fatalf("nil Table 未返回有限未命中错误: %v", err)
	}
}

func TestCompileRejectsAmbiguousAndInvalidRoutes(t *testing.T) {
	tests := []struct {
		name  string
		specs []router.RouteSpec
	}{
		{
			name: "duplicate name",
			specs: []router.RouteSpec{
				routeSpec("same", "model-a", "pool"),
				routeSpec("same", "model-b", "pool"),
			},
		},
		{
			name: "duplicate exact model",
			specs: []router.RouteSpec{
				routeSpec("first", "model-a", "pool"),
				routeSpec("second", "model-a", "pool"),
			},
		},
		{
			name: "duplicate wildcard",
			specs: []router.RouteSpec{
				routeSpec("first", router.WildcardModel, "pool"),
				routeSpec("second", router.WildcardModel, "pool"),
			},
		},
		{
			name: "unknown protocol",
			specs: []router.RouteSpec{{
				Name: "route", Protocol: "other", Path: router.OpenAIChatCompletionsPath, Model: "m", Pool: "pool",
			}},
		},
		{
			name:  "path does not match protocol",
			specs: []router.RouteSpec{routeSpecWithPath("route", router.AnthropicMessagesPath)},
		},
		{
			name:  "empty model",
			specs: []router.RouteSpec{routeSpec("route", "", "pool")},
		},
		{
			name:  "whitespace model",
			specs: []router.RouteSpec{routeSpec("route", " \t\n", "pool")},
		},
		{
			name:  "model exceeds shared byte limit",
			specs: []router.RouteSpec{routeSpec("route", strings.Repeat("m", 129), "pool")},
		},
		{
			name:  "invalid route name",
			specs: []router.RouteSpec{routeSpec("Invalid", "model", "pool")},
		},
		{
			name:  "invalid pool name",
			specs: []router.RouteSpec{routeSpec("route", "model", "bad.pool")},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if table, err := router.Compile(test.specs); err == nil || table != nil {
				t.Fatalf("非法配置未被拒绝: table=%v err=%v", table, err)
			}
		})
	}
}

func TestCompileAcceptsModelByteBoundaryWithoutTrimming(t *testing.T) {
	model := strings.Repeat("m", 128)
	table, err := router.Compile([]router.RouteSpec{
		routeSpec("wildcard", router.WildcardModel, "wildcard-pool"),
		routeSpec("boundary", model, "boundary-pool"),
		routeSpec("spaced", " model-a ", "spaced-pool"),
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		model string
		route string
		pool  string
	}{
		{model: model, route: "boundary", pool: "boundary-pool"},
		{model: " model-a ", route: "spaced", pool: "spaced-pool"},
		{model: "model-a", route: "wildcard", pool: "wildcard-pool"},
	} {
		route, err := table.Lookup(openAIMatch(test.model))
		if err != nil || route.Name() != test.route || route.Pool() != test.pool {
			t.Fatalf("原始模型键匹配错误: route=%+v err=%v", route, err)
		}
	}
}

func TestCompileCopiesInputAndRoutes(t *testing.T) {
	specs := []router.RouteSpec{routeSpec("original", "model-a", "original-pool")}
	table, err := router.Compile(specs)
	if err != nil {
		t.Fatal(err)
	}
	specs[0] = routeSpec("changed", "model-b", "changed-pool")

	match := router.Match{
		Protocol: router.ProtocolOpenAI,
		Path:     router.OpenAIChatCompletionsPath,
		Model:    "model-a",
	}
	route, err := table.Lookup(match)
	if err != nil || route.Name() != "original" || route.Pool() != "original-pool" {
		t.Fatalf("编译结果受输入切片修改影响: route=%+v err=%v", route, err)
	}

	routes := table.Routes()
	if len(routes) != 1 || routes[0].Model() != "model-a" {
		t.Fatalf("Routes 副本错误: %+v", routes)
	}
	routes[0] = router.Route{}
	route, err = table.Lookup(match)
	if err != nil || route.Name() != "original" {
		t.Fatalf("Routes 返回值可修改内部表: route=%+v err=%v", route, err)
	}
}

func TestCompileEmptyAndRouteLimit(t *testing.T) {
	empty, err := router.Compile(nil)
	if err != nil || empty.Len() != 0 || len(empty.Routes()) != 0 {
		t.Fatalf("空路由表应合法: table=%v err=%v", empty, err)
	}
	if _, err := empty.Lookup(router.Match{}); !errors.Is(err, router.ErrRouteNotFound) {
		t.Fatalf("空表查询错误 = %v", err)
	}

	specs := make([]router.RouteSpec, router.MaxRoutes)
	for index := range specs {
		specs[index] = routeSpec(fmt.Sprintf("route-%04d", index), fmt.Sprintf("model-%04d", index), "pool")
	}
	table, err := router.Compile(specs)
	if err != nil || table.Len() != router.MaxRoutes {
		t.Fatalf("最大合法路由数被拒绝: len=%d err=%v", table.Len(), err)
	}
	if _, err := router.Compile(append(specs, routeSpec("overflow", "overflow", "pool"))); err == nil {
		t.Fatal("超过路由上限未拒绝")
	}
}

func routeSpec(name, model, pool string) router.RouteSpec {
	return router.RouteSpec{
		Name:     name,
		Protocol: router.ProtocolOpenAI,
		Path:     router.OpenAIChatCompletionsPath,
		Model:    model,
		Pool:     pool,
	}
}

func routeSpecWithPath(name, path string) router.RouteSpec {
	spec := routeSpec(name, "model", "pool")
	spec.Path = path
	return spec
}

// openAIMatch 构造 OpenAI 文本对话的最小匹配输入，避免各用例重复填写协议与路径。
func openAIMatch(model string) router.Match {
	return router.Match{
		Protocol: router.ProtocolOpenAI,
		Path:     router.OpenAIChatCompletionsPath,
		Model:    model,
	}
}
