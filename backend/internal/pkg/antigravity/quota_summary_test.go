//go:build unit

package antigravity

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

const defaultQuotaSummaryBodyLimit int64 = 8 << 20

// 实测形状：2 组 × 2 窗口（gemini-5h / gemini-weekly / 3p-5h / 3p-weekly）
const quotaSummaryRealShape = `{"groups":[
  {"displayName":"Gemini Models","description":"Gemini 系列模型共享额度",
   "buckets":[
     {"bucketId":"gemini-5h","displayName":"Gemini Models 5h","window":"5h","resetTime":"2026-10-05T12:00:00Z","remainingFraction":0.97,"description":""},
     {"bucketId":"gemini-weekly","displayName":"Gemini Models 7d","window":"7d","resetTime":"2026-10-05T12:00:00Z","remainingFraction":0.6,"description":""}
   ]},
  {"displayName":"Claude & GPT","description":"第三方模型共享额度",
   "buckets":[
     {"bucketId":"3p-5h","displayName":"Claude & GPT 5h","window":"5h","resetTime":"2026-10-05T12:00:00Z","remainingFraction":1.0,"description":""},
     {"bucketId":"3p-weekly","displayName":"Claude & GPT 7d","window":"7d","resetTime":"2026-10-05T12:00:00Z","remainingFraction":0.0,"description":""}
   ]}
]}`

func TestFetchUserQuotaSummary_Success(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("请求方法不匹配: got %s, want POST", r.Method)
		}
		if !strings.HasSuffix(r.URL.Path, "/v1internal:retrieveUserQuotaSummary") {
			t.Errorf("URL 路径不匹配: got %s", r.URL.Path)
		}
		if auth := r.Header.Get("Authorization"); auth != "Bearer test-token" {
			t.Errorf("Authorization 不匹配: got %q", auth)
		}
		if ct := r.Header.Get("Content-Type"); ct != "application/json" {
			t.Errorf("Content-Type 不匹配: got %q", ct)
		}
		if ua := r.Header.Get("User-Agent"); ua != GetUserAgent() {
			t.Errorf("User-Agent 不匹配: got %q", ua)
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("读取请求体失败: %v", err)
		}
		if string(body) != "{}" {
			t.Errorf("请求体不匹配: got %s, want {}", body)
		}

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(quotaSummaryRealShape))
	}))
	defer server.Close()

	withMockBaseURLs(t, []string{server.URL})

	client := mustNewClient(t, "")
	resp, err := client.FetchUserQuotaSummary(context.Background(), "test-token", defaultQuotaSummaryBodyLimit)
	require.NoError(t, err)
	require.NotNil(t, resp)
	require.Len(t, resp.Groups, 2)

	g0 := resp.Groups[0]
	require.Equal(t, "Gemini Models", g0.DisplayName)
	require.Len(t, g0.Buckets, 2)
	require.Equal(t, "gemini-5h", g0.Buckets[0].BucketID)
	require.Equal(t, "gemini-weekly", g0.Buckets[1].BucketID)

	g1 := resp.Groups[1]
	require.Equal(t, "Claude & GPT", g1.DisplayName)
	require.Len(t, g1.Buckets, 2)
	require.Equal(t, "3p-5h", g1.Buckets[0].BucketID)
	require.Equal(t, "3p-weekly", g1.Buckets[1].BucketID)
}

// remainingFraction 三态解码：缺字段 ⇒ nil、JSON null ⇒ nil、合法 0 ⇒ &0
func TestQuotaSummaryBucket_RemainingFractionThreeStates(t *testing.T) {
	tests := []struct {
		name     string
		jsonBody string
		wantNil  bool
		wantVal  float64
	}{
		{
			name:     "缺字段",
			jsonBody: `{"bucketId":"gemini-5h"}`,
			wantNil:  true,
		},
		{
			name:     "null",
			jsonBody: `{"bucketId":"gemini-5h","remainingFraction":null}`,
			wantNil:  true,
		},
		{
			name:     "合法零值",
			jsonBody: `{"bucketId":"3p-weekly","remainingFraction":0.0}`,
			wantNil:  false,
			wantVal:  0.0,
		},
		{
			name:     "普通值",
			jsonBody: `{"bucketId":"gemini-5h","remainingFraction":0.97}`,
			wantNil:  false,
			wantVal:  0.97,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var bucket UserQuotaBucket
			require.NoError(t, json.Unmarshal([]byte(tt.jsonBody), &bucket))
			if tt.wantNil {
				require.Nil(t, bucket.RemainingFraction, "预期 remainingFraction 为 nil")
				return
			}
			require.NotNil(t, bucket.RemainingFraction, "预期 remainingFraction 非 nil")
			require.InDelta(t, tt.wantVal, *bucket.RemainingFraction, 1e-9)
		})
	}
}

func TestFetchUserQuotaSummary_URLFallback(t *testing.T) {
	var badHit, goodHit bool

	errServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		badHit = true
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":"boom"}`))
	}))
	defer errServer.Close()

	okServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		goodHit = true
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(quotaSummaryRealShape))
	}))
	defer okServer.Close()

	withMockBaseURLs(t, []string{errServer.URL, okServer.URL})

	client := mustNewClient(t, "")
	resp, err := client.FetchUserQuotaSummary(context.Background(), "test-token", defaultQuotaSummaryBodyLimit)
	require.NoError(t, err)
	require.True(t, badHit, "第一个 URL 应被尝试")
	require.True(t, goodHit, "回退后第二个 URL 应被调用")
	require.Len(t, resp.Groups, 2)
}

func TestFetchUserQuotaSummary_BodyLimitExceeded(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(quotaSummaryRealShape))
	}))
	defer server.Close()

	withMockBaseURLs(t, []string{server.URL})

	client := mustNewClient(t, "")
	_, err := client.FetchUserQuotaSummary(context.Background(), "test-token", 8)
	require.ErrorContains(t, err, "响应超过 8 字节")
}

func TestFetchUserQuotaSummary_NonOKStatus(t *testing.T) {
	const gatekeeperBody = `{"error":{"code":13,"message":"quota summary denied"}}`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(gatekeeperBody))
	}))
	defer server.Close()

	withMockBaseURLs(t, []string{server.URL})

	client := mustNewClient(t, "")
	_, err := client.FetchUserQuotaSummary(context.Background(), "test-token", defaultQuotaSummaryBodyLimit)
	require.ErrorContains(t, err, "retrieveUserQuotaSummary 失败 (HTTP 403)")
}

func TestFetchUserQuotaSummary_CanceledCtx(t *testing.T) {
	var hit bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hit = true // 已取消的 ctx 下请求不应到达；若到达，下方断言因 err==nil 判红
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"groups":[]}`))
	}))
	defer server.Close()

	withMockBaseURLs(t, []string{server.URL})

	client := mustNewClient(t, "")
	cctx, cancel := context.WithCancel(context.Background())
	cancel() // 已取消的 ctx ⇒ Client.Do 即刻返回错误，零 sleep

	_, err := client.FetchUserQuotaSummary(cctx, "test-token", defaultQuotaSummaryBodyLimit)
	require.Error(t, err)
	require.False(t, hit, "已取消的 ctx 下请求不应到达服务器")
}
