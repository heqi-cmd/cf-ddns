package probe

import (
	"testing"
	"time"
)

func TestStripPort(t *testing.T) {
	cases := map[string]string{
		"[2606:4700:3036::ce:d6ea]:443": "2606:4700:3036::ce:d6ea",
		"[2606:4700:3036::ce:d6ea]":     "2606:4700:3036::ce:d6ea",
		"1.2.3.4:443":                   "1.2.3.4",
		"1.2.3.4":                       "1.2.3.4",
		"www.example.com":               "www.example.com",
	}
	for in, want := range cases {
		if got := stripPort(in); got != want {
			t.Errorf("stripPort(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestParseCandidates(t *testing.T) {
	rows := []string{
		"91.193.58.199#CMCC-IPv4_CMLiu_1",
		"104.18.33.247#CMCC-IPv4_CMLiu_2",
		"[2606:4700:3036::ce:d6ea]:443#CMCC-IPv6_CMLiu_1",
		"www.shopify.com#CF-Domain_1",
		"91.193.58.199#重复条目",
		"???#无法识别",
	}

	// 未开启 ipv6：IPv6 条目被忽略
	got := ParseCandidates(rows, false)
	if len(got) != 3 {
		t.Fatalf("候选数量 = %d, want 3: %+v", len(got), got)
	}
	if got[0].Host != "91.193.58.199" || got[0].Kind != KindIPv4 || got[0].Index != 0 {
		t.Errorf("候选[0] = %+v", got[0])
	}
	if got[2].Host != "www.shopify.com" || got[2].Kind != KindDomain || got[2].Index != 2 {
		t.Errorf("候选[2] = %+v", got[2])
	}

	// 开启 ipv6：IPv6 条目保留（端口和方括号被剥掉），顺序与列表一致
	got6 := ParseCandidates(rows, true)
	if len(got6) != 4 {
		t.Fatalf("开启 ipv6 后候选数量 = %d, want 4: %+v", len(got6), got6)
	}
	v6 := got6[2]
	if v6.Host != "2606:4700:3036::ce:d6ea" || v6.Kind != KindIPv6 || v6.Index != 2 {
		t.Errorf("IPv6 候选 = %+v", v6)
	}
	if v6.RecordType() != "AAAA" {
		t.Errorf("IPv6 候选 RecordType = %q, want AAAA", v6.RecordType())
	}
}

func TestCandidateRecordType(t *testing.T) {
	cases := map[AddrKind]string{
		KindIPv4:   "A",
		KindIPv6:   "AAAA",
		KindDomain: "CNAME",
	}
	for kind, want := range cases {
		if got := (Candidate{Kind: kind}).RecordType(); got != want {
			t.Errorf("RecordType(kind=%d) = %q, want %q", kind, got, want)
		}
	}
}

func TestSelectBest(t *testing.T) {
	results := []ScanResult{
		{Candidate: Candidate{Host: "a", Index: 2}, DataCenter: "HKG", TCPDuration: 10 * time.Millisecond},
		{Candidate: Candidate{Host: "b", Index: 0}, DataCenter: "SJC", TCPDuration: 50 * time.Millisecond},
		{Candidate: Candidate{Host: "c", Index: 1}, DataCenter: "HKG", TCPDuration: 30 * time.Millisecond},
	}

	if best, err := SelectBest(results, "", PickOrder); err != nil || best.Host != "b" {
		t.Errorf("pick=order 应选上游排名最靠前的 b，实际 %v (err=%v)", best.Host, err)
	}
	if best, err := SelectBest(results, "", PickLatency); err != nil || best.Host != "a" {
		t.Errorf("pick=latency 应选延迟最低的 a，实际 %v (err=%v)", best.Host, err)
	}
	if best, err := SelectBest(results, "hkg", PickOrder); err != nil || best.Host != "c" {
		t.Errorf("colo=hkg 过滤后应选 c，实际 %v (err=%v)", best.Host, err)
	}
	if _, err := SelectBest(results, "NRT", PickOrder); err == nil {
		t.Error("colo 无匹配时应返回错误")
	}
	if _, err := SelectBest(nil, "", PickOrder); err == nil {
		t.Error("空结果应返回错误")
	}
}
