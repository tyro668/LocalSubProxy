// Command localsubproxy_native is the kernel as a C shared library for the
// desktop app (Flutter FFI). The JSON API lives in internal/native and is
// shared with the headless CLI (`localsubproxy serve`).
package main

/*
#include <stdlib.h>
*/
import "C"

import (
	"unsafe"

	"github.com/tyro668/LocalSubProxy/internal/native"
)

//export LSP_Start
func LSP_Start(raw *C.char) *C.char {
	in := ""
	if raw != nil {
		in = C.GoString(raw)
	}
	return C.CString(native.Start(in))
}

//export LSP_Call
func LSP_Call(raw *C.char) *C.char {
	if raw == nil {
		return C.CString(`{"error":"request required","ok":false}`)
	}
	return C.CString(native.Call(C.GoString(raw)))
}

//export LSP_Free
func LSP_Free(ptr unsafe.Pointer) {
	if ptr != nil {
		C.free(ptr)
	}
}

func main() {}
