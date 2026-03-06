// CFWarpXray 复刻 vh-warp 逻辑：WARP 通过命令行（warp-svc/warp-cli）初始化与自愈，
// 代理由进程内 Xray（VLESS 16666 + HTTP 16667）提供，出口经系统路由走 WARP。
package main

import (
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"CFWarpXray/internal/logger"
	"CFWarpXray/warp"
	"CFWarpXray/xray"
)

const (
	// defaultLogDir 默认日志目录；可通过环境变量 WARP_XRAY_LOG_DIR 覆盖
	defaultLogDir = "/var/log/warp-xray"
	// monitorInterval 健康检查轮询间隔，与 vh-warp warp-monitor.sh 一致
	monitorInterval = 5 * time.Second
	// xrayStartRetries 全流程重启后 Xray 启动失败时的最大重试次数
	xrayStartRetries = 3
	// xrayStartRetryGap 上述重试之间的等待时间
	xrayStartRetryGap = 2 * time.Second
)

func main() {
	logDir := os.Getenv("WARP_XRAY_LOG_DIR")
	if logDir == "" {
		logDir = defaultLogDir
	}

	// 1. WARP 完整初始化：清理旧进程 → TUN → dbus → warp-svc → 注册/连接 → proxy mode 或 TUN mode
	ztCfg, err := warp.InitWarpWithConfig(logDir)
	if err != nil {
		logger.Stderr(logger.LevelError, "main", fmt.Sprintf("WARP 初始化失败，退出: %v", err))
		warp.Disconnect()
		os.Exit(1)
	}

	// 2. 启动 Xray 代理（VLESS 16666 + HTTP 16667，出口根据 WARP 模式决定；日志写入 logDir）
	xrayLogLevel := os.Getenv("WARP_XRAY_LOG_LEVEL")
	if xrayLogLevel == "" {
		xrayLogLevel = "info"
	}
	// 根据 WARP 模式决定 Xray 出站配置：
	// - proxy 模式：出站走 WARP Local Proxy（默认 127.0.0.1:40000）
	// - tun 模式：出站直连（freedom），因为 WARP 已接管全局路由
	var xrayConfig []byte
	if ztCfg.ServiceMode == "proxy" {
		xrayConfig, err = xray.BuildConfigProxy(xrayLogLevel, logDir, warp.WarpProxyPort())
	} else {
		xrayConfig, err = xray.BuildConfigDirect(xrayLogLevel, logDir)
	}
	if err != nil {
		logger.Stderr(logger.LevelError, "main", fmt.Sprintf("生成 Xray 配置失败: %v", err))
		warp.Disconnect()
		os.Exit(1)
	}
	// 将实际传给 Xray 的 JSON 写入 logDir，便于核对（与 xray-core 官方 log/inbounds/outbounds 格式一致）
	configPath := filepath.Join(logDir, "xray-config.json")
	if err := os.WriteFile(configPath, xrayConfig, 0644); err != nil {
		logger.Stderr(logger.LevelWarn, "main", "写入 xray-config.json 失败: "+err.Error())
	}
	var runner xray.Runner
	if err := runner.Start(xrayConfig); err != nil {
		logger.Stderr(logger.LevelError, "main", fmt.Sprintf("Xray 启动失败，退出: %v", err))
		warp.Disconnect()
		os.Exit(1)
	}
	defer func() { _ = runner.Stop() }() // 进程退出（含 panic）时尽量关闭 Xray

	logger.Stdout(logger.LevelInfo, "main",
		fmt.Sprintf("服务就绪，代理端口 VLESS %d / HTTP %d（WARP proxy mode），日志目录 %s", xray.PortVLESS, xray.PortHTTP, logDir))

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(sigCh)

	monitorLog := filepath.Join(logDir, "monitor.log")
	ticker := time.NewTicker(monitorInterval)
	defer ticker.Stop()

	// 串行化每次监控，避免全流程重启未完成时下一轮 tick 并发执行
	var monitorMu sync.Mutex
	// 启动后立即执行一次健康检查，不等待首个 tick
	monitorMu.Lock()
	runMonitor(logDir, monitorLog, &runner, xrayConfig)
	monitorMu.Unlock()

	// 3. 监控循环：每 5 秒检查 warp-svc 存活与 WARP 连接状态，异常时自愈
	for {
		select {
		case <-sigCh:
			logger.Stdout(logger.LevelInfo, "main", "收到退出信号，断开 WARP 并关闭 Xray 后退出")
			warp.Disconnect()
			_ = runner.Stop()
			os.Exit(0)
		case <-ticker.C:
			monitorMu.Lock()
			runMonitor(logDir, monitorLog, &runner, xrayConfig)
			monitorMu.Unlock()
		}
	}
}

// runMonitor 执行一次健康检查与自愈逻辑：
//   - 若 warp-svc 未运行：全流程重启 WARP，然后重试启动 Xray 至多 xrayStartRetries 次；
//   - 若 WARP 未 Connected：先尝试 ReconnectWarp，失败则全流程重启，再启动/重试 Xray；
//   - 重连成功后重启 Xray 使代理流量重新经 WARP。
// 内部 panic 会被 recover 并写入 monitor.log 与 stderr，不导致主进程退出。
func runMonitor(logDir, monitorLog string, runner *xray.Runner, xrayConfig []byte) {
	defer func() {
		if v := recover(); v != nil {
			msg := "监控 panic 已恢复: " + fmt.Sprint(v)
			logger.Stderr(logger.LevelError, "main", msg)
			logger.ToFileOnly(monitorLog, logger.LevelError, "main", msg)
		}
	}()
	if !warp.IsWarpSvcRunning() {
		logger.ToFileOnly(monitorLog, logger.LevelWarn, "main", "warp-svc 未运行，执行全流程重启")
		_ = runner.Stop()
		if err := warp.FullRestartWarp(logDir); err != nil {
			logger.ToFileOnly(monitorLog, logger.LevelError, "main", "全流程重启失败: "+err.Error())
			return
		}
		var lastErr error
		for i := 0; i < xrayStartRetries; i++ {
			if err := runner.Start(xrayConfig); err != nil {
				lastErr = err
				logger.ToFileOnly(monitorLog, logger.LevelWarn, "main",
					fmt.Sprintf("全流程重启后 Xray 启动失败，第 %d/%d 次: %v", i+1, xrayStartRetries, err))
			} else {
				logger.ToFileOnly(monitorLog, logger.LevelInfo, "main", "全流程重启完成")
				return
			}
			if i < xrayStartRetries-1 {
				time.Sleep(xrayStartRetryGap)
			}
		}
		logger.ToFileOnly(monitorLog, logger.LevelError, "main",
			fmt.Sprintf("全流程重启后 Xray 启动仍失败（%v），下次周期再试", lastErr))
		return
	}
	if !warp.IsConnected() {
		logger.ToFileOnly(monitorLog, logger.LevelWarn, "main", "WARP 已断开，尝试重连并重启 Xray")
		currentMode := warp.GetCurrentWarpMode()
		if err := warp.EnsureWarpMode(currentMode, nil); err != nil {
			logger.ToFileOnly(monitorLog, logger.LevelWarn, "main", fmt.Sprintf("重置 WARP %s 模式失败: %s", currentMode, err.Error()))
		}
		if err := warp.ReconnectWarp(); err != nil {
			logger.ToFileOnly(monitorLog, logger.LevelWarn, "main", "重连失败，触发全流程重启: "+err.Error())
			_ = runner.Stop()
			if err2 := warp.FullRestartWarp(logDir); err2 != nil {
				logger.ToFileOnly(monitorLog, logger.LevelError, "main", "全流程重启失败: "+err2.Error())
				if startErr := runner.Start(xrayConfig); startErr != nil {
					logger.ToFileOnly(monitorLog, logger.LevelError, "main", "部分恢复 Xray 失败: "+startErr.Error())
				} else {
					logger.ToFileOnly(monitorLog, logger.LevelWarn, "main", "已尝试部分恢复 Xray，请检查 WARP proxy 状态")
				}
				return
			}
			var startErr error
			for i := 0; i < xrayStartRetries; i++ {
				if err := runner.Start(xrayConfig); err == nil {
					logger.ToFileOnly(monitorLog, logger.LevelInfo, "main", "全流程重启完成")
					return
				}
				startErr = err
				if i < xrayStartRetries-1 {
					time.Sleep(xrayStartRetryGap)
				}
			}
			logger.ToFileOnly(monitorLog, logger.LevelError, "main",
				fmt.Sprintf("全流程重启后 Xray 启动仍失败（%v），下次周期再试", startErr))
			return
		}
		logger.ToFileOnly(monitorLog, logger.LevelInfo, "main", "WARP 已重连，重启 Xray")
		if err := runner.Restart(); err != nil {
			logger.ToFileOnly(monitorLog, logger.LevelError, "main", "Xray 重启失败: "+err.Error())
		}
	}
}
