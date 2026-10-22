package native

import (
	"fmt"
	"sync"
	"testing"
	"time"
	"unsafe"
)

// fakeCaller is the DLL of the binding's tests (04 §12.4, 01 §7.7): a table of exports that read and write the
// memory the binding passes exactly as the DLL would, through the same pointer arithmetic, so that the binding's
// logic runs on the Mac and CI ubuntu. Tests replace exports with set; every call is recorded. An export with no
// handler answers IM_ERR_NOT_IMPLEMENTED, like WA11's skeleton DLL.
//
// Its defaults: im_version answers v1.0; im_init accepts a v1.0 im_init_params and keeps a copy; im_clock reads the
// fake QPC clock (plus clockOffset); im_build_info writes buildJSON under the snprintf rule; im_read_logs hands out
// the queued logs; im_shutdown, im_cancel_pick and im_release_source are recorded.
type fakeCaller struct {
	t *testing.T

	mu          sync.Mutex
	procs       map[proc]fakeProc
	calls       []fakeCall
	clockOffset int64  // added to im_clock's mono_ns: the cross-check's failure case
	buildJSON   string // im_build_info's document
	logs        []string
	init        *imInitParams // a copy of the last im_init's params
	initDirs    [2]string     // its ffmpeg_dir and openh264_path, "" for NULL
	released    []string      // im_release_source's ids
	start       time.Time
}

// fakeProc is one export: args are the C arguments, as the binding passed them. It returns the C result (a status or
// length: fakeResult) and, for a negative status, im_last_error's text.
type fakeProc func(args []uintptr) (r uintptr, lastErr string)

type fakeCall struct {
	proc proc
	args []uintptr
}

// fakeBuildJSON is a build info document shaped like 04 §5.6's.
const fakeBuildJSON = `{"engine":"0.2.0-test","abi":65536,"commit":"0affed6","testHooks":true,"os":"windows",` +
	`"arch":"amd64","compiler":"MSVC 19.44","ffmpeg":{"loaded":false,"dir":".","tag":"deps-ffmpeg-9.0.2-r1",` +
	`"avcodec":"","avutil":"","license":""},"opus":"libopus 1.5.2","openh264":{"loaded":false,"version":null}}`

func newFakeCaller(t *testing.T) *fakeCaller {
	f := &fakeCaller{t: t, procs: map[proc]fakeProc{}, buildJSON: fakeBuildJSON, start: time.Now()}
	f.set(procVersion, func([]uintptr) (uintptr, string) { return fakeResult(imABIVersion), "" })
	f.set(procInit, func(args []uintptr) (uintptr, string) {
		p := fakePtr[imInitParams](args[0])
		if p.structSize < uint32(unsafe.Sizeof(imInitParams{})) {
			return fakeResult(imErrABI), "abi: im_init_params.struct_size below v1.0"
		}
		cp := *p
		f.mu.Lock()
		f.init = &cp
		f.initDirs = [2]string{fakeCString(uintptr(unsafe.Pointer(p.ffmpegDir))),
			fakeCString(uintptr(unsafe.Pointer(p.openh264Path)))}
		f.mu.Unlock()
		return fakeResult(imOK), ""
	})
	f.set(procShutdown, func([]uintptr) (uintptr, string) { return fakeGarbage, "" })
	f.set(procClock, func(args []uintptr) (uintptr, string) {
		s := fakePtr[imClockSample](args[0])
		f.mu.Lock()
		off := f.clockOffset
		f.mu.Unlock()
		s.monoNS = f.qpcNS() + off
		s.unixNS = time.Now().UnixNano()
		s.uncertaintyNS = 500
		return fakeResult(imOK), ""
	})
	f.set(procBuildInfo, func(args []uintptr) (uintptr, string) {
		f.mu.Lock()
		doc := f.buildJSON
		f.mu.Unlock()
		return fakeSnprintf(args[0], args[1], doc), ""
	})
	f.set(procReadLogs, func(args []uintptr) (uintptr, string) {
		if int32(args[1]) <= 8192 {
			return fakeResult(imErrInvalidArg), "im_read_logs: cap must exceed 8192"
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		if len(f.logs) == 0 {
			return fakeResult(0), ""
		}
		doc := f.logs[0]
		f.logs = f.logs[1:]
		if len(doc) >= int(int32(args[1])) {
			f.t.Errorf("fakeCaller: a queued log chunk of %d bytes does not fit cap %d", len(doc), int32(args[1]))
			return fakeResult(0), ""
		}
		copy(fakeBytes(args[0], len(doc)), doc)
		return fakeResult(int32(len(doc))), ""
	})
	f.set(procCancelPick, func([]uintptr) (uintptr, string) { return fakeResult(imOK), "" })
	f.set(procReleaseSource, func(args []uintptr) (uintptr, string) {
		id := fakeCString(args[0])
		f.mu.Lock()
		f.released = append(f.released, id)
		f.mu.Unlock()
		return fakeGarbage, ""
	})
	return f
}

// set replaces an export's handler.
func (f *fakeCaller) set(p proc, fn fakeProc) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.procs[p] = fn
}

func (f *fakeCaller) call(p proc, args ...uintptr) (uintptr, string) {
	f.mu.Lock()
	f.calls = append(f.calls, fakeCall{proc: p, args: append([]uintptr(nil), args...)})
	fn := f.procs[p]
	f.mu.Unlock()
	if fn == nil {
		return fakeResult(imErrNotImplemented), p.String() + ": not in the fake"
	}
	r, lastErr := fn(args)
	// The real caller reads im_last_error only after a fallible export failed.
	if !p.fallible() || int32(r) >= 0 {
		lastErr = ""
	}
	return r, lastErr
}

// qpcNS is a monotonic clock in nanoseconds, like QPC.
func (f *fakeCaller) qpcNS() int64 { return int64(time.Since(f.start)) + 1e12 }

// count is how many calls of p were made.
func (f *fakeCaller) count(p proc) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.calls {
		if c.proc == p {
			n++
		}
	}
	return n
}

// callsOf returns the arguments of every call of p.
func (f *fakeCaller) callsOf(p proc) [][]uintptr {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out [][]uintptr
	for _, c := range f.calls {
		if c.proc == p {
			out = append(out, c.args)
		}
	}
	return out
}

// sequence returns the exports called so far, in order.
func (f *fakeCaller) sequence() []proc {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]proc, len(f.calls))
	for i, c := range f.calls {
		out[i] = c.proc
	}
	return out
}

// total is the number of calls so far.
func (f *fakeCaller) total() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

func (f *fakeCaller) setBuildJSON(doc string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.buildJSON = doc
}

func (f *fakeCaller) setClockOffset(ns int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.clockOffset = ns
}

func (f *fakeCaller) queueLogs(chunks ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.logs = append(f.logs, chunks...)
}

func (f *fakeCaller) initParams() (*imInitParams, [2]string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.init, f.initDirs
}

// fakeGarbage is the result register of a void export or the upper half of an int32 result: whatever the callee
// left there. The binding must look at the low 32 bits only.
var fakeGarbage = ^uintptr(0) &^ 0xFFFF_FFFF

// fakeResult is the result register of an export that returned the int32 v: v in the low 32 bits, garbage above.
func fakeResult(v int32) uintptr { return fakeGarbage | uintptr(uint32(v)) }

// fakePtr turns an argument back into the pointer the binding passed. Reading the uintptr's bits as an
// unsafe.Pointer through *unsafe.Pointer avoids a uintptr → unsafe.Pointer conversion, which vet's unsafeptr check
// flags and -race's checkptr rejects for Go memory; the binding pinned the memory for the call (addrOf).
func fakePtr[T any](a uintptr) *T {
	return (*T)(*(*unsafe.Pointer)(unsafe.Pointer(&a)))
}

// fakeBytes is the n bytes at a.
func fakeBytes(a uintptr, n int) []byte {
	if a == 0 || n <= 0 {
		return nil
	}
	return unsafe.Slice(fakePtr[byte](a), n)
}

// fakeCString reads the NUL-terminated string at a; 0 is "".
func fakeCString(a uintptr) string {
	if a == 0 {
		return ""
	}
	p := fakePtr[byte](a)
	n := 0
	for *(*byte)(unsafe.Add(unsafe.Pointer(p), n)) != 0 {
		n++
	}
	return string(unsafe.Slice(p, n))
}

// fakeSnprintf writes doc to (buf, cap) under the snprintf rule (04 §5.1) and returns the full length.
func fakeSnprintf(buf, capArg uintptr, doc string) uintptr {
	c := int(int32(capArg))
	if c < 0 || (c > 0 && buf == 0) {
		return fakeResult(imErrInvalidArg)
	}
	if c > 0 {
		dst := fakeBytes(buf, c)
		n := copy(dst[:c-1], doc)
		dst[n] = 0
	}
	return fakeResult(int32(len(doc)))
}

// fakeFails returns an export that fails with st and the detail text.
func fakeFails(st int32, lastErr string) fakeProc {
	return func([]uintptr) (uintptr, string) { return fakeResult(st), lastErr }
}

// String is for test failure messages.
func (c fakeCall) String() string { return fmt.Sprintf("%s%#v", c.proc, c.args) }
