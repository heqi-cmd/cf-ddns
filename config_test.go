package main

import (
	"strings"
	"testing"

	"cf-ddns/internal/probe"
)

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
		t.Errorf("默认线路应被排到最前，实际顺序 %+v", out.Lines)
	}
	if out.Lines[1].Pick != probe.PickOrder {
		t.Errorf("显式配置的线路 pick 默认应为 order，实际 %q", out.Lines[1].Pick)
	}

	cfg.Record = "cf.other.com"
	if _, err := finalizeConfig(cfg); err == nil {
		t.Error("record 不是 domain 的子域名时应报错")
	}
}

func TestFinalizeConfigDuplicateLine(t *testing.T) {
	cfg := defaultConfig()
	cfg.DryRun = true
	cfg.Provider = providerDNSPod
	cfg.Lines = []LineConfig{
		{Line: "移动", IPsURL: "http://x"},
		{Line: "移动", IPsURL: "http://y"},
	}
	if _, err := finalizeConfig(cfg); err == nil {
		t.Error("重复线路应报错")
	}
}
