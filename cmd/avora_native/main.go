// Command avora_native is the kernel as a C shared library for the
// desktop app (Flutter FFI). The JSON API lives in internal/native and is
// shared with the headless CLI (`avora serve`).
package main

/*
#include <stdlib.h>
*/
import "C"

import (
	"unsafe"

	"github.com/tyro668/avora/internal/native"
)

//export AVORA_Start
func AVORA_Start(raw *C.char) *C.char {
	in := ""
	if raw != nil {
		in = C.GoString(raw)
	}
	return C.CString(native.Start(in))
}

//export AVORA_Call
func AVORA_Call(raw *C.char) *C.char {
	if raw == nil {
		return C.CString(`{"error":"request required","ok":false}`)
	}
	return C.CString(native.Call(C.GoString(raw)))
}

//export AVORA_Free
func AVORA_Free(ptr unsafe.Pointer) {
	if ptr != nil {
		C.free(ptr)
	}
}

func main() {}
