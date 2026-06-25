package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	dialTimeout = 1 * time.Second
	probeMax    = 2 * time.Second
	cfAPIBase   = "https://api.cloudflare.com/client/v4"
)

// candidate 是一个待测速对象，host 可能是 IPv4 地址，也可能是域名（CNAME 目标）
type candidate struct {
	host     string
	isDomain bool
}

type scanResult struct {
	candidate
	dataCenter  string
	tcpDuration time.Duration
}

// Config 对应 -config 指定的 JSON 配置文件，字段含义与同名命令行参数一致
type Config struct {
	CFToken  string `json:"cf_token"`
	ZoneID   string `json:"zone_id"`
	Record   string `json:"record"`
	Proxied  bool   `json:"proxied"`
	TTL      int    `json:"ttl"`
	Interval string `json:"interval"` // 例如 "10m"，由 time.ParseDuration 解析
	IPsURL   string `json:"ips_url"`
	Colo     string `json:"colo"`
	Task     int    `json:"task"`
	DryRun   bool   `json:"dry_run"`
}

func defaultConfig() Config {
	return Config{
		Proxied:  false,
		TTL:      60,
		Interval: "10m",
		IPsURL:   "https://github.com/DustinWin/BestCF/releases/download/bestcf/cmcc-ip.txt",
		Task:     50,
	}
}

// loadConfigFile 在默认值基础上用 JSON 文件中出现的字段覆盖
func loadConfigFile(path string) (Config, error) {
	cfg := defaultConfig()
	data, err := os.ReadFile(path)
	if err != nil {
		return cfg, err
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return cfg, fmt.Errorf("解析配置文件失败: %v", err)
	}
	return cfg, nil
}

func main() {
	configPath := flag.String("config", "", "JSON 配置文件路径（指定后忽略下面的其它参数，全部从文件读取）")
	cfToken := flag.String("cf-token", "", "Cloudflare API Token (需要该 Zone 的 DNS:Edit 权限)")
	zoneID := flag.String("zone-id", "", "Cloudflare Zone ID")
	record := flag.String("record", "", "要更新的域名记录，例如 cf.example.com")
	proxied := flag.Bool("proxied", false, "是否开启 Cloudflare 代理（小黄云），直连优选 IP 场景应为 false")
	ttl := flag.Int("ttl", 60, "DNS 记录 TTL（秒），proxied 为 true 时该值被 Cloudflare 忽略")
	interval := flag.Duration("interval", 10*time.Minute, "扫描+更新的间隔")
	ipsURL := flag.String("ips-url", "https://github.com/DustinWin/BestCF/releases/download/bestcf/cmcc-ip.txt", "BestCF 优选 IP 列表地址（每行格式 IP#备注），按运营商选择对应文件：\n  bestcf-ip.txt 通用 / cmcc-ip.txt 移动 / cucc-ip.txt 联通 / ctcc-ip.txt 电信")
	coloFilter := flag.String("colo", "", "筛选数据中心，例如 HKG,SJC（逗号分隔，留空不过滤）")
	maxThreads := flag.Int("task", 50, "并发测速协程数")
	dryRun := flag.Bool("dry-run", false, "仅拉取列表+测速，不调用 Cloudflare API（用于本地测试）")

	flag.Parse()

	var cfg Config
	if *configPath != "" {
		loaded, err := loadConfigFile(*configPath)
		if err != nil {
			log.Fatalf("加载配置文件失败: %v", err)
		}
		cfg = loaded
	} else {
		cfg = Config{
			CFToken:  *cfToken,
			ZoneID:   *zoneID,
			Record:   *record,
			Proxied:  *proxied,
			TTL:      *ttl,
			Interval: interval.String(),
			IPsURL:   *ipsURL,
			Colo:     *coloFilter,
			Task:     *maxThreads,
			DryRun:   *dryRun,
		}
	}

	if !cfg.DryRun && (cfg.CFToken == "" || cfg.ZoneID == "" || cfg.Record == "") {
		log.Fatal("必须指定 cf_token / zone_id / record（或开启 dry_run 仅测试测速逻辑）")
	}

	loopInterval, err := time.ParseDuration(cfg.Interval)
	if err != nil {
		log.Fatalf("interval 格式无效: %v", err)
	}

	for {
		start := time.Now()
		if err := runOnce(cfg.IPsURL, cfg.Colo, cfg.Task, cfg.CFToken, cfg.ZoneID, cfg.Record, cfg.Proxied, cfg.TTL, cfg.DryRun); err != nil {
			log.Printf("本轮执行失败: %v", err)
		}
		if cfg.DryRun {
			return
		}
		elapsed := time.Since(start)
		sleep := loopInterval - elapsed
		if sleep < 0 {
			sleep = 0
		}
		log.Printf("本轮耗时 %v，%v 后开始下一轮", elapsed, sleep)
		time.Sleep(sleep)
	}
}

func runOnce(ipsURL, coloFilter string, maxThreads int, cfToken, zoneID, record string, proxied bool, ttl int, dryRun bool) error {
	lines, err := fetchLines(ipsURL)
	if err != nil {
		return fmt.Errorf("获取 BestCF 优选 IP 列表失败: %v", err)
	}

	candidates := parseCandidates(lines)
	if len(candidates) == 0 {
		return fmt.Errorf("列表中未解析出可用的候选地址")
	}
	log.Printf("本轮候选数量: %d（IP 和域名混合，类型由各自条目自动识别）", len(candidates))

	best, err := scanBestCandidate(candidates, coloFilter, maxThreads)
	if err != nil {
		return err
	}
	recordType := "A"
	if best.isDomain {
		recordType = "CNAME"
	}
	log.Printf("本轮最优结果: %s（%s 记录）数据中心: %s 延迟: %v", best.host, recordType, best.dataCenter, best.tcpDuration)

	if dryRun {
		log.Printf("dry-run 模式，跳过 Cloudflare DNS 更新")
		return nil
	}

	currentType, currentContent, recordID, err := getCurrentRecord(cfToken, zoneID, record)
	if err != nil {
		return fmt.Errorf("查询当前 DNS 记录失败: %v", err)
	}

	if currentType == recordType && currentContent == best.host {
		log.Printf("最优结果与当前 DNS 记录一致 (%s %s)，无需更新", currentType, currentContent)
		return nil
	}

	if recordID == "" {
		if err := writeRecord(cfToken, zoneID, "", recordType, record, best.host, proxied, ttl); err != nil {
			return fmt.Errorf("创建 DNS 记录失败: %v", err)
		}
		log.Printf("已创建 DNS 记录 %s -> %s %s", record, recordType, best.host)
		return nil
	}

	if err := writeRecord(cfToken, zoneID, recordID, recordType, record, best.host, proxied, ttl); err != nil {
		return fmt.Errorf("更新 DNS 记录失败: %v", err)
	}
	log.Printf("已更新 DNS 记录 %s: %s %s -> %s %s", record, currentType, currentContent, recordType, best.host)
	return nil
}

// fetchLines 拉取远程文本，按行返回非空内容
func fetchLines(url string) ([]string, error) {
	resp, err := http.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP状态码: %d", resp.StatusCode)
	}

	var lines []string
	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line != "" {
			lines = append(lines, line)
		}
	}
	return lines, scanner.Err()
}

// parseCandidates 解析 BestCF 列表格式（每行 "地址#备注"，IPv6 用方括号包裹）。
// 每个条目自动判断是 IPv4 地址（最终写 A 记录）还是域名（最终写 CNAME 记录），IPv6 直接忽略。
func parseCandidates(lines []string) []candidate {
	var result []candidate
	seen := make(map[string]bool)
	for _, line := range lines {
		field := strings.SplitN(line, "#", 2)[0]
		field = strings.TrimSpace(field)
		field = strings.Trim(field, "[]")

		if field == "" || seen[field] {
			continue
		}

		if ip := net.ParseIP(field); ip != nil {
			if ip.To4() == nil {
				log.Printf("忽略 IPv6 候选: %s（当前仅支持 IPv4/域名）", field)
				continue
			}
			seen[field] = true
			result = append(result, candidate{host: field, isDomain: false})
			continue
		}

		if !isValidDomain(field) {
			log.Printf("忽略无法识别的条目: %s（既不是合法 IPv4 也不是合法域名）", field)
			continue
		}
		seen[field] = true
		result = append(result, candidate{host: field, isDomain: true})
	}
	return result
}

// isValidDomain 做一个宽松的域名格式校验：必须包含至少一个点，且不含空格/路径分隔符
func isValidDomain(s string) bool {
	if s == "" || !strings.Contains(s, ".") {
		return false
	}
	return !strings.ContainsAny(s, " /\\:")
}

// scanBestCandidate 并发测速，返回延迟最低（且符合 colo 过滤条件）的候选（IP 或域名）
func scanBestCandidate(candidates []candidate, coloFilter string, maxThreads int) (scanResult, error) {
	var filters []string
	if coloFilter != "" {
		filters = strings.Split(coloFilter, ",")
	}

	var wg sync.WaitGroup
	var mu sync.Mutex
	var best scanResult
	found := false

	thread := make(chan struct{}, maxThreads)
	var done int32
	total := len(candidates)

	for _, c := range candidates {
		wg.Add(1)
		thread <- struct{}{}
		go func(c candidate) {
			defer func() {
				<-thread
				wg.Done()
				cur := atomic.AddInt32(&done, 1)
				fmt.Printf("已测速: %d / %d\r", cur, total)
			}()

			r, ok := probeHost(c)
			if !ok {
				return
			}
			if len(filters) > 0 {
				matched := false
				for _, f := range filters {
					if strings.EqualFold(r.dataCenter, strings.TrimSpace(f)) {
						matched = true
						break
					}
				}
				if !matched {
					return
				}
			}

			mu.Lock()
			if !found || r.tcpDuration < best.tcpDuration {
				best = r
				found = true
			}
			mu.Unlock()
		}(c)
	}
	wg.Wait()
	fmt.Println()

	if !found {
		return scanResult{}, fmt.Errorf("未发现满足条件的有效候选")
	}
	return best, nil
}

// probeHost 探测单个候选（IP 或域名）的延迟和数据中心（通过 CF-RAY 响应头）
// net.Dialer.Dial 对域名会自动做 DNS 解析，所以 IP 和域名走同一套探测逻辑
func probeHost(c candidate) (scanResult, bool) {
	dialer := &net.Dialer{Timeout: dialTimeout}
	start := time.Now()
	conn, err := dialer.Dial("tcp", net.JoinHostPort(c.host, "80"))
	if err != nil {
		return scanResult{}, false
	}
	defer conn.Close()
	tcpDuration := time.Since(start)

	req, err := http.NewRequest("GET", "http://"+net.JoinHostPort(c.host, "80"), nil)
	if err != nil {
		return scanResult{}, false
	}
	req.Header.Set("User-Agent", "Mozilla/5.0")
	req.Close = true

	conn.SetDeadline(time.Now().Add(probeMax))
	if err := req.Write(conn); err != nil {
		return scanResult{}, false
	}

	resp, err := http.ReadResponse(bufio.NewReader(conn), req)
	if err != nil {
		return scanResult{}, false
	}
	defer resp.Body.Close()

	cfRay := strings.TrimSpace(resp.Header.Get("CF-RAY"))
	if cfRay == "" {
		return scanResult{}, false
	}
	parts := strings.Split(cfRay, "-")
	if len(parts) < 2 {
		return scanResult{}, false
	}
	dataCenter := strings.TrimSpace(parts[len(parts)-1])
	if dataCenter == "" {
		return scanResult{}, false
	}

	return scanResult{candidate: c, dataCenter: dataCenter, tcpDuration: tcpDuration}, true
}

// ======================== Cloudflare DNS API ========================

type cfAPIError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type cfDNSRecord struct {
	ID      string `json:"id"`
	Type    string `json:"type"`
	Content string `json:"content"`
}

type cfListResponse struct {
	Success bool          `json:"success"`
	Errors  []cfAPIError  `json:"errors"`
	Result  []cfDNSRecord `json:"result"`
}

type cfWriteResponse struct {
	Success bool         `json:"success"`
	Errors  []cfAPIError `json:"errors"`
}

func cfErrString(errs []cfAPIError) string {
	var parts []string
	for _, e := range errs {
		parts = append(parts, fmt.Sprintf("[%d] %s", e.Code, e.Message))
	}
	return strings.Join(parts, "; ")
}

// getCurrentRecord 查询当前记录（不限类型，因为同一个 name 下 A/CNAME 只会存在一种）。
// 返回 (当前记录类型, 当前内容, 记录ID, error)；记录不存在时类型/内容/ID均为空字符串
func getCurrentRecord(token, zoneID, record string) (string, string, string, error) {
	url := fmt.Sprintf("%s/zones/%s/dns_records?name=%s", cfAPIBase, zoneID, record)
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return "", "", "", err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", "", "", err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", "", "", err
	}

	var data cfListResponse
	if err := json.Unmarshal(body, &data); err != nil {
		return "", "", "", fmt.Errorf("解析响应失败: %v", err)
	}
	if !data.Success {
		return "", "", "", fmt.Errorf("Cloudflare API 返回失败: %s", cfErrString(data.Errors))
	}
	if len(data.Result) == 0 {
		return "", "", "", nil
	}
	return data.Result[0].Type, data.Result[0].Content, data.Result[0].ID, nil
}

// writeRecord 创建（recordID为空）或更新（recordID非空）DNS 记录，recordType 为 "A" 或 "CNAME"
func writeRecord(token, zoneID, recordID, recordType, record, content string, proxied bool, ttl int) error {
	payload := map[string]any{
		"type":    recordType,
		"name":    record,
		"content": content,
		"ttl":     ttl,
		"proxied": proxied,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}

	method := "POST"
	url := fmt.Sprintf("%s/zones/%s/dns_records", cfAPIBase, zoneID)
	if recordID != "" {
		method = "PUT"
		url = fmt.Sprintf("%s/zones/%s/dns_records/%s", cfAPIBase, zoneID, recordID)
	}

	req, err := http.NewRequest(method, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}

	var data cfWriteResponse
	if err := json.Unmarshal(respBody, &data); err != nil {
		return fmt.Errorf("解析响应失败: %v", err)
	}
	if !data.Success {
		return fmt.Errorf("Cloudflare API 返回失败: %s", cfErrString(data.Errors))
	}
	return nil
}
