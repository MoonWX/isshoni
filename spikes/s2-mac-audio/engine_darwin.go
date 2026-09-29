package main

/*
#cgo CFLAGS: -x objective-c -fobjc-arc -mmacosx-version-min=14.4 -Wall
#cgo LDFLAGS: -mmacosx-version-min=14.4 -framework CoreAudio -framework AudioToolbox -framework Foundation -framework AppKit -framework WebKit
#include <stdlib.h>
#include "isshoni_audio.h"
#include "testapps.h"
*/
import "C"

import (
	"encoding/json"
	"errors"
	"fmt"
	"runtime"
	"strconv"
	"unsafe"
)

const (
	modeIncludeSet = C.IM_AUDIO_MODE_INCLUDE_SET
	modeEndpoint   = C.IM_AUDIO_MODE_ENDPOINT
	ruleApp        = C.IM_RULE_APP
	ruleInstance   = C.IM_RULE_INSTANCE
	flagExcludeMic = C.IM_FLAG_EXCLUDE_MIC_USERS
	flagNoBundleID = C.IM_FLAG_NO_BUNDLE_IDS
	flagAggOutput  = C.IM_FLAG_AGG_WITH_OUTPUT
	flagNoXfade    = C.IM_FLAG_NO_CROSSFADE
	chunkFrames    = C.IM_AUDIO_CHUNK_FRAMES
	errTimeout     = C.IM_ERR_TIMEOUT
)

// engine wraps the native engine (linked in; one per process).
type engine struct{}

func loadEngine() (*engine, error) {
	if v := C.im_version(); v != C.IM_ABI_VERSION {
		return nil, fmt.Errorf("engine ABI %#x, want %#x", int(v), C.IM_ABI_VERSION)
	}
	return &engine{}, nil
}

// do runs f and turns a negative status into an error with the engine's
// thread-local message (read on the same OS thread).
func do(name string, f func() C.int32_t) (int32, error) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	rc := int32(f())
	if rc >= 0 {
		return rc, nil
	}
	var buf [512]C.char
	C.im_last_error(&buf[0], C.int32_t(len(buf)))
	msg := C.GoString(&buf[0])
	if msg == "" {
		msg = "status " + strconv.Itoa(int(rc))
	}
	return rc, &engineError{Op: name, Status: rc, Msg: msg}
}

type engineError struct {
	Op     string
	Status int32
	Msg    string
}

func (e *engineError) Error() string { return fmt.Sprintf("%s: %s (%d)", e.Op, e.Msg, e.Status) }

func isTimeout(err error) bool {
	var ee *engineError
	return errors.As(err, &ee) && ee.Status == errTimeout
}

func jsonCall(name string, f func(buf *C.char, n C.int32_t) C.int32_t) (string, error) {
	for size := 64 << 10; size <= 32<<20; size *= 4 {
		buf := (*C.char)(C.malloc(C.size_t(size)))
		n, err := do(name, func() C.int32_t { return f(buf, C.int32_t(size)) })
		if err == nil {
			s := C.GoStringN(buf, C.int(n))
			C.free(unsafe.Pointer(buf))
			return s, nil
		}
		C.free(unsafe.Pointer(buf))
		if n != C.IM_ERR_BUFFER_TOO_SMALL {
			return "", err
		}
	}
	return "", fmt.Errorf("%s: output too large", name)
}

type probeResult struct {
	OS         string  `json:"os"`
	ProcessTap bool    `json:"process_tap"`
	BundleIDs  bool    `json:"bundle_ids"`
	RespSPI    bool    `json:"responsible_pid_spi"`
	TCC        int     `json:"tcc_audio_capture"`
	Device     string  `json:"device"`
	DeviceRate float64 `json:"device_rate"`
	Error      string  `json:"error"`
}

func (e *engine) Probe() (probeResult, error) {
	var p probeResult
	s, err := jsonCall("probe", func(b *C.char, n C.int32_t) C.int32_t { return C.im_audio_probe(b, n) })
	if err == nil {
		err = json.Unmarshal([]byte(s), &p)
	}
	return p, err
}

// SetRules replaces the rules: app bundle IDs and instance pids.
func (e *engine) SetRules(apps []string, pids []int) error {
	if _, err := do("clear_rules", func() C.int32_t { return C.im_audio_clear_rules() }); err != nil {
		return err
	}
	for _, a := range apps {
		if err := e.addRule(ruleApp, a); err != nil {
			return err
		}
	}
	for _, p := range pids {
		if err := e.addRule(ruleInstance, strconv.Itoa(p)); err != nil {
			return err
		}
	}
	return nil
}

func (e *engine) addRule(kind int, value string) error {
	cs := C.CString(value)
	defer C.free(unsafe.Pointer(cs))
	_, err := do("add_rule", func() C.int32_t { return C.im_audio_add_rule(C.int32_t(kind), cs) })
	return err
}

func (e *engine) AddAppRule(app string) error { return e.addRule(ruleApp, app) }

func (e *engine) ListApps(flags uint32) (string, error) {
	return jsonCall("list_apps", func(b *C.char, n C.int32_t) C.int32_t { return C.im_audio_list_apps(C.uint32_t(flags), b, n) })
}

func (e *engine) Start(mode int, flags uint32) error {
	_, err := do("start", func() C.int32_t { return C.im_audio_start(C.int32_t(mode), C.uint32_t(flags)) })
	return err
}

// Read returns one 10 ms chunk (interleaved stereo) and its capture time (host ns).
func (e *engine) Read(buf []float32, timeoutMs int) (int, int64, error) {
	var ns C.int64_t
	n, err := do("read", func() C.int32_t {
		return C.im_audio_read((*C.float)(unsafe.Pointer(&buf[0])), C.int32_t(len(buf)/2), &ns, C.int32_t(timeoutMs))
	})
	return int(n), int64(ns), err
}

func (e *engine) Status() (string, error) {
	return jsonCall("status", func(b *C.char, n C.int32_t) C.int32_t { return C.im_audio_status(b, n) })
}

func (e *engine) Stop() { C.im_audio_stop() }

func nowNs() int64 { return int64(C.im_now_ns()) }

type appView struct {
	Pid         int      `json:"pid"`
	Ppid        int      `json:"ppid"`
	Rpid        int      `json:"rpid"`
	Name        string   `json:"name"`
	BundleID    string   `json:"bundle_id"`
	Apps        []string `json:"apps"`
	Responsible string   `json:"responsible"`
	Output      bool     `json:"output"`
	Input       bool     `json:"input"`
	Mic         bool     `json:"mic"`
	InputDevs   []string `json:"input_devices"`
	Excluded    bool     `json:"excluded"`
	Reason      string   `json:"reason"`
}

// label is how an app is shown and matched in the self-test.
func (a appView) label() string {
	if a.BundleID != "" {
		return a.BundleID
	}
	return a.Name
}

type eventView struct {
	TNs int64  `json:"t_ns"`
	Msg string `json:"msg"`
}

type statusView struct {
	Running      bool        `json:"running"`
	Device       string      `json:"device"`
	Aggregate    string      `json:"aggregate"`
	TapRate      float64     `json:"tap_rate"`
	BundleIDs    []string    `json:"bundle_ids"`
	Apps         []appView   `json:"apps"`
	Events       []eventView `json:"events"`
	MaxPeak      float64     `json:"max_peak"`
	Peak         float64     `json:"peak"`
	SilentMs     float64     `json:"silent_ms"`
	Updates      int         `json:"updates"`
	UpdateErrors int         `json:"update_errors"`
	Rebuilds     int         `json:"rebuilds"`
	Crossfades   int         `json:"crossfades"`
	XfadeAborts  int         `json:"crossfade_aborts"`
	MaxUpdateMs  float64     `json:"max_update_ms"`
	Overflow     int         `json:"overflow_frames"`
	Warnings     []string    `json:"warnings"`
}

// Test helpers (cgo is not allowed in _test.go files).

func playTone(freq, amp, seconds float64, holdMic bool) error {
	var buf [256]C.char
	mic := C.int(0)
	if holdMic {
		mic = 1
	}
	if C.im_test_tone(C.double(freq), C.double(amp), C.double(seconds), mic, &buf[0], C.int(len(buf))) != 0 {
		return errors.New(C.GoString(&buf[0]))
	}
	return nil
}

func stopTone() { C.im_test_stop() }

// playWebViewTone must run on the main thread (see onMain).
func playWebViewTone(freq, seconds float64) (string, error) {
	var buf [256]C.char
	if C.im_test_webview_tone(C.double(freq), C.double(seconds), &buf[0], C.int(len(buf))) != 0 {
		return "", errors.New(C.GoString(&buf[0]))
	}
	return C.GoString(&buf[0]), nil
}

func resample(in []float32, inRate float64, outCap int) []float32 {
	out := make([]float32, outCap*2)
	n := C.im_test_resample((*C.float)(unsafe.Pointer(&in[0])), C.int32_t(len(in)/2), C.double(inRate),
		(*C.float)(unsafe.Pointer(&out[0])), C.int32_t(outCap))
	if n < 0 {
		return nil
	}
	return out[:int(n)*2]
}
