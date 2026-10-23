package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"sort"
	"strings"

	"cf-ddns/internal/probe"
)

const (
	providerCloudflare = "cloudflare"
	providerDNSPod     = "dnspod"

	// lineDefault 是 DNSPod 的"默认"线路名，未命中运营商线路的用户（教育网/广电/境外等）走这条
	lineDefault = "默认"

	// coloCodeLen 是 Cloudflare 数据中心代码的固定长度（如 HKG/SJC/LAX）
	coloCodeLen = 3

	// ttlCloudflare 是 Cloudflare 模式的默认 TTL（秒）
	ttlCloudflare = 60
	// ttlDNSPod 是 DNSPod 免费套餐允许的最小 TTL（秒）
	ttlDNSPod = 600
)

// NormalizeLineName 把用户书写的线路名归一化为服务商使用的标准线路名。
// 忽略首尾空白与大小写，支持各运营商常见别名；未知的非空名称原样保留，空白名返回空串。
func NormalizeLineName(line string) string {
	name := strings.TrimSpace(line)
	switch strings.ToLower(name) {
	case "default", "def", "默认":
		return lineDefault
	case "cmcc", "mobile", "yd", "移动":
		return "移动"
	case "cucc", "unicom", "lt", "联通":
		return "联通"
	case "ctcc", "telecom", "dx", "电信":
		return "电信"
	}
	return name
}

// NormalizeColoList 把逗号分隔的数据中心代码列表规范化：逐项 trim、转大写、丢弃空项、
// 按首次出现顺序去重，并以 "," 重新拼接。每个代码必须恰好是 3 个 ASCII 英文字母。
func NormalizeColoList(colo string) (string, error) {
	parts := strings.Split(colo, ",")
	out := make([]string, 0, len(parts))
	seen := make(map[string]bool, len(parts))
	for _, raw := range parts {
		code := strings.ToUpper(strings.TrimSpace(raw))
		if code == "" {
			continue
		}
		if !isColoCode(code) {
			return "", fmt.Errorf("colo 代码 %q 非法：必须为 3 位英文字母（A-Z），如 HKG/SJC/LAX", code)
		}
		if seen[code] {
			continue
		}
		seen[code] = true
		out = append(out, code)
	}
	return strings.Join(out, ","), nil
}

// isColoCode 判断 s 是否为 3 位大写 ASCII 英文字母组成的 colo 代码
func isColoCode(s string) bool {
	if len(s) != coloCodeLen {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < 'A' || s[i] > 'Z' {
			return false
		}
	}
	return true
}

// LineConfig 描述一条解析线路：用哪个候选列表、怎么选优
type LineConfig struct {
	Line   string `json:"line"`           // 线路名，须与 DNS 服务商一致：默认 / 移动 / 联通 / 电信
	IPsURL string `json:"ips_url"`        // 该线路的候选列表地址（BestCF 按运营商提供不同文件）
	Colo   string `json:"colo,omitempty"` // 数据中心过滤，逗号分隔；留空继承全局 colo
	Pick   string `json:"pick,omitempty"` // 选优方式：order 按上游列表排名（默认）/ latency 按本机实测延迟
}

// Config 对应 -config 指定的 JSON 配置文件（UTF-8 编码），字段含义见 README
type Config struct {
	Provider string `json:"provider"` // "dnspod"（智能分线路解析）或 "cloudflare"（旧版单记录模式，默认）

	// Cloudflare（provider = "cloudflare" 时必填）
	CFToken string `json:"cf_token"`
	ZoneID  string `json:"zone_id"`
	Proxied bool   `json:"proxied"`

	// DNSPod（provider = "dnspod" 时必填，使用腾讯云 API 密钥）
	SecretID  string `json:"secret_id"`
	SecretKey string `json:"secret_key"`
	Domain    string `json:"domain"` // DNSPod 中的主域名，例如 example.com

	Record   string       `json:"record"`   // 要维护的完整记录名，例如 cf.example.com
	TTL      int          `json:"ttl"`      // 0 表示按服务商默认：cloudflare 60 / dnspod 600（免费套餐下限）
	Interval string       `json:"interval"` // 例如 "10m"，由 time.ParseDuration 解析
	IPsURL   string       `json:"ips_url"`  // 旧版单线路字段：未配置 lines 时等价于一条"默认"线路
	IPv6     bool         `json:"ipv6"`     // true 时同时优选 IPv6 候选并维护 AAAA 记录（需本机具备 IPv6 连通性）
	Colo     string       `json:"colo"`
	Task     int          `json:"task"`
	DryRun   bool         `json:"dry_run"`
	Lines    []LineConfig `json:"lines"`
}

func defaultConfig() Config {
	return Config{
		Proxied:  false,
		Interval: "10m",
		IPsURL:   "https://github.com/DustinWin/BestCF/releases/download/bestcf/cmcc-ip.txt",
		Task:     50,
	}
}

func loadConfigFile(path string) (Config, error) {
	cfg := defaultConfig()
	data, err := os.ReadFile(path)
	if err != nil {
		return cfg, fmt.Errorf("读取配置文件失败: %v", err)
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return cfg, fmt.Errorf("解析配置文件 JSON 失败: %v", err)
	}
	return cfg, nil
}

// finalizeConfig 补全默认值并校验配置的自洽性
func finalizeConfig(cfg Config) (Config, error) {
	cfg.Provider = normalizeProviderName(cfg.Provider)
	if err := checkProviderName(cfg.Provider); err != nil {
		return cfg, err
	}

	if len(cfg.Lines) == 0 {
		// 兼容旧版单线路配置：ips_url + 一条默认线路，并保持旧版"本机实测延迟"的选优方式
		cfg.Lines = []LineConfig{{Line: lineDefault, IPsURL: cfg.IPsURL, Pick: probe.PickLatency}}
	}

	if err := normalizeColoConfig(&cfg); err != nil {
		return cfg, err
	}

	seen, err := finalizeLines(&cfg)
	if err != nil {
		return cfg, err
	}

	pinDefaultLineFirst(cfg.Lines)
	if cfg.Task <= 0 {
		cfg.Task = 50
	}

	if err := applyProviderDefaults(&cfg, seen); err != nil {
		return cfg, err
	}
	return cfg, nil
}

func normalizeProviderName(p string) string {
	if p == "" {
		return providerCloudflare
	}
	return p
}

func checkProviderName(p string) error {
	if p != providerCloudflare && p != providerDNSPod {
		return fmt.Errorf("provider 仅支持 %q 和 %q，当前为 %q", providerDNSPod, providerCloudflare, p)
	}
	return nil
}

func normalizeColoConfig(cfg *Config) error {
	if cfg.Colo != "" {
		norm, err := NormalizeColoList(cfg.Colo)
		if err != nil {
			return err
		}
		cfg.Colo = norm
	}
	return nil
}

func finalizeLines(cfg *Config) (map[string]bool, error) {
	seen := make(map[string]bool, len(cfg.Lines))
	for i := range cfg.Lines {
		if err := finalizeLine(&cfg.Lines[i], cfg.Colo, seen, i); err != nil {
			return nil, err
		}
	}
	return seen, nil
}

func finalizeLine(ln *LineConfig, globalColo string, seen map[string]bool, idx int) error {
	normLine := NormalizeLineName(ln.Line)
	if normLine == "" {
		return fmt.Errorf("第 %d 条线路未指定 line 名称", idx+1)
	}
	ln.Line = normLine
	if seen[ln.Line] {
		return fmt.Errorf("线路 %q 重复配置", ln.Line)
	}
	seen[ln.Line] = true

	if ln.IPsURL == "" {
		return fmt.Errorf("线路 %q 未配置 ips_url", ln.Line)
	}

	if ln.Colo == "" {
		ln.Colo = globalColo
	} else {
		norm, err := NormalizeColoList(ln.Colo)
		if err != nil {
			return err
		}
		ln.Colo = norm
	}

	if ln.Pick == "" {
		ln.Pick = probe.PickOrder
	}
	return nil
}

func pinDefaultLineFirst(lines []LineConfig) {
	sort.SliceStable(lines, func(a, b int) bool {
		return lines[a].Line == lineDefault && lines[b].Line != lineDefault
	})
}

func applyProviderDefaults(cfg *Config, seen map[string]bool) error {
	switch cfg.Provider {
	case providerCloudflare:
		return applyCloudflareDefaults(cfg)
	case providerDNSPod:
		return applyDNSPodDefaults(cfg, seen)
	}
	return nil
}

func applyCloudflareDefaults(cfg *Config) error {
	applyTTLDefault(cfg, ttlCloudflare)
	if err := requireDefaultLineOnly(cfg); err != nil {
		return err
	}
	return checkCloudflareRequired(cfg)
}

func applyTTLDefault(cfg *Config, ttl int) {
	if cfg.TTL == 0 {
		cfg.TTL = ttl
	}
}

func requireDefaultLineOnly(cfg *Config) error {
	for _, ln := range cfg.Lines {
		if ln.Line != lineDefault {
			return fmt.Errorf("Cloudflare DNS 不支持按运营商线路解析（配置了线路 %q）。分线路智能解析请把 provider 设为 %q 并把域名解析托管到 DNSPod，见 README", ln.Line, providerDNSPod)
		}
	}
	return nil
}

func checkCloudflareRequired(cfg *Config) error {
	if cfg.DryRun {
		return nil
	}
	if cfg.CFToken == "" || cfg.ZoneID == "" || cfg.Record == "" {
		return errors.New("cloudflare 模式必须指定 cf_token / zone_id / record（或开启 dry_run 仅测试测速逻辑）")
	}
	return nil
}

func applyDNSPodDefaults(cfg *Config, seen map[string]bool) error {
	applyTTLDefault(cfg, ttlDNSPod)
	if !cfg.DryRun {
		if err := validateDNSPodCredentials(cfg); err != nil {
			return err
		}
	}
	if !seen[lineDefault] {
		log.Printf("注意: 未配置 %q 线路。未命中运营商线路的用户（教育网/广电/境外等）能否解析，取决于该记录名下是否已有手工维护的默认线路记录", lineDefault)
	}
	return nil
}

func validateDNSPodCredentials(cfg *Config) error {
	if cfg.SecretID == "" || cfg.SecretKey == "" {
		return errors.New("dnspod 模式必须指定 secret_id / secret_key（腾讯云 API 密钥，获取方式见 README）")
	}
	if cfg.Domain == "" || cfg.Record == "" {
		return errors.New("dnspod 模式必须指定 domain（DNSPod 中的主域名）和 record（要维护的完整记录名）")
	}
	if cfg.Record != cfg.Domain && !strings.HasSuffix(cfg.Record, "."+cfg.Domain) {
		return fmt.Errorf("record (%s) 必须等于 domain (%s) 或是它的子域名", cfg.Record, cfg.Domain)
	}
	return nil
}
