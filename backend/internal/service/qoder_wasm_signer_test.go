package service

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// TestQoderWasmSignerRuntimeAuthAndPrepareInfer 覆盖设计 §6 用例 3：真实 qoder_auth_wasm.wasm 实例化、RuntimeAuthFields 与 SignAndEncrypt。
func TestQoderWasmSignerRuntimeAuthAndPrepareInfer(t *testing.T) {
	signer := DefaultQoderWasmSigner()
	creds := &QoderCredentials{
		AccessToken:  "test-oauth-access-token-123456",
		RefreshToken: "test-refresh-token-123456",
		UID:          "user-qoder-001",
		OrgID:        "org-qoder-001",
	}
	EnsureDerivedFields(creds)

	rtFields, err := signer.RuntimeAuthFields(creds)
	if err != nil {
		t.Fatalf("RuntimeAuthFields failed: %v", err)
	}
	if strings.TrimSpace(rtFields.EncryptUserInfo) == "" || strings.TrimSpace(rtFields.Key) == "" {
		t.Fatalf("expected non-empty EncryptUserInfo and Key, got %+v", rtFields)
	}

	plaintextReq, err := json.Marshal(map[string]any{
		"request_id":   "req-unit-test-001",
		"session_id":   "sess-unit-test-001",
		"model":        "MModel",
		"model_config": map[string]any{"key": "MModel", "source": "system"},
		"messages":     []map[string]any{{"role": "user", "content": "hello qoder"}},
		"stream":       true,
	})
	if err != nil {
		t.Fatalf("marshal plaintext req: %v", err)
	}

	signed, err := signer.SignAndEncrypt(context.Background(), creds, plaintextReq, map[string]string{
		"X-Custom-Base": "ok",
	})
	if err != nil {
		t.Fatalf("SignAndEncrypt failed: %v", err)
	}
	if !strings.HasPrefix(signed.URL, "https://api1.qoder.sh/algo/api/v2/service/pro/sse/agent_chat_generation") {
		t.Fatalf("unexpected signed URL: %s", signed.URL)
	}
	if len(signed.CiphertextBody) == 0 || string(signed.CiphertextBody) == string(plaintextReq) {
		t.Fatalf("expected encrypted non-plaintext body, len=%d", len(signed.CiphertextBody))
	}
	if !strings.HasPrefix(signed.Headers["Authorization"], "Bearer COSY.") {
		t.Fatalf("expected Authorization header with Bearer COSY.<jwt>, got %q", signed.Headers["Authorization"])
	}
	for _, hdr := range []string{
		"Cosy-Date", "Cosy-ClientType", "Cosy-Key", "Cosy-MachineId",
		"Cosy-MachineToken", "Cosy-MachineType", "Cosy-RequestId",
		"Cosy-Version", "Cosy-User", "Cosy-Infer-Model-Key", "Cosy-Infer-Model-Source",
	} {
		if strings.TrimSpace(signed.Headers[hdr]) == "" {
			t.Fatalf("expected non-empty signed header %s, headers=%v", hdr, signed.Headers)
		}
	}
	if signed.Headers["Cosy-RequestId"] != "req-unit-test-001" {
		t.Fatalf("unexpected Cosy-RequestId: %s", signed.Headers["Cosy-RequestId"])
	}
	if signed.Headers["Cosy-MachineToken"] != creds.UMIDToken || signed.Headers["Cosy-MachineType"] != "win32" {
		t.Fatalf("unexpected machine headers: %v", signed.Headers)
	}
	if signed.Headers["Content-Type"] != "text/plain;charset=UTF-8" || signed.Headers["Accept"] != "text/event-stream" {
		t.Fatalf("unexpected content-type or accept headers: %v", signed.Headers)
	}
	if signed.Headers["X-Custom-Base"] != "ok" {
		t.Fatalf("expected base header preserved")
	}
}
