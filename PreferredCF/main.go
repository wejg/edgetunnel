// PreferredCF：从指定 IP 库拉取 CF 候选 IP，测延迟后输出延迟最低的前 N 个。
// 用法：go run . [--library=cm-list] [--port=443] [--top=16] [--concurrency=8] [--max-ips=512]
package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"io"
	"math/rand"
	"net"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// 测速参数：每个 IP 请求 3 次，丢弃第 1 次（DNS/冷启动），取后 2 次平均；单次请求超时 5 秒，连接单独超时便于区分「连不上」与「连上但慢」
const (
	probeRuns        = 3
	probeSkip        = 1
	probeDelay       = 5 * time.Second
	probeDialTimeout = 3 * time.Second // 连接阶段超时，超过则失败，便于与总超时区分
)

// 各 IP 库的拉取地址（与 EDT-Pages 管理端一致）
var ipLibraryURLs = map[string]string{
	"cf-official":   "https://cf.090227.xyz/ips-v4",
	"cm-list":       "https://raw.githubusercontent.com/cmliu/cmliu/main/CF-CIDR.txt",
	"as13335":       "https://raw.githubusercontent.com/ipverse/asn-ip/master/as/13335/ipv4-aggregated.txt",
	"as209242":      "https://raw.githubusercontent.com/ipverse/asn-ip/master/as/209242/ipv4-aggregated.txt",
	"reverse-proxy": "https://zip.cm.edu.kg/all.txt",
}

// 拉取 IP 列表时允许的最大响应体大小，防止异常或恶意响应导致 OOM
const maxFetchBodyBytes = 10 * 1024 * 1024 // 10MB

// logStderr 将进度日志打到 stderr，不影响 stdout 的 IP 结果输出（便于管道）
func logStderr(format string, args ...interface{}) {
	fmt.Fprintf(os.Stderr, "[优选] "+format+"\n", args...)
}

// exitWith 打印错误到 stderr 并退出，供脚本根据退出码判断失败
func exitWith(code int, format string, args ...interface{}) {
	logStderr("[错误] "+format, args...)
	os.Exit(code)
}

func main() {
	// 命令行参数：IP 库与测速/输出行为
	library := flag.String("library", "cm-list", "IP library: cf-official, cm-list, as13335, as209242, reverse-proxy") // 候选 IP 来源：CIDR 或反代列表
	port := flag.String("port", "443", "Port for probe and output")                                                     // 测速与输出使用的端口，需与节点一致
	top := flag.Int("top", 16, "Number of lowest-latency IPs to output")                                                // 输出延迟最低的前 N 个，供订阅/配置使用
	concurrency := flag.Int("concurrency", 8, "Concurrent probe workers (1-32)")                                        // 并发测速协程数，越大越快、占网越多
	maxIPs := flag.Int("max-ips", 512, "Max candidate IPs to fetch/generate")                                           // 最多拉取/生成的候选 IP 数，CIDR 会随机生成至该数
	quiet := flag.Bool("quiet", false, "Only output IP:port, no latency")                                               // 仅输出 IP:port，不输出延迟毫秒，便于管道处理
	flag.Parse()

	// 参数校验：避免无效输入导致静默异常或难以排查
	if *top < 1 {
		exitWith(1, "参数 top 必须 >= 1，当前为 %d", *top)
	}
	if *maxIPs < 1 {
		exitWith(1, "参数 max-ips 必须 >= 1，当前为 %d", *maxIPs)
	}
	portNum, err := strconv.Atoi(strings.TrimSpace(*port))
	if err != nil || portNum < 1 || portNum > 65535 {
		exitWith(1, "参数 port 必须为 1-65535 的整数，当前为 %q", *port)
	}
	// 标准化为无前导零，避免 "0443" 与列表里的 "443" 不匹配
	*port = strconv.Itoa(portNum)

	if *concurrency < 1 {
		*concurrency = 1
	}
	if *concurrency > 32 {
		*concurrency = 32
	}

	startMain := time.Now()
	logStderr("======== 优选 IP 开始 ========")
	logStderr("参数: library=%s | port=%s | top=%d | concurrency=%d | max-ips=%d | quiet=%v", *library, *port, *top, *concurrency, *maxIPs, *quiet)

	url, ok := ipLibraryURLs[*library]
	if !ok {
		logStderr("未知 library: %q", *library)
		fmt.Fprintf(flag.CommandLine.Output(), "unknown library %q\n", *library)
		flag.Usage()
		os.Exit(1)
	}

	// ---------- 步骤 1：拉取 IP 列表 ----------
	logStderr("---------- 步骤 1/3：拉取 IP 列表 ----------")
	logStderr("URL: %s", url)
	logStderr("拉取超时: 30s")
	startFetch := time.Now()
	body, err := fetchURL(url)
	elapsedFetch := time.Since(startFetch)
	if err != nil {
		logStderr("[失败] 拉取错误: %v", err)
		fmt.Fprintf(flag.CommandLine.Output(), "fetch IP list: %v\n", err)
		os.Exit(1)
	}
	lineCount := strings.Count(body, "\n")
	if len(body) > 0 && !strings.HasSuffix(body, "\n") {
		lineCount++
	}
	logStderr("[完成] 状态 200 | 大小 %d 字节 | 行数约 %d | 耗时 %v", len(body), lineCount, elapsedFetch.Round(time.Millisecond))

	// ---------- 步骤 2：解析候选 IP ----------
	logStderr("---------- 步骤 2/3：解析候选 IP ----------")
	logStderr("解析策略: library=%s, port=%s, 最多取 %d 个候选 (CIDR 将随机生成至该数)", *library, *port, *maxIPs)
	startParse := time.Now()
	candidates, err := parseIPList(body, *library, *port, *maxIPs)
	elapsedParse := time.Since(startParse)
	if err != nil {
		logStderr("[失败] 解析错误: %v", err)
		fmt.Fprintf(flag.CommandLine.Output(), "parse IP list: %v\n", err)
		os.Exit(1)
	}
	if len(candidates) == 0 {
		logStderr("[失败] 解析后候选数为 0，请检查库或端口")
		fmt.Fprintln(flag.CommandLine.Output(), "no candidate IPs")
		os.Exit(1)
	}
	logStderr("[完成] 候选数量=%d | 耗时 %v", len(candidates), elapsedParse.Round(time.Millisecond))

	// ---------- 步骤 3：测速 ----------
	logStderr("---------- 步骤 3/3：测速 ----------")
	logStderr("测速策略: 每 IP 请求 3 次，丢弃第 1 次，取后 2 次平均；连接超时 3s、单次总超时 5s；延迟=完整响应(body 收齐)耗时")
	logStderr("并发数: %d | 候选总数: %d", *concurrency, len(candidates))
	logStderr("说明: 大量 IP 测不通时会很快结束，仅统计测通的延迟")
	startProbe := time.Now()
	results := probeConcurrent(candidates, *port, *concurrency)
	elapsedProbe := time.Since(startProbe)

	successCount := len(results)
	failCount := len(candidates) - successCount
	successPct := 0.0
	if len(candidates) > 0 {
		successPct = 100 * float64(successCount) / float64(len(candidates))
	}
	logStderr("[完成] 测速总耗时 %v | 成功 %d | 失败 %d | 成功率 %.1f%%", elapsedProbe.Round(time.Millisecond), successCount, failCount, successPct)

	if len(results) == 0 {
		logStderr("[失败] 无测通结果，请检查网络或端口")
		fmt.Fprintln(flag.CommandLine.Output(), "no successful probes")
		os.Exit(1)
	}

	// 稳定排序：同延迟时保持原有顺序，便于复现与对比
	sort.SliceStable(results, func(i, j int) bool { return results[i].LatencyMs < results[j].LatencyMs })
	minMs := results[0].LatencyMs
	maxMs := results[len(results)-1].LatencyMs
	medianMs := results[len(results)/2].LatencyMs
	logStderr("延迟分布: 最小 %dms | 最大 %dms | 中位数 %dms", minMs, maxMs, medianMs)

	n := *top
	if n > len(results) {
		n = len(results)
		logStderr("注意: 有效结果不足 %d 个，仅输出 %d 个", *top, n)
	}
	results = results[:n]

	logStderr("---------- 输出 ----------")
	logStderr("输出前 %d 个低延迟 IP (quiet=%v)", n, *quiet)
	for i, r := range results {
		if *quiet {
			fmt.Println(r.IPPort)
		} else {
			fmt.Printf("%s %dms\n", r.IPPort, r.LatencyMs)
		}
		if i < 3 {
			logStderr("  第 %d: %s %dms", i+1, r.IPPort, r.LatencyMs)
		}
	}
	if n > 3 {
		logStderr("  ... 共 %d 条 (以上仅展示前 3 条)", n)
	}
	totalElapsed := time.Since(startMain)
	logStderr("======== 全部完成，总耗时 %v ========", totalElapsed.Round(time.Millisecond))
}

// 拉取 IP 列表时使用的 User-Agent，减少被源站限流或 403 的概率
const fetchUserAgent = "PreferredCF/1.0 (IP latency probe)"

// fetchURL 拉取给定 URL 的文本内容，超时 30 秒，响应体限制 10MB 防止 OOM，带简单 User-Agent
func fetchURL(u string) (string, error) {
	client := &http.Client{Timeout: 30 * time.Second}
	req, err := http.NewRequest(http.MethodGet, u, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", fetchUserAgent)
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("status %s", resp.Status)
	}
	limited := io.LimitReader(resp.Body, maxFetchBodyBytes)
	var sb strings.Builder
	sb.Grow(1024 * 1024) // 预分配 1MB，减少大响应时的多次扩容
	sc := bufio.NewScanner(limited)
	for sc.Scan() {
		sb.Write(sc.Bytes())
		sb.WriteByte('\n')
	}
	if err := sc.Err(); err != nil {
		return "", fmt.Errorf("read body: %w", err)
	}
	return sb.String(), nil
}

// parseIPList 根据 library 类型解析：reverse-proxy 走专用格式(IP:PORT#备注)，否则先看是否有 CIDR，再决定按 CIDR 或纯 IP 处理
func parseIPList(body, library, port string, maxIPs int) ([]string, error) {
	if library == "reverse-proxy" {
		return processReverseProxyList(body, port, maxIPs)
	}
	lines, err := parseLines(body)
	if err != nil {
		return nil, err
	}
	if len(lines) == 0 {
		return nil, fmt.Errorf("empty list")
	}
	hasCIDR := false
	for _, l := range lines {
		if strings.Contains(l, "/") {
			hasCIDR = true
			break
		}
	}
	if hasCIDR {
		return processCIDRList(lines, port, maxIPs)
	}
	return processPlainIPList(lines, port, maxIPs)
}

// parseLines 按行切分，跳过空行和 # 开头的注释；单行超过 bufio.Scanner 默认缓冲会返回错误
func parseLines(body string) ([]string, error) {
	var out []string
	sc := bufio.NewScanner(strings.NewReader(body))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		out = append(out, line)
	}
	return out, sc.Err()
}

// processReverseProxyList 解析反代列表：每行格式 IP:PORT#备注，只保留 port 数值匹配的（如 0443 与 443 视为同一端口），超过 maxIPs 时随机截断
func processReverseProxyList(body, targetPort string, maxIPs int) ([]string, error) {
	targetPortNum, err := strconv.Atoi(targetPort)
	if err != nil || targetPortNum < 1 || targetPortNum > 65535 {
		return nil, fmt.Errorf("invalid target port %q", targetPort)
	}
	var filtered []string
	sc := bufio.NewScanner(strings.NewReader(body))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		// 格式: IP:PORT#备注（与 EDT-Pages 正则 ^([^:]+):(\d+)#(.+)$ 一致，必须有 # 且 # 后非空）
		if !strings.Contains(line, "#") {
			continue
		}
		before, remark, _ := strings.Cut(line, "#")
		remark = strings.TrimSpace(remark)
		if remark == "" {
			continue
		}
		before = strings.TrimSpace(before)
		parts := strings.Split(before, ":")
		if len(parts) != 2 {
			continue
		}
		ip, portStr := strings.TrimSpace(parts[0]), strings.TrimSpace(parts[1])
		portNum, err := strconv.Atoi(portStr)
		if err != nil || portNum != targetPortNum {
			continue
		}
		port := strconv.Itoa(portNum) // 输出用规范形式
		parsed := net.ParseIP(ip)
		if parsed == nil || parsed.To4() == nil {
			continue // 仅支持 IPv4，与测速端 nip 域名一致
		}
		filtered = append(filtered, parsed.To4().String()+":"+port+"#"+remark)
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	if len(filtered) > maxIPs {
		rand.Shuffle(len(filtered), func(i, j int) { filtered[i], filtered[j] = filtered[j], filtered[i] })
		filtered = filtered[:maxIPs]
	}
	return filtered, nil
}

// isIPv4CIDR 判断一行是否为合法 IPv4 CIDR（仅保留 IPv4，避免 IPv6 CIDR 导致一直生成空 IP）
func isIPv4CIDR(line string) bool {
	network, bitsStr, ok := strings.Cut(line, "/")
	if !ok {
		return false
	}
	bits, err := strconv.Atoi(strings.TrimSpace(bitsStr))
	if err != nil || bits < 0 || bits > 32 {
		return false
	}
	ip := net.ParseIP(strings.TrimSpace(network))
	return ip != nil && ip.To4() != nil
}

// processCIDRList 从 CIDR 行中随机生成不重复 IP，直到达到 maxIPs 或无法再生成；仅保留 IPv4 CIDR
func processCIDRList(lines []string, port string, maxIPs int) ([]string, error) {
	var cidrs []string
	for _, line := range lines {
		if !strings.Contains(line, "/") {
			continue
		}
		if !isIPv4CIDR(line) {
			continue
		}
		cidrs = append(cidrs, line)
	}
	if len(cidrs) == 0 {
		return nil, fmt.Errorf("no IPv4 CIDR lines")
	}
	seen := make(map[string]struct{})
	var out []string
	for len(out) < maxIPs {
		before := len(out)
		for _, cidr := range cidrs {
			if len(out) >= maxIPs {
				break
			}
			ip := generateRandomIPFromCIDR(cidr)
			if ip == "" {
				continue
			}
			key := ip + ":" + port
			if _, ok := seen[key]; ok {
				continue
			}
			seen[key] = struct{}{}
			out = append(out, key)
		}
		if len(out) == before {
			break
		}
	}
	return out, nil
}

// generateRandomIPFromCIDR 从 CIDR（如 1.2.3.0/24）中随机取一个 IPv4；/32 或 hostBits>=32 直接返回网络地址
func generateRandomIPFromCIDR(cidr string) string {
	network, bitsStr, ok := strings.Cut(cidr, "/")
	if !ok {
		return ""
	}
	bits, err := strconv.Atoi(strings.TrimSpace(bitsStr))
	if err != nil || bits < 0 || bits > 32 {
		return ""
	}
	ip := net.ParseIP(strings.TrimSpace(network))
	if ip == nil || ip.To4() == nil {
		return ""
	}
	ip = ip.To4()
	networkInt := uint32(ip[0])<<24 | uint32(ip[1])<<16 | uint32(ip[2])<<8 | uint32(ip[3])
	hostBits := 32 - bits
	if hostBits <= 0 {
		return ip.String()
	}
	if hostBits >= 32 {
		return ip.String()
	}
	hostCount := uint32(1) << hostBits
	mask := uint32(0xFFFFFFFF) << hostBits
	networkInt &= mask
	randomHost := uint32(rand.Int63n(int64(hostCount)))
	ipInt := networkInt + randomHost
	a := byte(ipInt >> 24)
	b := byte(ipInt >> 16)
	c := byte(ipInt >> 8)
	d := byte(ipInt)
	return fmt.Sprintf("%d.%d.%d.%d", a, b, c, d)
}

// processPlainIPList 每行一个 IP（取第一个 token，兼容行尾注释或空格），仅 IPv4，去重并加上 port，最多取 maxIPs 个
func processPlainIPList(lines []string, port string, maxIPs int) ([]string, error) {
	seen := make(map[string]struct{})
	var out []string
	for _, line := range lines {
		if strings.Contains(line, "/") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		parsed := net.ParseIP(fields[0])
		if parsed == nil || parsed.To4() == nil {
			continue // 仅支持 IPv4
		}
		canon := parsed.To4().String()
		key := canon + ":" + port
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, key)
		if len(out) >= maxIPs {
			break
		}
	}
	return out, nil
}

// probeResult 单次测速结果：IP:port 与平均延迟（毫秒）
type probeResult struct {
	IPPort    string
	LatencyMs int
}

// ipToHex 将 IPv4 转为十六进制字符串，用于 nip.lfree.org 子域名（如 1.2.3.4 -> 01020304）
func ipToHex(ip string) string {
	parts := strings.Split(ip, ".")
	if len(parts) != 4 {
		return ""
	}
	var sb strings.Builder
	for _, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 || n > 255 {
			return ""
		}
		sb.WriteString(fmt.Sprintf("%02x", n))
	}
	return sb.String()
}

// probeSingle 对单个 IP 测速：请求 3 次 https://<hex>.nip.lfree.org:port/cdn-cgi/trace，丢弃第 1 次，取后 2 次平均。
// 延迟 = 从发起请求到读完全部响应 body 的耗时（更准），避免只测到“首包”时间。
func probeSingle(client *http.Client, ipPort, port string) *probeResult {
	ipPortForParse := ipPort
	if idx := strings.Index(ipPortForParse, "#"); idx >= 0 {
		ipPortForParse = ipPortForParse[:idx]
	}
	ip, _, _ := strings.Cut(ipPortForParse, ":")
	if ip == "" {
		ip = ipPortForParse
	}
	hexIP := ipToHex(ip)
	if hexIP == "" {
		return nil
	}
	var times []float64
	for i := 0; i < probeRuns; i++ {
		u := fmt.Sprintf("https://%s.nip.lfree.org:%s/cdn-cgi/trace?_t=%d", hexIP, port, time.Now().UnixNano())
		start := time.Now()
		resp, err := client.Get(u)
		if err != nil {
			if i == 0 {
				return nil
			}
			continue
		}
		if resp.StatusCode != http.StatusOK {
			_ = resp.Body.Close()
			if i == 0 {
				return nil
			}
			continue
		}
		// 读完全部 body 再计时，得到的是完整往返延迟；读不完整则本次不计入
		_, copyErr := io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		if copyErr != nil {
			if i == 0 {
				return nil
			}
			continue
		}
		elapsed := time.Since(start).Seconds() * 1000
		if i >= probeSkip {
			times = append(times, elapsed)
		}
	}
	if len(times) == 0 {
		return nil
	}
	var sum float64
	for _, t := range times {
		sum += t
	}
	avg := int(sum/float64(len(times)) + 0.5)
	return &probeResult{IPPort: ipPort, LatencyMs: avg}
}

// probeConcurrent 使用 worker 池并发测速，定期打印进度（已测数量/总数）；连接单独超时便于区分连不上与连上但慢
func probeConcurrent(candidates []string, port string, concurrency int) []probeResult {
	transport := &http.Transport{
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			d := &net.Dialer{Timeout: probeDialTimeout}
			return d.DialContext(ctx, network, addr)
		},
	}
	client := &http.Client{
		Transport: transport,
		Timeout:   probeDelay,
	}
	total := len(candidates)
	jobs := make(chan string, total)
	for _, c := range candidates {
		jobs <- c
	}
	close(jobs)
	results := make(chan *probeResult, total)
	var done, successCount int32
	var wg sync.WaitGroup
	// 立即打一条初始进度，避免“长时间无输出”的错觉
	logStderr("测速进度: 已测 0/%d | 已通 0 | 成功率 0.0%%", total)
	// 进度：每 3 秒打印一次已测数量、已通数量、成功率
	go func() {
		ticker := time.NewTicker(3 * time.Second)
		defer ticker.Stop()
		for atomic.LoadInt32(&done) < int32(total) {
			<-ticker.C
			d := atomic.LoadInt32(&done)
			s := atomic.LoadInt32(&successCount)
			pct := 0.0
			if d > 0 {
				pct = 100 * float64(s) / float64(d)
			}
			logStderr("测速进度: 已测 %d/%d | 已通 %d | 成功率 %.1f%%", d, total, s, pct)
		}
	}()
	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for ipPort := range jobs {
				r := probeSingle(client, ipPort, port)
				atomic.AddInt32(&done, 1)
				if r != nil {
					atomic.AddInt32(&successCount, 1)
					results <- r
				}
			}
		}()
	}
	go func() {
		wg.Wait()
		close(results)
	}()
	var out []probeResult
	for r := range results {
		out = append(out, *r)
	}
	// 打一条最终进度，便于与「测速完成」日志对应
	if total > 0 {
		pct := 100 * float64(len(out)) / float64(total)
		logStderr("测速进度: 已测 %d/%d | 已通 %d | 成功率 %.1f%%", total, total, len(out), pct)
	}
	return out
}

