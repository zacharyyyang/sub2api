package service

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

var lastMimoSetErrorCalls atomic.Int32

func (r *fakeMimoAccountRepo) SetError(_ context.Context, _ int64, _ string) error {
	lastMimoSetErrorCalls.Add(1)
	return nil
}

func newTestMimoGateway(t *testing.T, handler func(req *http.Request) (*http.Response, error), refreshHandler func(req *http.Request) (*http.Response, error)) (*OpenAIGatewayService, *Account, *fakeMimoAccountRepo) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	acc := &Account{
		ID:       301,
		Platform: PlatformMimo,
		Status:   StatusActive,
		Credentials: map[string]any{
			"user_id":       "100000001",
			"pass_token":    "V1:fake_pass_token==",
			"c_user_id":     "cuid_301",
			"service_token": "st_initial_301",
			"mimo_ph":       "ph_301",
		},
	}
	repo := &fakeMimoAccountRepo{accounts: map[int64]*Account{301: acc}}
	tp := NewMimoTokenProvider(repo, &http.Client{Transport: qoderRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		if refreshHandler != nil {
			return refreshHandler(req)
		}
		if req.URL.Path == "/pass/serviceLogin" {
			body := `&&&START&&&{"code":0,"location":"https://mimo.test/sts?sid=mimopc","nonce":123456,"ssecurity":"dGVzdF9zc2VjdXJpdHlfa2V5","cUserId":"cuid_refreshed_301"}`
			return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
		}
		hdr := make(http.Header)
		hdr.Add("Set-Cookie", "serviceToken=st_refreshed_301; Path=/")
		hdr.Add("Set-Cookie", "cUserId=cuid_refreshed_301; Path=/")
		hdr.Add("Set-Cookie", "mimo_ph=ph_refreshed_301; Path=/")
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader("ok")), Header: hdr}, nil
	})}, "https://mimo.test/pass/serviceLogin")
	tp.sleepFn = func(time.Duration) {}
	svc := &OpenAIGatewayService{
		accountRepo:       repo,
		httpUpstream:      &fakeQoderUpstream{handler: handler},
		mimoTokenProvider: tp,
	}
	return svc, acc, repo
}

// TestMimoChatCompletionsForwardAnd401Replay 覆盖设计 §6 用例 5：
// ① mimo-v2.6-pro-thinking 流式/非流式转发、模型名还原与思考档位映射；
// ② 未开流首次 401，SSO 换票后重放 200；③ 未开流 401 重放后仍返回 401 时停止重放并标错。
func TestMimoChatCompletionsForwardAnd401Replay(t *testing.T) {
	var capturedBody []byte
	var capturedHeader http.Header
	svc, acc, _ := newTestMimoGateway(t, func(req *http.Request) (*http.Response, error) {
		capturedHeader = req.Header.Clone()
		capturedBody, _ = io.ReadAll(req.Body)
		if req.Header.Get("Accept") == "text/event-stream" {
			sse := "data: {\"id\":\"c1\",\"model\":\"mimo-v2.6-pro\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"Hi\"}}]}\n\n" +
				"data: {\"id\":\"c1\",\"model\":\"mimo-v2.6-pro\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":12,\"completion_tokens\":6,\"prompt_tokens_details\":{\"cached_tokens\":4}}}\n\n" +
				"data: [DONE]\n\n"
			return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(sse)), Header: make(http.Header)}, nil
		}
		respJSON := `{"id":"c0","model":"mimo-v2.6-pro","choices":[{"index":0,"message":{"role":"assistant","content":"Hello"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":5}}`
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(respJSON)), Header: make(http.Header)}, nil
	}, nil)

	w1 := httptest.NewRecorder()
	c1, _ := gin.CreateTestContext(w1)
	res1, err := svc.forwardMimoChatCompletions(context.Background(), c1, acc, []byte(`{"model":"mimo-v2.6-pro-thinking","stream":false,"messages":[{"role":"user","content":"hi"}]}`))
	if err != nil || w1.Code != http.StatusOK || res1.UpstreamModel != "mimo-v2.6-pro" || res1.ReasoningEffort == nil || *res1.ReasoningEffort != "high" {
		t.Fatalf("non-stream failed: code=%d res=%+v err=%v", w1.Code, res1, err)
	}
	if !strings.Contains(string(capturedBody), `"model":"mimo-v2.6-pro"`) || !strings.Contains(string(capturedBody), `"reasoning_effort":"high"`) || capturedHeader.Get("x-mimo-source") != "mimocode-cli" || !strings.Contains(capturedHeader.Get("Cookie"), "serviceToken=st_refreshed_301") {
		t.Fatalf("unexpected upstream request: body=%s header=%v", string(capturedBody), capturedHeader)
	}

	w2 := httptest.NewRecorder()
	c2, _ := gin.CreateTestContext(w2)
	res2, err := svc.forwardMimoChatCompletions(context.Background(), c2, acc, []byte(`{"model":"mimo-v2.6-pro-thinking","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	if err != nil || res2.Usage.InputTokens != 12 || res2.Usage.OutputTokens != 6 || res2.Usage.CacheReadInputTokens != 4 || !strings.Contains(w2.Body.String(), `"model":"mimo-v2.6-pro-thinking"`) {
		t.Fatalf("stream failed: res=%+v body=%s err=%v", res2, w2.Body.String(), err)
	}

	var attempts, ssoSeq int32
	svc401, acc401, repo401 := newTestMimoGateway(t, func(req *http.Request) (*http.Response, error) {
		if atomic.AddInt32(&attempts, 1) == 1 {
			return &http.Response{StatusCode: http.StatusUnauthorized, Body: io.NopCloser(strings.NewReader(`{"message":"expired"}`)), Header: make(http.Header)}, nil
		}
		if !strings.Contains(req.Header.Get("Cookie"), "serviceToken=st_replayed_301") {
			t.Fatalf("expected refreshed cookie on replay, got %s", req.Header.Get("Cookie"))
		}
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`{"id":"c2","choices":[{"message":{"content":"ok"}}],"usage":{"prompt_tokens":3,"completion_tokens":1}}`)), Header: make(http.Header)}, nil
	}, func(req *http.Request) (*http.Response, error) {
		if req.URL.Path == "/pass/serviceLogin" {
			body := `&&&START&&&{"code":0,"location":"https://mimo.test/sts?sid=mimopc","nonce":123456,"ssecurity":"dGVzdF9zc2VjdXJpdHlfa2V5","cUserId":"cuid_301"}`
			return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
		}
		st := map[bool]string{true: "st_replayed_301", false: "st_first_301"}[atomic.AddInt32(&ssoSeq, 1) > 1]
		hdr := make(http.Header)
		hdr.Add("Set-Cookie", "serviceToken="+st+"; Path=/")
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader("ok")), Header: hdr}, nil
	})
	w3 := httptest.NewRecorder()
	c3, _ := gin.CreateTestContext(w3)
	if _, err := svc401.forwardMimoChatCompletions(context.Background(), c3, acc401, []byte(`{"model":"mimo-v2.6-pro","messages":[{"role":"user","content":"hi"}]}`)); err != nil || w3.Code != http.StatusOK || atomic.LoadInt32(&attempts) != 2 || repo401.updates != 2 {
		t.Fatalf("expected 401 replay 200, got code=%d attempts=%d updates=%d err=%v", w3.Code, attempts, repo401.updates, err)
	}

	var still401Calls int32
	lastMimoSetErrorCalls.Store(0)
	svcFail, accFail, _ := newTestMimoGateway(t, func(_ *http.Request) (*http.Response, error) {
		atomic.AddInt32(&still401Calls, 1)
		return &http.Response{StatusCode: http.StatusUnauthorized, Body: io.NopCloser(strings.NewReader(`{"message":"still unauthorized"}`)), Header: make(http.Header)}, nil
	}, nil)
	w4 := httptest.NewRecorder()
	c4, _ := gin.CreateTestContext(w4)
	if _, err4 := svcFail.forwardMimoChatCompletions(context.Background(), c4, accFail, []byte(`{"model":"mimo-v2.6-pro","messages":[{"role":"user","content":"hi"}]}`)); err4 == nil || w4.Code != http.StatusUnauthorized || atomic.LoadInt32(&still401Calls) != 2 || accFail.Status != StatusError || lastMimoSetErrorCalls.Load() != 1 {
		t.Fatalf("expected 401 stop after 2 calls and StatusError, got code=%d calls=%d status=%s setErr=%d err=%v", w4.Code, still401Calls, accFail.Status, lastMimoSetErrorCalls.Load(), err4)
	}
}

// TestMimoAnthropicMessagesAdapterAndModality422 覆盖设计 §6 用例 6：
// ① 入站含 system、历史 thinking、tool_use/tool_result 转发还原；② omni 传图放行；③ 非视觉传图返 422。
func TestMimoAnthropicMessagesAdapterAndModality422(t *testing.T) {
	var upstreamPayload map[string]any
	svc, acc, _ := newTestMimoGateway(t, func(req *http.Request) (*http.Response, error) {
		raw, _ := io.ReadAll(req.Body)
		_ = json.Unmarshal(raw, &upstreamPayload)
		resp := `{"id":"msg_1","choices":[{"finish_reason":"tool_calls","message":{"role":"assistant","reasoning_content":"think1","content":"ans1","tool_calls":[{"id":"call_1","type":"function","function":{"name":"bash","arguments":"{\"cmd\":\"ls\"}"}}]}}],"usage":{"prompt_tokens":20,"completion_tokens":10,"prompt_tokens_details":{"cached_tokens":5}}}`
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(resp)), Header: make(http.Header)}, nil
	}, nil)

	w1 := httptest.NewRecorder()
	c1, _ := gin.CreateTestContext(w1)
	req1 := []byte(`{"model":"mimo-v2.6-pro","system":"sys prompt","thinking":{"type":"enabled","budget_tokens":16000},"messages":[{"role":"user","content":"u1"},{"role":"assistant","content":[{"type":"thinking","thinking":"old"},{"type":"tool_use","id":"call_0","name":"read","input":{"path":"a.go"}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"call_0","content":"pkg main"}]}]}`)
	res1, err := svc.forwardMimoAnthropicMessages(context.Background(), c1, acc, req1, "")
	if err != nil || w1.Code != http.StatusOK || res1.Usage.InputTokens != 20 || res1.Usage.CacheReadInputTokens != 5 {
		t.Fatalf("forwardMimoAnthropicMessages failed: code=%d res=%+v err=%v", w1.Code, res1, err)
	}
	if upstreamPayload["reasoning_effort"] != "high" || !strings.Contains(w1.Body.String(), `"type":"thinking"`) || !strings.Contains(w1.Body.String(), `"type":"tool_use"`) || !strings.Contains(w1.Body.String(), `"stop_reason":"tool_use"`) {
		t.Fatalf("unexpected upstream payload or response: payload=%v resp=%s", upstreamPayload, w1.Body.String())
	}

	w2 := httptest.NewRecorder()
	c2, _ := gin.CreateTestContext(w2)
	omniImgReq := []byte(`{"model":"mimo-v2-omni","messages":[{"role":"user","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"AAAA"}},{"type":"text","text":"look"}]}]}`)
	if _, err := svc.forwardMimoAnthropicMessages(context.Background(), c2, acc, omniImgReq, ""); err != nil || w2.Code != http.StatusOK {
		t.Fatalf("expected omni image allowed, got code=%d err=%v", w2.Code, err)
	}
	if b, _ := json.Marshal(upstreamPayload["messages"]); !strings.Contains(string(b), "image_url") {
		t.Fatalf("expected image_url in omni messages, got %s", string(b))
	}

	for _, badBody := range []string{
		`{"model":"mimo-v2.6-pro","messages":[{"role":"user","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"AAAA"}}]}]}`,
		`{"model":"mimo-v2.6-pro","messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":[{"type":"image","source":{"type":"url","url":"https://a.com/x.png"}}]}]}]}`,
	} {
		wBad := httptest.NewRecorder()
		cBad, _ := gin.CreateTestContext(wBad)
		if _, errBad := svc.forwardMimoAnthropicMessages(context.Background(), cBad, acc, []byte(badBody), ""); errBad == nil || wBad.Code != http.StatusUnprocessableEntity || !strings.Contains(wBad.Body.String(), "MODEL_MODALITY_UNSUPPORTED") {
			t.Fatalf("expected 422 MODEL_MODALITY_UNSUPPORTED, got code=%d body=%s err=%v", wBad.Code, wBad.Body.String(), errBad)
		}
	}
}

// TestMimoAnthropicMessagesSSEConverter 覆盖设计 §6 用例 7：流式 SSE 顺序与 usage 汇总。
func TestMimoAnthropicMessagesSSEConverter(t *testing.T) {
	chunks := []string{
		`data: {"id":"chatcmpl-99","choices":[{"index":0,"delta":{"reasoning_content":"let me think"}}]}`,
		`data: {"id":"chatcmpl-99","choices":[{"index":0,"delta":{"content":"here is tool"}}]}`,
		`data: {"id":"chatcmpl-99","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_abc","type":"function","function":{"name":"read_file","arguments":"{\"path\":"}}]}}]}`,
		`data: {"id":"chatcmpl-99","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"main.go\"}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":50,"completion_tokens":15,"prompt_tokens_details":{"cached_tokens":10}}}`,
		`data: [DONE]`,
	}
	svc, acc, _ := newTestMimoGateway(t, func(_ *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(strings.Join(chunks, "\n\n") + "\n\n")), Header: make(http.Header)}, nil
	}, nil)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	res, err := svc.forwardMimoAnthropicMessages(context.Background(), c, acc, []byte(`{"model":"mimo-v2.6-pro","stream":true,"messages":[{"role":"user","content":"go"}]}`), "")
	if err != nil || w.Code != http.StatusOK || res.Usage.InputTokens != 50 || res.Usage.OutputTokens != 15 || res.Usage.CacheReadInputTokens != 10 {
		t.Fatalf("stream forwardMimoAnthropicMessages failed: code=%d res=%+v err=%v", w.Code, res, err)
	}
	out, lastPos := w.Body.String(), -1
	for _, token := range []string{"event: message_start", `"type":"thinking"`, `"thinking_delta"`, `"type":"text"`, `"text_delta"`, `"name":"read_file"`, `"type":"tool_use"`, `"input_json_delta"`, "event: message_delta", `"stop_reason":"tool_use"`, `"input_tokens":40`, "event: message_stop"} {
		idx := strings.Index(out, token)
		if idx < 0 || idx < lastPos {
			t.Fatalf("missing or out-of-order token %q in SSE output:\n%s", token, out)
		}
		lastPos = idx
	}
}
