// CFWarpXray 复刻 vh-warp 逻辑：WARP 通过命令行（warp-svc/warp-cli）初始化与自愈，
// 代理由进程内 Xray（SOCKS 16666 + HTTP 16667）提供，出口经系统路由走 WARP。
package main

import (
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"CFWarpXray/warp"
	"CFWarpXray/xray"
)

const (
	defaultLogDir     = "/var/log/warp-xray" // 默认日志目录，可通过环境变量 WARP_XRAY_LOG_DIR 覆盖
	monitorInterval   = 5 * time.Second      // 监控轮询间隔（与 vh-warp warp-monitor.sh 一致）
	xrayStartRetries  = 3                    // 全流程重启后 Xray 启动失败时的重试次数
	xrayStartRetryGap = 2 * time.Second      // 上述重试间隔
)

func main() {
	logDir := os.Getenv("WARP_XRAY_LOG_DIR")
	if logDir == "" {
		logDir = defaultLogDir
	}

	// 1. WARP 完整初始化（清理 → TUN → dbus → warp-svc → 注册/连接 → iptables）
	if err := warp.InitWarp(logDir); err != nil {
		log.Printf("[致命] WARP 初始化失败: %v", err)
		os.Exit(1)
	}

	// 2. 启动 Xray 代理（SOCKS 16666 + HTTP 16667，出口 freedom 走 WARP）
	var runner xray.Runner
	if err := runner.Start(nil); err != nil {
		log.Printf("[致命] Xray 启动失败: %v", err)
		os.Exit(1)
	}

	log.Printf("[启动] 服务就绪，代理端口 SOCKS %d / HTTP %d，日志目录 %s", xray.PortSOCKS, xray.PortHTTP, logDir)

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(sigCh)

	monitorLog := filepath.Join(logDir, "monitor.log")
	ticker := time.NewTicker(monitorInterval)
	defer ticker.Stop()

	// 启动后立即做一次健康检查，不等待首个 tick 周期
	runMonitor(logDir, monitorLog, &runner)

	// 3. 进入监控循环：每 5 秒检查 warp-svc 存活与 WARP 连接状态，异常时自愈
	for {
		select {
		case <-sigCh:
			log.Println("[退出] 收到信号，关闭 Xray 后退出")
			_ = runner.Stop()
			os.Exit(0)
		case <-ticker.C:
			runMonitor(logDir, monitorLog, &runner)
		}
	}
}

// runMonitor 执行一次监控：warp-svc 不在则全流程重启；仅未 Connected 则重连 WARP 并重启 Xray。
// 全流程重启后 Xray 启动失败会重试若干次，避免偶发端口占用或时序问题导致代理长时间不可用。
func runMonitor(logDir, monitorLog string, runner *xray.Runner) {
	if !warp.IsWarpSvcRunning() {
		warp.LogToFile(monitorLog, "[监控] warp-svc 未运行，执行全流程重启")
		_ = runner.Stop()
		if err := warp.FullRestartWarp(logDir); err != nil {
			warp.LogToFile(monitorLog, "[监控] 全流程重启失败: "+err.Error())
			return
		}
		var lastErr error
		for i := 0; i < xrayStartRetries; i++ {
			if err := runner.Start(nil); err != nil {
				lastErr = err
				warp.LogToFile(monitorLog, fmt.Sprintf("[监控] 全流程重启后 Xray 启动失败，第 %d/%d 次: %v", i+1, xrayStartRetries, err))
			} else {
				warp.LogToFile(monitorLog, "[监控] 全流程重启完成")
				return
			}
			if i < xrayStartRetries-1 {
				time.Sleep(xrayStartRetryGap)
			}
		}
		warp.LogToFile(monitorLog, fmt.Sprintf("[监控] 全流程重启后 Xray 启动仍失败（%v），下次周期再试", lastErr))
		return
	}
	if !warp.IsConnected() {
		warp.LogToFile(monitorLog, "[监控] WARP 已断开，尝试重连并重启 Xray")
		if err := warp.ReconnectWarp(); err != nil {
			warp.LogToFile(monitorLog, "[监控] 重连失败，触发全流程重启: "+err.Error())
			_ = runner.Stop()
			if err2 := warp.FullRestartWarp(logDir); err2 != nil {
				warp.LogToFile(monitorLog, "[监控] 全流程重启失败: "+err2.Error())
				return
			}
			for i := 0; i < xrayStartRetries; i++ {
				if err := runner.Start(nil); err == nil {
					return
				}
				if i < xrayStartRetries-1 {
					time.Sleep(xrayStartRetryGap)
				}
			}
			return
		}
		warp.LogToFile(monitorLog, "[监控] WARP 已重连，重启 Xray")
		if err := runner.Restart(); err != nil {
			warp.LogToFile(monitorLog, "[监控] Xray 重启失败: "+err.Error())
		}
	}
}
