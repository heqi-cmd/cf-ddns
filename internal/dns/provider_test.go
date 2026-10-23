package dns

import (
	"testing"
)

func TestSameRecordContent(t *testing.T) {
	if !sameRecordContent("www.Shopify.com.", "www.shopify.com") {
		t.Error("应忽略末尾点和大小写")
	}
	if sameRecordContent("1.2.3.4", "1.2.3.5") {
		t.Error("不同值不应相等")
	}
}

// fakeProvider 记录 EnsureRecord 触发的读写动作，模拟一条线路上已有的记录
type fakeProvider struct {
	rec     *Record
	created int
	updated int
	lastRT  string
	lastVal string
}

func (f *fakeProvider) Get(_ string, types []string) (*Record, error) {
	if f.rec != nil && typeIn(f.rec.Type, types) {
		return f.rec, nil
	}
	return nil, nil
}

func (f *fakeProvider) Create(_, recordType, content string) error {
	f.created++
	f.lastRT, f.lastVal = recordType, content
	return nil
}

func (f *fakeProvider) Update(_, _, recordType, content string) error {
	f.updated++
	f.lastRT, f.lastVal = recordType, content
	return nil
}

func TestFormatRecordChange(t *testing.T) {
	ch, msg := FormatRecordChange(nil, "A", "1.1.1.1")
	if !ch || msg != "已创建记录 A 1.1.1.1" {
		t.Fatalf("create: got (%v, %q)", ch, msg)
	}

	ch, msg = FormatRecordChange(&Record{ID: "1", Type: "CNAME", Content: "www.Example.com."}, "cname", "www.example.com")
	if ch || msg != "最优结果与当前记录一致 (CNAME www.example.com)，无需更新" {
		t.Fatalf("noop: got (%v, %q)", ch, msg)
	}

	ch, msg = FormatRecordChange(&Record{ID: "1", Type: "A", Content: "1.1.1.1"}, "A", "2.2.2.2")
	if !ch || msg != "已更新记录 A 1.1.1.1 -> A 2.2.2.2" {
		t.Fatalf("update: got (%v, %q)", ch, msg)
	}
}

func TestEnsureRecord(t *testing.T) {
	mainTypes := []string{"A", "CNAME"}

	// 无现有记录 → 创建
	p := &fakeProvider{}
	if _, err := EnsureRecord(p, "移动", mainTypes, "A", "1.2.3.4"); err != nil {
		t.Fatal(err)
	}
	if p.created != 1 || p.updated != 0 || p.lastRT != "A" || p.lastVal != "1.2.3.4" {
		t.Errorf("应创建 A 记录: %+v", p)
	}

	// 值一致 → 不动
	p = &fakeProvider{rec: &Record{ID: "1", Type: "A", Content: "1.2.3.4"}}
	if _, err := EnsureRecord(p, "移动", mainTypes, "A", "1.2.3.4"); err != nil {
		t.Fatal(err)
	}
	if p.created != 0 || p.updated != 0 {
		t.Errorf("值一致时不应有写操作: %+v", p)
	}

	// CNAME 规范化与末尾点剥离
	pCNAME := &fakeProvider{}
	if _, err := EnsureRecord(pCNAME, "移动", mainTypes, " cname ", " www.Example.com. "); err != nil {
		t.Fatalf("EnsureRecord error: %v", err)
	}
	if pCNAME.created != 1 || pCNAME.lastRT != "CNAME" || pCNAME.lastVal != "www.Example.com" {
		t.Fatalf("got created=%d rt=%q val=%q", pCNAME.created, pCNAME.lastRT, pCNAME.lastVal)
	}

	// 空白参数拦截
	pErr := &fakeProvider{}
	if _, err := EnsureRecord(pErr, "移动", []string{"A"}, "   ", "1.2.3.4"); err == nil {
		t.Fatal("expected error for blank recordType")
	}
	if _, err := EnsureRecord(pErr, "移动", []string{"A"}, "A", "   "); err == nil {
		t.Fatal("expected error for blank content")
	}
	if _, err := EnsureRecord(pErr, "移动", []string{"CNAME"}, "cname", " ... "); err == nil {
		t.Fatal("expected error for dot-only CNAME content")
	}

	// 更新时 ID 为空拦截
	pNoID := &fakeProvider{rec: &Record{ID: " ", Type: "A", Content: "1.1.1.1"}}
	if _, err := EnsureRecord(pNoID, "移动", []string{"A"}, "A", "2.2.2.2"); err == nil {
		t.Fatal("expected EnsureRecord to reject empty existing Record.ID on update")
	}
}

func TestTypeIn(t *testing.T) {
	if !typeIn("A", []string{"cname", "a"}) {
		t.Error("typeIn should match case-insensitively")
	}
	if typeIn("TXT", []string{"A", "AAAA"}) {
		t.Error("typeIn should return false for absent type")
	}
}
