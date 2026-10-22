// The GPU path (docs/m2/04-windows-engine.md §8, §7.8, E6; README WA33): everything between a captured BGRA frame and
// the NV12 picture an encoder takes. One set per session, all on the session's device:
//
//   Device (device.h)          D3D11 on the chosen encoder's adapter, the recursive context lock, device-loss mapping
//   FrameSlots (slots.h)       three BGRA slots, lock-free: the capture side publishes, the video thread takes
//   CursorOverlay (cursor.h)   DD's pointer drawn into the slot copy (shapes from cursor_shape.h); IM_START_HIDE_CURSOR
//   Converter (converter.h)    per layer: the VideoProcessor, or the shader converter as fallback and reference
//   Nv12Surface, Nv12Readback  our own NV12 picture and its readback, for CPU encoders (nv12.h)
//   TestPattern (test_pattern.h)  "test:pattern" as real pixels, drawn into a slot
//
// A session's frame, in order:
//   capture side   FrameSlots::BeginWrite → copy (WGC, DD) or TestPattern::Render → FrameSlots::Publish(meta)
//   video thread   FrameSlots::TakeLatest (the pacer's OnFrame gets meta.capture_ts_ns and the skipped count; the
//                  current slot stays the last frame for refreshes and static keyframes) → CursorOverlay::Blend (DD
//                  only) → per encode of layer l: Converter[l]::Convert into the encoder's frame (an FFmpeg pool slice)
//                  or into an Nv12Surface that Nv12Readback copies into the encoder's planes.
//
// The portable parts (geometry.h, cpu_model.h, triple_buffer.h, cursor_shape.h, pattern_layout.h) include no OS
// header; gpu_warp_test checks the D3D parts against them on WARP.
#pragma once

#include "gpu/converter.h"
#include "gpu/cursor.h"
#include "gpu/cursor_shape.h"
#include "gpu/device.h"
#include "gpu/frame.h"
#include "gpu/geometry.h"
#include "gpu/nv12.h"
#include "gpu/pattern_layout.h"
#include "gpu/slots.h"
#include "gpu/test_pattern.h"
