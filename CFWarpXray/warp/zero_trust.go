package warp

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

const (
	zeroTrustConfigEnv     = "WARP_XRAY_ZERO_TRUST_CONFIG"
	zeroTrustConfigPathAbs = "/etc/cfwarpxray/zero-trust.yaml"
	warpMDMConfigPath      = "/var/lib/cloudflare-warp/mdm.xml"
)

// ZeroTrustConfig 描述本项目使用的 Zero Trust 注册参数。
type ZeroTrustConfig struct {
	Enabled          bool   `yaml:"enabled"`
	Organization     string `yaml:"organization"`
	AuthClientID     string `yaml:"auth_client_id"`
	AuthClientSecret string `yaml:"auth_client_secret"`
	ServiceMode      string `yaml:"service_mode"`
	ProxyPort        int    `yaml:"proxy_port"`
	AutoConnect      int    `yaml:"auto_connect"`
}

func loadZeroTrustConfig() (*ZeroTrustConfig, string, error) {
	raw, path, err := readZeroTrustConfigFile()
	if err != nil {
		return nil, path, fmt.Errorf("读取 Zero Trust 配置失败: %w", err)
	}
	var cfg ZeroTrustConfig
	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		return nil, path, fmt.Errorf("解析 Zero Trust 配置失败: %w", err)
	}
	if !cfg.Enabled {
		return nil, path, fmt.Errorf("Zero Trust 配置未启用（enabled=false）")
	}
	cfg.Organization = strings.TrimSpace(cfg.Organization)
	cfg.AuthClientID = strings.TrimSpace(cfg.AuthClientID)
	cfg.AuthClientSecret = strings.TrimSpace(cfg.AuthClientSecret)
	cfg.ServiceMode = strings.TrimSpace(cfg.ServiceMode)
	if cfg.Organization == "" {
		return nil, path, fmt.Errorf("Zero Trust 配置缺少 organization")
	}
	if cfg.AuthClientID == "" {
		return nil, path, fmt.Errorf("Zero Trust 配置缺少 auth_client_id")
	}
	if cfg.AuthClientSecret == "" {
		return nil, path, fmt.Errorf("Zero Trust 配置缺少 auth_client_secret")
	}
	if cfg.ServiceMode == "" {
		cfg.ServiceMode = "proxy"
	}
	if cfg.ProxyPort <= 0 || cfg.ProxyPort > 65535 {
		cfg.ProxyPort = WarpProxyPort()
	}
	if cfg.AutoConnect < 0 {
		cfg.AutoConnect = 1
	}
	return &cfg, path, nil
}

func readZeroTrustConfigFile() ([]byte, string, error) {
	envPath := strings.TrimSpace(getEnv(zeroTrustConfigEnv))
	if envPath != "" {
		raw, err := os.ReadFile(envPath)
		return raw, envPath, err
	}

	// 优先绝对路径，避免 systemd 等服务场景受 cwd 影响。
	candidates := []string{zeroTrustConfigPathAbs}
	if exePath, err := os.Executable(); err == nil {
		candidates = append(candidates, filepath.Join(filepath.Dir(exePath), "config", "zero-trust.yaml"))
	}
	candidates = append(candidates, "config/zero-trust.yaml")

	var lastErr error
	for _, p := range candidates {
		raw, err := os.ReadFile(p)
		if err == nil {
			return raw, p, nil
		}
		lastErr = err
	}
	return nil, zeroTrustConfigPathAbs, fmt.Errorf("未找到可用配置文件（依次尝试: %s）: %w", strings.Join(candidates, ", "), lastErr)
}

// ApplyZeroTrustConfig 读取并校验 Zero Trust 配置，随后写入 WARP 的 MDM 配置文件。
func ApplyZeroTrustConfig(logStep func(string)) (*ZeroTrustConfig, error) {
	cfg, path, err := loadZeroTrustConfig()
	if err != nil {
		if logStep != nil {
			logStep("Zero Trust 配置校验失败: " + err.Error())
		}
		return nil, err
	}
	if logStep != nil {
		logStep("Zero Trust 配置已加载: " + path)
	}
	if err := ensureZeroTrustMDMConfig(cfg); err != nil {
		if logStep != nil {
			logStep("写入 Zero Trust MDM 配置失败: " + err.Error())
		}
		return nil, err
	}
	if logStep != nil {
		logStep("Zero Trust MDM 配置已写入: " + warpMDMConfigPath)
	}
	return cfg, nil
}

func ensureZeroTrustMDMConfig(cfg *ZeroTrustConfig) error {
	if cfg == nil {
		return fmt.Errorf("Zero Trust 配置为空")
	}
	if err := os.MkdirAll(filepath.Dir(warpMDMConfigPath), 0755); err != nil {
		return fmt.Errorf("创建 MDM 配置目录失败: %w", err)
	}
	content := buildWarpMDMXML(cfg)
	if err := os.WriteFile(warpMDMConfigPath, []byte(content), 0600); err != nil {
		return fmt.Errorf("写入 %s 失败: %w", warpMDMConfigPath, err)
	}
	return nil
}

func buildWarpMDMXML(cfg *ZeroTrustConfig) string {
	return fmt.Sprintf(`<dict>
    <key>organization</key>
    <string>%s</string>
    <key>auth_client_id</key>
    <string>%s</string>
    <key>auth_client_secret</key>
    <string>%s</string>
    <key>service_mode</key>
    <string>%s</string>
    <key>proxy_port</key>
    <integer>%d</integer>
    <key>auto_connect</key>
    <integer>%d</integer>
    <key>onboarding</key>
    <false/>
</dict>
`, xmlEscape(cfg.Organization), xmlEscape(cfg.AuthClientID), xmlEscape(cfg.AuthClientSecret), xmlEscape(cfg.ServiceMode), cfg.ProxyPort, cfg.AutoConnect)
}

func xmlEscape(s string) string {
	replacer := strings.NewReplacer(
		"&", "&amp;",
		"<", "&lt;",
		">", "&gt;",
		`"`, "&quot;",
		"'", "&apos;",
	)
	return replacer.Replace(s)
}
