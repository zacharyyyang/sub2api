package service

import (
	"regexp"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/pkg/claude"
	"github.com/gin-gonic/gin"
)

const (
	// ContextWindow1MTokens is the 1M context window size declared in /v1/models
	// and /v1/models/:model responses for 1M-capable models.
	ContextWindow1MTokens = 1000000

	// ContextWindow1MSuffix is the model ID suffix recognized by Claude Code CLI
	// and Claude Code Desktop to unlock 1,000,000 context window locally.
	ContextWindow1MSuffix = "[1m]"

	// ClaudeFable5ShellPrefix is the primary shell prefix applied to non-Claude
	// models in model discovery responses so both Claude Code Desktop (i9t regex
	// `/^(?:claude-)?(?:fable|mythos)(?:-|$)/`) and Claude Code CLI
	// (`.includes("claude-fable-5")`) unlock the 5-level thinking effort selector
	// (low/medium/high/xhigh/max) and 1M context support.
	ClaudeFable5ShellPrefix = "claude-fable-5-"

	// ClaudeFable5EscapedShellPrefix is used when the wrapped non-Claude model ID
	// contains '_' or matches Claude Code Desktop's third-party model blocklist
	// regex (yCe), or would otherwise collide with native claude-fable-5 suffixes.
	ClaudeFable5EscapedShellPrefix = "claude-fable-5-_"

	// ClaudeFableShellPrefix and ClaudeMythosShellPrefix are also accepted on
	// inbound requests for compatibility with manual mappings.
	ClaudeFableShellPrefix         = "claude-fable-"
	ClaudeFableEscapedShellPrefix  = "claude-fable-_"
	ClaudeMythosShellPrefix        = "claude-mythos-"
	ClaudeMythosEscapedShellPrefix = "claude-mythos-_"
)

// claudeCodeDesktopBlockedTokenPattern mirrors Claude Code Desktop app-3.1623.0's
// yCe regex (`Mo(e)` / `No(e, t.id)`), which hides any /v1/models entry whose ID
// matches a known third-party vendor/model substring.
var claudeCodeDesktopBlockedTokenPattern = regexp.MustCompile(
	`(?i)ark-code|astron|command-r|deepseek|doubao|gemini|gemma|glm|gpt|grok|hermes|hy3|kimi|lfm|\bling\b|llama|longcat|mimo|minimax|mistral|mixtral|moonshot|nemotron|openai|phi-|qianfan|qwen|tc-code|\bunic\b|yi-|stepfun|step-3|seed-|bytedance|hunyuan|granite|amazon\.nova|nova-|devstral|ministral|ernie|codex|arcee|trinity|abab|phi\d|\bk2\.|\bm2\.|jamba|arctic|solar|mercury|zamba|kat-coder|\bds-|dpsk`,
)

// NormalizeInboundModelID strips any trailing "[1m]" suffix (case-insensitive,
// idempotent) and unwraps Claude Code Desktop shell prefixes
// ("claude-fable-5-_", "claude-fable-5-", "claude-fable-", "claude-mythos-")
// for non-native models, returning the canonical upstream/routing model ID and
// whether "[1m]" was requested.
func NormalizeInboundModelID(raw string) (canonical string, context1MRequested bool) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return "", false
	}

	var stripped bool
	trimmed, stripped = stripTrailing1MSuffixes(trimmed)
	if stripped {
		context1MRequested = true
	}

	if unwrapped, ok := unwrapCCDShellModelID(trimmed); ok {
		trimmed = unwrapped
		trimmed, stripped = stripTrailing1MSuffixes(trimmed)
		if stripped {
			context1MRequested = true
		}
	}

	return trimmed, context1MRequested
}

func stripTrailing1MSuffixes(modelID string) (string, bool) {
	trimmed := strings.TrimSpace(modelID)
	stripped := false
	for len(trimmed) >= len(ContextWindow1MSuffix) &&
		strings.EqualFold(trimmed[len(trimmed)-len(ContextWindow1MSuffix):], ContextWindow1MSuffix) {
		trimmed = strings.TrimSpace(trimmed[:len(trimmed)-len(ContextWindow1MSuffix)])
		stripped = true
	}
	return trimmed, stripped
}

func unwrapCCDShellModelID(modelID string) (string, bool) {
	trimmed := strings.TrimSpace(modelID)
	lower := strings.ToLower(trimmed)

	switch {
	case strings.HasPrefix(lower, ClaudeFable5EscapedShellPrefix):
		rest := trimmed[len(ClaudeFable5EscapedShellPrefix):]
		if rest == "" {
			return trimmed, false
		}
		return unescapeCCDShellModelID(rest), true

	case strings.HasPrefix(lower, ClaudeFable5ShellPrefix):
		if isNativeClaudeFableModel(trimmed) {
			return trimmed, false
		}
		rest := trimmed[len(ClaudeFable5ShellPrefix):]
		if rest == "" {
			return trimmed, false
		}
		return rest, true

	case strings.HasPrefix(lower, ClaudeFableEscapedShellPrefix):
		rest := trimmed[len(ClaudeFableEscapedShellPrefix):]
		if rest == "" {
			return trimmed, false
		}
		return unescapeCCDShellModelID(rest), true

	case strings.HasPrefix(lower, ClaudeFableShellPrefix):
		if isNativeClaudeFableModel(trimmed) {
			return trimmed, false
		}
		rest := trimmed[len(ClaudeFableShellPrefix):]
		if rest == "" {
			return trimmed, false
		}
		return rest, true

	case strings.HasPrefix(lower, ClaudeMythosEscapedShellPrefix):
		rest := trimmed[len(ClaudeMythosEscapedShellPrefix):]
		if rest == "" {
			return trimmed, false
		}
		return unescapeCCDShellModelID(rest), true

	case strings.HasPrefix(lower, ClaudeMythosShellPrefix):
		if isNativeClaudeMythosModel(trimmed) {
			return trimmed, false
		}
		rest := trimmed[len(ClaudeMythosShellPrefix):]
		if rest == "" {
			return trimmed, false
		}
		return rest, true
	}

	return trimmed, false
}

// escapeCCDShellModelID losslessly encodes a raw model ID so that:
//  1. Existing '_' characters are doubled to "__".
//  2. Every substring matching Claude Code Desktop's yCe blocklist has a single
//     '_' inserted after its first character (e.g., "gpt-5.4" -> "g_pt-5.4",
//     "kimi-k2.5" -> "k_imi-k_2.5").
func escapeCCDShellModelID(raw string) string {
	escaped := strings.ReplaceAll(raw, "_", "__")
	for step := 0; step < 32; step++ {
		loc := claudeCodeDesktopBlockedTokenPattern.FindStringIndex(escaped)
		if loc == nil || loc[1]-loc[0] < 2 {
			break
		}
		insertAt := loc[0] + 1
		escaped = escaped[:insertAt] + "_" + escaped[insertAt:]
	}
	return escaped
}

// unescapeCCDShellModelID reverses escapeCCDShellModelID: "__" restores to "_",
// and single "_" (inserted to break yCe matches) is removed.
func unescapeCCDShellModelID(escaped string) string {
	var b strings.Builder
	b.Grow(len(escaped))
	for i := 0; i < len(escaped); i++ {
		if escaped[i] == '_' {
			if i+1 < len(escaped) && escaped[i+1] == '_' {
				b.WriteByte('_')
				i++
			}
			continue
		}
		b.WriteByte(escaped[i])
	}
	return b.String()
}

// WrapModelWithCCDShell wraps a non-Claude model ID with the reversible
// Claude Code Desktop shell prefix ("claude-fable-5-<raw>" or
// "claude-fable-5-_<escaped>"). Native Claude models are returned unchanged.
func WrapModelWithCCDShell(modelID string) string {
	canonical, _ := NormalizeInboundModelID(modelID)
	if canonical == "" || IsNativeClaudeModelID(canonical) {
		return canonical
	}
	if strings.Contains(canonical, "_") ||
		claudeCodeDesktopBlockedTokenPattern.MatchString(canonical) ||
		looksLikeNativeClaudeModelSuffix(canonical) {
		return ClaudeFable5EscapedShellPrefix + escapeCCDShellModelID(canonical)
	}
	return ClaudeFable5ShellPrefix + canonical
}

// isNativeClaudeFableModel reports whether modelID (already stripped of "[1m]")
// is a native Anthropic Claude Fable model (e.g., "claude-fable-5",
// "claude-fable-5-1", "claude-fable-5-1-thinking", "claude-fable-5.1") rather
// than a shell-wrapped non-Claude model (e.g., "claude-fable-5-sonus-fable5.1",
// "claude-fable-5-_g_pt-5.4", "claude-fable-gpt-5.4").
func isNativeClaudeFableModel(modelID string) bool {
	lower := strings.ToLower(strings.TrimSpace(modelID))
	if !strings.HasPrefix(lower, ClaudeFableShellPrefix) {
		return false
	}
	rest := lower[len(ClaudeFableShellPrefix):]
	if rest == "5" || strings.HasPrefix(rest, "5.") {
		return true
	}
	if strings.HasPrefix(rest, "5-") {
		after5 := rest[len("5-"):]
		return looksLikeNativeClaudeModelSuffix(after5)
	}
	return false
}

func isNativeClaudeMythosModel(modelID string) bool {
	lower := strings.ToLower(strings.TrimSpace(modelID))
	if lower == "claude-mythos" {
		return true
	}
	if !strings.HasPrefix(lower, ClaudeMythosShellPrefix) {
		return false
	}
	after := lower[len(ClaudeMythosShellPrefix):]
	return looksLikeNativeClaudeModelSuffix(after)
}

func looksLikeNativeClaudeModelSuffix(suffix string) bool {
	lower := strings.ToLower(strings.TrimSpace(suffix))
	if lower == "" {
		return false
	}
	if lower[0] >= '0' && lower[0] <= '9' {
		return true
	}
	for _, mod := range []string{
		"thinking",
		"high",
		"medium",
		"low",
		"xhigh",
		"max",
		"fast",
		"latest",
		"preview",
	} {
		if lower == mod || strings.HasPrefix(lower, mod+"-") {
			return true
		}
	}
	return false
}

// IsNativeClaudeModelID reports whether modelID is a native Claude model ID
// (contains "claude" without being a shell-wrapped non-Claude model).
func IsNativeClaudeModelID(modelID string) bool {
	canonical, _ := NormalizeInboundModelID(modelID)
	lower := strings.ToLower(strings.TrimSpace(canonical))
	return strings.Contains(lower, "claude")
}

// stripAnthropicVendorDecoration normalizes Bedrock region/vendor prefixes and
// Vertex "@" version suffixes so Claude model family checks work uniformly.
func stripAnthropicVendorDecoration(modelID string) string {
	lower := strings.ToLower(strings.TrimSpace(modelID))
	// Region-qualified prefixes first, bare "anthropic." last.
	for _, prefix := range []string{
		"us-gov.anthropic.",
		"global.anthropic.",
		"apac.anthropic.",
		"us.anthropic.",
		"eu.anthropic.",
		"jp.anthropic.",
		"au.anthropic.",
		"anthropic.",
	} {
		if strings.HasPrefix(lower, prefix) {
			lower = strings.TrimPrefix(lower, prefix)
			break
		}
	}
	if idx := strings.IndexByte(lower, '@'); idx > 0 {
		lower = lower[:idx]
	}
	return lower
}

// IsContext1MCapableClaudeModel reports whether a Claude model supports the 1M
// context window.
func IsContext1MCapableClaudeModel(modelID string) bool {
	canonical, _ := NormalizeInboundModelID(modelID)
	lower := stripAnthropicVendorDecoration(canonical)
	if lower == "" || !strings.Contains(lower, "claude") {
		return false
	}
	// Explicit 200k variants and older/smaller families stay on 200K.
	if strings.Contains(lower, "-200k") ||
		strings.Contains(lower, "claude-haiku") ||
		strings.Contains(lower, "claude-3-") ||
		strings.Contains(lower, "claude-3.") ||
		strings.Contains(lower, "claude-2") ||
		strings.Contains(lower, "claude-instant") ||
		strings.Contains(lower, "claude-opus-4-5") ||
		strings.Contains(lower, "claude-opus-4-1") ||
		strings.Contains(lower, "claude-opus-4-0") ||
		strings.HasPrefix(lower, "claude-opus-4-2025") ||
		strings.Contains(lower, "claude-sonnet-4-5") ||
		strings.Contains(lower, "claude-sonnet-4-0") ||
		strings.HasPrefix(lower, "claude-sonnet-4-2025") {
		return false
	}

	for _, prefix := range []string{
		"claude-fable-5",
		"claude-mythos",
		"claude-opus-4-6",
		"claude-opus-4-7",
		"claude-opus-4-8",
		"claude-opus-5",
		"claude-sonnet-4-6",
		"claude-sonnet-5",
	} {
		if lower == prefix || strings.HasPrefix(lower, prefix+"-") || strings.HasPrefix(lower, prefix+".") {
			return true
		}
	}
	return false
}

// IsContext1MCapableModel reports whether any GenUR-supported model across all
// platforms supports a >=1M token context window.
func IsContext1MCapableModel(modelID string) bool {
	canonical, _ := NormalizeInboundModelID(modelID)
	lower := strings.ToLower(strings.TrimSpace(canonical))
	if lower == "" || strings.Contains(lower, "-200k") {
		return false
	}

	if strings.Contains(lower, "claude") {
		return IsContext1MCapableClaudeModel(lower)
	}

	// Gemini / Antigravity: gemini-2.5-*, gemini-3*, gemini-4*, gemini-live-* (excluding image models)
	if strings.HasPrefix(lower, "gemini-") {
		if strings.Contains(lower, "-image") {
			return false
		}
		return strings.HasPrefix(lower, "gemini-2.5-") ||
			strings.HasPrefix(lower, "gemini-3") ||
			strings.HasPrefix(lower, "gemini-4") ||
			strings.HasPrefix(lower, "gemini-live-")
	}

	// OpenAI: gpt-5.4*, gpt-5.5*, gpt-5.6*, gpt-6* (excluding image models)
	if strings.HasPrefix(lower, "gpt-") {
		if strings.Contains(lower, "-image") {
			return false
		}
		return strings.HasPrefix(lower, "gpt-5.4") ||
			strings.HasPrefix(lower, "gpt-5.5") ||
			strings.HasPrefix(lower, "gpt-5.6") ||
			strings.HasPrefix(lower, "gpt-6")
	}

	// xAI Grok: grok-4* (excluding grok-imagine-*)
	if strings.HasPrefix(lower, "grok-") {
		if strings.HasPrefix(lower, "grok-imagine") {
			return false
		}
		return strings.HasPrefix(lower, "grok-4")
	}

	// Qoder / MiMo / DashScope / Wanbiao 1M models
	switch lower {
	case "sonus-fable5.1", "smodel",
		"cantus-opus5.5", "cmodel",
		"ultimate-gpt6.1sol", "ultimate",
		"qwen3.6-plus", "qwen3.5-plus",
		"mimo-v2-pro", "mimo-v2-omni":
		return true
	}
	for _, prefix := range []string{
		"qwen3.8-",
		"qwen3.7-",
		"qwen3.6-plus-",
		"qwen3.5-plus-",
		"mimo-v2.6",
		"kimi-k2.5",
		"kimi-k2.6",
		"glm-5",
	} {
		if strings.HasPrefix(lower, prefix) {
			return true
		}
	}

	return false
}

// FormatModelForDiscovery transforms a canonical model ID and display name into
// the Claude Code Desktop discovery representation:
//   - Non-Claude models are wrapped with WrapModelWithCCDShell so CCD's fail-all
//     and yCe filters accept them and both CCD and CLI enable the 5-level
//     effort picker. The shell is applied only when exitPlatform is "claude";
//     any other value, including empty or unknown, leaves the ID unwrapped.
//   - Models with >=1M context window receive the "[1m]" suffix on their ID,
//     " 1M" on their display name (if non-empty and not already suffixed), and
//     context_window = 1000000.
func FormatModelForDiscovery(modelID, displayName, exitPlatform string) (outID, outDisplayName string, contextWindow int) {
	canonical, _ := NormalizeInboundModelID(modelID)
	if canonical == "" {
		return modelID, displayName, 0
	}

	if exitPlatform == "claude" {
		outID = WrapModelWithCCDShell(canonical)
	} else {
		outID = canonical
	}
	outDisplayName = strings.TrimSpace(displayName)
	if outDisplayName == "" {
		outDisplayName = canonical
	}
	if IsContext1MCapableModel(canonical) {
		outID += ContextWindow1MSuffix
		contextWindow = ContextWindow1MTokens
		if !has1MDisplayLabel(outDisplayName) {
			outDisplayName += " 1M"
		}
	}
	return outID, outDisplayName, contextWindow
}

func has1MDisplayLabel(displayName string) bool {
	upper := strings.ToUpper(strings.TrimSpace(displayName))
	return strings.HasSuffix(upper, " 1M") ||
		strings.HasSuffix(upper, "(1M)") ||
		strings.HasSuffix(upper, "[1M]")
}

// EnsureRequestContext1MBetaHeader appends claude.BetaContext1M to the inbound
// request's anthropic-beta header when the client requested a "[1m]" model ID
// for a 1M-capable Claude model and has not already sent the beta token.
func EnsureRequestContext1MBetaHeader(c *gin.Context, modelID string, context1MRequested bool) {
	if !context1MRequested || c == nil || c.Request == nil {
		return
	}
	if !IsContext1MCapableClaudeModel(modelID) {
		return
	}
	existing := getHeaderRaw(c.Request.Header, "anthropic-beta")
	if containsBetaToken(existing, claude.BetaContext1M) {
		return
	}
	if strings.TrimSpace(existing) == "" {
		setHeaderRaw(c.Request.Header, "anthropic-beta", claude.BetaContext1M)
		return
	}
	setHeaderRaw(c.Request.Header, "anthropic-beta", existing+","+claude.BetaContext1M)
}
