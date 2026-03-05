# CFWarpXray

复刻 [vh-warp](https://github.com/uxiaohan/vh-warp) 逻辑：WARP 通过命令行（warp-svc/warp-cli）初始化与自愈，代理由进程内 Xray（SOCKS 16666 + HTTP 16667）提供，出口经系统路由走 WARP。

## 环境要求

- Linux（Debian/Ubuntu 等）；**cloudflare-warp**（warp-svc、warp-cli）若未安装，程序会在 Debian/Ubuntu 上尝试自动安装（需 root 与网络，可设 `WARP_XRAY_AUTO_INSTALL=0` 禁用）
- 可选：`ip`、`iptables`、`dbus`、`pgrep`、`pkill`（与 vh-warp 一致）
- 运行需 root 或具备相应权限（创建 TUN、配置 iptables、自动安装时执行 apt-get）

## 资源与内存

本方案**内存占用较高**，主要来自：

| 组件 | 说明与大致占用 |
|------|----------------|
| **warp-svc** | Cloudflare 官方常驻进程，约 **150～400MB+**，连接/重连时可能更高 |
| **Xray**（进程内） | 代理核心，约 **几十～一百多 MB**，随连接数增加 |
| **本程序** | Go 进程本身，约 **几十 MB** |

**2 核 2G 的机器**在同时跑系统、sshd 等时，很容易被 OOM 杀进程。建议：

- **推荐**：至少 **2.5G～3G 内存**，或 **2G + 512MB～1G swap** 缓解；
- 若必须用 2G：先加 swap（例如 `sudo fallocate -l 1G /swapfile && sudo chmod 600 /swapfile && sudo mkswap /swapfile && sudo swapon /swapfile`），再跑本程序，并留意 `dmesg` 是否出现 OOM。

## 环境变量

| 变量 | 说明 | 默认值 |
|------|------|--------|
| `WARP_XRAY_LOG_DIR` | 日志目录（init.log、monitor.log、warp-svc.log、xray-access.log、xray-error.log） | `/var/log/warp-xray` |
| `WARP_XRAY_LOG_LEVEL` | Xray 内核日志级别：`debug`、`info`、`warning`、`error`、`none`；设为 `info` 可看到连接等更多日志 | `warning` |
| `WARP_XRAY_AUTO_INSTALL` | 设为 `0` 时禁用自动安装 cloudflare-warp；未设置或非 0 时，若未检测到 warp-svc 会在 Debian/Ubuntu 上尝试 `apt-get install cloudflare-warp`（需 root 与网络） | 启用 |

## 代理端口

- **16666**：SOCKS5
- **16667**：HTTP 代理

## 构建与运行

```bash
go build -o cfwarpxray .
./cfwarpxray
```

### 以 systemd 常驻运行

复制二进制和 unit 文件后启用服务，断 SSH 也会继续跑：

```bash
# 1. 复制二进制（若用 /root/main 则跳过，并在下面步骤 3 里改服务文件中的路径）
sudo cp cfwarpxray /usr/local/bin/

# 2. 复制服务文件
sudo cp cfwarpxray.service /etc/systemd/system/

# 3. 若二进制在 /root/main，先改服务里的路径：
#    sudo sed -i 's|/usr/local/bin/cfwarpxray|/root/main|' /etc/systemd/system/cfwarpxray.service

# 4. 重载并启用、启动
sudo systemctl daemon-reload
sudo systemctl enable cfwarpxray
sudo systemctl start cfwarpxray
sudo systemctl status cfwarpxray
```

常用命令：`status` 查看状态，`stop` 停止，`restart` 重启；日志：`journalctl -u cfwarpxray -f`。

可选环境变量可放在 `/etc/default/cfwarpxray`（如 `WARP_XRAY_LOG_DIR=/var/log/warp-xray`），服务会自动读取。

## Docker

镜像需安装 cloudflare-warp，运行时需与 vh-warp 相同的权限（见 Dockerfile 顶部注释）：

- `--cap-add=NET_ADMIN --cap-add=NET_RAW --cap-add=MKNOD`
- `--device-cgroup-rule 'c 10:200 rwm'`
- `-p 16666:16666 -p 16667:16667`

```bash
docker build -t cfwarpxray .
docker run -d --name cfwarpxray --cap-add=NET_ADMIN --cap-add=NET_RAW --cap-add=MKNOD \
  --device-cgroup-rule 'c 10:200 rwm' -p 16666:16666 -p 16667:16667 cfwarpxray
```

## 日志与排错

- 初始化失败：查看 stderr 及 `$WARP_XRAY_LOG_DIR/init.log`
- 运行中自愈：查看 `monitor.log`
- warp-svc 自身输出：`warp-svc.log`
- Xray 入站/错误：`xray-access.log`、`xray-error.log`
- 未找到命令：确认已安装 cloudflare-warp 且 `warp-svc`、`warp-cli` 在 PATH 中
- iptables 失败：确认有 NET_ADMIN 权限或容器已按上例加 cap
- 代理无流量：程序会自动开启 `net.ipv4.ip_forward=1`（与 vh-warp 的 `--sysctl net.ipv4.ip_forward=1` 一致），若仍无流量请检查 iptables 与 WARP 网卡

## 与 vh-warp 的差异

| 项目 | vh-warp | CFWarpXray |
|------|---------|------------|
| 代理 | GOST 二进制，单端口 16666（mixed） | Xray 库，16666 SOCKS + 16667 HTTP |
| WARP | shell 脚本 + warp-svc/warp-cli | Go 内 exec 调用相同命令 |
| 监控 | warp-monitor.sh + supervisor | Go 内 5 秒轮询 + 互斥串行 |
| 入口 | init-warp.sh 后 supervisord | 单进程：init 后直接监控循环 |
| 系统 | 镜像内预装 iptables、Docker 传 `ip_forward=1` | 可自动安装 iptables、程序内开启 ip_forward |
