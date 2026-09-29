//go:build darwin

package main

/*
#cgo CFLAGS: -x objective-c -fobjc-arc -mmacosx-version-min=14.4 -O2 -Wall -Wno-unused-function
#cgo LDFLAGS: -mmacosx-version-min=14.4 -framework VideoToolbox -framework CoreMedia -framework CoreVideo -framework IOSurface -framework CoreFoundation -framework Foundation -framework IOKit
#include <stdlib.h>
#include "vtbench.h"
*/
import "C"

import (
	"encoding/json"
	"errors"
	"fmt"
	"unsafe"
)

const (
	fmtBGRA = C.VTB_FMT_BGRA
	fmtNV12 = C.VTB_FMT_NV12

	modePaced      = C.VTB_MODE_PACED
	modeThroughput = C.VTB_MODE_THROUGHPUT

	flagKeyframe     = C.VTB_F_KEYFRAME
	flagDropped      = C.VTB_F_DROPPED
	flagNoOutput     = C.VTB_F_NO_OUTPUT
	flagForcedKF     = C.VTB_F_FORCED_KF
	flagCallback     = C.VTB_F_CALLBACK
	flagSubmitFailed = C.VTB_F_SUBMIT_FAILED
	flagSubmitted    = C.VTB_F_SUBMITTED
	flagTruncated    = C.VTB_F_TRUNCATED
)

// EncConfig mirrors vtb_enc_config.
type EncConfig struct {
	Width, Height  int
	FPS            int
	ExpectedFPS    int // ExpectedFrameRate; 0 = not set
	Bitrate        int64
	DataRateFactor float64
	KeyintSec      float64
	LLRC           bool
	Speed          bool
	RealTime       bool
	RequireHW      bool
	Software       bool // EnableHardwareAcceleratedVideoEncoder = false
}

func (e EncConfig) c() C.vtb_enc_config {
	b := func(v bool) C.int32_t {
		if v {
			return 1
		}
		return 0
	}
	return C.vtb_enc_config{
		width: C.int32_t(e.Width), height: C.int32_t(e.Height), fps: C.int32_t(e.FPS),
		expected_fps: C.int32_t(e.ExpectedFPS),
		bitrate:      C.int64_t(e.Bitrate), data_rate_factor: C.double(e.DataRateFactor),
		keyint_sec: C.double(e.KeyintSec), low_latency_rc: b(e.LLRC), prioritize_speed: b(e.Speed),
		real_time: b(e.RealTime), require_hw: b(e.RequireHW), software: b(e.Software),
	}
}

// RunConfig mirrors vtb_run_config.
type RunConfig struct {
	Mode       int
	Frames     int
	MaxSeconds float64
	Inflight   int
	ForceKF    int
	EncodeFull bool
	Preview    *EncConfig
	StreamCap  int64
}

// Frame is one native frame record with the flag words merged.
type Frame struct {
	TargetNs, SubmitNs, ReturnNs, DoneNs, XferNs int64
	StreamOff                                    int64
	StreamLen, Bytes, FrameIndex                 int32
	SubmitStatus, CbStatus                       int32
	Flags                                        int32
}

func (f Frame) has(flag int32) bool { return f.Flags&flag != 0 }

// LayerRaw is what one layer produced in a run.
type LayerRaw struct {
	Info   map[string]any
	Frames []Frame
	Stream []byte
}

// Bench is a pre-rendered frame ring for one size and pixel format.
type Bench struct {
	p           *C.vtb_bench
	Width       int
	Height      int
	Format      int
	RingLen     int
	PrerenderMs float64
}

func OpenBench(w, h, format, ringMax int, ringBytes int64) (*Bench, error) {
	var errbuf [256]C.char
	p := C.vtb_open(C.int32_t(w), C.int32_t(h), C.int32_t(format), C.int32_t(ringMax), C.int64_t(ringBytes),
		&errbuf[0], C.int32_t(len(errbuf)))
	if p == nil {
		return nil, errors.New(C.GoString(&errbuf[0]))
	}
	return &Bench{p: p, Width: w, Height: h, Format: format, RingLen: int(C.vtb_ring_len(p)),
		PrerenderMs: float64(C.vtb_prerender_ms(p))}, nil
}

func (b *Bench) Close() {
	if b.p != nil {
		C.vtb_close(b.p)
		b.p = nil
	}
}

// Run executes one benchmark and copies every layer's results into Go memory.
func (b *Bench) Run(full EncConfig, rc RunConfig) (int32, []LayerRaw) {
	fc := full.c()
	r := C.vtb_run_config{
		mode: C.int32_t(rc.Mode), frames: C.int32_t(rc.Frames), max_seconds: C.double(rc.MaxSeconds),
		inflight: C.int32_t(rc.Inflight), force_kf_frame: C.int32_t(rc.ForceKF),
		stream_cap_bytes: C.int64_t(rc.StreamCap),
	}
	if rc.EncodeFull {
		r.encode_full = 1
	}
	if rc.Preview != nil {
		r.preview = 1
		r.preview_cfg = rc.Preview.c()
	}
	st := int32(C.vtb_run(b.p, &fc, &r))
	n := int(C.vtb_layer_count(b.p))
	layers := make([]LayerRaw, 0, n)
	for i := range n {
		var lr LayerRaw
		js := C.vtb_layer_info_json(b.p, C.int32_t(i))
		if err := json.Unmarshal([]byte(C.GoString(js)), &lr.Info); err != nil {
			lr.Info = map[string]any{"json_error": err.Error()}
		}
		C.vtb_free(unsafe.Pointer(js))

		var fp *C.vtb_frame
		cnt := int(C.vtb_layer_frames(b.p, C.int32_t(i), &fp))
		if cnt > 0 && fp != nil {
			src := unsafe.Slice(fp, cnt)
			lr.Frames = make([]Frame, cnt)
			for k, f := range src {
				lr.Frames[k] = Frame{
					TargetNs: int64(f.target_ns), SubmitNs: int64(f.submit_ns), ReturnNs: int64(f.return_ns),
					DoneNs: int64(f.done_ns), XferNs: int64(f.xfer_ns), StreamOff: int64(f.stream_off),
					StreamLen: int32(f.stream_len), Bytes: int32(f.bytes), FrameIndex: int32(f.frame_index),
					SubmitStatus: int32(f.submit_status), CbStatus: int32(f.cb_status),
					Flags: int32(f.sflags) | int32(f.cflags),
				}
			}
		}
		var sp *C.uint8_t
		sl := int64(C.vtb_layer_stream(b.p, C.int32_t(i), &sp))
		if sl > 0 && sp != nil {
			lr.Stream = C.GoBytes(unsafe.Pointer(sp), C.int(sl))
		}
		layers = append(layers, lr)
	}
	return st, layers
}

// DecodeResult mirrors vtb_decode_result.
type DecodeResult struct {
	AccessUnits   int     `json:"access_units"`
	Decoded       int     `json:"decoded"`
	Failed        int     `json:"failed"`
	FirstError    int32   `json:"first_error,omitempty"`
	Width         int     `json:"width"`
	Height        int     `json:"height"`
	Delayed       int     `json:"delayed_outputs"` // frames the decoder held back (reorder buffering)
	FormatChanges int     `json:"format_descriptions"`
	PSNRFrames    int     `json:"psnr_frames,omitempty"`
	PSNRYMean     float64 `json:"psnr_y_mean_db,omitempty"`
	PSNRYMin      float64 `json:"psnr_y_min_db,omitempty"`
	DecodeMs      float64 `json:"decode_ms"`
	OK            bool    `json:"ok"`
}

// Decode feeds the access units through VTDecompressionSession.
func (b *Bench) Decode(stream []byte, aus []AURef, psnr bool) (DecodeResult, error) {
	var res DecodeResult
	if len(aus) == 0 || len(stream) == 0 {
		return res, fmt.Errorf("nothing to decode")
	}
	off := make([]C.int64_t, len(aus))
	ln := make([]C.int32_t, len(aus))
	fr := make([]C.int32_t, len(aus))
	for i, a := range aus {
		off[i], ln[i], fr[i] = C.int64_t(a.Off), C.int32_t(a.Len), C.int32_t(a.Frame)
	}
	var out C.vtb_decode_result
	p := 0
	if psnr {
		p = 1
	}
	C.vtb_decode_check(b.p, (*C.uint8_t)(unsafe.Pointer(&stream[0])), &off[0], &ln[0], &fr[0], C.int32_t(len(aus)),
		C.int32_t(p), &out)
	res = DecodeResult{
		AccessUnits: len(aus), Decoded: int(out.decoded), Failed: int(out.failed), FirstError: int32(out.first_error),
		Width: int(out.width), Height: int(out.height), Delayed: int(out.delayed),
		FormatChanges: int(out.format_changes), PSNRFrames: int(out.psnr_frames),
		PSNRYMean: float64(out.psnr_y_mean), PSNRYMin: float64(out.psnr_y_min), DecodeMs: float64(out.decode_ms),
	}
	res.OK = res.Failed == 0 && res.Decoded == len(aus)
	return res, nil
}

// BeginActivity holds an NSProcessInfo activity until the returned func is called.
func BeginActivity(latencyCritical bool) func() {
	lc := C.int32_t(0)
	if latencyCritical {
		lc = 1
	}
	tok := C.vtb_begin_activity(lc)
	return func() { C.vtb_end_activity(tok) }
}

func EncodersJSON() json.RawMessage {
	p := C.vtb_encoders_json()
	defer C.vtb_free(unsafe.Pointer(p))
	return json.RawMessage(C.GoString(p))
}

func SystemJSON() json.RawMessage {
	p := C.vtb_system_json()
	defer C.vtb_free(unsafe.Pointer(p))
	return json.RawMessage(C.GoString(p))
}
