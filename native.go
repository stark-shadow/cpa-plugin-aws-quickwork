//go:build cgo

package main

/*
#include <stdint.h>
#include <stdlib.h>

typedef struct { void *ptr; size_t len; } qw_buffer;
typedef int (*qw_host_call)(void *, const char *, const uint8_t *, size_t, qw_buffer *);
typedef void (*qw_free)(void *, size_t);
typedef struct {
    uint32_t abi_version;
    void *host_ctx;
    qw_host_call call;
    qw_free free_buffer;
} qw_host;
typedef struct {
    uint32_t abi_version;
    int (*call)(char *, uint8_t *, size_t, qw_buffer *);
    qw_free free_buffer;
    void (*shutdown)(void);
} qw_plugin;

extern int quickWorkCall(char *, uint8_t *, size_t, qw_buffer *);
extern void quickWorkFree(void *, size_t);
extern void quickWorkShutdown(void);

static inline void configure_plugin(qw_plugin *out, uint32_t abi) {
    out->abi_version = abi;
    out->call = quickWorkCall;
    out->free_buffer = quickWorkFree;
    out->shutdown = quickWorkShutdown;
}
static inline int call_host(qw_host h, const char *method, const uint8_t *data, size_t len, qw_buffer *out) {
    return h.call(h.host_ctx, method, data, len, out);
}
static inline void free_host(qw_host h, qw_buffer b) { h.free_buffer(b.ptr, b.len); }
*/
import "C"

import (
	"encoding/json"
	"errors"
	"sync"
	"unsafe"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

var (
	nativeHost C.qw_host
	nativeMu   sync.RWMutex
	instance   engine
)

//export cliproxy_plugin_init
func cliproxy_plugin_init(host *C.qw_host, out *C.qw_plugin) C.int {
	if host == nil || out == nil || uint32(host.abi_version) != pluginabi.ABIVersion || host.call == nil || host.free_buffer == nil || host.host_ctx == nil {
		return 1
	}
	nativeMu.Lock()
	nativeHost = *host
	nativeMu.Unlock()
	C.configure_plugin(out, C.uint32_t(pluginabi.ABIVersion))
	return 0
}

func callback(method string, value any) bool {
	_, err := callHost(method, value)
	return err == nil
}

func hostRequest(callbackID string, req pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
	var response pluginapi.HTTPResponse
	result, err := callHost(pluginabi.MethodHostHTTPDo, map[string]any{"host_callback_id": callbackID, "request": req})
	if err != nil {
		return response, err
	}
	err = json.Unmarshal(result, &response)
	return response, err
}

func callHost(method string, value any) (json.RawMessage, error) {
	nativeMu.RLock()
	host := nativeHost
	nativeMu.RUnlock()
	if host.call == nil {
		return nil, errors.New("host callback unavailable")
	}
	data, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	name := C.CString(method)
	defer C.free(unsafe.Pointer(name))
	body := C.CBytes(data)
	defer C.free(body)
	var out C.qw_buffer
	code := C.call_host(host, name, (*C.uint8_t)(body), C.size_t(len(data)), &out)
	if out.ptr != nil {
		defer C.free_host(host, out)
	}
	if code != 0 || out.ptr == nil || out.len > maxRPC {
		return nil, errors.New("host callback failed")
	}
	var response pluginabi.Envelope
	if err := json.Unmarshal(C.GoBytes(out.ptr, C.int(out.len)), &response); err != nil || !response.OK {
		return nil, errors.New("host rejected callback")
	}
	return response.Result, nil
}

//export quickWorkCall
func quickWorkCall(method *C.char, raw *C.uint8_t, size C.size_t, out *C.qw_buffer) (code C.int) {
	if out == nil {
		return 1
	}
	out.ptr, out.len = nil, 0
	defer func() {
		if recover() != nil {
			if out.ptr != nil {
				C.free(out.ptr)
			}
			encoded, _ := pluginabi.NewErrorEnvelope("internal_error", "QuickWork plugin internal error", 500)
			out.ptr, out.len = C.CBytes(encoded), C.size_t(len(encoded))
			code = 0
		}
	}()
	var encoded []byte
	if method == nil || size > maxRPC || (size > 0 && raw == nil) {
		encoded, _ = pluginabi.NewErrorEnvelope("invalid_request", "Invalid QuickWork RPC input", 400)
	} else {
		encoded = envelope(instance.call(C.GoString(method), C.GoBytes(unsafe.Pointer(raw), C.int(size)), callback, hostRequest))
	}
	out.ptr, out.len = C.CBytes(encoded), C.size_t(len(encoded))
	return 0
}

//export quickWorkFree
func quickWorkFree(ptr unsafe.Pointer, _ C.size_t) { C.free(ptr) }

//export quickWorkShutdown
func quickWorkShutdown() {
	instance.stop()
	nativeMu.Lock()
	nativeHost = C.qw_host{}
	nativeMu.Unlock()
}
