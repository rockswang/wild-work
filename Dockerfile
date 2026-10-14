# wild-work Docker 部署
#
# 构建镜像：
#   docker build -t wild-work .
# 一键启动（推荐）：
#   docker compose up -d
# 直接运行（记得先设置管理员密码）：
#   docker run -d --name wild-work -p 7863:7863 \
#     -e WILDWORK_ADMIN_PASSWORD=你的强密码 \
#     -v ./auths:/app/auths -v ./data:/app/data \
#     wild-work

# ---------- 构建阶段 ----------
FROM golang:1.25-bookworm AS builder

WORKDIR /src

# 先复制 go.mod/go.sum，利用 Docker 缓存加速依赖下载
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# 与项目 CI 的 linux 构建保持一致（CGO_ENABLED=0，见 .github/workflows/build.yml）
RUN CGO_ENABLED=0 go build -trimpath -o /out/wild-work ./cmd/wild-work

# ---------- 运行阶段 ----------
FROM debian:bookworm-slim

# ca-certificates：上游渠道全是 HTTPS，没有它请求会失败
# tzdata：定时签到按本地时间触发，需要时区数据
RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates tzdata \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /app
COPY --from=builder /out/wild-work /app/wild-work

# 持久化目录（容器删除重建后账号与状态不丢失）
VOLUME ["/app/auths", "/app/data"]

EXPOSE 7863

# 容器内必须监听 0.0.0.0，否则宿主机端口映射进不来
ENV WILDWORK_LISTEN=0.0.0.0:7863

# --no-tray：无头模式（容器里没有桌面托盘）
# 注意：监听 0.0.0.0 时必须设置 WILDWORK_ADMIN_PASSWORD，否则程序拒绝启动
ENTRYPOINT ["/app/wild-work", "--no-tray"]
