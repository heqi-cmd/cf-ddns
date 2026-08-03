// Package dns 抽象 DNS 服务商的记录读写：按线路维护 A/AAAA/CNAME 解析记录。
package dns

import (
	"fmt"
	"strings"
)

// Record 表示某条线路上当前生效的一条解析记录
type Record struct {
	ID      string
	Type    string // "A" / "AAAA" / "CNAME"
	Content string
}

// Provider 抽象一个 DNS 服务商：按线路读写解析记录。
// line 使用服务商的线路名称（DNSPod："默认"/"移动"/"联通"/"电信"）；
// 不支持智能线路的服务商（Cloudflare）只会收到"默认"。
type Provider interface {
	// Get 返回该线路上当前类型属于 types 的记录，不存在时返回 (nil, nil)
	Get(line string, types []string) (*Record, error)
	Create(line, recordType, content string) error
	Update(id, line, recordType, content string) error
}

// EnsureRecord 把 line 线路上类型属于 queryTypes 的记录收敛到 recordType/content，返回动作描述用于日志。
// 主槽位用 queryTypes=[A, CNAME]（二者互斥、共用一个槽位），v6 槽位用 [AAAA]
func EnsureRecord(p Provider, line string, queryTypes []string, recordType, content string) (string, error) {
	cur, err := p.Get(line, queryTypes)
	if err != nil {
		return "", fmt.Errorf("查询当前 DNS 记录失败: %v", err)
	}

	if cur == nil {
		if err := p.Create(line, recordType, content); err != nil {
			return "", fmt.Errorf("创建 DNS 记录失败: %v", err)
		}
		return fmt.Sprintf("已创建记录 %s %s", recordType, content), nil
	}

	if cur.Type == recordType && sameRecordContent(cur.Content, content) {
		return fmt.Sprintf("最优结果与当前记录一致 (%s %s)，无需更新", cur.Type, content), nil
	}

	if err := p.Update(cur.ID, line, recordType, content); err != nil {
		return "", fmt.Errorf("更新 DNS 记录失败: %v", err)
	}
	return fmt.Sprintf("已更新记录 %s %s -> %s %s", cur.Type, cur.Content, recordType, content), nil
}

// sameRecordContent 比较记录值是否等价（忽略大小写和 DNSPod 返回 CNAME 值时携带的末尾点）
func sameRecordContent(a, b string) bool {
	return strings.EqualFold(strings.TrimSuffix(a, "."), strings.TrimSuffix(b, "."))
}

func typeIn(t string, types []string) bool {
	for _, x := range types {
		if strings.EqualFold(t, x) {
			return true
		}
	}
	return false
}
