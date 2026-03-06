# 运行时需 cap: NET_ADMIN, NET_RAW, MKNOD 及 device cgroup (c 10:200)，与 vh-warp 一致
# Build stage
FROM golang:1.25-bookworm AS builder
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -o /cfwarpxray .

# Runtime stage (replicate vh-warp: debian + cloudflare-warp, no GOST/supervisor)
FROM debian:bookworm-slim

ENV DEBIAN_FRONTEND=noninteractive TZ=Asia/Shanghai

RUN apt update && apt install -y --no-install-recommends \
    curl gnupg2 ca-certificates procps iproute2 dbus iptables \
    && apt clean && rm -rf /var/lib/apt/lists/*

# Install Cloudflare WARP (same as vh-warp)
RUN curl -fsSL https://pkg.cloudflareclient.com/pubkey.gpg | gpg --dearmor -o /usr/share/keyrings/cloudflare-warp-archive-keyring.gpg \
    && echo "deb [arch=$(dpkg --print-architecture) signed-by=/usr/share/keyrings/cloudflare-warp-archive-keyring.gpg] https://pkg.cloudflareclient.com/ bookworm main" | tee /etc/apt/sources.list.d/cloudflare-client.list \
    && apt update && apt install -y cloudflare-warp \
    && apt clean && rm -rf /var/lib/apt/lists/*

RUN mkdir -p /var/log/warp-xray /etc/cfwarpxray /var/lib/cloudflare-warp

COPY --from=builder /cfwarpxray /usr/local/bin/cfwarpxray
# Zero Trust 配置从 builder 阶段复制，确保任意构建上下文下镜像内都有该文件；可用 -v 挂载覆盖
COPY --from=builder /app/config/zero-trust.yaml /etc/cfwarpxray/zero-trust.yaml

EXPOSE 16666 16667

CMD ["/usr/local/bin/cfwarpxray"]
