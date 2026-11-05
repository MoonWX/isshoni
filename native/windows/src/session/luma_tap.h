// The luma tap's producer (docs/m2/04-windows-engine.md §5.8, §14 avsync; README WF16): IM_TEST_HOOKS builds only. In
// any other build this header declares nothing, luma_tap.cpp compiles to nothing, and im_start refuses the tap's start
// flag (luma_sample.h's kStartFlag), so a shipped DLL never emits IM_PACKET_DEBUG.
//
// For every frame the session's video thread takes from its frame slots (session_object.h's TakeFrame), the tap copies
// the frame's fixed region (luma::CenterBox) into one of kDepth staging textures behind an event query, on the
// session's device. Poll, on the same thread, maps the copies the GPU has finished, oldest first, without waiting for
// it (GetData with DONOTFLUSH, Map with DO_NOT_WAIT), computes the region's mean luma and pushes the reading, with the
// frame's capture time, into the session's debug ring (IM_PACKET_DEBUG, luma_sample.h's payload). So the video thread
// never waits for the GPU on the tap's behalf, and a reading lags its frame by a wake-up or two of the video thread.
// A frame that finds every staging texture busy has no reading (counted in the next one's `missed`).
//
// The tap needs pixels: a session runs it only on the GPU path (a capture that writes slots, or the test pattern drawn
// on a device, which the tap's start flag asks for even with IM_ENC_FAKE; session_object.cpp's StartFrames). A D3D
// failure stops the tap (one warn log, no more readings); device loss is the session's to find and end (§9.8).
//
// Threads: Create on the engine worker (im_start); OnFrame, Poll and pending on the video thread; destroyed after the
// video thread, before the device (Session::ReleaseVideo). The D3D calls take the device's context lock, never across
// a wait (§6.1).
#pragma once

#if defined(IM_TEST_HOOKS) && IM_TEST_HOOKS

#include <array>
#include <cstdint>
#include <memory>

#include "gpu/device.h"
#include "gpu/slots.h"
#include "im/ring/packet_ring.h"
#include "isshoni_media.h"
#include "session/luma_sample.h"

namespace im::session {

// The debug ring's size (§6.2's rings are for media; this one carries 48-byte readings): 64 readings, about 1 s at
// 60 fps, before the consumer falls behind (then the ring drops the newest and the next reading carries DISCONTINUITY).
inline constexpr ring::RingConfig kDebugRingConfig{64, 64u << 10, ring::OverflowPolicy::kAudio};

class LumaTap {
public:
  static constexpr uint32_t kDepth = 8; // staging textures in flight

  // The tap on device, pushing into ring (both outlive it). Failures set the thread's detail (Device::Fail).
  static im_status Create(gpu::Device &device, ring::PacketRing *ring, std::unique_ptr<LumaTap> *out) noexcept;

  LumaTap(const LumaTap &) = delete;
  LumaTap &operator=(const LumaTap &) = delete;

  // The video thread took slot as a new frame; skipped frames were published since the previous one and never taken.
  void OnFrame(const gpu::Slot &slot, uint64_t skipped) noexcept;
  // Pushes the readings whose copies the GPU has finished, oldest first; never waits for the GPU.
  void Poll() noexcept;
  // Whether copies are in flight (the video thread then polls again soon, also on a static screen).
  bool pending() const noexcept { return count_ != 0; }

  // Readings that got into the ring (for logs and tests).
  uint64_t readings() const noexcept { return readings_; }

private:
  struct Entry {
    gpu::ComPtr<ID3D11Texture2D> staging; // kBoxSide × kBoxSide BGRA, D3D11_USAGE_STAGING, CPU read
    gpu::ComPtr<ID3D11Query> query;       // D3D11_QUERY_EVENT behind the copy
    luma::Sample sample;                  // all but the mean, filled at OnFrame
  };

  LumaTap(gpu::Device &device, ring::PacketRing *ring) noexcept : device_(device), ring_(ring) {}
  // Stops the tap after a D3D failure (logged once).
  void Stop(HRESULT hr, const char *what) noexcept;

  gpu::Device &device_;
  ring::PacketRing *ring_;
  std::array<Entry, kDepth> entries_;
  uint32_t head_ = 0;  // the oldest copy in flight
  uint32_t count_ = 0; // copies in flight
  uint64_t missed_ = 0;
  uint64_t readings_ = 0;
  bool stopped_ = false;
};

} // namespace im::session

#endif // IM_TEST_HOOKS
