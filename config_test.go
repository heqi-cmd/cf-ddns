package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"cf-ddns/internal/probe"
)

func TestNormalizeLineName(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"default", "默认"},
		{"DEF", "默认"},
		{"默认", "默认"},
		{" cmcc ", "移动"},
		{"mobile", "移动"},
		{"YD", "移动"},
		{"移动", "移动"},
		{"cucc", "联通"},
		{"unicom", "联通"},
		{"LT", "联通"},
		{"联通", "联通"},
		{"ctcc", "电信"},
		{"telecom", "电信"},
		{"DX", "电信"},
		{"电信", "电信"},
		{" custom ", "custom"},
		{"", ""},
		{"   ", ""},
	}
	for _, tc := range cases {
		if got := NormalizeLineName(tc.in); got != tc.want {
			t.Errorf("NormalizeLineName(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestNormalizeColoList(t *testing.T) {
	got, err := NormalizeColoList(" hkg , sjc, HKG , , lax ")
	if err != nil || got != "HKG,SJC,LAX" {
		t.Fatalf("NormalizeColoList got=(%q, %v), want (HKG,SJC,LAX, nil)", got, err)
	}

	empty, err := NormalizeColoList(" ,  , ")
	if err != nil || empty != "" {
		t.Fatalf("NormalizeColoList empty got=(%q, %v), want ('', nil)", empty, err)
	}

	for _, bad := range []string{"HK1", "ABCD", "HK", "H_G"} {
		if _, err := NormalizeColoList(bad); err == nil {
			t.Errorf("expected error for invalid IATA code %q", bad)
		}
	}
}

func TestFinalizeConfigLegacy(t *testing.T) {
	cfg := defaultConfig()
	cfg.DryRun = true
	out, err := finalizeConfig(cfg)
	if err != nil {
		t.Fatalf("旧版配置应通过校验: %v", err)
	}
	if out.Provider != providerCloudflare {
		t.Errorf("provider 默认应为 cloudflare，实际 %q", out.Provider)
	}
	if len(out.Lines) != 1 || out.Lines[0].Line != lineDefault || out.Lines[0].Pick != probe.PickLatency {
		t.Errorf("未配置 lines 时应合成一条默认线路（pick=latency），实际 %+v", out.Lines)
	}
	if out.TTL != 60 {
		t.Errorf("cloudflare 模式 TTL 默认应为 60，实际 %d", out.TTL)
	}
	if out.IPv6 {
		t.Error("ipv6 默认应为关闭")
	}
}

func TestFinalizeConfigCloudflareRejectsISPLines(t *testing.T) {
	cfg := defaultConfig()
	cfg.DryRun = true
	cfg.Lines = []LineConfig{
		{Line: "默认", IPsURL: "http://x"},
		{Line: "移动", IPsURL: "http://x"},
	}
	if _, err := finalizeConfig(cfg); err == nil || !strings.Contains(err.Error(), "Cloudflare") {
		t.Errorf("cloudflare 模式配置运营商线路应报错，实际 err=%v", err)
	}

	cfAlias := defaultConfig()
	cfAlias.DryRun = true
	cfAlias.Lines = []LineConfig{{Line: "cmcc", IPsURL: "http://x"}}
	if _, err := finalizeConfig(cfAlias); err == nil {
		t.Error("cloudflare 模式配置 cmcc 别名应报错")
	}
}

func TestFinalizeConfigDNSPod(t *testing.T) {
	cfg := defaultConfig()
	cfg.Provider = providerDNSPod
	cfg.SecretID = "id"
	cfg.SecretKey = "key"
	cfg.Domain = "example.com"
	cfg.Record = "cf.example.com"
	cfg.Lines = []LineConfig{
		{Line: "移动", IPsURL: "http://x"},
		{Line: "默认", IPsURL: "http://x"},
	}
	out, err := finalizeConfig(cfg)
	if err != nil {
		t.Fatalf("dnspod 配置应通过校验: %v", err)
	}
	if out.TTL != 600 {
		t.Errorf("dnspod 模式 TTL 默认应为 600，实际 %d", out.TTL)
	}
	if out.Lines[0].Line != lineDefault {
		t.Errorf("默认线路应被置顶排序，实际 Lines[0].Line=%q", out.Lines[0].Line)
	}
}

func TestFinalizeConfigLineAliasesAndOrdering(t *testing.T) {
	cfg := defaultConfig()
	cfg.DryRun = true
	cfg.Provider = providerDNSPod
	cfg.Lines = []LineConfig{
		{Line: "cmcc", IPsURL: "http://x"},
		{Line: "DEFAULT", IPsURL: "http://y"},
		{Line: "unicom", IPsURL: "http://z"},
		{Line: "dx", IPsURL: "http://w"},
	}
	out, err := finalizeConfig(cfg)
	if err != nil {
		t.Fatalf("finalizeConfig unexpected error: %v", err)
	}
	want := []string{"默认", "移动", "联通", "电信"}
	for i, w := range want {
		if out.Lines[i].Line != w {
			t.Fatalf("Lines[%d].Line = %q, want %q", i, out.Lines[i].Line, w)
		}
	}
}

func TestFinalizeConfigDuplicateLineError(t *testing.T) {
	cfg := defaultConfig()
	cfg.DryRun = true
	cfg.Provider = providerDNSPod
	cfg.Lines = []LineConfig{
		{Line: "cmcc", IPsURL: "http://x"},
		{Line: "移动", IPsURL: "http://y"},
	}
	if _, err := finalizeConfig(cfg); err == nil {
		t.Fatal("expected duplicate line error when both cmcc and 移动 are configured")
	}
}

func TestFinalizeConfigColoInheritanceAndValidation(t *testing.T) {
	cfg := defaultConfig()
	cfg.DryRun = true
	cfg.Provider = providerDNSPod
	cfg.Colo = " hkg , sjc , HKG "
	cfg.Lines = []LineConfig{
		{Line: "默认", IPsURL: "http://x"},
		{Line: "移动", IPsURL: "http://y", Colo: " lax , nrt , LAX "},
	}
	out, err := finalizeConfig(cfg)
	if err != nil {
		t.Fatalf("finalizeConfig error: %v", err)
	}
	if out.Colo != "HKG,SJC" || out.Lines[0].Colo != "HKG,SJC" || out.Lines[1].Colo != "LAX,NRT" {
		t.Fatalf("unexpected Colo normalization: global=%q line0=%q line1=%q", out.Colo, out.Lines[0].Colo, out.Lines[1].Colo)
	}

	badCfg := defaultConfig()
	badCfg.DryRun = true
	badCfg.Colo = "HKG,BAD1"
	if _, err := finalizeConfig(badCfg); err == nil {
		t.Fatal("expected finalizeConfig to reject invalid global Colo")
	}
}

func TestLoadConfigFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(`{"provider":"dnspod","secret_id":"id","secret_key":"key","domain":"example.com","record":"cf.example.com"}`), 0644); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadConfigFile(path)
	if err != nil {
		t.Fatalf("loadConfigFile error: %v", err)
	}
	if cfg.Provider != providerDNSPod || cfg.SecretID != "id" {
		t.Errorf("unexpected loaded cfg: %+v", cfg)
	}

	if _, err := loadConfigFile(filepath.Join(dir, "not_exist.json")); err == nil {
		t.Error("expected error for non-existent file")
	}
}
