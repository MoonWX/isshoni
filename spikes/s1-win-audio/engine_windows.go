//go:build windows

package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Mirrors native/isshoni_audio.h.
const (
	abiVersion        = 0x000001
	modeIncludeSet    = 0
	modeExcludeOne    = 1
	modeEndpoint      = 2
	ruleApp           = 0
	ruleInstance      = 1
	flagExcludeMic    = 0x1
	chunkFrames       = 480
	errTimeout        = -7
	errBufferTooSmall = -8
)

var modeNames = map[int]string{modeIncludeSet: "include-set", modeExcludeOne: "exclude-one", modeEndpoint: "endpoint"}

// engine wraps isshoni_audio.dll, loaded without cgo.
type engine struct {
	version, probe, clearRules, addRule, listApps, start, read, status, stop, lastError *windows.LazyProc
}

func loadEngine() (*engine, error) {
	exe, err := os.Executable()
	if err != nil {
		return nil, err
	}
	path := filepath.Join(filepath.Dir(exe), "isshoni_audio.dll")
	dll := windows.NewLazyDLL(path)
	if err := dll.Load(); err != nil {
		return nil, fmt.Errorf("load %s: %w", path, err)
	}
	e := &engine{
		version: dll.NewProc("im_version"), probe: dll.NewProc("im_audio_probe"),
		clearRules: dll.NewProc("im_audio_clear_rules"), addRule: dll.NewProc("im_audio_add_rule"),
		listApps: dll.NewProc("im_audio_list_apps"), start: dll.NewProc("im_audio_start"),
		read: dll.NewProc("im_audio_read"), status: dll.NewProc("im_audio_status"),
		stop: dll.NewProc("im_audio_stop"), lastError: dll.NewProc("im_last_error"),
	}
	for _, p := range []*windows.LazyProc{e.version, e.probe, e.clearRules, e.addRule, e.listApps, e.start, e.read, e.status, e.stop, e.lastError} {
		if err := p.Find(); err != nil {
			return nil, fmt.Errorf("%s is missing %s (stale build?): %w", path, p.Name, err)
		}
	}
	if v, _, _ := e.version.Call(); int32(v) != abiVersion {
		return nil, fmt.Errorf("engine ABI %#x, want %#x", v, abiVersion)
	}
	return e, nil
}

// do runs one DLL call (f must call LazyProc.Call itself, with every
// unsafe.Pointer→uintptr conversion written inside that call expression: only
// then does //go:uintptrescapes keep the memory alive and unmoved for the
// call). On failure it fetches the thread-local error on the same OS thread.
func (e *engine) do(name string, f func() uintptr) (int32, error) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	n := int32(f())
	if n >= 0 {
		return n, nil
	}
	buf := make([]byte, 1024)
	m, _, _ := e.lastError.Call(uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)))
	msg := ""
	if int32(m) > 0 {
		msg = string(buf[:m])
	}
	return n, fmt.Errorf("%s: status %d %s", name, n, msg)
}

func (e *engine) jsonCall(p *windows.LazyProc, prefix ...uintptr) (string, error) {
	for size := 64 << 10; ; size *= 4 {
		buf := make([]byte, size)
		n, err := e.do(p.Name, func() uintptr {
			var r uintptr
			switch len(prefix) {
			case 0:
				r, _, _ = p.Call(uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)))
			default:
				r, _, _ = p.Call(prefix[0], uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)))
			}
			return r
		})
		if n == errBufferTooSmall && size < 16<<20 {
			continue
		}
		if err != nil {
			return "", err
		}
		return string(buf[:n]), nil
	}
}

type probeResult struct {
	OSBuild         int     `json:"os_build"`
	ProcessLoopback bool    `json:"process_loopback"`
	Format          string  `json:"format"`
	MasterVolume    float64 `json:"master_volume"`
	MasterMuted     bool    `json:"master_muted"`
	Device          string  `json:"device"`
	Error           string  `json:"error"`
}

func (e *engine) Probe() (probeResult, error) {
	var r probeResult
	s, err := e.jsonCall(e.probe)
	if err == nil {
		err = json.Unmarshal([]byte(s), &r)
	}
	return r, err
}

func (e *engine) SetRules(apps []string, pids []int) error {
	if _, err := e.do("im_audio_clear_rules", func() uintptr { r, _, _ := e.clearRules.Call(); return r }); err != nil {
		return err
	}
	for _, a := range apps {
		if err := e.addRuleStr(ruleApp, a); err != nil {
			return err
		}
	}
	for _, p := range pids {
		if err := e.addRuleStr(ruleInstance, fmt.Sprint(p)); err != nil {
			return err
		}
	}
	return nil
}

func (e *engine) addRuleStr(kind int, value string) error {
	b, err := windows.BytePtrFromString(value)
	if err != nil {
		return err
	}
	_, err = e.do("im_audio_add_rule", func() uintptr {
		r, _, _ := e.addRule.Call(uintptr(kind), uintptr(unsafe.Pointer(b)))
		return r
	})
	return err
}

func (e *engine) AddAppRule(app string) error { return e.addRuleStr(ruleApp, app) }

func (e *engine) ListApps(flags uint32) (string, error) {
	return e.jsonCall(e.listApps, uintptr(flags))
}

func (e *engine) Start(mode int, flags uint32) error {
	_, err := e.do("im_audio_start", func() uintptr { r, _, _ := e.start.Call(uintptr(mode), uintptr(flags)); return r })
	return err
}

// Read returns one 10 ms chunk (480 stereo frames) and its capture time in ns.
func (e *engine) Read(buf []float32, timeoutMs int) (int, int64, error) {
	var ts int64
	n, err := e.do("im_audio_read", func() uintptr {
		r, _, _ := e.read.Call(uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)/2), uintptr(unsafe.Pointer(&ts)), uintptr(timeoutMs))
		return r
	})
	return int(n), ts, err
}

func (e *engine) Status() (string, error) { return e.jsonCall(e.status) }

func (e *engine) Stop() { e.stop.Call() }

// statusView is the subset of the status JSON the CLI prints.
type statusView struct {
	Mode    string `json:"mode"`
	Streams []struct {
		Label, Kind, State, Error string
		Pid                       int     `json:"pid"`
		Peak                      float64 `json:"peak"`
		MaxPeak                   float64 `json:"max_peak"`
		Underruns                 int     `json:"underruns"`
	} `json:"streams"`
	Sessions []struct {
		Pid      int    `json:"pid"`
		Exe      string `json:"exe"`
		State    string `json:"state"`
		Excluded bool   `json:"excluded"`
		Reason   string `json:"reason"`
		Note     string `json:"note"`
	} `json:"sessions"`
	Warnings []string `json:"warnings"`
	Events   []struct {
		TNs   int64  `json:"t_ns"`
		Label string `json:"label"`
		Event string `json:"event"`
	} `json:"events"`
	Totals struct {
		Discontinuities int `json:"discontinuities"`
		Overflows       int `json:"overflows"`
		Underruns       int `json:"underruns"`
		Trims           int `json:"trims"`
		DroppedChunks   int `json:"dropped_chunks"`
		MixerResyncs    int `json:"mixer_resyncs"`
	} `json:"totals"`
}
