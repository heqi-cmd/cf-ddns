// Package probe 负责候选地址的拉取、解析、并发测速与选优。
package probe

import (
	"bufio"
	"fmt"
	"log"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	dialTimeout = 1 * time.Second
	probeMax    = 2 * time.Second
)

// 选优策略
const (
	PickOrder   = "order"   // 按上游列表排名选优（列表本身已按对应线路质量排序）
	PickLatency = "latency" // 按本机实测延迟选优（只适合本机与该线路同运营商的场景）
)

// AddrKind 是候选地址的类型，决定最终写入的 DNS 记录类型
type AddrKind int

const (
	KindIPv4   AddrKind = iota // A 记录
	KindIPv6                   // AAAA 记录
	KindDomain                 // CNAME 记录
)

// Candidate 是一个待测速对象，Host 可能是 IPv4/IPv6 地址，也可能是域名（CNAME 目标）
type Candidate struct {
	Host  string
	Kind  AddrKind
	Index int // 在候选列表中的原始位置，pick=order 时代表上游排名
}

func (c Candidate) RecordType() string {
	switch c.Kind {
	case KindIPv6:
		return "AAAA"
	case KindDomain:
		return "CNAME"
	}
	return "A"
}

// ScanResult 是一次成功探测的结果
type ScanResult struct {
	Candidate
	DataCenter  string
	TCPDuration time.Duration
}

// Cache 在一轮之内按 host 缓存探测结果，多条线路的列表包含相同地址时不重复测速
type Cache struct {
	mu sync.Mutex
	m  map[string]outcome
}

type outcome struct {
	dataCenter  string
	tcpDuration time.Duration
	ok          bool
}

func NewCache() *Cache {
	return &Cache{m: make(map[string]outcome)}
}

// FetchList 拉取远程文本，按行返回非空内容
func FetchList(url string) ([]string, error) {
	client := &http.Client{Timeout: 60 * time.Second}
	resp, err := client.Get(url)
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

// ParseCandidates 解析 BestCF 列表格式（每行 "地址#备注"，可能带端口，IPv6 用方括号包裹）。
// 每个条目自动识别为 IPv4（A 记录）、IPv6（AAAA 记录，需开启 ipv6）或域名（CNAME 记录）。
func ParseCandidates(rows []string, ipv6 bool) []Candidate {
	var result []Candidate
	seen := make(map[string]bool)
	var skippedV6, skippedOther int
	for _, row := range rows {
		field := strings.TrimSpace(strings.SplitN(row, "#", 2)[0])
		field = stripPort(field)
		if field == "" || seen[field] {
			continue
		}

		kind := KindDomain
		if ip := net.ParseIP(field); ip != nil {
			if ip.To4() != nil {
				kind = KindIPv4
			} else if ipv6 {
				kind = KindIPv6
			} else {
				skippedV6++
				continue
			}
		} else if !isValidDomain(field) {
			skippedOther++
			continue
		}
		seen[field] = true
		result = append(result, Candidate{Host: field, Kind: kind, Index: len(result)})
	}
	if skippedV6 > 0 || skippedOther > 0 {
		var parts []string
		if skippedV6 > 0 {
			parts = append(parts, fmt.Sprintf("%d 个 IPv6 条目（未开启 ipv6）", skippedV6))
		}
		if skippedOther > 0 {
			parts = append(parts, fmt.Sprintf("%d 个无法识别条目", skippedOther))
		}
		log.Printf("已忽略 %s", strings.Join(parts, "、"))
	}
	return result
}

// stripPort 去掉候选地址携带的端口和 IPv6 方括号，
// 例如 "[2606::1]:443" -> "2606::1"、"1.2.3.4:443" -> "1.2.3.4"
func stripPort(field string) string {
	if strings.HasPrefix(field, "[") {
		if host, _, err := net.SplitHostPort(field); err == nil {
			return host
		}
		return strings.Trim(field, "[]")
	}
	if strings.Count(field, ":") == 1 {
		if host, _, err := net.SplitHostPort(field); err == nil {
			return host
		}
	}
	return field
}

// isValidDomain 做一个宽松的域名格式校验：必须包含至少一个点，且不含空格/路径分隔符
func isValidDomain(s string) bool {
	if s == "" || !strings.Contains(s, ".") {
		return false
	}
	return !strings.ContainsAny(s, " /\\:")
}

// Scan 并发探测候选列表，返回所有存活（能建连且返回 CF-RAY）的结果
func Scan(candidates []Candidate, maxThreads int, cache *Cache) []ScanResult {
	var results []ScanResult
	var toProbe []Candidate

	cache.mu.Lock()
	for _, c := range candidates {
		if out, hit := cache.m[c.Host]; hit {
			if out.ok {
				results = append(results, ScanResult{Candidate: c, DataCenter: out.dataCenter, TCPDuration: out.tcpDuration})
			}
		} else {
			toProbe = append(toProbe, c)
		}
	}
	cache.mu.Unlock()

	if hits := len(candidates) - len(toProbe); hits > 0 {
		log.Printf("%d 个候选命中本轮缓存，实际需测速 %d 个", hits, len(toProbe))
	}

	var wg sync.WaitGroup
	var mu sync.Mutex
	thread := make(chan struct{}, maxThreads)
	var done int32
	total := len(toProbe)

	for _, c := range toProbe {
		wg.Add(1)
		thread <- struct{}{}
		go func(c Candidate) {
			defer func() {
				<-thread
				wg.Done()
				cur := atomic.AddInt32(&done, 1)
				fmt.Printf("已测速: %d / %d\r", cur, total)
			}()

			r, ok := probeHost(c)

			cache.mu.Lock()
			cache.m[c.Host] = outcome{dataCenter: r.DataCenter, tcpDuration: r.TCPDuration, ok: ok}
			cache.mu.Unlock()

			if !ok {
				return
			}
			mu.Lock()
			results = append(results, r)
			mu.Unlock()
		}(c)
	}
	wg.Wait()
	if total > 0 {
		fmt.Println()
	}
	return results
}

// SelectBest 从存活结果中按 colo 过滤后选优：
// pick=order 取上游列表中排名最靠前的（信任上游按线路做的优选排序），
// pick=latency 取本机实测建连延迟最低的（旧版行为）
func SelectBest(results []ScanResult, coloFilter, pick string) (ScanResult, error) {
	var filters []string
	if coloFilter != "" {
		for _, f := range strings.Split(coloFilter, ",") {
			filters = append(filters, strings.TrimSpace(f))
		}
	}

	var best ScanResult
	found := false
	for _, r := range results {
		if len(filters) > 0 && !coloMatched(r.DataCenter, filters) {
			continue
		}
		if !found {
			best = r
			found = true
			continue
		}
		if pick == PickLatency {
			if r.TCPDuration < best.TCPDuration {
				best = r
			}
		} else if r.Index < best.Index {
			best = r
		}
	}

	if !found {
		return ScanResult{}, fmt.Errorf("未发现满足条件的有效候选")
	}
	return best, nil
}

func coloMatched(dataCenter string, filters []string) bool {
	for _, f := range filters {
		if strings.EqualFold(dataCenter, f) {
			return true
		}
	}
	return false
}

// probeHost 探测单个候选（IP 或域名）的延迟和数据中心（通过 CF-RAY 响应头）。
// net.Dialer.Dial 对域名会自动做 DNS 解析，所以 IP 和域名走同一套探测逻辑
func probeHost(c Candidate) (ScanResult, bool) {
	dialer := &net.Dialer{Timeout: dialTimeout}
	start := time.Now()
	conn, err := dialer.Dial("tcp", net.JoinHostPort(c.Host, "80"))
	if err != nil {
		return ScanResult{}, false
	}
	defer conn.Close()
	tcpDuration := time.Since(start)

	req, err := http.NewRequest("GET", "http://"+net.JoinHostPort(c.Host, "80"), nil)
	if err != nil {
		return ScanResult{}, false
	}
	req.Header.Set("User-Agent", "Mozilla/5.0")
	req.Close = true

	conn.SetDeadline(time.Now().Add(probeMax))
	if err := req.Write(conn); err != nil {
		return ScanResult{}, false
	}

	resp, err := http.ReadResponse(bufio.NewReader(conn), req)
	if err != nil {
		return ScanResult{}, false
	}
	defer resp.Body.Close()

	cfRay := strings.TrimSpace(resp.Header.Get("CF-RAY"))
	if cfRay == "" {
		return ScanResult{}, false
	}
	parts := strings.Split(cfRay, "-")
	if len(parts) < 2 {
		return ScanResult{}, false
	}
	dataCenter := strings.TrimSpace(parts[len(parts)-1])
	if dataCenter == "" {
		return ScanResult{}, false
	}

	return ScanResult{Candidate: c, DataCenter: dataCenter, TCPDuration: tcpDuration}, true
}
