// Package warp 通过系统命令（warp-svc、warp-cli）完成 Cloudflare WARP 的
// 初始化与自愈，并切换到 Local Proxy 模式供上层代理进程使用。
package warp

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"CFWarpXray/internal/logger"
)

// env 用于测试时替换；生产环境为 os.Getenv
var getEnv = os.Getenv

const (
	// WarpSvcStartWait warp-svc 启动后等待其就绪的时间（官方推荐）
	WarpSvcStartWait = 3 * time.Second
	// WarpCliPollInterval warp-cli 轮询时的间隔
	WarpCliPollInterval = time.Second
	// WarpCliReadyTimeout 等待 warp-cli 可响应的最长时间
	WarpCliReadyTimeout = 60 * time.Second
	// ConnectPollTimeout 等待 WARP 状态变为 Connected 的最长时间（首次连接可能经历多阶段，见 CF 文档 connectivity-status）
	ConnectPollTimeout = 180 * time.Second
	// ReconnectMaxRetries 断线重连时的最大重试次数
	ReconnectMaxRetries = 5
	// ReconnectRetrySleep 重连每次尝试之间的间隔
	ReconnectRetrySleep = 2 * time.Second
	// WarpProxyPortDefault WARP Local Proxy 默认端口
	WarpProxyPortDefault = 40000
	// RegistrationNewRetries registration new 失败（如 IPC 超时）时的重试次数
	RegistrationNewRetries = 3
	// RegistrationNewRetrySleep 每次 registration new 重试前的等待
	RegistrationNewRetrySleep = 10 * time.Second
	// WarpCliPostReadyWait warp-cli 就绪后额外等待时间，再执行 registration（避免 daemon 未完全就绪导致 IPC 超时）
	WarpCliPostReadyWait = 8 * time.Second
	// ConnectProgressLogInterval 轮询 Connected 时进度日志的最短间隔，避免刷屏
	ConnectProgressLogInterval = 15 * time.Second
)

const componentWarp = "warp"

// InitWarp 执行完整初始化流程：清理旧进程 → TUN 设备 → dbus → warp-svc →
// warp-cli 注册/连接，并切换到 Local Proxy 模式（不接管全局默认路由）。
// logDir 为日志目录，用于 warp-svc.log、init.log 等（如 /var/log/warp-xray）；空字符串时使用当前目录 "."。
// 返回 nil 表示全部步骤成功，否则返回包含步骤信息的错误。
func InitWarp(logDir string) error {
	if logDir == "" {
		logDir = "."
	}
	if err := os.MkdirAll(logDir, 0755); err != nil {
		return fmt.Errorf("创建日志目录 %s: %w", logDir, err)
	}
	initLog := filepath.Join(logDir, "init.log")
	logInfo := func(msg string) { logger.ToFile(initLog, logger.LevelInfo, componentWarp, msg) }

	logInfo("===== WARP + Xray 启动 =====")
	CleanOldProcess()
	logInfo("初始化环境")

	if err := EnsureTUN(); err != nil {
		return fmt.Errorf("准备 TUN 设备: %w", err)
	}
	logInfo("TUN 设备就绪")

	if err := EnsureDbus(); err != nil {
		return fmt.Errorf("启动 dbus: %w", err)
	}
	logInfo("dbus 就绪")

	if err := ensureWarpInstalled(initLog, logInfo); err != nil {
		return fmt.Errorf("启动 warp-svc: %w", err)
	}
	ztCfg, err := ApplyZeroTrustConfig(logInfo)
	if err != nil {
		return fmt.Errorf("加载 Zero Trust 配置: %w", err)
	}
	logInfo(fmt.Sprintf("Zero Trust 已启用：organization=%s, service_mode=%s, proxy_port=%d",
		ztCfg.Organization, ztCfg.ServiceMode, ztCfg.ProxyPort))
	logInfo("  → MDM 已写入 /var/lib/cloudflare-warp/mdm.xml，warp-svc 启动后将据此向 Zero Trust 注册")
	warpSvcLog := filepath.Join(logDir, "warp-svc.log")
	if err := StartWarpSvc(warpSvcLog); err != nil {
		return fmt.Errorf("启动 warp-svc: %w", err)
	}
	logInfo("warp-svc 已启动")

	if err := WarpCliReady(WarpCliReadyTimeout); err != nil {
		return fmt.Errorf("等待 warp-cli 就绪: %w", err)
	}
	logInfo("warp-cli 可用")
	time.Sleep(WarpCliPostReadyWait)

	logInfo("[步骤] 检查 Zero Trust 设备注册状态...")
	if err := RegisterIfNeeded(logInfo, false); err != nil {
		return fmt.Errorf("注册/检查设备: %w", err)
	}
	logInfo("[步骤] Zero Trust 注册状态检查完成")

	if err := EnsureWarpProxyMode(logInfo); err != nil {
		return fmt.Errorf("切换 WARP Proxy 模式: %w", err)
	}
	logInfo("WARP Proxy 模式已启用")
	logInfo("[步骤] 开始连接 WARP（轮询 status 等待 Connected，最多 3 分钟）...")
	if err := ConnectWarp(ConnectPollTimeout, logInfo); err != nil {
		return fmt.Errorf("连接 WARP: %w", err)
	}
	logInfo("WARP 已连接")
	logInfo("Proxy 模式无需配置全局路由/iptables 策略")
	logInfo("========================================")
	logInfo("WARP 初始化完成")
	return nil
}

// WarpProxyPort 返回期望的 WARP Local Proxy 端口，支持环境变量 WARP_XRAY_WARP_PROXY_PORT 覆盖。
func WarpProxyPort() int {
	v := strings.TrimSpace(getEnv("WARP_XRAY_WARP_PROXY_PORT"))
	if v == "" {
		return WarpProxyPortDefault
	}
	if p, err := strconv.Atoi(v); err == nil && p > 0 && p <= 65535 {
		return p
	}
	return WarpProxyPortDefault
}

// EnsureWarpProxyMode 设置 WARP 为 proxy 模式，并尽力设置代理端口。
// 对默认端口 40000，若旧版本客户端不支持 set-proxy-port，会容忍失败（通常默认即 40000）。
func EnsureWarpProxyMode(logInfo func(string)) error {
	modeCandidates := []struct {
		name string
		args []string
	}{
		{name: "set-mode proxy", args: []string{"--accept-tos", "set-mode", "proxy"}},
		{name: "mode proxy", args: []string{"--accept-tos", "mode", "proxy"}},
	}
	var modeOK bool
	var modeErrs []string
	for _, c := range modeCandidates {
		if logInfo != nil {
			logInfo("  → 尝试 warp-cli " + c.name)
		}
		out, err := exec.Command("warp-cli", c.args...).CombinedOutput()
		if err == nil {
			modeOK = true
			break
		}
		modeErrs = append(modeErrs, fmt.Sprintf("%s: %v (%s)", c.name, err, strings.TrimSpace(string(out))))
	}
	if !modeOK {
		return fmt.Errorf("设置 WARP proxy mode 失败（命令兼容尝试均失败）: %s", strings.Join(modeErrs, " | "))
	}
	port := WarpProxyPort()
	// 新版 warp-cli 用 "proxy port"；旧版用 "set-proxy-port"。先试新版避免多余失败日志。
	portCandidates := []struct {
		name string
		args []string
	}{
		{name: fmt.Sprintf("proxy port %d", port), args: []string{"--accept-tos", "proxy", "port", fmt.Sprintf("%d", port)}},
		{name: fmt.Sprintf("set-proxy-port %d", port), args: []string{"--accept-tos", "set-proxy-port", fmt.Sprintf("%d", port)}},
	}
	var portOK bool
	for _, c := range portCandidates {
		if logInfo != nil {
			logInfo("  → 尝试 warp-cli " + c.name)
		}
		if out, err := exec.Command("warp-cli", c.args...).CombinedOutput(); err == nil {
			portOK = true
			break
		} else if logInfo != nil {
			logInfo("  → " + c.name + " 失败: " + strings.TrimSpace(string(out)))
		}
	}
	if !portOK {
		if port == WarpProxyPortDefault {
			if logInfo != nil {
				logInfo("  → 代理端口命令不支持，继续使用客户端默认端口 40000")
			}
			return nil
		}
		return fmt.Errorf("设置 WARP proxy 端口失败，且当前端口非默认值 %d", port)
	}
	return nil
}

// ensureWarpInstalled 若 PATH 中无 warp-svc，则尝试安装 cloudflare-warp（仅 Debian/Ubuntu）。
// 可通过环境变量 WARP_XRAY_AUTO_INSTALL=0 关闭自动安装。需 root 与网络。
func ensureWarpInstalled(initLog string, logInfo func(string)) error {
	if _, err := exec.LookPath("warp-svc"); err == nil {
		return nil
	}
	if getEnv("WARP_XRAY_AUTO_INSTALL") == "0" {
		return wrapCmdErr("warp-svc", exec.ErrNotFound)
	}
	logInfo("未检测到 warp-svc，尝试自动安装 cloudflare-warp（Debian/Ubuntu）")
	if _, err := exec.LookPath("apt-get"); err != nil {
		return fmt.Errorf("%w；非 Debian/Ubuntu 或无 apt-get，请手动安装 cloudflare-warp", wrapCmdErr("warp-svc", exec.ErrNotFound))
	}
	env := append(os.Environ(), "DEBIAN_FRONTEND=noninteractive")
	run := func(name string, args ...string) error {
		cmd := exec.Command(name, args...)
		cmd.Env = env
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("%s %v: %w, 输出: %s", name, args, err, bytes.TrimSpace(out))
		}
		return nil
	}
	if err := run("apt-get", "update"); err != nil {
		return fmt.Errorf("apt-get update 失败: %w", err)
	}
	if err := run("apt-get", "install", "-y", "curl", "gnupg2", "ca-certificates"); err != nil {
		return fmt.Errorf("安装 curl/gpg 失败: %w", err)
	}
	keyringDir := "/usr/share/keyrings"
	if err := os.MkdirAll(keyringDir, 0755); err != nil {
		return fmt.Errorf("创建 %s: %w", keyringDir, err)
	}
	keyringPath := keyringDir + "/cloudflare-warp-archive-keyring.gpg"
	curl := exec.Command("curl", "-fsSL", "https://pkg.cloudflareclient.com/pubkey.gpg")
	curl.Env = env
	gpg := exec.Command("gpg", "--dearmor", "-o", keyringPath)
	gpg.Env = env
	curlOut, err := curl.StdoutPipe()
	if err != nil {
		return fmt.Errorf("curl 管道: %w", err)
	}
	gpg.Stdin = curlOut
	if err := curl.Start(); err != nil {
		return fmt.Errorf("curl: %w", err)
	}
	if err := gpg.Run(); err != nil {
		_ = curl.Wait()
		return fmt.Errorf("gpg keyring: %w", err)
	}
	_ = curl.Wait()
	codename := "bookworm"
	if out, err := exec.Command("lsb_release", "-cs").CombinedOutput(); err == nil {
		codename = strings.TrimSpace(string(out))
	}
	archOut, err := exec.Command("dpkg", "--print-architecture").CombinedOutput()
	if err != nil {
		return fmt.Errorf("dpkg --print-architecture: %w", err)
	}
	arch := strings.TrimSpace(string(archOut))
	line := fmt.Sprintf("deb [arch=%s signed-by=%s] https://pkg.cloudflareclient.com/ %s main", arch, keyringPath, codename)
	if err := os.WriteFile("/etc/apt/sources.list.d/cloudflare-client.list", []byte(line+"\n"), 0644); err != nil {
		return fmt.Errorf("写入 apt 源: %w", err)
	}
	if err := run("apt-get", "update"); err != nil {
		return fmt.Errorf("apt-get update（Cloudflare 源）失败: %w", err)
	}
	if err := run("apt-get", "install", "-y", "cloudflare-warp"); err != nil {
		return fmt.Errorf("apt-get install cloudflare-warp 失败: %w", err)
	}
	if _, err := exec.LookPath("warp-svc"); err != nil {
		return fmt.Errorf("安装后仍未找到 warp-svc: %w", err)
	}
	logInfo("cloudflare-warp 安装完成")
	return nil
}

// CleanOldProcess 清理残留的 warp-svc 进程，避免旧实例占用状态导致初始化失败。
func CleanOldProcess() {
	_ = exec.Command("pkill", "-x", "warp-svc").Run()
	time.Sleep(2 * time.Second)
}

// EnsureTUN 确保 TUN 设备存在（/dev/net/tun）。
func EnsureTUN() error {
	const tunPath = "/dev/net/tun"
	if _, err := os.Stat(tunPath); err == nil {
		return nil
	}
	if err := os.MkdirAll("/dev/net", 0755); err != nil {
		return err
	}
	if out, err := exec.Command("mknod", tunPath, "c", "10", "200").CombinedOutput(); err != nil {
		return fmt.Errorf("mknod %s: %v, %s", tunPath, err, bytes.TrimSpace(out))
	}
	return os.Chmod(tunPath, 0600)
}

// EnsureDbus 确保系统 dbus-daemon 运行（WARP 依赖）。
func EnsureDbus() error {
	if IsProcessRunning("dbus-daemon") {
		return nil
	}
	if err := exec.Command("service", "dbus", "start").Run(); err == nil {
		time.Sleep(2 * time.Second)
		if IsProcessRunning("dbus-daemon") {
			return nil
		}
		return fmt.Errorf("service dbus start 后未检测到 dbus-daemon")
	}
	cmd := exec.Command("dbus-daemon", "--system")
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("启动 dbus-daemon: %w", err)
	}
	time.Sleep(2 * time.Second)
	if !IsProcessRunning("dbus-daemon") {
		return fmt.Errorf("dbus-daemon 启动后未持续运行")
	}
	return nil
}

// StartWarpSvc 在后台启动 warp-svc 并将输出写入 logPath。
func StartWarpSvc(logPath string) error {
	if IsProcessRunning("warp-svc") {
		return nil
	}
	f, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		return err
	}
	cmd := exec.Command("warp-svc")
	cmd.Stdout = f
	cmd.Stderr = f
	if err := cmd.Start(); err != nil {
		_ = f.Close()
		return wrapCmdErr("warp-svc", err)
	}
	_ = f.Close()
	time.Sleep(WarpSvcStartWait)
	if !IsProcessRunning("warp-svc") {
		return fmt.Errorf("warp-svc 启动后未持续运行")
	}
	return nil
}

// IsProcessRunning 通过 pgrep -x 判断进程是否运行。
func IsProcessRunning(name string) bool {
	return exec.Command("pgrep", "-x", name).Run() == nil
}

// WarpCliReady 轮询 warp-cli status，直到可响应。
func WarpCliReady(timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		out, err := exec.Command("warp-cli", "--accept-tos", "status").CombinedOutput()
		if err == nil && len(out) > 0 {
			return nil
		}
		if err != nil && errors.Is(err, exec.ErrNotFound) {
			return wrapCmdErr("warp-cli", err)
		}
		time.Sleep(WarpCliPollInterval)
	}
	return fmt.Errorf("warp-cli 在 %v 内未就绪", timeout)
}

// RegisterIfNeeded 检查并按需注册 WARP 设备。
func RegisterIfNeeded(logStep func(string), allowInteractive bool) error {
	if logStep != nil {
		logStep("  → 执行 warp-cli registration show")
	}
	out, err := exec.Command("warp-cli", "--accept-tos", "registration", "show").CombinedOutput()
	if err != nil && errors.Is(err, exec.ErrNotFound) {
		return wrapCmdErr("warp-cli registration show", err)
	}
	if logStep != nil {
		summary := firstLine(bytes.TrimSpace(out))
		if summary != "" {
			logStep("  → registration show: " + summary)
		}
	}
	if err == nil && bytes.Contains(out, []byte("Device ID")) {
		if logStep != nil {
			logStep("  → 已有 Device ID，已加入 Zero Trust 组织")
		}
		return nil
	}
	if !allowInteractive {
		if logStep != nil {
			logStep("  → 当前无 Device ID；首次 connect 时将由 Service Token 完成注册（请确认后台 Device enrollment 已添加 Service Auth）")
		}
		return nil
	}
	if logStep != nil {
		logStep("  → 执行 warp-cli registration delete 清理旧注册（若有）")
	}
	_ = exec.Command("warp-cli", "--accept-tos", "registration", "delete").Run()
	time.Sleep(2 * time.Second)

	var lastErr error
	for attempt := 1; attempt <= RegistrationNewRetries; attempt++ {
		if attempt > 1 {
			if logStep != nil {
				logStep(fmt.Sprintf("  → 第 %d 次重试 registration new（共 %d 次）", attempt, RegistrationNewRetries))
			}
			time.Sleep(RegistrationNewRetrySleep)
		}
		cmd := exec.Command("warp-cli", "--accept-tos", "registration", "new")
		out, err := cmd.CombinedOutput()
		if err == nil {
			time.Sleep(2 * time.Second)
			return nil
		}
		lastErr = fmt.Errorf("warp-cli registration new: %w（输出: %s）", err, bytes.TrimSpace(out))
	}
	return lastErr
}

func firstLine(b []byte) string {
	idx := bytes.IndexByte(b, '\n')
	if idx >= 0 {
		return strings.TrimSpace(string(b[:idx]))
	}
	return strings.TrimSpace(string(b))
}

func wrapCmdErr(cmd string, err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, exec.ErrNotFound) {
		return fmt.Errorf("%s 失败（未找到命令，请确认已安装 cloudflare-warp）: %w", cmd, err)
	}
	return fmt.Errorf("%s: %w", cmd, err)
}

// ConnectWarp 执行 connect 并轮询直到 Connected。
func ConnectWarp(timeout time.Duration, logProgress func(string)) error {
	out, err := exec.Command("warp-cli", "--accept-tos", "status").CombinedOutput()
	if err != nil && errors.Is(err, exec.ErrNotFound) {
		return wrapCmdErr("warp-cli status", err)
	}
	if err == nil && bytes.Contains(out, []byte("Connected")) {
		if logProgress != nil {
			logProgress("  → 当前已 Connected，跳过连接")
		}
		return nil
	}
	if logProgress != nil {
		logProgress("  → 执行 warp-cli connect，开始轮询 status（最多 3 分钟）")
	}
	if err := exec.Command("warp-cli", "--accept-tos", "connect").Run(); err != nil {
		return wrapCmdErr("warp-cli connect", err)
	}
	deadline := time.Now().Add(timeout)
	var lastLog time.Time
	for time.Now().Before(deadline) {
		out, err := exec.Command("warp-cli", "--accept-tos", "status").CombinedOutput()
		if err == nil && bytes.Contains(out, []byte("Connected")) {
			return nil
		}
		if logProgress != nil {
			now := time.Now()
			if now.Sub(lastLog) >= ConnectProgressLogInterval {
				status := firstLine(bytes.TrimSpace(out))
				if status != "" {
					logProgress("  等待 WARP Connected，当前: " + status)
				}
				lastLog = now
			}
		}
		time.Sleep(WarpCliPollInterval)
	}
	return fmt.Errorf("WARP 在 %v 内未连接（请检查 Zero Trust 后台 Device enrollment 是否已添加 Service Auth、Token 是否有效；可执行 warp-cli registration show 与 warp-cli status 排查）", timeout)
}

// IsConnected 判断当前 WARP 是否处于 Connected。
func IsConnected() bool {
	out, err := exec.Command("warp-cli", "--accept-tos", "status").CombinedOutput()
	return err == nil && bytes.Contains(out, []byte("Connected"))
}

// IsWarpSvcRunning 判断 warp-svc 是否在运行。
func IsWarpSvcRunning() bool {
	return IsProcessRunning("warp-svc")
}

// ReconnectWarp 多次执行 warp-cli connect 并轮询 IsConnected，最多 ReconnectMaxRetries 次；
// 任一次检测到已连接则返回 nil，否则返回错误。
func ReconnectWarp() error {
	for i := 0; i < ReconnectMaxRetries; i++ {
		_ = exec.Command("warp-cli", "--accept-tos", "connect").Run()
		time.Sleep(ReconnectRetrySleep)
		if IsConnected() {
			return nil
		}
	}
	return fmt.Errorf("重连失败，已重试 %d 次", ReconnectMaxRetries)
}

// Disconnect 执行 warp-cli disconnect，用于程序报错退出前恢复默认路由（避免 SSH 等断线）。忽略错误。
func Disconnect() {
	_ = exec.Command("warp-cli", "--accept-tos", "disconnect").Run()
}

// FullRestartWarp 执行全流程重启：CleanOldProcess → EnsureDbus → StartWarpSvc →
// WarpCliReady → set-mode proxy → ConnectWarp。logDir 为空时使用 "."。
func FullRestartWarp(logDir string) error {
	if logDir == "" {
		logDir = "."
	}
	if err := os.MkdirAll(logDir, 0755); err != nil {
		return fmt.Errorf("创建日志目录 %s: %w", logDir, err)
	}
	CleanOldProcess()
	if err := EnsureTUN(); err != nil {
		return err
	}
	if err := EnsureDbus(); err != nil {
		return err
	}
	warpSvcLog := filepath.Join(logDir, "warp-svc.log")
	if err := StartWarpSvc(warpSvcLog); err != nil {
		return err
	}
	if err := WarpCliReady(WarpCliReadyTimeout); err != nil {
		return err
	}
	if err := EnsureWarpProxyMode(nil); err != nil {
		return err
	}
	if err := ConnectWarp(ConnectPollTimeout, nil); err != nil {
		return err
	}
	return nil
}
