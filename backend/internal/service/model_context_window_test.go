package service

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/claude"
	"github.com/gin-gonic/gin"
)

func TestModelContextWindowFormatShellByExitPlatform(t *testing.T) {
	const input = "sonus-fable5.1"

	outID, _, _ := FormatModelForDiscovery(input, "Sonus Fable 5.1", "claude")
	if !strings.HasPrefix(outID, ClaudeFable5ShellPrefix) {
		t.Fatalf("claude exit: expected shell prefix %q, got %q", ClaudeFable5ShellPrefix, outID)
	}

	for _, exitPlatform := range []string{"openai", "xai", "gemini", "", "unknown"} {
		outID, _, _ := FormatModelForDiscovery(input, "Sonus Fable 5.1", exitPlatform)
		if strings.HasPrefix(outID, ClaudeFableShellPrefix) {
			t.Fatalf("%q exit: expected no shell prefix, got %q", exitPlatform, outID)
		}
		if !strings.HasPrefix(outID, input) {
			t.Fatalf("%q exit: expected bare canonical ID %q, got %q", exitPlatform, input, outID)
		}
	}
}

func TestModelContextWindowFormat1MSuffix(t *testing.T) {
	cases := []struct {
		name       string
		modelID    string
		display    string
		exit       string
		wantID     string
		wantName   string
		wantWindow int
	}{
		{
			name:       "native claude 1M capable",
			modelID:    "claude-opus-4-6",
			display:    "Claude Opus 4.6",
			exit:       "claude",
			wantID:     "claude-opus-4-6[1m]",
			wantName:   "Claude Opus 4.6 1M",
			wantWindow: ContextWindow1MTokens,
		},
		{
			name:       "shelled 1M capable",
			modelID:    "sonus-fable5.1",
			display:    "Sonus Fable 5.1",
			exit:       "claude",
			wantID:     "claude-fable-5-sonus-fable5.1[1m]",
			wantName:   "Sonus Fable 5.1 1M",
			wantWindow: ContextWindow1MTokens,
		},
		{
			name:       "1M openai exit keeps 1M without shell",
			modelID:    "gpt-5.4",
			display:    "GPT-5.4",
			exit:       "openai",
			wantID:     "gpt-5.4[1m]",
			wantName:   "GPT-5.4 1M",
			wantWindow: ContextWindow1MTokens,
		},
		{
			name:       "1M xai exit keeps 1M without shell",
			modelID:    "grok-4.20",
			display:    "Grok 4.20",
			exit:       "xai",
			wantID:     "grok-4.20[1m]",
			wantName:   "Grok 4.20 1M",
			wantWindow: ContextWindow1MTokens,
		},
		{
			name:       "1M unknown exit keeps 1M without shell",
			modelID:    "qwen3.8-max",
			display:    "Qwen 3.8 Max",
			exit:       "",
			wantID:     "qwen3.8-max[1m]",
			wantName:   "Qwen 3.8 Max 1M",
			wantWindow: ContextWindow1MTokens,
		},
		{
			name:       "empty display name falls back to canonical",
			modelID:    "claude-opus-4-6",
			display:    "   ",
			exit:       "claude",
			wantID:     "claude-opus-4-6[1m]",
			wantName:   "claude-opus-4-6 1M",
			wantWindow: ContextWindow1MTokens,
		},
		{
			name:       "empty display name falls back to canonical when not 1M",
			modelID:    "gpt-5.2",
			display:    "",
			exit:       "openai",
			wantID:     "gpt-5.2",
			wantName:   "gpt-5.2",
			wantWindow: 0,
		},
		{
			name:       "non 1M claude",
			modelID:    "claude-haiku-4-5-20251001",
			display:    "Claude Haiku 4.5",
			exit:       "claude",
			wantID:     "claude-haiku-4-5-20251001",
			wantName:   "Claude Haiku 4.5",
			wantWindow: 0,
		},
		{
			name:       "non 1M openai",
			modelID:    "gpt-5.2",
			display:    "GPT-5.2",
			exit:       "openai",
			wantID:     "gpt-5.2",
			wantName:   "GPT-5.2",
			wantWindow: 0,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			outID, outName, window := FormatModelForDiscovery(tc.modelID, tc.display, tc.exit)
			if outID != tc.wantID {
				t.Fatalf("id: want %q, got %q", tc.wantID, outID)
			}
			if outName != tc.wantName {
				t.Fatalf("display name: want %q, got %q", tc.wantName, outName)
			}
			if window != tc.wantWindow {
				t.Fatalf("context window: want %d, got %d", tc.wantWindow, window)
			}
			if tc.wantWindow == 0 && strings.Contains(outID, ContextWindow1MSuffix) {
				t.Fatalf("expected no %q suffix, got %q", ContextWindow1MSuffix, outID)
			}
		})
	}
}

func TestModelContextWindowFormatNativeClaudeNoShell(t *testing.T) {
	for _, modelID := range []string{"claude-opus-4-6", "claude-sonnet-4-6"} {
		outID, _, _ := FormatModelForDiscovery(modelID, modelID, "claude")
		if strings.HasPrefix(outID, ClaudeFableShellPrefix) {
			t.Fatalf("native claude must not be shelled: got %q", outID)
		}
	}
}

func TestModelContextWindowFormatIdempotent(t *testing.T) {
	cases := []struct {
		modelID string
		exit    string
	}{
		{"claude-opus-4-6", "claude"},
		{"sonus-fable5.1", "claude"},
		{"gpt-5.4", "claude"},
		{"kimi_k2.5", "claude"},
		{"qwen3.8-max", "openai"},
	}

	for _, tc := range cases {
		id1, name1, window1 := FormatModelForDiscovery(tc.modelID, "Label", tc.exit)
		id2, name2, window2 := FormatModelForDiscovery(tc.modelID, "Label", tc.exit)
		if id1 != id2 || name1 != name2 || window1 != window2 {
			t.Fatalf("same input produced different output: (%q,%q,%d) vs (%q,%q,%d)",
				id1, name1, window1, id2, name2, window2)
		}

		id3, name3, window3 := FormatModelForDiscovery(id1, name1, tc.exit)
		if id3 != id1 || name3 != name1 || window3 != window1 {
			t.Fatalf("re-decorating decorated output changed it: (%q,%q,%d) -> (%q,%q,%d)",
				id1, name1, window1, id3, name3, window3)
		}
	}
}

func TestModelContextWindowStripVendorPrefixes(t *testing.T) {
	const bare = "claude-opus-4-6-v1"

	stripped := []string{
		"us-gov.anthropic.",
		"global.anthropic.",
		"apac.anthropic.",
		"us.anthropic.",
		"eu.anthropic.",
		"jp.anthropic.",
		"au.anthropic.",
		"anthropic.",
	}
	for _, prefix := range stripped {
		if got := stripAnthropicVendorDecoration(prefix + bare); got != bare {
			t.Fatalf("prefix %q: want %q, got %q", prefix, bare, got)
		}
	}

	notStripped := "ap.anthropic." + bare
	if got := stripAnthropicVendorDecoration(notStripped); got != notStripped {
		t.Fatalf("prefix %q must not be stripped: want %q, got %q", "ap.anthropic.", notStripped, got)
	}
}

func TestModelContextWindowFormatEscapedShell(t *testing.T) {
	// "gpt-5.4" matches the Desktop blocklist, so the claude exit must use the
	// escaped shell form instead of the plain prefix.
	outID, _, _ := FormatModelForDiscovery("gpt-5.4", "GPT-5.4", "claude")
	if !strings.HasPrefix(outID, ClaudeFable5EscapedShellPrefix) {
		t.Fatalf("expected escaped shell prefix %q, got %q", ClaudeFable5EscapedShellPrefix, outID)
	}
	if strings.HasPrefix(outID, ClaudeFable5ShellPrefix+"gpt") {
		t.Fatalf("blocklisted model must not use the plain shell form: %q", outID)
	}
	if canonical, oneM := NormalizeInboundModelID(outID); canonical != "gpt-5.4" || !oneM {
		t.Fatalf("escaped shell must round-trip to (%q,true), got (%q,%v)", "gpt-5.4", canonical, oneM)
	}

	// An already-shelled ID entering a non-Claude exit is unwrapped to the bare
	// canonical name, keeping the 1M suffix.
	shelled := "claude-fable-5-sonus-fable5.1"
	bare, _, window := FormatModelForDiscovery(shelled, "Sonus Fable 5.1", "openai")
	if bare != "sonus-fable5.1[1m]" {
		t.Fatalf("non-claude exit must unwrap the shell: want %q, got %q", "sonus-fable5.1[1m]", bare)
	}
	if window != ContextWindow1MTokens {
		t.Fatalf("context window: want %d, got %d", ContextWindow1MTokens, window)
	}
}

func TestModelContextWindowBedrockVariant1MCapable(t *testing.T) {
	for _, modelID := range []string{
		"apac.anthropic.claude-opus-4-6-v1[1m]",
		"us-gov.anthropic.claude-opus-4-6-v1[1m]",
		"jp.anthropic.claude-opus-4-6-v1[1m]",
		"au.anthropic.claude-opus-4-6-v1[1m]",
	} {
		if !IsContext1MCapableClaudeModel(modelID) {
			t.Fatalf("expected 1M-capable Bedrock variant: %q", modelID)
		}
	}
}

func TestModelContextWindowEnsure1MBetaHeaderAndUpstreamBeta(t *testing.T) {
	gin.SetMode(gin.TestMode)
	svc := &GatewayService{settingService: NewSettingService(&antigravitySettingRepoStub{}, nil)}
	body := []byte(`{"messages":[{"role":"user","content":"hi"}]}`)
	models := []string{
		"claude-opus-4-6[1m]",
		"claude-fable-5-1[1m]",
		"apac.anthropic.claude-opus-4-6-v1[1m]",
	}
	accounts := []*Account{
		{Platform: PlatformAnthropic, Type: AccountTypeOAuth},
		{Platform: PlatformAnthropic, Type: AccountTypeAPIKey},
	}

	for _, rawModel := range models {
		canonical, oneM := NormalizeInboundModelID(rawModel)
		if !oneM {
			t.Fatalf("expected 1M requested for %q", rawModel)
		}
		for _, acc := range accounts {
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
			EnsureRequestContext1MBetaHeader(c, canonical, oneM)
			EnsureRequestContext1MBetaHeader(c, canonical, oneM) // idempotent

			inHeader := c.GetHeader("anthropic-beta")
			policy := svc.evaluateBetaPolicy(context.Background(), inHeader, acc, canonical)
			if policy.blockErr != nil {
				t.Fatalf("%s (%s): unexpected block: %v", rawModel, acc.Type, policy.blockErr)
			}
			dropSet := mergeDropSets(policy.filterSet)
			tokenType := "apikey"
			if acc.IsOAuth() {
				tokenType = "oauth"
			}
			msgBeta, ok := svc.computeFinalAnthropicBeta(tokenType, true, canonical, c.Request.Header, body, dropSet)
			if !ok || strings.Count(msgBeta, claude.BetaContext1M) != 1 {
				t.Fatalf("%s (%s) messages beta: got %q (ok=%v)", rawModel, acc.Type, msgBeta, ok)
			}
			ctBeta, ok := svc.computeFinalCountTokensAnthropicBeta(tokenType, true, canonical, c.Request.Header, body, dropSet)
			if !ok || strings.Count(ctBeta, claude.BetaContext1M) != 1 {
				t.Fatalf("%s (%s) count_tokens beta: got %q (ok=%v)", rawModel, acc.Type, ctBeta, ok)
			}
		}
	}
}
