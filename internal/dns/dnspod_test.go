package dns

import (
	"strings"
	"testing"
)

func TestTC3AuthorizationFormat(t *testing.T) {
	ts := int64(1719817200) // 2024-07-01 UTC 上午
	auth := tc3Authorization("AKIDexample", "secret", "DescribeRecordList", []byte(`{"Domain":"example.com"}`), ts)

	if !strings.HasPrefix(auth, "TC3-HMAC-SHA256 Credential=AKIDexample/2024-07-01/dnspod/tc3_request, ") {
		t.Errorf("Credential 段格式不对: %s", auth)
	}
	if !strings.Contains(auth, "SignedHeaders=content-type;host;x-tc-action, Signature=") {
		t.Errorf("SignedHeaders 段格式不对: %s", auth)
	}
	// 相同输入必须产生确定性的签名
	if auth != tc3Authorization("AKIDexample", "secret", "DescribeRecordList", []byte(`{"Domain":"example.com"}`), ts) {
		t.Error("签名应是确定性的")
	}
}
