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
)

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

// finalizeConfig 补全默认值并校验配置的自洽性
func finalizeConfig(cfg Config) (Config, error) {
	if cfg.Provider == "" {
		cfg.Provider = providerCloudflare
	}
	if cfg.Provider != providerCloudflare && cfg.Provider != providerDNSPod {
		return cfg, fmt.Errorf("provider 仅支持 %q 和 %q，当前为 %q", providerDNSPod, providerCloudflare, cfg.Provider)
	}

	if len(cfg.Lines) == 0 {
		// 兼容旧版单线路配置：ips_url + 一条默认线路，并保持旧版"本机实测延迟"的选优方式
		cfg.Lines = []LineConfig{{Line: lineDefault, IPsURL: cfg.IPsURL, Pick: probe.PickLatency}}
	}

	seen := make(map[string]bool)
	for i := range cfg.Lines {
		ln := &cfg.Lines[i]
		if ln.Line == "" {
			return cfg, fmt.Errorf("lines[%d] 缺少 line 字段（线路名，如 %q/%q/%q/%q）", i, lineDefault, "移动", "联通", "电信")
		}
		if ln.IPsURL == "" {
			return cfg, fmt.Errorf("线路 %s 缺少 ips_url", ln.Line)
		}
		if seen[ln.Line] {
			return cfg, fmt.Errorf("线路 %s 配置重复", ln.Line)
		}
		seen[ln.Line] = true
		if ln.Pick == "" {
			ln.Pick = probe.PickOrder
		}
		if ln.Pick != probe.PickOrder && ln.Pick != probe.PickLatency {
			return cfg, fmt.Errorf("线路 %s 的 pick 仅支持 %q（按上游列表排名）和 %q（按本机实测延迟），当前为 %q",
				ln.Line, probe.PickOrder, probe.PickLatency, ln.Pick)
		}
		if ln.Colo == "" {
			ln.Colo = cfg.Colo
		}
	}

	// DNSPod 要求名下先有默认线路记录才允许添加其它线路，把默认线路排到最前优先处理
	sort.SliceStable(cfg.Lines, func(a, b int) bool {
		return cfg.Lines[a].Line == lineDefault && cfg.Lines[b].Line != lineDefault
	})

	if cfg.Task <= 0 {
		cfg.Task = 50
	}

	switch cfg.Provider {
	case providerCloudflare:
		if cfg.TTL == 0 {
			cfg.TTL = 60
		}
		for _, ln := range cfg.Lines {
			if ln.Line != lineDefault {
				return cfg, fmt.Errorf("Cloudflare DNS 不支持按运营商线路解析（配置了线路 %q）。分线路智能解析请把 provider 设为 %q 并把域名解析托管到 DNSPod，见 README", ln.Line, providerDNSPod)
			}
		}
		if !cfg.DryRun && (cfg.CFToken == "" || cfg.ZoneID == "" || cfg.Record == "") {
			return cfg, errors.New("cloudflare 模式必须指定 cf_token / zone_id / record（或开启 dry_run 仅测试测速逻辑）")
		}
	case providerDNSPod:
		if cfg.TTL == 0 {
			cfg.TTL = 600 // DNSPod 免费套餐允许的最小 TTL
		} else if cfg.TTL < 600 {
			log.Printf("注意: ttl=%d 低于 DNSPod 免费套餐的下限 600，免费套餐下 API 会拒绝写入", cfg.TTL)
		}
		if !cfg.DryRun {
			if cfg.SecretID == "" || cfg.SecretKey == "" {
				return cfg, errors.New("dnspod 模式必须指定 secret_id / secret_key（腾讯云 API 密钥，获取方式见 README）")
			}
			if cfg.Domain == "" || cfg.Record == "" {
				return cfg, errors.New("dnspod 模式必须指定 domain（DNSPod 中的主域名）和 record（要维护的完整记录名）")
			}
			if cfg.Record != cfg.Domain && !strings.HasSuffix(cfg.Record, "."+cfg.Domain) {
				return cfg, fmt.Errorf("record (%s) 必须等于 domain (%s) 或是它的子域名", cfg.Record, cfg.Domain)
			}
		}
		if !seen[lineDefault] {
			log.Printf("注意: 未配置 %q 线路。未命中运营商线路的用户（教育网/广电/境外等）能否解析，取决于该记录名下是否已有手工维护的默认线路记录", lineDefault)
		}
	}
	return cfg, nil
}
