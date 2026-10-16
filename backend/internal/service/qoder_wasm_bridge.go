package service

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"time"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
)

const qoderWasmBgModuleName = "./qoder_auth_wasm_bg.js"

type wasmSentinel int

const (
	wasmJSUndefined wasmSentinel = iota
	wasmJSNull
)

type wasmGlobalObj struct{}
type wasmCryptoObj struct{}
type wasmUint8Array struct {
	data []byte
}
type wasmMemSlice struct {
	ptr uint32
	len uint32
}

// qoderWasmEnv 承载单个 WASM 签名会话的堆对象表（heap）与绑定模块引用。
type qoderWasmEnv struct {
	mod       api.Module
	heap      []any
	heapNext  int
	lastError error
}

func newQoderWasmEnv() *qoderWasmEnv {
	e := &qoderWasmEnv{}
	e.reset()
	return e
}

func (e *qoderWasmEnv) reset() {
	heap := make([]any, 1028)
	for i := 0; i < 1024; i++ {
		heap[i] = wasmJSUndefined
	}
	heap[1024] = wasmJSUndefined
	heap[1025] = wasmJSNull
	heap[1026] = true
	heap[1027] = false
	e.heap = heap
	e.heapNext = 1028
	e.lastError = nil
	e.mod = nil
}
func (e *qoderWasmEnv) addHeap(obj any) uint32 {
	if obj == nil {
		obj = wasmJSUndefined
	}
	if e.heapNext == len(e.heap) {
		e.heap = append(e.heap, len(e.heap)+1)
	}
	idx := e.heapNext
	if nextIdx, ok := e.heap[idx].(int); ok {
		e.heapNext = nextIdx
	} else {
		e.heapNext = len(e.heap)
	}
	e.heap[idx] = obj
	return uint32(idx)
}

func (e *qoderWasmEnv) getObj(idx uint32) any {
	if int(idx) < 0 || int(idx) >= len(e.heap) {
		return wasmJSUndefined
	}
	return e.heap[idx]
}

func (e *qoderWasmEnv) dropObj(idx uint32) {
	if idx >= 1028 && int(idx) < len(e.heap) {
		e.heap[idx] = e.heapNext
		e.heapNext = int(idx)
	}
}

func (e *qoderWasmEnv) takeObj(idx uint32) any {
	ret := e.getObj(idx)
	e.dropObj(idx)
	return ret
}

func (e *qoderWasmEnv) readBytes(ptr, length uint32) []byte {
	if length == 0 || e.mod == nil {
		return nil
	}
	b, ok := e.mod.Memory().Read(ptr, length)
	if !ok {
		return nil
	}
	out := make([]byte, len(b))
	copy(out, b)
	return out
}

func (e *qoderWasmEnv) readStr(ptr, length uint32) string {
	return string(e.readBytes(ptr, length))
}

func (e *qoderWasmEnv) passStr(ctx context.Context, s string) (uint32, uint32, error) {
	b := []byte(s)
	if len(b) == 0 {
		malloc := e.mod.ExportedFunction("__wbindgen_export2")
		res, err := malloc.Call(ctx, 0, 1)
		if err != nil {
			return 0, 0, err
		}
		return uint32(res[0]), 0, nil
	}
	malloc := e.mod.ExportedFunction("__wbindgen_export2")
	res, err := malloc.Call(ctx, uint64(len(b)), 1)
	if err != nil {
		return 0, 0, err
	}
	ptr := uint32(res[0])
	if !e.mod.Memory().Write(ptr, b) {
		return 0, 0, errors.New("wasm memory write out of bounds")
	}
	return ptr, uint32(len(b)), nil
}

func instantiateQoderBgModule(ctx context.Context, rt wazero.Runtime, env *qoderWasmEnv) (api.Module, error) {
	builder := rt.NewHostModuleBuilder(qoderWasmBgModuleName)
	env.registerWasmTypeAndRefImports(builder)
	env.registerWasmGlobalAndEnvImports(builder)
	env.registerWasmCryptoImports(builder)
	env.registerWasmUint8ArrayImports(builder)
	env.registerWasmMapAndErrorImports(builder)
	return builder.Instantiate(ctx)
}

func (e *qoderWasmEnv) registerWasmTypeAndRefImports(b wazero.HostModuleBuilder) {
	b.NewFunctionBuilder().WithFunc(func(_ context.Context, a uint32) uint32 {
		return 0
	}).Export("__wbg___wbindgen_is_function_49868bde5eb1e745")
	b.NewFunctionBuilder().WithFunc(func(_ context.Context, a uint32) uint32 {
		switch e.getObj(a).(type) {
		case wasmGlobalObj, wasmCryptoObj, *wasmUint8Array, wasmMemSlice, map[string]string:
			return 1
		default:
			return 0
		}
	}).Export("__wbg___wbindgen_is_object_40c5a80572e8f9d3")
	b.NewFunctionBuilder().WithFunc(func(_ context.Context, a uint32) uint32 {
		if _, ok := e.getObj(a).(string); ok {
			return 1
		}
		return 0
	}).Export("__wbg___wbindgen_is_string_b29b5c5a8065ba1a")
	b.NewFunctionBuilder().WithFunc(func(_ context.Context, a uint32) uint32 {
		if e.getObj(a) == wasmJSUndefined {
			return 1
		}
		return 0
	}).Export("__wbg___wbindgen_is_undefined_c0cca72b82b86f4d")
	b.NewFunctionBuilder().WithFunc(func(_ context.Context, a uint32) uint32 {
		return e.addHeap(e.getObj(a))
	}).Export("__wbindgen_object_clone_ref")
	b.NewFunctionBuilder().WithFunc(func(_ context.Context, a uint32) {
		e.dropObj(a)
	}).Export("__wbindgen_object_drop_ref")
	b.NewFunctionBuilder().WithFunc(func(_ context.Context, ptr, length uint32) uint32 {
		return e.addHeap(wasmMemSlice{ptr: ptr, len: length})
	}).Export("__wbindgen_cast_0000000000000001")
	b.NewFunctionBuilder().WithFunc(func(_ context.Context, ptr, length uint32) uint32 {
		return e.addHeap(e.readStr(ptr, length))
	}).Export("__wbindgen_cast_0000000000000002")
}

func (e *qoderWasmEnv) registerWasmGlobalAndEnvImports(b wazero.HostModuleBuilder) {
	globalFn := func(_ context.Context) uint32 { return e.addHeap(wasmGlobalObj{}) }
	b.NewFunctionBuilder().WithFunc(globalFn).Export("__wbg_static_accessor_GLOBAL_THIS_a1248013d790bf5f")
	b.NewFunctionBuilder().WithFunc(globalFn).Export("__wbg_static_accessor_GLOBAL_f2e0f995a21329ff")
	b.NewFunctionBuilder().WithFunc(globalFn).Export("__wbg_static_accessor_SELF_24f78b6d23f286ea")
	b.NewFunctionBuilder().WithFunc(globalFn).Export("__wbg_static_accessor_WINDOW_59fd959c540fe405")
	b.NewFunctionBuilder().WithFunc(func(_ context.Context, _ uint32) uint32 {
		return e.addHeap(wasmCryptoObj{})
	}).Export("__wbg_crypto_38df2bab126b63dc")
	undef1 := func(_ context.Context, _ uint32) uint32 { return e.addHeap(wasmJSUndefined) }
	b.NewFunctionBuilder().WithFunc(undef1).Export("__wbg_msCrypto_bd5a034af96bcba6")
	b.NewFunctionBuilder().WithFunc(undef1).Export("__wbg_process_44c7a14e11e9f69e")
	b.NewFunctionBuilder().WithFunc(undef1).Export("__wbg_versions_276b2795b1c6a219")
	b.NewFunctionBuilder().WithFunc(undef1).Export("__wbg_node_84ea875411254db1")
	b.NewFunctionBuilder().WithFunc(func(_ context.Context) uint32 {
		return e.addHeap(wasmJSUndefined)
	}).Export("__wbg_require_b4edbdcf3e2a1ef0")
	b.NewFunctionBuilder().WithFunc(func(_ context.Context, _, _, _ uint32) uint32 {
		return e.addHeap(wasmJSUndefined)
	}).Export("__wbg_call_d578befcc3145dee")
}

func (e *qoderWasmEnv) registerWasmCryptoImports(b wazero.HostModuleBuilder) {
	b.NewFunctionBuilder().WithFunc(func(_ context.Context, _, arrIdx uint32) {
		switch target := e.getObj(arrIdx).(type) {
		case *wasmUint8Array:
			_, _ = rand.Read(target.data)
		case wasmMemSlice:
			buf := make([]byte, target.len)
			_, _ = rand.Read(buf)
			if e.mod != nil {
				_ = e.mod.Memory().Write(target.ptr, buf)
			}
		}
	}).Export("__wbg_getRandomValues_c44a50d8cfdaebeb")
	b.NewFunctionBuilder().WithFunc(func(_ context.Context, ptr, length uint32) {
		if length == 0 || e.mod == nil {
			return
		}
		buf := make([]byte, length)
		_, _ = rand.Read(buf)
		_ = e.mod.Memory().Write(ptr, buf)
	}).Export("__wbg_getRandomValues_d49329ff89a07af1")
	b.NewFunctionBuilder().WithFunc(func(_ context.Context, _, arrIdx uint32) {
		if u8, ok := e.takeObj(arrIdx).(*wasmUint8Array); ok {
			_, _ = rand.Read(u8.data)
		}
	}).Export("__wbg_randomFillSync_6c25eac9869eb53c")
}

func (e *qoderWasmEnv) registerWasmUint8ArrayImports(b wazero.HostModuleBuilder) {
	b.NewFunctionBuilder().WithFunc(func(_ context.Context, length uint32) uint32 {
		return e.addHeap(&wasmUint8Array{data: make([]byte, length)})
	}).Export("__wbg_new_with_length_9cedd08484b73942")
	b.NewFunctionBuilder().WithFunc(func(_ context.Context, idx uint32) uint32 {
		switch v := e.getObj(idx).(type) {
		case *wasmUint8Array:
			return uint32(len(v.data))
		case wasmMemSlice:
			return v.len
		default:
			return 0
		}
	}).Export("__wbg_length_0c32cb8543c8e4c8")
	b.NewFunctionBuilder().WithFunc(func(_ context.Context, idx, start, end uint32) uint32 {
		if u8, ok := e.getObj(idx).(*wasmUint8Array); ok {
			s, en := max(0, int(start)), min(len(u8.data), int(end))
			if s > en {
				s = en
			}
			return e.addHeap(&wasmUint8Array{data: u8.data[s:en]})
		}
		return e.addHeap(&wasmUint8Array{})
	}).Export("__wbg_subarray_0f98d3fb634508ad")
	b.NewFunctionBuilder().WithFunc(func(_ context.Context, ptr, length, srcIdx uint32) {
		if e.mod == nil || length == 0 {
			return
		}
		if u8, ok := e.getObj(srcIdx).(*wasmUint8Array); ok {
			n := int(length)
			if len(u8.data) < n {
				n = len(u8.data)
			}
			_ = e.mod.Memory().Write(ptr, u8.data[:n])
		}
	}).Export("__wbg_prototypesetcall_3e05eb9545565046")
}

func (e *qoderWasmEnv) registerWasmMapAndErrorImports(b wazero.HostModuleBuilder) {
	b.NewFunctionBuilder().WithFunc(func(_ context.Context) uint32 {
		return e.addHeap(make(map[string]string))
	}).Export("__wbg_new_99cabae501c0a8a0")
	b.NewFunctionBuilder().WithFunc(func(_ context.Context, mapIdx, kIdx, vIdx uint32) uint32 {
		if m, ok := e.getObj(mapIdx).(map[string]string); ok {
			k := fmt.Sprint(e.getObj(kIdx))
			v := fmt.Sprint(e.getObj(vIdx))
			m[k] = v
		}
		return e.addHeap(e.getObj(mapIdx))
	}).Export("__wbg_set_08463b1df38a7e29")
	b.NewFunctionBuilder().WithFunc(func(_ context.Context) float64 {
		return float64(time.Now().UnixMilli())
	}).Export("__wbg_now_88621c9c9a4f3ffc")
	b.NewFunctionBuilder().WithFunc(func(_ context.Context, ptr, length uint32) uint32 {
		return e.addHeap(errors.New(e.readStr(ptr, length)))
	}).Export("__wbg_Error_2e59b1b37a9a34c3")
	b.NewFunctionBuilder().WithFunc(func(_ context.Context, ptr, length uint32) {
		msg := e.readStr(ptr, length)
		e.lastError = errors.New(msg)
		panic("qoder_wasm_throw: " + msg)
	}).Export("__wbg___wbindgen_throw_81fc77679af83bc6")
}
