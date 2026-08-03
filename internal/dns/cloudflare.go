package dns

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"
)

const cfAPIBase = "https://api.cloudflare.com/client/v4"

// cloudflareProvider 维护 Cloudflare DNS 上的单条记录。
// Cloudflare 没有运营商线路概念，接口中的 line 参数被忽略（配置校验已限定只有"默认"）。
type cloudflareProvider struct {
	token   string
	zoneID  string
	record  string
	proxied bool
	ttl     int
}

// NewCloudflare 创建 Cloudflare 服务商，record 为完整记录名
func NewCloudflare(token, zoneID, record string, proxied bool, ttl int) Provider {
	return &cloudflareProvider{token: token, zoneID: zoneID, record: record, proxied: proxied, ttl: ttl}
}

type cfAPIError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type cfDNSRecord struct {
	ID      string `json:"id"`
	Type    string `json:"type"`
	Content string `json:"content"`
}

type cfListResponse struct {
	Success bool          `json:"success"`
	Errors  []cfAPIError  `json:"errors"`
	Result  []cfDNSRecord `json:"result"`
}

type cfWriteResponse struct {
	Success bool         `json:"success"`
	Errors  []cfAPIError `json:"errors"`
}

func cfErrString(errs []cfAPIError) string {
	var parts []string
	for _, e := range errs {
		parts = append(parts, fmt.Sprintf("[%d] %s", e.Code, e.Message))
	}
	return strings.Join(parts, "; ")
}

func (p *cloudflareProvider) Get(_ string, types []string) (*Record, error) {
	url := fmt.Sprintf("%s/zones/%s/dns_records?name=%s", cfAPIBase, p.zoneID, p.record)
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+p.token)
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	var data cfListResponse
	if err := json.Unmarshal(body, &data); err != nil {
		return nil, fmt.Errorf("解析响应失败: %v", err)
	}
	if !data.Success {
		return nil, fmt.Errorf("Cloudflare API 返回失败: %s", cfErrString(data.Errors))
	}

	var found *Record
	matched := 0
	for _, r := range data.Result {
		if !typeIn(r.Type, types) {
			continue
		}
		matched++
		if found == nil {
			found = &Record{ID: r.ID, Type: r.Type, Content: r.Content}
		}
	}
	if matched > 1 {
		log.Printf("注意: %s 名下存在 %d 条 %s 记录，本工具只维护其中一条，建议手动清理多余记录", p.record, matched, strings.Join(types, "/"))
	}
	return found, nil
}

func (p *cloudflareProvider) Create(_, recordType, content string) error {
	return p.write("", recordType, content)
}

func (p *cloudflareProvider) Update(id, _, recordType, content string) error {
	return p.write(id, recordType, content)
}

// write 创建（recordID 为空）或更新（recordID 非空）DNS 记录
func (p *cloudflareProvider) write(recordID, recordType, content string) error {
	payload := map[string]any{
		"type":    recordType,
		"name":    p.record,
		"content": content,
		"ttl":     p.ttl,
		"proxied": p.proxied,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}

	method := "POST"
	url := fmt.Sprintf("%s/zones/%s/dns_records", cfAPIBase, p.zoneID)
	if recordID != "" {
		method = "PUT"
		url = fmt.Sprintf("%s/zones/%s/dns_records/%s", cfAPIBase, p.zoneID, recordID)
	}

	req, err := http.NewRequest(method, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+p.token)
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}

	var data cfWriteResponse
	if err := json.Unmarshal(respBody, &data); err != nil {
		return fmt.Errorf("解析响应失败: %v", err)
	}
	if !data.Success {
		return fmt.Errorf("Cloudflare API 返回失败: %s", cfErrString(data.Errors))
	}
	return nil
}
