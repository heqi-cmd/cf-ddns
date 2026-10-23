// Package dns 抽象 DNS 服务商的记录读写：按线路维护 A/AAAA/CNAME 解析记录。
package dns

import (
	"errors"
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

// FormatRecordChange 计算把既有记录 prev 收敛到 nextType/nextContent 的动作：
// 返回是否需要写入，以及用于日志的中文摘要（创建 / 无需更新 / 更新）。
func FormatRecordChange(prev *Record, nextType, nextContent string) (bool, string) {
	normType := strings.ToUpper(strings.TrimSpace(nextType))
	normContent := cleanRecordContent(normType, nextContent)
	if prev == nil {
		return true, fmt.Sprintf("已创建记录 %s %s", normType, normContent)
	}
	if strings.EqualFold(prev.Type, normType) && sameRecordContent(prev.Content, normContent) {
		return false, fmt.Sprintf("最优结果与当前记录一致 (%s %s)，无需更新", normType, normContent)
	}
	return true, fmt.Sprintf("已更新记录 %s %s -> %s %s", prev.Type, cleanRecordContent(prev.Type, prev.Content), normType, normContent)
}

// EnsureRecord 把 line 线路上类型属于 queryTypes 的记录收敛到 recordType/content，返回动作描述用于日志。
// 主槽位用 queryTypes=[A, CNAME]（二者互斥、共用一个槽位），v6 槽位用 [AAAA]
func EnsureRecord(p Provider, line string, queryTypes []string, recordType, content string) (string, error) {
	normType := strings.ToUpper(strings.TrimSpace(recordType))
	if normType == "" {
		return "", errors.New("recordType 不能为空")
	}
	normContent := cleanRecordContent(normType, content)
	if normContent == "" {
		return "", errors.New("记录内容不能为空")
	}

	cur, err := p.Get(line, queryTypes)
	if err != nil {
		return "", fmt.Errorf("查询当前 DNS 记录失败: %v", err)
	}

	changed, summary := FormatRecordChange(cur, normType, normContent)
	if !changed {
		return summary, nil
	}

	if cur == nil {
		if err := p.Create(line, normType, normContent); err != nil {
			return "", fmt.Errorf("创建 DNS 记录失败: %v", err)
		}
		return summary, nil
	}

	if strings.TrimSpace(cur.ID) == "" {
		return "", fmt.Errorf("更新 DNS 记录失败: 线路 %s 的现有记录缺少 id，无法更新", line)
	}

	if err := p.Update(cur.ID, line, normType, normContent); err != nil {
		return "", fmt.Errorf("更新 DNS 记录失败: %v", err)
	}
	return summary, nil
}

// cleanRecordContent 清洗记录内容：首尾空白 trim，CNAME 额外去除末尾的点
func cleanRecordContent(recordType, content string) string {
	val := strings.TrimSpace(content)
	if strings.EqualFold(recordType, "CNAME") {
		val = strings.TrimRight(val, ".")
		val = strings.TrimSpace(val)
	}
	return val
}

// sameRecordContent 比较记录值是否等价（忽略首尾空白、大小写和 CNAME 值的末尾点）
func sameRecordContent(a, b string) bool {
	return strings.EqualFold(trimRecordValue(a), trimRecordValue(b))
}

// trimRecordValue 去掉首尾空白和末尾的点，用于记录值的等价比较
func trimRecordValue(s string) string {
	return strings.TrimRight(strings.TrimSpace(s), ".")
}

func typeIn(t string, types []string) bool {
	for _, x := range types {
		if strings.EqualFold(t, x) {
			return true
		}
	}
	return false
}
