# WARP 模式切换说明

本项目支持两种 WARP 运行模式，通过配置文件 `config/zero-trust.yaml` 中的 `service_mode` 字段控制：

## 模式说明

### 1. Proxy 模式 (`service_mode: "proxy"`)
- **推荐模式**，性能更好，干扰更少
- WARP 提供本地 SOCKS5 代理 (默认端口 40000)
- Xray 通过本地代理出站，所有流量经 WARP 加密
- 系统默认路由不受影响，不影响本地网络服务

### 2. TUN 全局模式 (`service_mode: "warp"` 或其他值)
- WARP 接管系统默认路由，所有流量经 TUN 设备转发
- Xray 直连出站（freedom），因为 WARP 已全局代理
- 可能影响本地网络服务，性能相对较低

## 配置方法

编辑 `config/zero-trust.yaml` 文件：

```yaml
# Proxy 模式（推荐）
service_mode: "proxy"
proxy_port: 40000

# 或 TUN 全局模式
service_mode: "warp"
# proxy_port 在 TUN 模式下不生效
```

## 使用建议

1. **优先使用 Proxy 模式**：性能更好，不影响系统路由
2. **仅在需要全局代理时使用 TUN 模式**：如需要代理所有系统级流量
3. **测试前备份配置**：模式切换可能影响网络连接

## 技术实现

- Proxy 模式：Xray 出站 → WARP 本地代理 → WARP TUN → 互联网
- TUN 模式：Xray 出站 → 系统路由 → WARP TUN → 互联网

两种模式下流量都会经过 WARP 加密，只是代理层级不同。