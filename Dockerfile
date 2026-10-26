# 多阶段构建：编译阶段拉取模块并产出静态二进制，运行阶段只保留该二进制。
FROM golang:1.26-alpine AS builder

WORKDIR /src

# 先复制模块文件，依赖未变时复用 go mod download 层缓存。
COPY go.mod go.sum ./
RUN go mod download

# 源码须包含 embed.go 以及 src/frontend 下被 go:embed 引用的模板与静态资源。
COPY . .

# SQLite 驱动为纯 Go（modernc.org/sqlite），无需 C 工具链。
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/maildrop ./cmd/maildrop

FROM alpine:3.22

# 时区数据与 CA 证书：日志时间与出站 TLS 需要。运行时不安装 curl。
RUN apk add --no-cache tzdata ca-certificates

WORKDIR /app

COPY --from=builder /out/maildrop /app/maildrop

# 数据含邮件正文与访问令牌，仅允许容器内 root 读取。
RUN mkdir -p /app/data && \
    chmod 700 /app/data

# 暴露端口 (SMTP: 25, Web: 5000)
EXPOSE 25 5000

# 丢弃响应体，避免 Docker 健康日志每 30 秒重复保存整页 HTML。
# 运行时未安装 curl，使用镜像自带的 BusyBox wget。
HEALTHCHECK --interval=30s --timeout=10s --start-period=5s --retries=3 \
    CMD wget -q -O /dev/null http://127.0.0.1:5000/ || exit 1

# 继续以 root 运行：已有部署的 bind mount 中 data/mailbox.db 为 root 所有，
# 且容器内需要绑定 25 端口。
CMD ["/app/maildrop"]
