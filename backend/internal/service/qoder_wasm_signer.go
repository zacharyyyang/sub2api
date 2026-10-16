package service

import (
	"context"
	_ "embed"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
)

//go:embed qoder_auth_wasm.wasm
var qoderAuthWasmBytes []byte

const (
	qoderAppVersion      = "0.4.7"
	qoderDefaultEndpoint = "https://api1.qoder.sh"
	qoderClientMetadata  = `{"client_type":"5","business_product":"cli","business_type":"agent","scene":"assistant"}`
)

type QoderRuntimeAuthFields struct {
	EncryptUserInfo string `json:"encrypt_user_info"`
	Key             string `json:"key"`
}

// QoderSignedRequest 表示 WASM 签名加密后的上游请求载荷与头部。
type QoderSignedRequest struct {
	URL            string
	Headers        map[string]string
	CiphertextBody []byte
	Body           []byte
}
type QoderWasmSigner struct {
	mu       sync.Mutex
	rt       wazero.Runtime
	env      *qoderWasmEnv
	compiled wazero.CompiledModule
}

var (
	defaultQoderSignerOnce sync.Once
	defaultQoderSignerInst *QoderWasmSigner
)

// DefaultQoderWasmSigner 返回进程级共享的 QoderWasmSigner 单例。
func DefaultQoderWasmSigner() *QoderWasmSigner {
	defaultQoderSignerOnce.Do(func() { defaultQoderSignerInst = NewQoderWasmSigner() })
	return defaultQoderSignerInst
}

// NewQoderWasmSigner 创建新的 QoderWasmSigner。
func NewQoderWasmSigner() *QoderWasmSigner { return &QoderWasmSigner{} }

func (s *QoderWasmSigner) instantiateCallModule(ctx context.Context) (api.Module, error) {
	if s.compiled == nil {
		initCtx := context.Background()
		rt := wazero.NewRuntime(initCtx)
		env := newQoderWasmEnv()
		if _, err := instantiateQoderBgModule(initCtx, rt, env); err != nil {
			_ = rt.Close(initCtx)
			return nil, fmt.Errorf("qoder wasm host module init failed: %w", err)
		}
		compiled, err := rt.CompileModule(initCtx, qoderAuthWasmBytes)
		if err != nil {
			_ = rt.Close(initCtx)
			return nil, fmt.Errorf("qoder wasm compile failed: %w", err)
		}
		s.rt, s.env, s.compiled = rt, env, compiled
	}
	s.env.reset()
	mod, err := s.rt.InstantiateModule(ctx, s.compiled, wazero.NewModuleConfig().WithName(""))
	if err != nil {
		return nil, fmt.Errorf("qoder wasm instantiate failed: %w", err)
	}
	s.env.mod = mod
	return mod, nil
}
func (s *QoderWasmSigner) RuntimeAuthFields(creds *QoderCredentials) (*QoderRuntimeAuthFields, error) {
	if creds == nil {
		return nil, errors.New("qoder credentials are nil")
	}
	EnsureDerivedFields(creds)
	ctx := context.Background()
	s.mu.Lock()
	defer s.mu.Unlock()
	mod, err := s.instantiateCallModule(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = mod.Close(ctx) }()
	return s.callRuntimeAuthFields(ctx, mod, creds)
}

func (s *QoderWasmSigner) callRuntimeAuthFields(ctx context.Context, mod api.Module, creds *QoderCredentials) (out *QoderRuntimeAuthFields, err error) {
	defer func() {
		if r := recover(); r != nil {
			if s.env != nil && s.env.lastError != nil {
				err = s.env.lastError
			} else {
				err = fmt.Errorf("qoder wasm panic: %v", r)
			}
		}
	}()
	rawJSON, _ := json.Marshal(map[string]any{
		"uid": creds.UID, "organization_id": creds.OrgID, "organization_tags": []any{},
		"data_policy_agreed": true, "security_oauth_token": creds.AccessToken,
	})
	spFn := mod.ExportedFunction("__wbindgen_add_to_stack_pointer")
	retRes, err := spFn.Call(ctx, api.EncodeI32(-16))
	if err != nil {
		return nil, err
	}
	retPtr := uint32(retRes[0])
	var dPtr, dLen uint32
	defer func() {
		_, _ = spFn.Call(ctx, api.EncodeI32(16))
		if dPtr != 0 {
			_, _ = mod.ExportedFunction("__wbindgen_export4").Call(ctx, uint64(dPtr), uint64(dLen), 1)
		}
	}()
	p0, l0, err := s.env.passStr(ctx, string(rawJSON))
	if err != nil {
		return nil, err
	}
	if _, err := mod.ExportedFunction("generate_runtime_auth_fields").Call(ctx, uint64(retPtr), uint64(p0), uint64(l0)); err != nil {
		return nil, err
	}
	r0, r1, r2, r3 := readWasmRetQuad(mod, retPtr)
	if r3 != 0 {
		return nil, fmt.Errorf("generate_runtime_auth_fields failed: %v", s.env.takeObj(uint32(r2)))
	}
	dPtr, dLen = uint32(r0), uint32(r1)
	var fields QoderRuntimeAuthFields
	if err := json.Unmarshal([]byte(s.env.readStr(dPtr, dLen)), &fields); err != nil {
		return nil, fmt.Errorf("parse runtime auth fields failed: %w", err)
	}
	return &fields, nil
}
func (s *QoderWasmSigner) SignAndEncrypt(ctx context.Context, creds *QoderCredentials, plaintextBody []byte, baseHeaders map[string]string) (*QoderSignedRequest, error) {
	if creds == nil {
		return nil, errors.New("qoder credentials are nil")
	}
	EnsureDerivedFields(creds)
	if ctx == nil {
		ctx = context.Background()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	mod, err := s.instantiateCallModule(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = mod.Close(ctx) }()

	rtFields, err := s.callRuntimeAuthFields(ctx, mod, creds)
	if err != nil {
		return nil, err
	}
	userInfoJSON, _ := json.Marshal(map[string]any{
		"uid": creds.UID, "organization_id": creds.OrgID, "organization_tags": []any{},
		"data_policy_agreed": true, "encrypt_user_info": rtFields.EncryptUserInfo, "key": rtFields.Key,
	})
	ctxPtr, err := s.callFourStringExport(ctx, mod, "qodercontext_new", nil, creds.MachineID, qoderAppVersion, string(userInfoJSON), qoderClientMetadata)
	if err != nil {
		return nil, err
	}
	defer func() { _, _ = mod.ExportedFunction("__wbg_qodercontext_free").Call(ctx, uint64(ctxPtr), 0) }()

	reqID, modelKey, modelSource := extractInferMeta(plaintextBody)
	resPtr, err := s.callFourStringExport(ctx, mod, "qodercontext_prepareInferRequest", &ctxPtr, qoderDefaultEndpoint, string(plaintextBody), modelKey, modelSource)
	if err != nil {
		return nil, err
	}
	defer func() { _, _ = mod.ExportedFunction("__wbg_requestresult_free").Call(ctx, uint64(resPtr), 0) }()
	cipherText := s.callStrGetter(ctx, mod, "requestresult_body", resPtr)
	inferURL := s.callStrGetter(ctx, mod, "requestresult_url", resPtr)
	hdrRes, err := mod.ExportedFunction("requestresult_headers").Call(ctx, uint64(resPtr))
	if err != nil {
		return nil, err
	}
	inferHeaders, _ := s.env.takeObj(uint32(hdrRes[0])).(map[string]string)
	headers := mergeQoderSignedHeaders(creds, rtFields, reqID, inferHeaders, baseHeaders)
	cipherBytes := []byte(cipherText)
	return &QoderSignedRequest{URL: inferURL, Headers: headers, CiphertextBody: cipherBytes, Body: cipherBytes}, nil
}

func (s *QoderWasmSigner) callFourStringExport(ctx context.Context, mod api.Module, exportName string, extraArg *uint32, a, b, c, d string) (uint32, error) {
	spFn := mod.ExportedFunction("__wbindgen_add_to_stack_pointer")
	retRes, err := spFn.Call(ctx, api.EncodeI32(-16))
	if err != nil {
		return 0, err
	}
	retPtr := uint32(retRes[0])
	defer func() { _, _ = spFn.Call(ctx, api.EncodeI32(16)) }()

	var args [8]uint64
	for i, str := range []string{a, b, c, d} {
		ptr, length, err := s.env.passStr(ctx, str)
		if err != nil {
			return 0, err
		}
		args[i*2], args[i*2+1] = uint64(ptr), uint64(length)
	}
	callArgs := []uint64{uint64(retPtr), args[0], args[1], args[2], args[3], args[4], args[5], args[6], args[7]}
	if extraArg != nil {
		callArgs = []uint64{uint64(retPtr), uint64(*extraArg), args[0], args[1], args[2], args[3], args[4], args[5], args[6], args[7]}
	}
	if _, err := mod.ExportedFunction(exportName).Call(ctx, callArgs...); err != nil {
		return 0, err
	}
	r0, r1, r2, _ := readWasmRetQuad(mod, retPtr)
	if r2 != 0 {
		return 0, fmt.Errorf("%s failed: %v", exportName, s.env.takeObj(uint32(r1)))
	}
	return uint32(r0), nil
}

func (s *QoderWasmSigner) callStrGetter(ctx context.Context, mod api.Module, exportName string, resPtr uint32) string {
	spFn := mod.ExportedFunction("__wbindgen_add_to_stack_pointer")
	retRes, err := spFn.Call(ctx, api.EncodeI32(-16))
	if err != nil {
		return ""
	}
	retPtr := uint32(retRes[0])
	defer func() { _, _ = spFn.Call(ctx, api.EncodeI32(16)) }()
	_, _ = mod.ExportedFunction(exportName).Call(ctx, uint64(retPtr), uint64(resPtr))
	ptr, length, _, _ := readWasmRetQuad(mod, retPtr)
	if ptr == 0 {
		return ""
	}
	val := s.env.readStr(uint32(ptr), uint32(length))
	_, _ = mod.ExportedFunction("__wbindgen_export4").Call(ctx, uint64(uint32(ptr)), uint64(uint32(length)), 1)
	return val
}

func readWasmRetQuad(mod api.Module, retPtr uint32) (int32, int32, int32, int32) {
	if buf, ok := mod.Memory().Read(retPtr, 16); ok && len(buf) >= 16 {
		return int32(binary.LittleEndian.Uint32(buf[0:4])), int32(binary.LittleEndian.Uint32(buf[4:8])), int32(binary.LittleEndian.Uint32(buf[8:12])), int32(binary.LittleEndian.Uint32(buf[12:16]))
	}
	return 0, 0, 0, 0
}
func extractInferMeta(plaintextBody []byte) (reqID, modelKey, modelSource string) {
	modelKey, modelSource = "auto", "system"
	var p struct {
		RequestID   string            `json:"request_id"`
		Model       string            `json:"model"`
		ModelConfig map[string]string `json:"model_config"`
	}
	if json.Unmarshal(plaintextBody, &p) == nil {
		reqID = p.RequestID
		if k := p.ModelConfig["key"]; k != "" {
			modelKey = k
		} else if p.Model != "" {
			modelKey = p.Model
		}
		if src := p.ModelConfig["source"]; src != "" {
			modelSource = src
		}
	}
	return reqID, modelKey, modelSource
}

func mergeQoderSignedHeaders(creds *QoderCredentials, rtFields *QoderRuntimeAuthFields, reqID string, inferHeaders, baseHeaders map[string]string) map[string]string {
	headers := make(map[string]string, len(baseHeaders)+len(inferHeaders)+12)
	for k, v := range baseHeaders {
		headers[k] = v
	}
	headers["Cosy-Date"], headers["Cosy-RequestId"] = time.Now().UTC().Format(http.TimeFormat), reqID
	headers["Cosy-Version"], headers["X-Requested-With"] = qoderAppVersion, "XMLHttpRequest"
	headers["Cosy-User"], headers["Cosy-Key"] = rtFields.EncryptUserInfo, rtFields.Key
	for k, v := range inferHeaders {
		headers[k] = v
	}
	if mk := inferHeaders["X-Model-Key"]; mk != "" {
		headers["Cosy-Infer-Model-Key"] = mk
	}
	if ms := inferHeaders["X-Model-Source"]; ms != "" {
		headers["Cosy-Infer-Model-Source"] = ms
	}
	if headers["Authorization"] == "" {
		headers["Authorization"] = "Bearer " + creds.AccessToken
	}
	headers["Cosy-MachineToken"], headers["Cosy-MachineType"] = creds.UMIDToken, creds.MachineType
	headers["Content-Type"], headers["Accept"] = "text/plain;charset=UTF-8", "text/event-stream"
	return headers
}
