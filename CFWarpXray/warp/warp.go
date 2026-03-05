// Package warp 通过系统命令（warp-svc、warp-cli、iptables 等）完成 Cloudflare WARP 的
// 初始化与自愈，复刻 vh-warp 的 init-warp.sh / warp-monitor.sh 逻辑，供 main 在启动与
// 监控循环中调用。
package warp

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
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
)

const componentWarp = "warp"

// InitWarp 执行完整初始化流程：清理旧进程 → TUN 设备 → dbus → warp-svc →
// warp-cli 注册/连接 → 检测 WARP 网卡并配置 iptables。
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
	warpSvcLog := filepath.Join(logDir, "warp-svc.log")
	if err := StartWarpSvc(warpSvcLog); err != nil {
		return fmt.Errorf("启动 warp-svc: %w", err)
	}
	logInfo("warp-svc 已启动")

	if err := WarpCliReady(WarpCliReadyTimeout); err != nil {
		return fmt.Errorf("等待 warp-cli 就绪: %w", err)
	}
	logInfo("warp-cli 可用")

	logInfo("[步骤] 检查/注册设备（registration show → 若无则 registration new）...")
	if err := RegisterIfNeeded(logInfo); err != nil {
		return fmt.Errorf("注册/检查设备: %w", err)
	}
	logInfo("[步骤] 设备已注册")

	if err := ensureIptablesInstalled(initLog, logInfo); err != nil {
		return fmt.Errorf("准备 iptables: %w", err)
	}
	if err := SaveDefaultRoute(logDir); err != nil {
		logInfo("警告: 保存默认路由失败（" + err.Error() + "），将跳过策略路由，外网连代理可能断线")
	}
	logInfo("[步骤] 开始连接 WARP（轮询 status 等待 Connected，最多 3 分钟）...")
	if err := ConnectWarp(ConnectPollTimeout, logInfo); err != nil {
		return fmt.Errorf("连接 WARP: %w", err)
	}
	logInfo("WARP 已连接")

	logInfo("[步骤] 获取 WARP 网卡（ip link 解析 CloudflareWARP/warp）...")
	iface, err := GetWarpInterface()
	if err != nil {
		return fmt.Errorf("获取 WARP 网卡: %w", err)
	}
	logInfo("WARP 网卡: " + iface)

	if err := EnsureIPForward(); err != nil {
		return fmt.Errorf("开启 IPv4 转发: %w", err)
	}
	logInfo("IPv4 转发已开启")
	logInfo("[步骤] 配置 iptables（NAT + FORWARD）与策略路由（代理回复走原网卡）...")
	if err := ConfigureIPTables(iface, logDir); err != nil {
		return fmt.Errorf("配置 iptables: %w", err)
	}
	logInfo("iptables 规则配置完成")
	logInfo("========================================")
	logInfo("WARP 初始化完成")
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

// ensureIptablesInstalled 若未检测到 iptables（PATH 及 /usr/sbin、/sbin），则尝试 apt-get install iptables（仅 Debian/Ubuntu）。
// 可通过环境变量 WARP_XRAY_AUTO_INSTALL=0 关闭自动安装。需 root 与网络。
func ensureIptablesInstalled(initLog string, logInfo func(string)) error {
	if iptablesPath() != "iptables" {
		return nil
	}
	if getEnv("WARP_XRAY_AUTO_INSTALL") == "0" {
		return fmt.Errorf("未找到 iptables（可设 WARP_XRAY_AUTO_INSTALL≠0 自动安装或手动 apt-get install iptables）: %w", exec.ErrNotFound)
	}
	if _, err := exec.LookPath("apt-get"); err != nil {
		return fmt.Errorf("未找到 iptables；非 Debian/Ubuntu 或无 apt-get，请手动安装 iptables: %w", exec.ErrNotFound)
	}
	logInfo("未检测到 iptables，尝试自动安装（Debian/Ubuntu）")
	env := append(os.Environ(), "DEBIAN_FRONTEND=noninteractive")
	cmd := exec.Command("apt-get", "update")
	cmd.Env = env
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("apt-get update 失败: %w, 输出: %s", err, bytes.TrimSpace(out))
	}
	cmd = exec.Command("apt-get", "install", "-y", "iptables")
	cmd.Env = env
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("apt-get install iptables 失败: %w, 输出: %s", err, bytes.TrimSpace(out))
	}
	if iptablesPath() == "iptables" {
		return fmt.Errorf("安装后仍未找到 iptables，请检查 PATH 或手动安装")
	}
	logInfo("iptables 安装完成")
	return nil
}

// CleanOldProcess 清理残留的 warp-svc 进程（pkill -x warp-svc），
// 避免端口或 TUN 设备占用导致新实例启动失败；调用后睡眠 2 秒等待进程完全退出。
func CleanOldProcess() {
	_ = exec.Command("pkill", "-x", "warp-svc").Run()
	time.Sleep(2 * time.Second)
}

// EnsureTUN 确保 TUN 字符设备 /dev/net/tun 存在；若不存在则创建目录并 mknod。
// 主设备号 10、次设备号 200 与内核 TUN 约定一致。需 root 或 CAP_MKNOD（容器内通常已具备）。
func EnsureTUN() error {
	const tunPath = "/dev/net/tun"
	if _, err := os.Stat(tunPath); err == nil {
		return nil
	}
	if err := os.MkdirAll("/dev/net", 0755); err != nil {
		return err
	}
	if out, err := exec.Command("mknod", tunPath, "c", "10", "200").CombinedOutput(); err != nil {
		return fmt.Errorf("mknod %s: %v, %s", tunPath, err, out)
	}
	return os.Chmod(tunPath, 0600)
}

// EnsureDbus 确保系统 D-Bus 已运行（WARP 官方客户端依赖）。若未运行则尝试
// service dbus start，失败则直接启动 dbus-daemon；启动后校验 dbus-daemon 进程是否存在。
func EnsureDbus() error {
	if IsProcessRunning("dbus-daemon") {
		return nil
	}
	// 优先使用 service 启动（Debian/Ubuntu）
	if err := exec.Command("service", "dbus", "start").Run(); err == nil {
		time.Sleep(2 * time.Second)
		if IsProcessRunning("dbus-daemon") {
			return nil
		}
		return fmt.Errorf("service dbus start 后未检测到 dbus-daemon 进程")
	}
	// 备用：直接启动 dbus-daemon
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

// EnsureIPForward 开启 IPv4 转发（/proc/sys/net/ipv4/ip_forward=1），否则 iptables FORWARD 规则不生效；与 vh-warp 的 --sysctl net.ipv4.ip_forward=1 一致。
func EnsureIPForward() error {
	const path = "/proc/sys/net/ipv4/ip_forward"
	if err := os.WriteFile(path, []byte("1"), 0644); err != nil {
		return fmt.Errorf("开启 ip_forward: %w（需 root 或相应权限）", err)
	}
	return nil
}

// StartWarpSvc 在后台启动 warp-svc，将其 stdout/stderr 重定向到 logPath。
// 若已有 warp-svc 在运行则直接返回 nil。启动后等待 WarpSvcStartWait 并校验进程存活。
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

// IsProcessRunning 通过 pgrep -x 判断指定进程名是否在运行。
func IsProcessRunning(name string) bool {
	err := exec.Command("pgrep", "-x", name).Run()
	return err == nil
}

// WarpCliReady 轮询执行 warp-cli --accept-tos status，直到命令成功且输出非空（表示 warp-svc 已就绪）。
// 超时返回错误；若 err 为 exec.ErrNotFound 则通过 wrapCmdErr 包装为友好提示。
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

// RegisterIfNeeded 检查 warp-cli registration show 输出是否含 "Device ID"；
// 若不包含（或命令返回 exit 1，表示未注册）则执行 registration new 并等待 2 秒。
// logStep 可选，用于输出详细步骤日志；nil 则不打印。
func RegisterIfNeeded(logStep func(string)) error {
	if logStep != nil {
		logStep("  → 执行 warp-cli registration show")
	}
	out, err := exec.Command("warp-cli", "--accept-tos", "registration", "show").CombinedOutput()
	if err != nil && errors.Is(err, exec.ErrNotFound) {
		return wrapCmdErr("warp-cli registration show", err)
	}
	if err == nil && bytes.Contains(out, []byte("Device ID")) {
		if logStep != nil {
			logStep("  → 已有 Device ID，跳过注册")
		}
		return nil
	}
	// 需要注册时先清理可能存在的旧注册，避免 "Old registration is still around" 导致 new 失败
	if logStep != nil {
		logStep("  → 执行 warp-cli registration delete 清理旧注册（若有）")
	}
	_ = exec.Command("warp-cli", "--accept-tos", "registration", "delete").Run()
	time.Sleep(2 * time.Second)

	const regNewRetries = 3
	const regNewGap = 5 * time.Second
	var lastErr error
	var lastOut string
	for attempt := 0; attempt < regNewRetries; attempt++ {
		if logStep != nil {
			if attempt > 0 {
				logStep(fmt.Sprintf("  → 重试 registration new（%d/%d）", attempt+1, regNewRetries))
			} else {
				logStep("  → 未注册，执行 warp-cli registration new，等待 2 秒")
			}
		}
		cmd := exec.Command("warp-cli", "--accept-tos", "registration", "new")
		out, err := cmd.CombinedOutput()
		lastOut = string(bytes.TrimSpace(out))
		if err == nil {
			time.Sleep(2 * time.Second)
			return nil
		}
		lastErr = err
		if logStep != nil && lastOut != "" {
			logStep("  → registration new 失败: " + lastOut)
		}
		if attempt < regNewRetries-1 {
			// 若提示旧注册仍在，或 IPC 超时（daemon 可能残留状态），先 delete 再重试
			needDelete := strings.Contains(lastOut, "Old registration") ||
				strings.Contains(lastOut, "IPC call hit a timeout") ||
				strings.Contains(lastOut, "Error communicating with daemon")
			if needDelete {
				if logStep != nil {
					logStep("  → 执行 warp-cli registration delete 后重试")
				}
				_ = exec.Command("warp-cli", "--accept-tos", "registration", "delete").Run()
				time.Sleep(3 * time.Second) // 给 daemon 足够时间清理状态
			}
			time.Sleep(regNewGap)
		}
	}
	if lastOut != "" {
		return fmt.Errorf("warp-cli registration new 重试 %d 次均失败: %w（最后输出: %s）", regNewRetries, lastErr, lastOut)
	}
	return fmt.Errorf("warp-cli registration new 重试 %d 次均失败: %w", regNewRetries, lastErr)
}

// firstLine 返回 b 的第一行（不含换行），用于日志中简短显示 status 输出。
func firstLine(b []byte) string {
	idx := bytes.IndexByte(b, '\n')
	if idx >= 0 {
		return strings.TrimSpace(string(b[:idx]))
	}
	return strings.TrimSpace(string(b))
}

// wrapCmdErr 包装 exec 错误：若为 ErrNotFound 则附加「未找到命令，请确认已安装 cloudflare-warp」等说明。
func wrapCmdErr(cmd string, err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, exec.ErrNotFound) {
		return fmt.Errorf("%s 失败（未找到命令，请确认已安装 cloudflare-warp）: %w", cmd, err)
	}
	return fmt.Errorf("%s: %w", cmd, err)
}

// ConnectWarp 若当前 warp-cli status 未包含 Connected，则执行 connect 并轮询 status，
// 在 timeout 内等待出现 Connected；超时返回错误。
// logProgress 可选，轮询期间每隔约 15 秒会调用一次（如 "等待 WARP Connected... 已等待 15s"），便于定位卡在连接阶段。
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
		logProgress("  → 执行 warp-cli connect，开始轮询 status（最多 3 分钟，期间会打印当前状态）...")
	}
	if err := exec.Command("warp-cli", "--accept-tos", "connect").Run(); err != nil {
		return wrapCmdErr("warp-cli connect", err)
	}
	deadline := time.Now().Add(timeout)
	start := time.Now()
	var lastLogTime time.Time
	const progressInterval = 15 * time.Second
	const firstLogInterval = 5 * time.Second // 首次进度 5 秒即打，避免“卡住”错觉
	for time.Now().Before(deadline) {
		out, err := exec.Command("warp-cli", "--accept-tos", "status").CombinedOutput()
		if err == nil && bytes.Contains(out, []byte("Connected")) {
			return nil
		}
		statusLine := firstLine(bytes.TrimSpace(out))
		if logProgress != nil {
			elapsed := time.Since(start).Truncate(time.Second)
			shouldLog := false
			if lastLogTime.IsZero() {
				shouldLog = time.Since(start) >= firstLogInterval
			} else {
				shouldLog = time.Since(lastLogTime) >= progressInterval
			}
			if shouldLog {
				if statusLine != "" {
					logProgress(fmt.Sprintf("  等待 WARP Connected... 已等待 %v，当前: %s", elapsed, statusLine))
				} else {
					logProgress(fmt.Sprintf("  等待 WARP Connected... 已等待 %v", elapsed))
				}
				lastLogTime = time.Now()
			}
		}
		time.Sleep(WarpCliPollInterval)
	}
	return fmt.Errorf("WARP 在 %v 内未连接", timeout)
}

// IsConnected 通过 warp-cli --accept-tos status 判断当前是否处于 Connected 状态。
func IsConnected() bool {
	out, err := exec.Command("warp-cli", "--accept-tos", "status").CombinedOutput()
	return err == nil && bytes.Contains(out, []byte("Connected"))
}

// IsWarpSvcRunning 判断 warp-svc 进程是否在运行（供监控循环决策是否需全流程重启）。
func IsWarpSvcRunning() bool {
	return IsProcessRunning("warp-svc")
}

// isInterfaceUp 根据 ip link show 单接口输出判断是否可用：UP 或 UNKNOWN（隧道接口常为 UNKNOWN）均视为可用，仅 DOWN 不可用。
func isInterfaceUp(out []byte) bool {
	if bytes.Contains(out, []byte("state DOWN")) || bytes.Contains(out, []byte("<DOWN")) {
		return false
	}
	return bytes.Contains(out, []byte("state UP")) ||
		bytes.Contains(out, []byte("<UP,")) ||
		bytes.Contains(out, []byte("state UNKNOWN")) ||
		bytes.Contains(out, []byte("<UNKNOWN,"))
}

// GetWarpInterface 从 ip link show 输出中解析 WARP 虚拟网卡名；接口需非 DOWN（接受 UP/UNKNOWN）。
// 优先匹配名称含 CloudflareWARP，其次匹配含 warp 的接口。若为 DOWN 会尝试 ip link set up 并轮询等待最多约 25 秒。
func GetWarpInterface() (string, error) {
	out, err := exec.Command("ip", "link", "show").CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("执行 ip link: %w", err)
	}
	re := regexp.MustCompile(`(?m)^(\d+):\s+([^:@]+)`)
	var preferred, fallback string
	sc := bufio.NewScanner(bytes.NewReader(out))
	for sc.Scan() {
		line := sc.Text()
		matches := re.FindStringSubmatch(line)
		if len(matches) < 3 {
			continue
		}
		name := strings.TrimSpace(matches[2])
		if strings.Contains(name, "CloudflareWARP") {
			preferred = name
			break
		}
		if fallback == "" && strings.Contains(name, "warp") {
			fallback = name
		}
	}
	iface := preferred
	if iface == "" {
		iface = fallback
	}
	if iface == "" {
		return "", fmt.Errorf("未找到 WARP 网卡（ip link 中无 CloudflareWARP/warp）")
	}
	const maxWait = 25
	const pollInterval = time.Second
	var triedUp bool
	for i := 0; i < maxWait; i++ {
		out2, err2 := exec.Command("ip", "link", "show", iface).CombinedOutput()
		if err2 != nil {
			return "", fmt.Errorf("查询网卡 %s 状态: %w", iface, err2)
		}
		if isInterfaceUp(out2) {
			return iface, nil
		}
		if !triedUp {
			_ = exec.Command("ip", "link", "set", iface, "up").Run()
			triedUp = true
		}
		if i < maxWait-1 {
			time.Sleep(pollInterval)
		}
	}
	out2, _ := exec.Command("ip", "link", "show", iface).CombinedOutput()
	return "", fmt.Errorf("WARP 网卡 %s 未就绪（已等待 %d 秒），当前 ip link 输出: %s", iface, maxWait, bytes.TrimSpace(out2))
}

// iptablesPath 返回 iptables 可执行文件路径；PATH 中找不到时尝试 /usr/sbin/iptables、/sbin/iptables（常见于精简系统）。
func iptablesPath() string {
	if p, err := exec.LookPath("iptables"); err == nil {
		return p
	}
	for _, p := range []string{"/usr/sbin/iptables", "/sbin/iptables"} {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return "iptables" // 最后仍用原名，便于错误信息里体现
}

// 代理端口，与 xray 包一致，用于 iptables INPUT 放行（安全组开了但本机 iptables 可能拦入站）
const portSOCKS = 16666
const portHTTP = 16667

// 策略路由用：打标 16666/16667 入站连接，回复包查 table 100 走原默认网关，避免走 WARP 导致源 IP 错、外网连代理断。
const mainRouteTable = 100
const proxyReplyFwmark = 1
const mainRouteFileName = ".main_route"

// SaveDefaultRoute 在连接 WARP 前执行，保存当前默认路由（via GW dev IFACE）到 logDir/.main_route，
// 供 ConfigureIPTables 做策略路由：代理端口的回复包走原网卡，外网连 16666 不断。
func SaveDefaultRoute(logDir string) error {
	gw, iface, err := parseDefaultRouteFromShow()
	if err != nil {
		gw, iface, err = parseDefaultRouteFromGet()
	}
	if err != nil {
		return err
	}
	path := filepath.Join(logDir, mainRouteFileName)
	body := gw + "\n" + iface + "\n"
	if err := os.WriteFile(path, []byte(body), 0600); err != nil {
		return fmt.Errorf("写入 %s: %w", path, err)
	}
	return nil
}

func parseDefaultRouteFromShow() (gw, iface string, err error) {
	out, err := exec.Command("ip", "route", "show", "default").CombinedOutput()
	if err != nil || len(out) == 0 {
		return "", "", fmt.Errorf("无法获取默认路由: %w", err)
	}
	line := strings.TrimSpace(string(bytes.Split(out, []byte("\n"))[0]))
	viaRe := regexp.MustCompile(`via\s+(\S+)`)
	devRe := regexp.MustCompile(`dev\s+(\S+)`)
	via := viaRe.FindStringSubmatch(line)
	dev := devRe.FindStringSubmatch(line)
	if len(dev) < 2 {
		return "", "", fmt.Errorf("无法解析 dev: %s", line)
	}
	iface = dev[1]
	if len(via) >= 2 {
		return via[1], iface, nil
	}
	// 无 via（如 default dev eth0），用 ip route get 取网关
	return parseDefaultRouteFromGet()
}

func parseDefaultRouteFromGet() (gw, iface string, err error) {
	out, err := exec.Command("ip", "route", "get", "8.8.8.8").CombinedOutput()
	if err != nil || len(out) == 0 {
		return "", "", fmt.Errorf("ip route get 8.8.8.8 失败: %w", err)
	}
	line := strings.TrimSpace(string(bytes.Split(out, []byte("\n"))[0]))
	viaRe := regexp.MustCompile(`via\s+(\S+)`)
	devRe := regexp.MustCompile(`dev\s+(\S+)`)
	via := viaRe.FindStringSubmatch(line)
	dev := devRe.FindStringSubmatch(line)
	if len(dev) < 2 {
		return "", "", fmt.Errorf("无法从 ip route get 解析: %s", line)
	}
	iface = dev[1]
	if len(via) >= 2 {
		return via[1], iface, nil
	}
	// 直连（无 via），table 100 需要 default；可写 0.0.0.0 表示本机直连，但 ip route add default via 0.0.0.0 可能无效
	// 保守：要求必须有 via
	return "", "", fmt.Errorf("未找到网关（via）: %s", line)
}

// readDefaultRoute 从 logDir/.main_route 读取保存的网关与网卡（一行网关、一行网卡）。
func readDefaultRoute(logDir string) (gw, iface string, err error) {
	path := filepath.Join(logDir, mainRouteFileName)
	b, err := os.ReadFile(path)
	if err != nil {
		return "", "", err
	}
	lines := strings.SplitN(strings.TrimSpace(string(b)), "\n", 2)
	if len(lines) < 2 {
		return "", "", fmt.Errorf("无效格式: %s", path)
	}
	return strings.TrimSpace(lines[0]), strings.TrimSpace(lines[1]), nil
}

// ConfigureIPTables 先清空 nat POSTROUTING 与 FORWARD 链，再添加与 vh-warp 一致的
// NAT MASQUERADE 与 FORWARD 规则，放行 16666/16667 入站；若有保存的默认路由则添加策略路由，
// 使发往代理客户端的回复包走原网卡（table 100），避免走 WARP 导致外网连代理断。
func ConfigureIPTables(iface string, logDir string) error {
	ipt := iptablesPath()
	flush := [][]string{
		{ipt, "-t", "nat", "-F", "POSTROUTING"},
		{ipt, "-t", "mangle", "-F", "OUTPUT"},
		{ipt, "-F", "FORWARD"},
	}
	for _, c := range flush {
		_ = exec.Command(c[0], c[1:]...).Run()
	}
	critical := [][]string{
		{ipt, "-t", "nat", "-A", "POSTROUTING", "-o", iface, "-j", "MASQUERADE"},
		{ipt, "-A", "FORWARD", "-o", iface, "-j", "ACCEPT"},
		{ipt, "-A", "FORWARD", "-i", iface, "-m", "state", "--state", "RELATED,ESTABLISHED", "-j", "ACCEPT"},
		{ipt, "-I", "INPUT", "-p", "tcp", "--dport", fmt.Sprintf("%d", portSOCKS), "-j", "ACCEPT"},
		{ipt, "-I", "INPUT", "-p", "tcp", "--dport", fmt.Sprintf("%d", portHTTP), "-j", "ACCEPT"},
	}
	for _, c := range critical {
		if out, err := exec.Command(c[0], c[1:]...).CombinedOutput(); err != nil {
			return fmt.Errorf("iptables %v 失败: %w, 输出: %s", c, err, out)
		}
	}
	// 策略路由：所有从 WARP 网卡发出的包打 fwmark，走 table 100 走原网卡
	// 避免任何经 WARP 出站的包（包括 Xray 作为客户端的连接）导致源 IP 问题
	gw, mainIface, err := readDefaultRoute(logDir)
	if err != nil {
		return nil // 无保存的路由则只做上面规则，不报错
	}
	_ = exec.Command("ip", "rule", "del", "fwmark", fmt.Sprintf("%d", proxyReplyFwmark), "table", fmt.Sprintf("%d", mainRouteTable)).Run()
	_ = exec.Command("ip", "route", "flush", "table", fmt.Sprintf("%d", mainRouteTable)).Run()
	// --- 策略路由核心：用 CONNMARK 让代理端口的 **整个连接** 所有包都走 table 100 ---
	//
	// 为什么不能只用 MARK？
	// Linux 内核对后续包有 conntrack 路由缓存，mangle OUTPUT 里用 MARK 只在首包
	// 触发 re-route，后续包可能跳过 re-route 直接沿用缓存的路由（走了 WARP 的 table 65743），
	// 导致 SOCKS5 握手数据发不回客户端 → curl 报 "connection to proxy closed"。
	//
	// 方案：
	//  1) mangle OUTPUT: --sport 16666/16667 的新连接 → CONNMARK --save-mark（把 fwmark 存到 connmark）
	//  2) mangle OUTPUT: 所有包先 CONNMARK --restore-mark（从 connmark 恢复 fwmark），这样后续包也有 fwmark
	//  3) ip rule fwmark 0x1 lookup 100
	//  restore 规则必须放在 save 之前（-A 追加顺序），因为新连接首包需要先 MARK → save，
	//  而后续包需要先 restore 拿到 mark。实际上 restore 对新连接首包是无效的（connmark 为 0），
	//  所以把 restore 放第一条，save 放最后一条，逻辑正确。

	mangleRules := [][]string{
		// restore: 每个包进入 OUTPUT 时，从 connmark 恢复 fwmark（后续包靠这条拿到标记并触发 re-route）
		{ipt, "-t", "mangle", "-A", "OUTPUT", "-j", "CONNMARK", "--restore-mark"},
	}
	for _, port := range []int{portSOCKS, portHTTP} {
		mangleRules = append(mangleRules,
			// 首包：源端口是代理端口的包打 fwmark
			[]string{ipt, "-t", "mangle", "-A", "OUTPUT", "-p", "tcp", "--sport", fmt.Sprintf("%d", port), "-j", "MARK", "--set-mark", fmt.Sprintf("%d", proxyReplyFwmark)},
			// 首包：把 fwmark 存入 connmark，后续包的 restore 就能拿到
			[]string{ipt, "-t", "mangle", "-A", "OUTPUT", "-p", "tcp", "--sport", fmt.Sprintf("%d", port), "-j", "CONNMARK", "--save-mark"},
		)
	}
	for _, c := range mangleRules {
		if out, e := exec.Command(c[0], c[1:]...).CombinedOutput(); e != nil {
			return fmt.Errorf("iptables %v: %w, 输出: %s", c[2:], e, out)
		}
	}

	if out, e := exec.Command("ip", "rule", "add", "fwmark", fmt.Sprintf("%d", proxyReplyFwmark), "table", fmt.Sprintf("%d", mainRouteTable)).CombinedOutput(); e != nil {
		return fmt.Errorf("ip rule add: %w, 输出: %s", e, out)
	}
	if out, e := exec.Command("ip", "route", "add", "default", "via", gw, "dev", mainIface, "table", fmt.Sprintf("%d", mainRouteTable)).CombinedOutput(); e != nil {
		return fmt.Errorf("ip route add table %d: %w, 输出: %s", mainRouteTable, e, out)
	}

	// 代理回复包走 table 100 从 eth0 出去时，源 IP 可能仍是 WARP 内部地址，需要 MASQUERADE 改成公网 IP
	natMasq := []string{ipt, "-t", "nat", "-A", "POSTROUTING", "-m", "mark", "--mark", fmt.Sprintf("%d", proxyReplyFwmark), "-o", mainIface, "-j", "MASQUERADE"}
	if out, e := exec.Command(natMasq[0], natMasq[1:]...).CombinedOutput(); e != nil {
		return fmt.Errorf("iptables nat POSTROUTING mark→MASQUERADE: %w, 输出: %s", e, out)
	}
	return nil
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
// WarpCliReady → ConnectWarp → GetWarpInterface → ConfigureIPTables。logDir 为空时使用 "."。
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
	if err := ConnectWarp(ConnectPollTimeout, nil); err != nil {
		return err
	}
	iface, err := GetWarpInterface()
	if err != nil {
		return err
	}
	if err := EnsureIPForward(); err != nil {
		return err
	}
	return ConfigureIPTables(iface, logDir)
}
