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

const (
	// WarpSvcStartWait warp-svc 启动后等待其就绪的时间（官方推荐）
	WarpSvcStartWait = 3 * time.Second
	// WarpCliPollInterval warp-cli 轮询时的间隔
	WarpCliPollInterval = time.Second
	// WarpCliReadyTimeout 等待 warp-cli 可响应的最长时间
	WarpCliReadyTimeout = 60 * time.Second
	// ConnectPollTimeout 等待 WARP 状态变为 Connected 的最长时间
	ConnectPollTimeout = 120 * time.Second
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

	warpSvcLog := filepath.Join(logDir, "warp-svc.log")
	if err := StartWarpSvc(warpSvcLog); err != nil {
		return fmt.Errorf("启动 warp-svc: %w", err)
	}
	logInfo("warp-svc 已启动")

	if err := WarpCliReady(WarpCliReadyTimeout); err != nil {
		return fmt.Errorf("等待 warp-cli 就绪: %w", err)
	}
	logInfo("warp-cli 可用")

	if err := RegisterIfNeeded(); err != nil {
		return fmt.Errorf("注册/检查设备: %w", err)
	}

	if err := ConnectWarp(ConnectPollTimeout); err != nil {
		return fmt.Errorf("连接 WARP: %w", err)
	}
	logInfo("WARP 已连接")

	iface, err := GetWarpInterface()
	if err != nil {
		return fmt.Errorf("获取 WARP 网卡: %w", err)
	}
	logInfo("WARP 网卡: " + iface)

	if err := ConfigureIPTables(iface); err != nil {
		return fmt.Errorf("配置 iptables: %w", err)
	}
	logInfo("iptables 规则配置完成")
	logInfo("========================================")
	logInfo("WARP 初始化完成")
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
// 若不包含则执行 registration new 并等待 2 秒，确保设备已注册后再连接。
func RegisterIfNeeded() error {
	out, err := exec.Command("warp-cli", "--accept-tos", "registration", "show").CombinedOutput()
	if err != nil {
		return wrapCmdErr("warp-cli registration show", err)
	}
	if bytes.Contains(out, []byte("Device ID")) {
		return nil
	}
	if err := exec.Command("warp-cli", "--accept-tos", "registration", "new").Run(); err != nil {
		return wrapCmdErr("warp-cli registration new", err)
	}
	time.Sleep(2 * time.Second)
	return nil
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
func ConnectWarp(timeout time.Duration) error {
	out, err := exec.Command("warp-cli", "--accept-tos", "status").CombinedOutput()
	if err != nil && errors.Is(err, exec.ErrNotFound) {
		return wrapCmdErr("warp-cli status", err)
	}
	if err == nil && bytes.Contains(out, []byte("Connected")) {
		return nil
	}
	if err := exec.Command("warp-cli", "--accept-tos", "connect").Run(); err != nil {
		return wrapCmdErr("warp-cli connect", err)
	}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		out, err := exec.Command("warp-cli", "--accept-tos", "status").CombinedOutput()
		if err == nil && bytes.Contains(out, []byte("Connected")) {
			return nil
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

// GetWarpInterface 从 ip link show 输出中解析 WARP 虚拟网卡名；接口必须处于 UP 状态。
// 优先匹配名称含 CloudflareWARP，其次匹配含 warp 的接口，避免误选其他网卡。
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
	out2, err2 := exec.Command("ip", "link", "show", iface).CombinedOutput()
	if err2 != nil {
		return "", fmt.Errorf("查询网卡 %s 状态: %w", iface, err2)
	}
	if !bytes.Contains(out2, []byte("state UP")) && !bytes.Contains(out2, []byte("<UP,")) {
		return "", fmt.Errorf("WARP 网卡 %s 未处于 UP 状态", iface)
	}
	return iface, nil
}

// ConfigureIPTables 先清空 nat POSTROUTING 与 FORWARD 链，再添加与 vh-warp 一致的
// NAT MASQUERADE 与 FORWARD 规则，使经指定 WARP 网卡出站的流量正确转发。任一 -A 规则失败即返回错误。
// 注意：flush 阶段错误被忽略（如无权限时 -F 可能失败），仅 -A 阶段失败会返回错误。
func ConfigureIPTables(iface string) error {
	flush := [][]string{
		{"iptables", "-t", "nat", "-F", "POSTROUTING"},
		{"iptables", "-F", "FORWARD"},
	}
	for _, c := range flush {
		_ = exec.Command(c[0], c[1:]...).Run()
	}
	critical := [][]string{
		{"iptables", "-t", "nat", "-A", "POSTROUTING", "-o", iface, "-j", "MASQUERADE"},
		{"iptables", "-A", "FORWARD", "-o", iface, "-j", "ACCEPT"},
		{"iptables", "-A", "FORWARD", "-i", iface, "-m", "state", "--state", "RELATED,ESTABLISHED", "-j", "ACCEPT"},
	}
	for _, c := range critical {
		if out, err := exec.Command(c[0], c[1:]...).CombinedOutput(); err != nil {
			return fmt.Errorf("iptables %v 失败: %w, 输出: %s", c, err, out)
		}
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
	if err := ConnectWarp(ConnectPollTimeout); err != nil {
		return err
	}
	iface, err := GetWarpInterface()
	if err != nil {
		return err
	}
	return ConfigureIPTables(iface)
}
