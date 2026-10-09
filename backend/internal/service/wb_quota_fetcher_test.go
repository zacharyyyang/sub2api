//go:build unit

package service

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func wbQuotaAccount() *Account {
	return &Account{ID: 3001, Platform: PlatformWB, Credentials: map[string]any{
		wbCredentialPTKey:        "ptk-1",
		wbCredentialEnterpriseID: "eid-9",
		wbCredentialClientID:     "cid-1",
		wbCredentialClientSecret: "csec-1",
	}}
}

func TestWbQuotaFetcher_CanFetch(t *testing.T) {
	fetcher := NewWbQuotaFetcher(nil, "")
	require.True(t, fetcher.CanFetch(wbQuotaAccount()))
	require.False(t, fetcher.CanFetch(&Account{ID: 3002, Platform: PlatformWB}))
	require.False(t, fetcher.CanFetch(&Account{ID: 3003, Platform: PlatformGrok, Credentials: map[string]any{"pt_key": "x"}}))
	require.False(t, fetcher.CanFetch(nil))
}

func TestWbQuotaFetcher_FetchQuota_DataCredit(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/eid-9/openapi/resources/overview", r.URL.Path)
		require.Equal(t, "Bearer ptk-1", r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"code":0,"data":{"credit":{"total":20000,"remaining":19537,"expiresAt":"2027-03-24"}}}`)
	}))
	defer server.Close()

	fetcher := NewWbQuotaFetcher(server.Client(), server.URL)
	result, err := fetcher.FetchQuota(context.Background(), wbQuotaAccount(), "")
	require.NoError(t, err)
	require.NotNil(t, result.UsageInfo)
	require.NotZero(t, result.UsageInfo.UpdatedAt)
	require.NotNil(t, result.UsageInfo.WbEnterpriseCredits)
	require.Equal(t, int64(19537), result.UsageInfo.WbEnterpriseCredits.Remaining)
	require.Equal(t, int64(20000), result.UsageInfo.WbEnterpriseCredits.Total)
	require.Equal(t, "2027-03-24", result.UsageInfo.WbEnterpriseCredits.ExpiresAt)
}

func TestWbQuotaFetcher_FetchQuota_RealAPIShape(t *testing.T) {
	// 2026-10-08 AC5 真机实证形状：data.items[] 元素以 resourceType（非 type）标记，
	// 带 unit/used/remainingRatio/sources 等附加字段——逐项直通解析。
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"code":0,"msg":"OK","requestId":"43a3300b-cfe8-40c9-8a46-939197df28a2","data":{
			"enterpriseId":"fz7gg6jzyrr4",
			"items":[
				{"resourceType":"directMode","unit":"count","total":0,"used":0,"remaining":0},
				{"resourceType":"credit","unit":"credit","total":20000,"used":528,"remaining":19472,
				 "remainingRatio":0.9736,"expiresAt":"2027-03-24T17:28:10+08:00","queriedAt":"2026-10-08T22:21:16+08:00",
				 "sources":[{"sourceType":"fixedPackage","total":10000,"used":528,"remaining":9472}]}
			]}}`)
	}))
	defer server.Close()

	fetcher := NewWbQuotaFetcher(server.Client(), server.URL)
	result, err := fetcher.FetchQuota(context.Background(), wbQuotaAccount(), "")
	require.NoError(t, err)
	require.NotNil(t, result.UsageInfo.WbEnterpriseCredits)
	require.Equal(t, int64(19472), result.UsageInfo.WbEnterpriseCredits.Remaining)
	require.Equal(t, int64(20000), result.UsageInfo.WbEnterpriseCredits.Total)
	require.Equal(t, "2027-03-24T17:28:10+08:00", result.UsageInfo.WbEnterpriseCredits.ExpiresAt)
}

func TestWbQuotaFetcher_FetchQuota_TopLevelCredit(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"credit":{"remaining":5,"total":10}}`)
	}))
	defer server.Close()

	fetcher := NewWbQuotaFetcher(server.Client(), server.URL)
	result, err := fetcher.FetchQuota(context.Background(), wbQuotaAccount(), "")
	require.NoError(t, err)
	require.NotNil(t, result.UsageInfo.WbEnterpriseCredits)
	require.Equal(t, int64(5), result.UsageInfo.WbEnterpriseCredits.Remaining)
	require.Equal(t, int64(10), result.UsageInfo.WbEnterpriseCredits.Total)
}

func TestWbQuotaFetcher_FetchQuota_ItemsList(t *testing.T) {
	// 列表元素带 credit 类型标记（设计判据：remaining/total + type/kind/resource 含 credit）
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"data":{"items":[
			{"type":"directMode","remaining":0,"total":0},
			{"type":"credit","remaining":19537,"total":20000,"expiresAt":"2027-03-24"}
		]}}`)
	}))
	defer server.Close()

	fetcher := NewWbQuotaFetcher(server.Client(), server.URL)
	result, err := fetcher.FetchQuota(context.Background(), wbQuotaAccount(), "")
	require.NoError(t, err)
	require.NotNil(t, result.UsageInfo.WbEnterpriseCredits)
	require.Equal(t, int64(19537), result.UsageInfo.WbEnterpriseCredits.Remaining)
	require.Equal(t, int64(20000), result.UsageInfo.WbEnterpriseCredits.Total)
	require.Equal(t, "2027-03-24", result.UsageInfo.WbEnterpriseCredits.ExpiresAt)
}

func TestWbQuotaFetcher_FetchQuota_DFS(t *testing.T) {
	// 深层嵌套 + type 含 credit 的类型标记 → DFS 兜底命中
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"data":{"modules":[{"name":"enterprise.gift","detail":{"type":"credit","remaining":42,"total":100}}]}}`)
	}))
	defer server.Close()

	fetcher := NewWbQuotaFetcher(server.Client(), server.URL)
	result, err := fetcher.FetchQuota(context.Background(), wbQuotaAccount(), "")
	require.NoError(t, err)
	require.NotNil(t, result.UsageInfo.WbEnterpriseCredits)
	require.Equal(t, int64(42), result.UsageInfo.WbEnterpriseCredits.Remaining)
	require.Equal(t, int64(100), result.UsageInfo.WbEnterpriseCredits.Total)
}

func TestWbQuotaFetcher_FetchQuota_Non2xx(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		fmt.Fprintf(w, `{"error":"unauthorized"}`)
	}))
	defer server.Close()

	fetcher := NewWbQuotaFetcher(server.Client(), server.URL)
	result, err := fetcher.FetchQuota(context.Background(), wbQuotaAccount(), "")
	require.NoError(t, err) // 值通道：错误不返回
	require.Nil(t, result.UsageInfo.WbEnterpriseCredits)
	require.NotZero(t, result.UsageInfo.UpdatedAt)
}

func TestWbQuotaFetcher_FetchQuota_Malformed(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{not-json`)
	}))
	defer server.Close()

	fetcher := NewWbQuotaFetcher(server.Client(), server.URL)
	result, err := fetcher.FetchQuota(context.Background(), wbQuotaAccount(), "")
	require.NoError(t, err)
	require.Nil(t, result.UsageInfo.WbEnterpriseCredits)
}

func TestWbQuotaFetcher_FetchQuota_MalformedCredit(t *testing.T) {
	// credit 项存在但 remaining 畸形 → 整体 nil（不造 0 兜底）
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"data":{"credit":{"remaining":"abc","total":20000}}}`)
	}))
	defer server.Close()

	fetcher := NewWbQuotaFetcher(server.Client(), server.URL)
	result, err := fetcher.FetchQuota(context.Background(), wbQuotaAccount(), "")
	require.NoError(t, err)
	require.Nil(t, result.UsageInfo.WbEnterpriseCredits)
}

func TestWbQuotaFetcher_FetchQuota_ZeroRemainingIsData(t *testing.T) {
	// remaining=0 是合法数据（余额用尽），必须显示而不是 nil
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"data":{"credit":{"remaining":0,"total":20000}}}`)
	}))
	defer server.Close()

	fetcher := NewWbQuotaFetcher(server.Client(), server.URL)
	result, err := fetcher.FetchQuota(context.Background(), wbQuotaAccount(), "")
	require.NoError(t, err)
	require.NotNil(t, result.UsageInfo.WbEnterpriseCredits)
	require.Equal(t, int64(0), result.UsageInfo.WbEnterpriseCredits.Remaining)
	require.Equal(t, int64(20000), result.UsageInfo.WbEnterpriseCredits.Total)
}

func TestWbQuotaFetcher_FetchQuota_ExpiresNumeric(t *testing.T) {
	// 秒级数字 expires_at → 规格化为日期
	unix := time.Date(2027, 3, 24, 0, 0, 0, 0, time.UTC).Unix()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"data":{"credit":{"remaining":5,"total":10,"expires_at":%d}}}`, unix)
	}))
	defer server.Close()

	fetcher := NewWbQuotaFetcher(server.Client(), server.URL)
	result, err := fetcher.FetchQuota(context.Background(), wbQuotaAccount(), "")
	require.NoError(t, err)
	require.Equal(t, "2027-03-24", result.UsageInfo.WbEnterpriseCredits.ExpiresAt)
}

func TestWbNumeric_RejectsNonIntegralAndOverflow(t *testing.T) {
	// 整数契约（AC3 畸形不造 0）：小数截断会制造「余额用尽」假象、超大值溢出 int64 → 必须拒绝
	cases := []any{
		float64(0.9),                        // 截断会变 0
		float64(10.9),                       // 截断缩水
		float64(-0.5),                       // 负数
		math.NaN(),                          // 非数
		math.Inf(1),                         // 正无穷
		float64(math.MaxInt64) * 2,          // 溢出 int64 上界
		float64(math.MaxInt64),              // float64 舍入成 2^63（> 比较漏网、int64 转换回绕）→ 必须拒
		float64(1 << 63),                    // 恰为 2^63，int64 域外第一值
		json.Number("123.5"),                // 字符串形态的小数同样拒绝
		json.Number("9223372036854775808"),  // 2^63：int64 上界外第一整数（精度无损地拒绝）
		json.Number("18446744073709551615"), // uint64 域最大值：同样拒绝
	}
	for i, v := range cases {
		got, ok := wbNumeric(v)
		require.False(t, ok, "case %d: %v must be rejected", i, v)
		require.Equal(t, int64(0), got)
	}
	// 合法边界仍接受：0 与安全整数
	got, ok := wbNumeric(float64(0))
	require.True(t, ok)
	require.Equal(t, int64(0), got)
	got, ok = wbNumeric(float64(1e15))
	require.True(t, ok)
	require.Equal(t, int64(1e15), got)
	// int64 域内最大可精确表示的 float64（2^63-2048）：接受且精确还原
	got, ok = wbNumeric(float64(1<<63) - 2048)
	require.True(t, ok)
	require.Equal(t, int64(9223372036854773760), got) // 2^63-2048
	// json.Number 大整数精度无损（UseNumber 解码后走该分支）：MaxInt64 文本精确接受
	got, ok = wbNumeric(json.Number("9223372036854775807"))
	require.True(t, ok)
	require.Equal(t, int64(math.MaxInt64), got)
}

func TestWbQuotaFetcher_FetchQuota_MissingCredentials(t *testing.T) {
	fetcher := NewWbQuotaFetcher(nil, "")
	account := &Account{ID: 3004, Platform: PlatformWB, Credentials: map[string]any{wbCredentialPTKey: "ptk-1"}}
	result, err := fetcher.FetchQuota(context.Background(), account, "")
	require.NoError(t, err)
	require.Nil(t, result.UsageInfo.WbEnterpriseCredits)

	account = &Account{ID: 3005, Platform: PlatformGrok}
	result, err = fetcher.FetchQuota(context.Background(), account, "")
	require.NoError(t, err)
	require.Nil(t, result.UsageInfo.WbEnterpriseCredits)
}
