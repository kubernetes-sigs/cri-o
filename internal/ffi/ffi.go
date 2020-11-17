package ffi

// #cgo CFLAGS: -I lib
// #cgo LDFLAGS: ${SRCDIR}/lib/libcri.a -ldl -lpthread -lrt -lm
// #include <ffi.h>
import "C"

import (
	"errors"
	"unsafe"
)

// Setup initializes the foreign function interface to the Rust lib
func Setup() error {
	return lastError()
}

// lastError can be used to retrieve the last error in case any ffi function
// returned nil as a result.
func lastError() error {
	errLen := C.last_error_length()

	if errLen == 0 {
		return nil
	}

	buf := make([]byte, errLen)
	C.last_error_message((*C.char)(unsafe.Pointer(&buf[0])), errLen)
	return errors.New(string(buf))
}
