package service

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/domain"
	"github.com/Wei-Shaw/sub2api/internal/model"
)

func TestMimoCredentialsAndRedaction(t *testing.T) {
	if domain.PlatformMimo != "mimo" || PlatformMimo != "mimo" || model.PlatformMimo != "mimo" {
		t.Fatalf("unexpected PlatformMimo: %s / %s / %s", domain.PlatformMimo, PlatformMimo, model.PlatformMimo)
	}
	if !domain.IsGroupPlatform(domain.PlatformMimo) || !domain.IsGroupPlatform(domain.PlatformQoder) {
		t.Fatalf("expected IsGroupPlatform(mimo/qoder) to be true")
	}
	allPlatforms := model.AllPlatforms()
	foundMimo := false
	for _, p := range allPlatforms {
		if p == model.PlatformMimo {
			foundMimo = true
			break
		}
	}
	if !foundMimo {
		t.Fatalf("expected model.AllPlatforms() to contain mimo, got %v", allPlatforms)
	}
	if len(domain.MimoModelWhitelist) != 6 {
		t.Fatalf("expected 6 models in MimoModelWhitelist, got %d", len(domain.MimoModelWhitelist))
	}
	if u, eff, err := domain.NormalizeMimoModel("mimo/mimo-v2.6-pro-thinking[1m]"); err != nil || u != "mimo-v2.6-pro" || eff != "high" {
		t.Fatalf("NormalizeMimoModel(-thinking) = %s, %s, %v", u, eff, err)
	}
	if u, eff, err := domain.NormalizeMimoModel("mimo-v2.6-pro:medium"); err != nil || u != "mimo-v2.6-pro" || eff != "medium" {
		t.Fatalf("NormalizeMimoModel(:medium) = %s, %s, %v", u, eff, err)
	}
	if _, _, err := domain.NormalizeMimoModel("mimo-v2.5"); err == nil {
		t.Fatalf("expected unsupported mimo model for mimo-v2.5")
	}
	if !domain.IsMimoVisionModel("mimo-v2-omni") || domain.IsMimoVisionModel("mimo-v2.6-pro") {
		t.Fatalf("unexpected IsMimoVisionModel results")
	}

	if err := ValidateMimoRequiredCredentials(map[string]any{"user_id": "100000001"}); err == nil {
		t.Fatalf("expected error when both pass_token and service_token are empty")
	}
	if err := ValidateMimoRequiredCredentials(map[string]any{"pass_token": "V1:fake=="}); err == nil {
		t.Fatalf("expected error when user_id is missing")
	}
	raw := map[string]any{
		"user_id":       "100000001",
		"pass_token":    "V1:fake_pass_token==",
		"c_user_id":     "fake_c_user_id_001",
		"service_token": "fake_service_token_001",
		"mimo_ph":       "fake_mimo_ph_001",
	}
	creds, err := NormalizeMimoCredentials(raw)
	if err != nil {
		t.Fatalf("NormalizeMimoCredentials failed: %v", err)
	}
	if creds.BaseURL != DefaultMimoBaseURL || !strings.HasPrefix(creds.DeviceID, "wb_") || len(creds.DeviceID) != 39 {
		t.Fatalf("unexpected derived fields: %+v", creds)
	}
	wantCookie := "userId=100000001; cUserId=fake_c_user_id_001; serviceToken=fake_service_token_001; mimo_ph=fake_mimo_ph_001"
	if gotCookie := BuildMimoInferenceCookie(*creds); gotCookie != wantCookie {
		t.Fatalf("BuildMimoInferenceCookie = %q, want %q", gotCookie, wantCookie)
	}
	redacted, status := RedactCredentials(raw)
	for _, k := range []string{"pass_token", "service_token", "mimo_ph"} {
		if _, exists := redacted[k]; exists {
			t.Fatalf("expected %s stripped by RedactCredentials", k)
		}
		if !status["has_"+k] {
			t.Fatalf("expected has_%s=true in status", k)
		}
	}
	merged := MergePreservingSensitiveCreds(raw, map[string]any{"user_id": "100000002"})
	if merged["pass_token"] != "V1:fake_pass_token==" || merged["service_token"] != "fake_service_token_001" || merged["mimo_ph"] != "fake_mimo_ph_001" {
		t.Fatalf("MergePreservingSensitiveCreds lost sensitive keys: %v", merged)
	}
}

func TestMimoMessagesAdapterPureConversion(t *testing.T) {
	reqBody := []byte(`{
		"model": "mimo-v2.6-pro",
		"system": [{"type": "text", "text": "sys prompt"}],
		"thinking": {"type": "enabled", "budget_tokens": 16000},
		"messages": [
			{"role": "user", "content": "hello"},
			{"role": "assistant", "content": [
				{"type": "thinking", "thinking": "old thought"},
				{"type": "redacted_thinking", "data": "secret"},
				{"type": "text", "text": "calling tool"},
				{"type": "tool_use", "id": "toolu_1", "name": "get_weather", "input": {"city": "BJ"}}
			]},
			{"role": "user", "content": [
				{"type": "tool_result", "tool_use_id": "toolu_1", "content": "sunny"},
				{"type": "text", "text": "thanks"}
			]}
		],
		"tools": [{"name": "get_weather", "description": "desc", "input_schema": {"type": "object"}}],
		"tool_choice": {"type": "any"},
		"max_tokens": 1024
	}`)
	chatBytes, err := AnthropicMessagesToMimoChat(reqBody, "mimo-v2.6-pro", "")
	if err != nil {
		t.Fatalf("AnthropicMessagesToMimoChat failed: %v", err)
	}
	var chatMap map[string]any
	if err := json.Unmarshal(chatBytes, &chatMap); err != nil {
		t.Fatalf("unmarshal chatBytes: %v", err)
	}
	if chatMap["reasoning_effort"] != "high" || chatMap["tool_choice"] != "required" {
		t.Fatalf("unexpected reasoning_effort or tool_choice: %v", chatMap)
	}
	msgs, _ := chatMap["messages"].([]any)
	if len(msgs) != 5 {
		t.Fatalf("expected 5 messages (system, user, assistant, tool, user), got %d: %v", len(msgs), msgs)
	}

	// Non-vision model with image block -> 422
	imgReq := []byte(`{"model":"mimo-v2.6-pro","messages":[{"role":"user","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"AAAA"}}]}]}`)
	if _, err := AnthropicMessagesToMimoChat(imgReq, "mimo-v2.6-pro", ""); err == nil {
		t.Fatalf("expected 422 error for image on non-vision model")
	} else if compatErr, ok := err.(*MimoMessagesCompatError); !ok || compatErr.Status != 422 {
		t.Fatalf("expected MimoMessagesCompatError 422, got %v", err)
	}
	// Vision model mimo-v2-omni with image block (including inside tool_result) -> allowed
	if _, err := AnthropicMessagesToMimoChat(imgReq, "mimo-v2-omni", ""); err != nil {
		t.Fatalf("expected mimo-v2-omni to allow image block, got %v", err)
	}
	toolImgReq := []byte(`{"model":"mimo-v2-omni","messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":[{"type":"text","text":"ok"},{"type":"image","source":{"type":"url","url":"https://example.com/a.png"}}]}]}]}`)
	if b, err := AnthropicMessagesToMimoChat(toolImgReq, "mimo-v2-omni", ""); err != nil || !strings.Contains(string(b), "image_url") {
		t.Fatalf("expected mimo-v2-omni tool_result image converted to image_url, got %s, %v", string(b), err)
	}

	// Non-stream response conversion
	rawResp := []byte(`{
		"id": "chatcmpl-123",
		"choices": [{
			"finish_reason": "tool_calls",
			"message": {
				"role": "assistant",
				"reasoning_content": "step by step",
				"content": "let me check",
				"tool_calls": [{"id": "call_9", "type": "function", "function": {"name": "get_weather", "arguments": "{\"city\":\"SH\"}"}}]
			}
		}],
		"usage": {"prompt_tokens": 100, "completion_tokens": 40, "prompt_tokens_details": {"cached_tokens": 20}}
	}`)
	anthBytes, usage, err := MimoChatToAnthropicMessage(rawResp, "mimo-v2.6-pro")
	if err != nil {
		t.Fatalf("MimoChatToAnthropicMessage failed: %v", err)
	}
	if usage.InputTokens != 100 || usage.OutputTokens != 40 || usage.CacheReadInputTokens != 20 {
		t.Fatalf("unexpected usage: %+v", usage)
	}
	var anthMap map[string]any
	_ = json.Unmarshal(anthBytes, &anthMap)
	contentBlocks, _ := anthMap["content"].([]any)
	anthUsage, _ := anthMap["usage"].(map[string]any)
	if anthMap["stop_reason"] != "tool_use" || len(contentBlocks) != 3 || anthUsage["input_tokens"] != float64(80) || anthUsage["cache_read_input_tokens"] != float64(20) {
		t.Fatalf("unexpected anthropic message: %v", anthMap)
	}
}

func TestResolveMimoPricing(t *testing.T) {
	bs := NewBillingService(nil, nil)
	proModels := []string{
		"mimo-v2.6-pro", "mimo-v2-pro", "mimo-v2-omni",
		"mimo-v2.6-pro-thinking", "mimo-v2-pro:high", "mimo/mimo-v2-omni",
	}
	for _, m := range proModels {
		p := resolveMimoPricing(m, bs.fallbackPrices)
		if p == nil {
			p = bs.getCNAndExtendedFallbackPricing(m)
		}
		if p == nil || p.InputPricePerToken != 0.435e-6 || p.OutputPricePerToken != 0.87e-6 || p.LongContextInputThreshold != 256000 || p.LongContextInputMultiplier != 2.0 {
			t.Fatalf("unexpected pro pricing for %s: %+v", m, p)
		}
	}
	flashModels := []string{"mimo-v2.6-flash", "mimo-v2-flash", "mimo-v2-flash-thinking", "mimo-v2.6-flash:low"}
	for _, m := range flashModels {
		p := resolveMimoPricing(m, bs.fallbackPrices)
		if p == nil || p.InputPricePerToken != 0.14e-6 || p.OutputPricePerToken != 0.28e-6 {
			t.Fatalf("unexpected flash pricing for %s: %+v", m, p)
		}
	}
	ultraModels := []string{"mimo-v2.6-pro-ultraspeed", "mimo-v2.6-pro-ultraspeed-thinking", "mimo-v2-pro-ultraspeed"}
	for _, m := range ultraModels {
		p := resolveMimoPricing(m, bs.fallbackPrices)
		if p == nil || p.InputPricePerToken != 4.35e-6 || p.OutputPricePerToken != 8.70e-6 {
			t.Fatalf("unexpected ultraspeed pricing for %s: %+v", m, p)
		}
	}
}
