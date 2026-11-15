# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## 当前状态

阶段 1“工程骨架与本地运行”已完成，并已交付阶段 3/4 的本地 file 配置垂直切片。`cmd/biz-gateway` 默认使用 8080，`cmd/ai-gateway` 默认使用 8081；两个薄 main 共享 `internal/command`、配置、日志和 HTTP 生命周期。AI 进程已提供 OpenAI/Anthropic 严格文本子集、非流式 JSON、SSE、本地 Mock、stable/canary Engine、确定性灰度、取消/超时/draining 和真实双进程协议测试。已有本地 YAML profile、环境变量/CLI 覆盖、请求 ID、访问日志、recovery、health/ready、优雅退出、Makefile、`deploy/Dockerfile` 双 target 构建和 `deploy/k8s/` 基础演示清单。Go module 为 `github.com/nexthopecoming/next-gateway`，Go 版本为 1.27.1。阶段 2 尚未开始；阶段 4A-1 已交付 core Router、Pool RR/WRR 和通用下游 HTTP Dispatcher 及真实本地后端测试，但当前 app 仍固定注入一个 Mock Upstream，`ai.proxy` 配置、AI 协议 Adapter 和进程接入尚未实现。etcd、热更新、Metrics、Compose、真实 Provider、故障切换、完整计费和 Tracing 尚未实现，阶段 3/4 仍保持进行中。

`docs/` 是开发基线：设计文档描述目标行为，`docs/implementation.md` 描述实际交付状态。不要把规划中的包、命令或能力当成已经存在。

## 仓库特定工作规则

- 需求或设计变化先更新 `docs/` 中对应文档，再实现；代码不得静默偏离文档。
- 每完成一个开发步骤，必须在同批改动中更新 `docs/implementation.md`，记录实际改动、验证命令、真实结果和已知限制。
- 新配置项默认不可热更新；只有设计文档明确列入动态白名单、具有安全应用器和测试后才能热更新。
- 默认本地 Mock 行为必须保持可运行；本地 HTTP 代理阶段只连接配置指定的受控下游，不接真实 Provider。真实 Provider、跨上游故障切换、完整计费和 OpenTelemetry 是后续范围，不能提前标记为已支持。
- 未经用户明确要求，不创建 Git commit 或 push。
- 每次开发的代码，必须在关键步骤、关键函数、关键脚本等有中文注释。
- Go import 固定按“标准库、第三方包、本项目包”三组排列，本项目 module 放最后一组；`gofmt` 不会自动完成该分组。

## 命令

优先使用 Makefile，确保两个二进制使用一致的构建信息：

```bash
# 构建、启动与配置校验
make build
make run-biz
make run-ai
make config-validate

# 测试、真实双进程集成测试与完整检查
make test
make test-race
make vet
make test-integration
make validate

# 构建两个 Docker 镜像；Dockerfile 在 deploy/，上下文仍是仓库根目录
make docker-build

# 只渲染 Kubernetes 清单，不连接集群
make k8s-render
```

不使用 Make 时：

```bash
go build ./cmd/...
go run ./cmd/biz-gateway serve
go run ./cmd/ai-gateway serve
go run ./cmd/biz-gateway config validate
go run ./cmd/ai-gateway config validate
go test ./...
go test -race ./...
go vet ./...

# 运行单个测试：替换 TestName 或包路径以运行其他用例
go test ./internal/core/config -run '^TestLoadPrecedence$' -count=1
```

当前没有已配置的独立 lint 工具。

Compose 文件规划在 `deploy/compose/`。文件落地后，从该目录执行：

```bash
docker compose up --build
```

Kubernetes 基础演示清单位于 `deploy/k8s/`，部署方法见 `deploy/README.md`；`make k8s-render` 只做本地 Kustomize 渲染，不连接集群，也不代表资源已通过集群 schema 校验或实际运行验证。

涉及热更新、灰度或 SSE 的改动需要运行 race 检测；当前已有两个本地 AI 实例的灰度一致性集成测试，但这不代表 etcd 多实例配置传播已经实现。Compose 和 etcd 测试只在对应实现存在后执行，不要把未运行的检查写成通过。

## 架构要点

完整设计见 `docs/architecture.md`，实现时最需要保持的边界如下：

- 服务网络层使用标准库 `net/http`，CLI 组合为 Cobra + pflag + Viper；不要再引入标准库 `flag` 的第二套解析流程。
- `core` 是共享基础库；`biz` 和 `ai` 各只有一套实现。双 core 指 core 内 stable 基础顺序执行链与 canary 钩子编排链共存，不是复制整个仓库或启动两套 biz/ai 服务；两者必须通过相同协议黄金测试。
- 两个可执行文件只通过 `internal/command` 选择角色，依赖方向为 `cmd -> internal/command -> internal/app -> core/biz/ai`。角色枚举由 `internal/core/role` 定义，`command`、`app` 与后续配置校验共享类型化角色，不使用字符串比较。`core` 不得 import `biz`/`ai`，`biz` 与 `ai` 不得相互 import；它们通过 core 契约协作。
- `biz-gateway`/`ai-gateway` 是部署职责划分，stable/canary 是 AI 请求内部执行器，不得把两个概念混用；两条 AI 路由只在 `ai-gateway` 注册，biz 进程不转发。
- 请求开始时固定 core 版本、配置快照和配置 revision。热更新及灰度回滚只影响新请求，不能切换进行中请求或 SSE 流。目标代理模式还要固定 Route、Pool、Endpoint 和 Transport；本阶段不自动重试，一个 SSE 流不能换下游续写。
- 通用下游 HTTP 转发、Route Table、Pool 和 Endpoint 负载均衡分别由 `core/proxy`、`core/router`、`core/balancer` 实现；AI 协议与 Provider 适配属于 `ai`。灰度分桶、路由选择和 Endpoint 选择是三个不同层次，详见 `docs/proxy-and-load-balancing.md`。当前仅 4A-1 core 库已交付，运行进程尚未接入。
- 同时提供 OpenAI `POST /v1/chat/completions` 与 Anthropic `POST /v1/messages` 的严格文本子集；未知字段及工具、图片等不支持能力返回 400，协议黄金样例见 `docs/api-and-canary.md`。
- SSE 必须逐事件 flush，并传播客户端取消、首包/空闲/总流超时和服务退出；heartbeat 使用 SSE 注释，响应开始后不能通过换 core 或换上游续写。
- 独立管理端口提供 Bearer API 与 Basic Auth Web 控制台。发布是 202 异步过程，通过 etcd lease 心跳展示实例收敛；浏览器写操作必须校验 CSRF，具体契约见 `docs/admin-console.md`。

## 配置与灰度的不变量

详细规则见 `docs/configuration.md` 和 `docs/api-and-canary.md`。

完整设计优先级为：显式 CLI > 显式环境变量 > 最新有效 etcd > 最后有效远程快照 > 本地 YAML > 内置默认值；当前本地实现只有显式 CLI > 环境变量 > YAML > 默认值。未来 etcd 模式下本地文件 Watch 不与健康远程配置竞争；非法远程配置保留旧运行状态且不得覆盖有效快照。本地 file 请求使用 `config_version=local-file`、`config_revision=0`，不伪装成远程发布状态。

灰度身份顺序为 `X-Tenant-ID`、Bearer API Key、`x-api-key`，缺失身份固定 stable。哈希输入为 `v1|identity_kind|identity_value`，取 SHA-256 前 8 字节的大端无符号整数模 10000，bucket 小于 `canary_basis_points` 时进入 canary。不要使用进程随机数、实例名或 Go 随机种子哈希；该算法变化会导致请求方重新分布。

这里是无 Redis 的确定性哈希分桶，不是网关实例选择的一致性哈希环。强制名单、调试 Header 和权重边界必须遵循文档中的决策顺序。

## 可观测性与扩展边界

- 日志使用 zap，Metrics 使用 Prometheus，健康与就绪语义见 `docs/observability.md`。
- 租户、API Key、请求 ID、原始 URL 和无界错误文本不能成为 Metrics 标签；身份原文和 AI 正文默认不进入日志。
- etcd 断连但仍有有效配置时保持 ready，同时暴露 Watch 降级状态，避免全实例被健康检查驱逐。
- 总体 Mock 阶段只建立 usage 事件，不实现价格、扣费或账单。Mock usage 只能用于测试，不能作为真实费用依据。

## 设计文档导航

- `docs/architecture.md`：模块职责、依赖方向、生命周期和部署。
- `docs/design-patterns.md`：设计模式的已采用范围、目标落点、不适用边界与实施顺序。
- `docs/proxy-and-load-balancing.md`：core 反向代理、Route/Pool/Endpoint、RR/WRR、Header/URL 安全、SSE 和代理验收。
- `docs/configuration.md`：配置字段、优先级、etcd revision、快照与热更新。
- `docs/admin-console.md`：管理认证、异步发布、回滚、实例心跳、Web 控制台与 CSRF。
- `docs/api-and-canary.md`：严格协议子集、SSE、Mock、黄金样例和灰度算法。
- `docs/observability.md`：日志字段、指标、标签基数和健康检查。
- `docs/implementation.md`：阶段计划、Definition of Done 和实际实现记录。
