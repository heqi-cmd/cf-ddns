package dns

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const (
	dnspodHost    = "dnspod.tencentcloudapi.com"
	dnspodService = "dnspod"
	dnspodVersion = "2021-03-23"
)

// dnspodProvider 通过腾讯云 DNSPod API（TC3-HMAC-SHA256 签名）按运营商线路读写解析记录。
// DNSPod 允许同一主机记录在不同线路上各存一条记录，权威解析时按访问者运营商返回对应线路的值。
type dnspodProvider struct {
	secretID  string
	secretKey string
	domain    string // 主域名，例如 example.com
	subDomain string // 主机记录，例如 cf；根域名为 "@"
	ttl       int
}

// NewDNSPod 创建 DNSPod 服务商，subDomain 为主机记录（根域名传 "@"）
func NewDNSPod(secretID, secretKey, domain, subDomain string, ttl int) Provider {
	return &dnspodProvider{secretID: secretID, secretKey: secretKey, domain: domain, subDomain: subDomain, ttl: ttl}
}

// dnspodAPIError 是 API 层面的业务错误（HTTP 200 但 Response.Error 非空）
type dnspodAPIError struct {
	code    string
	message string
}

func (e *dnspodAPIError) Error() string {
	return fmt.Sprintf("[%s] %s", e.code, e.message)
}

// isDNSPodNoData 判断是否为"没有符合条件的记录"——对本工具而言这不是错误
func isDNSPodNoData(err error) bool {
	apiErr, ok := err.(*dnspodAPIError)
	return ok && apiErr.code == "ResourceNotFound.NoDataOfRecord"
}

type dnspodRecord struct {
	RecordID uint64 `json:"RecordId"`
	Name     string `json:"Name"`
	Type     string `json:"Type"`
	Line     string `json:"Line"`
	Value    string `json:"Value"`
	Status   string `json:"Status"` // ENABLE / DISABLE
}

type dnspodResponse struct {
	Response struct {
		Error *struct {
			Code    string `json:"Code"`
			Message string `json:"Message"`
		} `json:"Error"`
		RecordList []dnspodRecord `json:"RecordList"`
	} `json:"Response"`
}

func (p *dnspodProvider) Get(line string, types []string) (*Record, error) {
	var resp dnspodResponse
	// 注意：DescribeRecordList 的主机记录过滤参数名是 Subdomain（区别于 CreateRecord 的 SubDomain）。
	// Limit 拉满并在本地再按 Name 过滤一遍，避免依赖服务端过滤行为
	err := p.call("DescribeRecordList", map[string]any{
		"Domain":    p.domain,
		"Subdomain": p.subDomain,
		"Limit":     3000,
	}, &resp)
	if err != nil {
		if isDNSPodNoData(err) {
			return nil, nil
		}
		return nil, err
	}

	var found *Record
	matched := 0
	for _, r := range resp.Response.RecordList {
		if r.Name != p.subDomain || r.Line != line {
			continue
		}
		if !typeIn(r.Type, types) {
			continue
		}
		matched++
		if found == nil {
			if r.Status == "DISABLE" {
				log.Printf("注意: %s.%s 线路 %s 的现有记录处于暂停状态，更新值后仍需在 DNSPod 控制台手动启用", p.subDomain, p.domain, line)
			}
			found = &Record{ID: strconv.FormatUint(r.RecordID, 10), Type: r.Type, Content: r.Value}
		}
	}
	if matched > 1 {
		log.Printf("注意: %s.%s 线路 %s 存在 %d 条 %s 记录，本工具只维护其中一条，建议手动清理多余记录", p.subDomain, p.domain, line, matched, strings.Join(types, "/"))
	}
	return found, nil
}

func (p *dnspodProvider) Create(line, recordType, content string) error {
	return p.call("CreateRecord", p.writeParams(0, line, recordType, content), &dnspodResponse{})
}

func (p *dnspodProvider) Update(id, line, recordType, content string) error {
	recordID, err := strconv.ParseUint(id, 10, 64)
	if err != nil {
		return fmt.Errorf("非法的记录 ID %q: %v", id, err)
	}
	return p.call("ModifyRecord", p.writeParams(recordID, line, recordType, content), &dnspodResponse{})
}

func (p *dnspodProvider) writeParams(recordID uint64, line, recordType, content string) map[string]any {
	params := map[string]any{
		"Domain":     p.domain,
		"SubDomain":  p.subDomain,
		"RecordType": recordType,
		"RecordLine": line,
		"Value":      content,
		"TTL":        p.ttl,
	}
	if recordID != 0 {
		params["RecordId"] = recordID
	}
	return params
}

// call 调用腾讯云 DNSPod API 并解析响应，API 业务错误以 *dnspodAPIError 返回
func (p *dnspodProvider) call(action string, params map[string]any, out *dnspodResponse) error {
	payload, err := json.Marshal(params)
	if err != nil {
		return err
	}

	timestamp := time.Now().Unix()
	req, err := http.NewRequest("POST", "https://"+dnspodHost, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	req.Header.Set("X-TC-Action", action)
	req.Header.Set("X-TC-Version", dnspodVersion)
	req.Header.Set("X-TC-Timestamp", strconv.FormatInt(timestamp, 10))
	req.Header.Set("Authorization", tc3Authorization(p.secretID, p.secretKey, action, payload, timestamp))

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("解析响应失败 (HTTP %d): %v", resp.StatusCode, err)
	}
	if apiErr := out.Response.Error; apiErr != nil {
		return &dnspodAPIError{code: apiErr.Code, message: apiErr.Message}
	}
	return nil
}

// tc3Authorization 计算腾讯云 API v3 的 TC3-HMAC-SHA256 签名。
// 算法参考 https://cloud.tencent.com/document/api/1427/56189
func tc3Authorization(secretID, secretKey, action string, payload []byte, timestamp int64) string {
	date := time.Unix(timestamp, 0).UTC().Format("2006-01-02")

	// 1. 拼接规范请求串（签名头的 value 统一小写）
	canonicalHeaders := "content-type:application/json; charset=utf-8\n" +
		"host:" + dnspodHost + "\n" +
		"x-tc-action:" + strings.ToLower(action) + "\n"
	signedHeaders := "content-type;host;x-tc-action"
	canonicalRequest := "POST\n/\n\n" + canonicalHeaders + "\n" + signedHeaders + "\n" + sha256Hex(payload)

	// 2. 拼接待签名字符串
	scope := date + "/" + dnspodService + "/tc3_request"
	stringToSign := "TC3-HMAC-SHA256\n" + strconv.FormatInt(timestamp, 10) + "\n" + scope + "\n" +
		sha256Hex([]byte(canonicalRequest))

	// 3. 派生签名密钥并计算签名
	secretDate := hmacSHA256([]byte("TC3"+secretKey), date)
	secretService := hmacSHA256(secretDate, dnspodService)
	secretSigning := hmacSHA256(secretService, "tc3_request")
	signature := hex.EncodeToString(hmacSHA256(secretSigning, stringToSign))

	return "TC3-HMAC-SHA256 Credential=" + secretID + "/" + scope +
		", SignedHeaders=" + signedHeaders + ", Signature=" + signature
}

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func hmacSHA256(key []byte, msg string) []byte {
	h := hmac.New(sha256.New, key)
	h.Write([]byte(msg))
	return h.Sum(nil)
}
