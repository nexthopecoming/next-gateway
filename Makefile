.DEFAULT_GOAL := build

GO ?= go
BIN_DIR ?= bin
VERSION ?= dev
# 无 Git 元数据时回退 none，构建时间在一次 make 调用中固定为 UTC。
COMMIT ?= $(shell git rev-parse --short HEAD 2>/dev/null || printf none)
BUILD_TIME ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
# 不自动继承环境中的代理 URL，避免意外传递凭据；命令行仍可覆盖公共代理。
GOPROXY := https://proxy.golang.org,direct

VERSION_PACKAGE := github.com/nexthopecoming/next-gateway/internal/core/version
LDFLAGS := -X $(VERSION_PACKAGE).Version=$(VERSION) -X $(VERSION_PACKAGE).Commit=$(COMMIT) -X $(VERSION_PACKAGE).BuildTime=$(BUILD_TIME)
GO_BUILD := CGO_ENABLED=0 $(GO) build -trimpath -ldflags '$(LDFLAGS)'
GO_RUN := CGO_ENABLED=0 $(GO) run -trimpath -ldflags '$(LDFLAGS)'
DOCKERFILE := deploy/Dockerfile
KUSTOMIZE_DIR := deploy/k8s
DOCKER_BUILD_ARGS := --build-arg GOPROXY=$(GOPROXY) --build-arg VERSION=$(VERSION) --build-arg COMMIT=$(COMMIT) --build-arg BUILD_TIME=$(BUILD_TIME)

.PHONY: build build-biz build-ai fmt fmt-check test test-race vet run-biz run-ai config-validate test-integration validate docker-build k8s-render clean

build: build-biz build-ai

# 两个目标共用同一构建参数，产物名称与 cmd 目录一一对应。
build-biz:
	@mkdir -p $(BIN_DIR)
	$(GO_BUILD) -o $(BIN_DIR)/biz-gateway ./cmd/biz-gateway

build-ai:
	@mkdir -p $(BIN_DIR)
	$(GO_BUILD) -o $(BIN_DIR)/ai-gateway ./cmd/ai-gateway

fmt:
	$(GO) fmt ./...

# gofmt 只负责格式，不自动整理三层 import；分组需在代码评审中保持。
fmt-check:
	@files="$$(gofmt -l cmd internal)"; \
	if [ -n "$$files" ]; then \
		printf '以下 Go 文件需要格式化:\n%s\n' "$$files"; \
		exit 1; \
	fi

test:
	$(GO) test ./...

test-race:
	$(GO) test -race ./...

vet:
	$(GO) vet ./...

run-biz:
	$(GO_RUN) ./cmd/biz-gateway serve

run-ai:
	$(GO_RUN) ./cmd/ai-gateway serve

# 不传 --config，专门验证两个入口各自选择的默认 profile。
config-validate: build
	$(BIN_DIR)/biz-gateway config validate
	$(BIN_DIR)/ai-gateway config validate

# 集成测试自行构建临时二进制，使用动态端口验证真实协议、跨实例灰度和退出信号。
test-integration:
	$(GO) test -tags=integration ./internal/app -run '^TestGateway' -count=1

validate: fmt-check vet test test-race config-validate test-integration

# Dockerfile 位于 deploy/，但上下文必须是仓库根目录以读取源码、go.mod 和 configs。
docker-build:
	docker build -f $(DOCKERFILE) --target biz-gateway $(DOCKER_BUILD_ARGS) -t next-gateway-biz:$(VERSION) .
	docker build -f $(DOCKERFILE) --target ai-gateway $(DOCKER_BUILD_ARGS) -t next-gateway-ai:$(VERSION) .

# 只渲染 Kustomize 清单，不连接集群，也不执行实际部署。
k8s-render:
	@kubectl kustomize $(KUSTOMIZE_DIR)

clean:
	rm -rf $(BIN_DIR) coverage.out
