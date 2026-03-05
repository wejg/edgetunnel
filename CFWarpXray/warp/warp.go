// Package warp 通过系统命令（warp-svc、warp-cli、iptables 等）完成 WARP 的初始化与自愈，
// 复刻 vh-warp 的 init-warp.sh / warp-monitor.sh 逻辑。
package warp

import (
	"bufio"
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// 与 vh-warp 脚本一致的超时与重试常量
const (
	WarpSvcStartWait     = 3 * time.Second  // warp-svc 启动后等待时间（官方推荐）
	WarpCliPollInterval  = time.Second      // warp-cli 轮询间隔
	WarpCliReadyTimeout  = 60 * time.Second // 等待 warp-cli 可用的最长时间
	ConnectPollTimeout   = 120 * time.Second // 等待 WARP Connected 的最长时间
	ReconnectMaxRetries = 5                 // 断线重连最大重试次数
	ReconnectRetrySleep  = 2 * time.Second  // 重连每次间隔
)

// InitWarp 执行完整初始化流程：清理旧进程 → TUN 设备 → dbus → warp-svc →
// warp-cli 注册/连接 → 检测 WARP 网卡并配置 iptables。
// logDir 为日志目录，用于 warp-svc.log、init.log 等（如 /var/log/warp-xray）。
func InitWarp(logDir string) error {
	if err := os.MkdirAll(logDir, 0755); err != nil {
		return fmt.Errorf("创建日志目录 %s: %w", logDir, err)
	}
	initLog := filepath.Join(logDir, "init.log")
	log := func(msg string) { logLine(initLog, msg) }

	log("===== WARP + Xray 启动 =====")
	CleanOldProcess()
	log("初始化环境")

	if err := EnsureTUN(); err != nil {
		return fmt.Errorf("ensure TUN: %w", err)
	}
	log("TUN 设备就绪")

	if err := EnsureDbus(); err != nil {
		return fmt.Errorf("ensure dbus: %w", err)
	}
	log("dbus 就绪")

	warpSvcLog := filepath.Join(logDir, "warp-svc.log")
	if err := StartWarpSvc(warpSvcLog); err != nil {
		return fmt.Errorf("start warp-svc: %w", err)
	}
	log("warp-svc 已启动")

	if err := WarpCliReady(WarpCliReadyTimeout); err != nil {
		return fmt.Errorf("warp-cli ready: %w", err)
	}
	log("warp-cli 可用")

	if err := RegisterIfNeeded(); err != nil {
		return fmt.Errorf("register if needed: %w", err)
	}

	if err := ConnectWarp(ConnectPollTimeout); err != nil {
		return fmt.Errorf("connect warp: %w", err)
	}
	log("WARP 已连接")

	iface, err := GetWarpInterface()
	if err != nil {
		return fmt.Errorf("get warp interface: %w", err)
	}
	log("WARP 网卡: " + iface)

	if err := ConfigureIPTables(iface); err != nil {
		return fmt.Errorf("configure iptables: %w", err)
	}
	log("iptables 规则配置完成")
	log("========================================")
	log("WARP 初始化完成")
	return nil
}

// logLine 将带时间戳的一行写入指定日志文件，并同时打印到标准输出。
func logLine(logPath, msg string) {
	ts := time.Now().Format("2006-01-02 15:04:05")
	line := "[" + ts + "] " + msg
	fmt.Println(line)
	f, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		return
	}
	_, _ = f.WriteString(line + "\n")
	_ = f.Close()
}

// LogToFile 向指定文件追加一行带时间戳的日志（供 main 或监控逻辑使用）。
func LogToFile(logPath, msg string) {
	logLine(logPath, msg)
}

// CleanOldProcess 清理残留的 warp-svc 进程，避免端口或设备占用冲突。
func CleanOldProcess() {
	_ = exec.Command("pkill", "-x", "warp-svc").Run()
	time.Sleep(2 * time.Second)
}

// EnsureTUN 确保 /dev/net/tun 存在；不存在则创建（需 root，容器内已具备）。
func EnsureTUN() error {
	const tunPath = "/dev/net/tun"
	if _, err := os.Stat(tunPath); err == nil {
		return nil
	}
	if err := os.MkdirAll("/dev/net", 0755); err != nil {
		return err
	}
	// 字符设备 主设备号 10 次设备号 200，与内核 TUN 约定一致
	if out, err := exec.Command("mknod", tunPath, "c", "10", "200").CombinedOutput(); err != nil {
		return fmt.Errorf("mknod %s: %v, %s", tunPath, err, out)
	}
	return os.Chmod(tunPath, 0600)
}

// EnsureDbus 确保 dbus 已运行（WARP 官方客户端依赖）。
func EnsureDbus() error {
	if IsProcessRunning("dbus-daemon") {
		return nil
	}
	// 优先使用 service 启动（Debian/Ubuntu）
	if err := exec.Command("service", "dbus", "start").Run(); err == nil {
		time.Sleep(2 * time.Second)
		return nil
	}
	// 备用：直接启动 dbus-daemon
	cmd := exec.Command("dbus-daemon", "--system")
	if err := cmd.Start(); err != nil {
		return err
	}
	time.Sleep(2 * time.Second)
	return nil
}

// StartWarpSvc 在后台启动 warp-svc，标准输出/错误重定向到 logPath。
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
		return err
	}
	// 子进程已继承 fd，父进程关闭自己的 fd 不影响子进程继续写该日志文件
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

// WarpCliReady 轮询直到 warp-cli 可正常响应（即 warp-svc 已就绪）。
func WarpCliReady(timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		out, err := exec.Command("warp-cli", "--accept-tos", "status").CombinedOutput()
		if err == nil && len(out) > 0 {
			return nil
		}
		time.Sleep(WarpCliPollInterval)
	}
	return fmt.Errorf("warp-cli 在 %v 内未就绪", timeout)
}

// RegisterIfNeeded 若尚未注册设备（registration show 无 Device ID），则执行 registration new。
func RegisterIfNeeded() error {
	out, err := exec.Command("warp-cli", "--accept-tos", "registration", "show").CombinedOutput()
	if err != nil {
		return err
	}
	if bytes.Contains(out, []byte("Device ID")) {
		return nil
	}
	if err := exec.Command("warp-cli", "--accept-tos", "registration", "new").Run(); err != nil {
		return err
	}
	time.Sleep(2 * time.Second)
	return nil
}

// ConnectWarp 若未处于 Connected，则执行 connect 并轮询直到 status 含 Connected。
func ConnectWarp(timeout time.Duration) error {
	out, _ := exec.Command("warp-cli", "--accept-tos", "status").CombinedOutput()
	if bytes.Contains(out, []byte("Connected")) {
		return nil
	}
	if err := exec.Command("warp-cli", "--accept-tos", "connect").Run(); err != nil {
		return err
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

// IsConnected 判断当前 warp-cli status 是否包含 Connected。
func IsConnected() bool {
	out, err := exec.Command("warp-cli", "--accept-tos", "status").CombinedOutput()
	return err == nil && bytes.Contains(out, []byte("Connected"))
}

// IsWarpSvcRunning 判断 warp-svc 进程是否存活。
func IsWarpSvcRunning() bool {
	return IsProcessRunning("warp-svc")
}

// GetWarpInterface 从 ip link 输出中解析 WARP 虚拟网卡名，且必须为 UP 状态。
// 优先匹配 CloudflareWARP，其次匹配含 warp 的接口名，避免误匹配其他网卡。
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

// ConfigureIPTables 配置 NAT 与 FORWARD，使经 WARP 网卡出站的流量正确转发（与 vh-warp 一致）。
// 关键规则（-A）失败时返回错误，便于排查权限或 iptables 不可用问题。
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

// ReconnectWarp 尝试重连 WARP，最多重试 ReconnectMaxRetries 次；成功返回 nil。
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

// FullRestartWarp 全流程重启：清理 warp-svc → 启动 dbus → 启动 warp-svc → 等待 cli → 连接 → 配置 iptables。
func FullRestartWarp(logDir string) error {
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
