// Package xray 在进程内通过 xray-core 提供 SOCKS5/HTTP 代理（替代 vh-warp 中的 GOST），
// 出口使用 freedom，走系统默认路由（即经 WARP 网卡出站）；与 vh-warp 暴露端口一致。
package xray

import (
	"encoding/json"
	"fmt"
	"sync"

	"CFWarpXray/internal/logger"
	"github.com/xtls/xray-core/core"
)

const (
	// PortSOCKS SOCKS5 代理端口，与 vh-warp 一致
	PortSOCKS = 16666
	// PortHTTP HTTP 代理端口（Xray 无单端口 mixed，故与 SOCKS 分开）
	PortHTTP = 16667
)

const componentXray = "xray"

// Config 为 Xray 的 JSON 配置结构，仅包含本程序用到的字段（log、inbounds、outbounds）。
type Config struct {
	Log       *LogConfig       `json:"log,omitempty"`
	Inbounds  []InboundObject  `json:"inbounds"`
	Outbounds []OutboundObject `json:"outbounds"`
}

// LogConfig 控制 Xray 内核日志级别，如 "warning" 可减少控制台输出。
type LogConfig struct {
	Loglevel string `json:"loglevel,omitempty"`
}

// InboundObject 对应 Xray 入站（listen、port、protocol、settings 等）。
type InboundObject struct {
	Listen   string      `json:"listen,omitempty"`
	Port     json.Number `json:"port"`
	Protocol string      `json:"protocol"`
	Tag      string      `json:"tag,omitempty"`
	Settings interface{} `json:"settings,omitempty"`
	Sniffing *Sniffing   `json:"sniffing,omitempty"`
}

// Sniffing 入站嗅探配置；本程序未启用。
type Sniffing struct {
	Enabled      bool     `json:"enabled"`
	DestOverride []string `json:"destOverride,omitempty"`
}

// OutboundObject 对应 Xray 出站（protocol、tag、settings）。
type OutboundObject struct {
	Protocol string      `json:"protocol"`
	Tag      string      `json:"tag,omitempty"`
	Settings interface{} `json:"settings,omitempty"`
}

// BuildConfig 生成 Xray JSON 配置：0.0.0.0:16666 SOCKS、0.0.0.0:16667 HTTP，
// 出站为 freedom，domainStrategy AsIs。logLevel 非空时设置 log.loglevel（如 "warning"）。
func BuildConfig(logLevel string) ([]byte, error) {
	cfg := Config{
		Inbounds: []InboundObject{
			{
				Listen:   "0.0.0.0",
				Port:     json.Number(fmt.Sprintf("%d", PortSOCKS)),
				Protocol: "socks",
				Tag:      "socks-in",
				Settings: map[string]interface{}{
					"auth": "noauth",
					"udp":  true,
				},
			},
			{
				Listen:   "0.0.0.0",
				Port:     json.Number(fmt.Sprintf("%d", PortHTTP)),
				Protocol: "http",
				Tag:      "http-in",
				Settings: map[string]interface{}{},
			},
		},
		Outbounds: []OutboundObject{
			{
				Protocol: "freedom",
				Tag:      "direct",
				Settings: map[string]interface{}{
					"domainStrategy": "AsIs",
				},
			},
		},
	}
	if logLevel != "" {
		cfg.Log = &LogConfig{Loglevel: logLevel}
	}
	return json.MarshalIndent(cfg, "", "  ")
}

// Runner 持有当前 Xray 实例与配置，提供 Start/Stop/Restart，供 main 启动与监控循环重启用。
type Runner struct {
	mu       sync.Mutex
	instance *core.Instance
	config   []byte
}

// Start 使用给定 JSON 配置启动 Xray；config 为 nil 时使用 BuildConfig("warning")。
// 若已有 instance 且未在运行（异常退出或已 Close），会先置 nil 再启动新实例，避免悬空引用。
func (r *Runner) Start(config []byte) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.instance != nil {
		if r.instance.IsRunning() {
			logger.Stdout(logger.LevelInfo, componentXray, "实例已在运行，跳过启动")
			return nil
		}
		r.instance = nil
	}
	if config == nil {
		var err error
		config, err = BuildConfig("warning")
		if err != nil {
			return err
		}
	}
	r.config = config
	inst, err := core.StartInstance("json", config)
	if err != nil {
		return fmt.Errorf("xray StartInstance: %w", err)
	}
	r.instance = inst
	logger.Stdout(logger.LevelInfo, componentXray, fmt.Sprintf("已启动，SOCKS %d / HTTP %d", PortSOCKS, PortHTTP))
	return nil
}

// Stop 关闭当前 Xray 实例并置空引用；若本无实例则直接返回 nil。
func (r *Runner) Stop() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.instance == nil {
		return nil
	}
	err := r.instance.Close()
	r.instance = nil
	logger.Stdout(logger.LevelInfo, componentXray, "已停止")
	return err
}

// Restart 先 Stop 再使用当前保存的配置 Start；监控发现 WARP 重连后调用以刷新代理。
func (r *Runner) Restart() error {
	cfg := r.config
	if cfg == nil {
		var err error
		cfg, err = BuildConfig("warning")
		if err != nil {
			return err
		}
	}
	_ = r.Stop()
	return r.Start(cfg)
}
