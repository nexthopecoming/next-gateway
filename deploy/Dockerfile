# 构建阶段与 go.mod 的 Go 版本保持一致，并集中注入两个二进制的构建信息。
FROM golang:1.27.1 AS builder
WORKDIR /src

# 构建代理只影响 builder；该参数可覆盖，但不得用于传递带凭据的私有代理 URL。
ARG GOPROXY=https://proxy.golang.org,direct
ENV GOPROXY=${GOPROXY}

COPY go.mod go.sum ./
RUN go mod download
COPY . .

ARG VERSION=dev
ARG COMMIT=none
ARG BUILD_TIME=unknown
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath \
    -ldflags "-X github.com/nexthopecoming/next-gateway/internal/core/version.Version=${VERSION} -X github.com/nexthopecoming/next-gateway/internal/core/version.Commit=${COMMIT} -X github.com/nexthopecoming/next-gateway/internal/core/version.BuildTime=${BUILD_TIME}" \
    -o /out/biz-gateway ./cmd/biz-gateway \
    && CGO_ENABLED=0 GOOS=linux go build -trimpath \
    -ldflags "-X github.com/nexthopecoming/next-gateway/internal/core/version.Version=${VERSION} -X github.com/nexthopecoming/next-gateway/internal/core/version.Commit=${COMMIT} -X github.com/nexthopecoming/next-gateway/internal/core/version.BuildTime=${BUILD_TIME}" \
    -o /out/ai-gateway ./cmd/ai-gateway

# 两个运行时均使用 nonroot 用户；HTTP 探针由部署环境提供，不在镜像内增加 shell/curl。
FROM gcr.io/distroless/static-debian12:nonroot AS biz-gateway
WORKDIR /
COPY --from=builder /out/biz-gateway /usr/local/bin/biz-gateway
COPY configs/biz-gateway.yaml /configs/biz-gateway.yaml
USER 65532:65532
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/biz-gateway"]
CMD ["serve"]

FROM gcr.io/distroless/static-debian12:nonroot AS ai-gateway
WORKDIR /
COPY --from=builder /out/ai-gateway /usr/local/bin/ai-gateway
COPY configs/ai-gateway.yaml /configs/ai-gateway.yaml
USER 65532:65532
EXPOSE 8081
ENTRYPOINT ["/usr/local/bin/ai-gateway"]
CMD ["serve"]
