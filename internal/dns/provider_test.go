package dns

import "testing"

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

	// 值不同 → 更新
	p = &fakeProvider{rec: &Record{ID: "1", Type: "A", Content: "1.2.3.4"}}
	if _, err := EnsureRecord(p, "移动", mainTypes, "A", "5.6.7.8"); err != nil {
		t.Fatal(err)
	}
	if p.updated != 1 || p.lastVal != "5.6.7.8" {
		t.Errorf("值不同时应更新: %+v", p)
	}

	// 已有 A 记录不影响 AAAA 槽位：按 [AAAA] 查询命不中 → 创建 AAAA
	p = &fakeProvider{rec: &Record{ID: "1", Type: "A", Content: "1.2.3.4"}}
	if _, err := EnsureRecord(p, "移动", []string{"AAAA"}, "AAAA", "2606::1"); err != nil {
		t.Fatal(err)
	}
	if p.created != 1 || p.lastRT != "AAAA" || p.lastVal != "2606::1" {
		t.Errorf("A 记录存在时 AAAA 槽位应独立创建: %+v", p)
	}

	// CNAME 值带末尾点视为一致
	p = &fakeProvider{rec: &Record{ID: "1", Type: "CNAME", Content: "www.Example.com."}}
	if _, err := EnsureRecord(p, "移动", mainTypes, "CNAME", "www.example.com"); err != nil {
		t.Fatal(err)
	}
	if p.created != 0 || p.updated != 0 {
		t.Errorf("CNAME 末尾点/大小写差异不应触发更新: %+v", p)
	}
}
