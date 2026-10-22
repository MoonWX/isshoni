//go:build windows

package native

import (
	"runtime"
	"syscall"
	"unsafe"
)

// lastErrorCap fits im_last_error's longest detail (1023 bytes, native/common's kMaxErrorBytes) and its NUL.
const lastErrorCap = 1024

// dllCaller is the caller over isshoni-media.dll's resolved exports (04 §12.4): syscall.SyscallN, no cgo.
type dllCaller struct {
	addr [procCount]uintptr
}

// call runs the export on a locked OS thread and, when it is fallible and failed, reads im_last_error before the
// thread is released: the detail is thread-local (S1's engine.do). The arguments' memory is pinned by the binding
// (addrOf) for the call.
func (c *dllCaller) call(p proc, args ...uintptr) (uintptr, string) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	r, _, _ := syscall.SyscallN(c.addr[p], args...)
	if !p.fallible() || int32(r) >= 0 {
		return r, ""
	}
	return r, c.lastError()
}

// lastError is im_last_error on the current (locked) thread, under the snprintf rule: a longer detail is cut.
func (c *dllCaller) lastError() string {
	buf := make([]byte, lastErrorCap)
	var pn runtime.Pinner
	pn.Pin(&buf[0])
	defer pn.Unpin()
	r, _, _ := syscall.SyscallN(c.addr[procLastError], uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)))
	n := int32(r)
	if n <= 0 {
		return ""
	}
	return string(buf[:min(int(n), len(buf)-1)])
}

func (c *dllCaller) qpcNS() int64 { return qpcNS() }
