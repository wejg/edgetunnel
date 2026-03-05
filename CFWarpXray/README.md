# CFWarpXray

复刻 [vh-warp](https://github.com/uxiaohan/vh-warp) 逻辑：WARP 通过命令行（warp-svc/warp-cli）初始化与自愈，代理由进程内 Xray（SOCKS 16666 + HTTP 16667）提供，出口经系统路由走 WARP。

## 环境要求

- Linux（Debian/Ubuntu 等），已安装 **cloudflare-warp**（warp-svc、warp-cli）
- 可选：`ip`、`iptables`、`dbus`、`pgrep`、`pkill`（与 vh-warp 一致）
- 运行需 root 或具备相应权限（创建 TUN、配置 iptables）

## 环境变量

| 变量 | 说明 | 默认值 |
|------|------|--------|
| `WARP_XRAY_LOG_DIR` | 日志目录（init.log、monitor.log、warp-svc.log） | `/var/log/warp-xray` |

## 代理端口

- **16666**：SOCKS5
- **16667**：HTTP 代理

## 构建与运行

```bash
go build -o cfwarpxray .
./cfwarpxray
```

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
- 未找到命令：确认已安装 cloudflare-warp 且 `warp-svc`、`warp-cli` 在 PATH 中
- iptables 失败：确认有 NET_ADMIN 权限或容器已按上例加 cap

## 与 vh-warp 的差异

| 项目 | vh-warp | CFWarpXray |
|------|---------|------------|
| 代理 | GOST 二进制，单端口 16666（mixed） | Xray 库，16666 SOCKS + 16667 HTTP |
| WARP | shell 脚本 + warp-svc/warp-cli | Go 内 exec 调用相同命令 |
| 监控 | warp-monitor.sh + supervisor | Go 内 5 秒轮询 + 互斥串行 |
| 入口 | init-warp.sh 后 supervisord | 单进程：init 后直接监控循环 |
