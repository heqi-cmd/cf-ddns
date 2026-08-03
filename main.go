// cf-ddns：分线路优选 Cloudflare IP/域名（IPv4/IPv6 双栈），并写入 DNS 解析记录。
//
// 智能解析模式（provider = "dnspod"）：为每条运营商线路（默认/移动/联通/电信）
// 分别拉取对应的 BestCF 优选列表、测速选优，然后写入 DNSPod 同名记录的对应线路。
// 移动/联通/电信用户访问同一个域名，DNSPod 会按访问者运营商返回各自线路的最优 IP。
//
// 兼容模式（provider = "cloudflare"，旧版行为）：单列表、单记录，写 Cloudflare DNS。
// Cloudflare DNS 不支持按运营商线路解析，因此该模式只允许"默认"一条线路。
package main

import (
	"errors"
	"flag"
	"fmt"
	"log"
	"strings"
	"time"

	"cf-ddns/internal/dns"
	"cf-ddns/internal/probe"
)

func main() {
	configPath := flag.String("config", "", "JSON 配置文件路径（分线路智能解析仅支持配置文件方式；指定后忽略下面的其它参数）")
	cfToken := flag.String("cf-token", "", "Cloudflare API Token (需要该 Zone 的 DNS:Edit 权限)")
	zoneID := flag.String("zone-id", "", "Cloudflare Zone ID")
	record := flag.String("record", "", "要更新的域名记录，例如 cf.example.com")
	proxied := flag.Bool("proxied", false, "是否开启 Cloudflare 代理（小黄云），直连优选 IP 场景应为 false")
	ttl := flag.Int("ttl", 60, "DNS 记录 TTL（秒），proxied 为 true 时该值被 Cloudflare 忽略")
	interval := flag.Duration("interval", 10*time.Minute, "扫描+更新的间隔")
	ipsURL := flag.String("ips-url", defaultConfig().IPsURL, "BestCF 优选 IP 列表地址（每行格式 IP#备注），按运营商选择对应文件：\n  bestcf-ip.txt 通用 / cmcc-ip.txt 移动 / cucc-ip.txt 联通 / ctcc-ip.txt 电信")
	coloFilter := flag.String("colo", "", "筛选数据中心，例如 HKG,SJC（逗号分隔，留空不过滤）")
	maxThreads := flag.Int("task", 50, "并发测速协程数")
	ipv6Flag := flag.Bool("ipv6", false, "同时优选 IPv6 候选并维护 AAAA 记录（需本机具备 IPv6 连通性）")
	dryRun := flag.Bool("dry-run", false, "仅拉取列表+测速，不更新 DNS（用于本地测试）")

	flag.Parse()

	var cfg Config
	if *configPath != "" {
		loaded, err := loadConfigFile(*configPath)
		if err != nil {
			log.Fatalf("加载配置文件失败: %v", err)
		}
		cfg = loaded
		// -dry-run / -ipv6 显式传入时覆盖配置文件，便于临时做本地测试
		flag.Visit(func(f *flag.Flag) {
			switch f.Name {
			case "dry-run":
				cfg.DryRun = *dryRun
			case "ipv6":
				cfg.IPv6 = *ipv6Flag
			}
		})
	} else {
		cfg = Config{
			CFToken:  *cfToken,
			ZoneID:   *zoneID,
			Record:   *record,
			Proxied:  *proxied,
			TTL:      *ttl,
			Interval: interval.String(),
			IPsURL:   *ipsURL,
			IPv6:     *ipv6Flag,
			Colo:     *coloFilter,
			Task:     *maxThreads,
			DryRun:   *dryRun,
		}
	}

	cfg, err := finalizeConfig(cfg)
	if err != nil {
		log.Fatalf("配置无效: %v", err)
	}

	loopInterval, err := time.ParseDuration(cfg.Interval)
	if err != nil {
		log.Fatalf("interval 格式无效: %v", err)
	}

	var provider dns.Provider
	if !cfg.DryRun {
		provider, err = buildProvider(cfg)
		if err != nil {
			log.Fatalf("初始化 DNS 服务商失败: %v", err)
		}
	}

	lineNames := make([]string, 0, len(cfg.Lines))
	for _, ln := range cfg.Lines {
		lineNames = append(lineNames, ln.Line)
	}
	stack := "IPv4"
	if cfg.IPv6 {
		stack = "IPv4+IPv6"
	}
	log.Printf("模式: %s（%s），记录: %s，线路: %s", cfg.Provider, stack, cfg.Record, strings.Join(lineNames, " / "))

	for {
		start := time.Now()
		if err := runOnce(cfg, provider); err != nil {
			log.Printf("本轮存在失败项: %v", err)
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

func buildProvider(cfg Config) (dns.Provider, error) {
	switch cfg.Provider {
	case providerCloudflare:
		return dns.NewCloudflare(cfg.CFToken, cfg.ZoneID, cfg.Record, cfg.Proxied, cfg.TTL), nil
	case providerDNSPod:
		sub := "@"
		if cfg.Record != cfg.Domain {
			sub = strings.TrimSuffix(cfg.Record, "."+cfg.Domain)
		}
		return dns.NewDNSPod(cfg.SecretID, cfg.SecretKey, cfg.Domain, sub, cfg.TTL), nil
	}
	return nil, fmt.Errorf("未知 provider: %s", cfg.Provider)
}

// runOnce 依次处理所有线路。单条线路失败不影响其它线路，最终汇总返回
func runOnce(cfg Config, provider dns.Provider) error {
	cache := probe.NewCache()
	var errs []error
	for _, ln := range cfg.Lines {
		if err := runLine(cfg, provider, ln, cache); err != nil {
			log.Printf("线路 %s: %v", ln.Line, err)
			errs = append(errs, fmt.Errorf("线路 %s: %v", ln.Line, err))
		}
	}
	return errors.Join(errs...)
}

// runLine 处理一条线路：拉取该线路的候选列表 → 测速 → 分槽位选优 → 收敛 DNS 记录。
// 主槽位由 IPv4/域名候选竞争一条 A 或 CNAME 记录（二者互斥）；开启 ipv6 后，
// v6 槽位由 IPv6 候选竞争一条 AAAA 记录，与 A 记录在同一线路上共存。
func runLine(cfg Config, provider dns.Provider, ln LineConfig, cache *probe.Cache) error {
	rows, err := probe.FetchList(ln.IPsURL)
	if err != nil {
		return fmt.Errorf("获取候选列表失败: %v", err)
	}
	candidates := probe.ParseCandidates(rows, cfg.IPv6)
	if len(candidates) == 0 {
		return errors.New("列表中未解析出可用的候选地址")
	}

	var nMain, nV6 int
	for _, c := range candidates {
		if c.Kind == probe.KindIPv6 {
			nV6++
		} else {
			nMain++
		}
	}
	if cfg.IPv6 {
		log.Printf("线路 %s: 候选 %d 个（IPv4/域名 %d、IPv6 %d），开始测速（pick=%s）", ln.Line, len(candidates), nMain, nV6, ln.Pick)
	} else {
		log.Printf("线路 %s: 候选 %d 个，开始测速（pick=%s）", ln.Line, len(candidates), ln.Pick)
	}

	results := probe.Scan(candidates, cfg.Task, cache)
	var mainResults, v6Results []probe.ScanResult
	for _, r := range results {
		if r.Kind == probe.KindIPv6 {
			v6Results = append(v6Results, r)
		} else {
			mainResults = append(mainResults, r)
		}
	}

	// 主槽位选优
	var mainBest probe.ScanResult
	mainOK := false
	var selectErrs []error
	if nMain > 0 {
		best, err := probe.SelectBest(mainResults, ln.Colo, ln.Pick)
		if err != nil {
			log.Printf("线路 %s: IPv4/域名候选全部不可用: %v", ln.Line, err)
			selectErrs = append(selectErrs, fmt.Errorf("IPv4/域名候选: %v", err))
		} else {
			mainBest, mainOK = best, true
			log.Printf("线路 %s: 最优 %s（%s 记录）数据中心: %s 延迟: %v",
				ln.Line, best.Host, best.RecordType(), best.DataCenter, best.TCPDuration)
		}
	}

	// v6 槽位选优；主记录是 CNAME 时无需也无法再挂 AAAA（CNAME 目标域名自带双栈解析）
	cnameMode := mainOK && mainBest.RecordType() == "CNAME"
	var v6Best probe.ScanResult
	v6OK := false
	if cfg.IPv6 && nV6 > 0 {
		if cnameMode {
			log.Printf("线路 %s: 主记录为 CNAME，其目标域名自带 IPv6 解析，跳过 AAAA 维护", ln.Line)
		} else {
			best, err := probe.SelectBest(v6Results, ln.Colo, ln.Pick)
			if err != nil {
				log.Printf("线路 %s: 无存活 IPv6 候选（本机可能没有 IPv6 网络），跳过 AAAA 维护", ln.Line)
				selectErrs = append(selectErrs, fmt.Errorf("IPv6 候选: %v", err))
			} else {
				v6Best, v6OK = best, true
				log.Printf("线路 %s: IPv6 最优 %s（AAAA 记录）数据中心: %s 延迟: %v",
					ln.Line, best.Host, best.DataCenter, best.TCPDuration)
			}
		}
	}

	if !mainOK && !v6OK {
		return errors.Join(selectErrs...)
	}

	if cfg.DryRun {
		log.Printf("线路 %s: dry-run 模式，跳过 DNS 更新", ln.Line)
		return nil
	}

	if mainOK {
		// 主记录切到 CNAME 前先确认该线路没有会冲突的 AAAA 残留（不自动删除，交给用户决定）
		if cnameMode && cfg.IPv6 {
			if aaaa, err := provider.Get(ln.Line, []string{"AAAA"}); err == nil && aaaa != nil {
				log.Printf("线路 %s: 最优为 CNAME (%s)，但该线路已有 AAAA 记录 (%s) 会与之冲突；请手动删除该 AAAA 记录或为该线路改用 IP 列表，本轮跳过主记录写入",
					ln.Line, mainBest.Host, aaaa.Content)
				mainOK = false
			}
		}
		if mainOK {
			action, err := dns.EnsureRecord(provider, ln.Line, []string{"A", "CNAME"}, mainBest.RecordType(), mainBest.Host)
			if err != nil {
				return err
			}
			log.Printf("线路 %s: %s", ln.Line, action)
		}
	}

	if v6OK {
		action, err := dns.EnsureRecord(provider, ln.Line, []string{"AAAA"}, "AAAA", v6Best.Host)
		if err != nil {
			return err
		}
		log.Printf("线路 %s: [IPv6] %s", ln.Line, action)
	}
	return nil
}
