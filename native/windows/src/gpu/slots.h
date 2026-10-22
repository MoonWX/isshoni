// The frame slots (docs/m2/04-windows-engine.md §8.2): three BGRA textures (D3D11_BIND_SHADER_RESOURCE |
// D3D11_BIND_RENDER_TARGET) over the lock-free triple buffer of triple_buffer.h. The capture side (the WGC callback,
// the DD thread, the test pattern's timer) writes its own slot and publishes it as the latest with a sequence number;
// the video thread takes the latest and keeps it as "the last frame" until a newer one arrives. A frame published
// while the previous one is still unread replaces it (the newest frame wins). Both sides use the one immediate
// context under the context lock, so a copy is ordered before the reads without fences.
//
// The slots start at the source's size. A capture whose content is larger (a window grown before the resize settles,
// 04 §7.7) grows the writer's texture in BeginWrite; textures never shrink, and each frame says how much of its texture
// it fills (FrameMeta::content).
#pragma once

#include <array>
#include <cstdint>
#include <memory>

#include "gpu/device.h"
#include "gpu/frame.h"
#include "gpu/geometry.h"
#include "gpu/triple_buffer.h"
#include "im/video/scale.h"
#include "isshoni_media.h"

namespace im::gpu {

// The pointer on a frame (Desktop Duplication only; WGC draws its own, 04 §7.8): the top-left of the pointer image in
// the frame's upright pixels.
struct CursorState {
  bool visible = false;
  int32_t x = 0;
  int32_t y = 0;
};

// What the capture side says about a frame it publishes.
struct FrameMeta {
  int64_t capture_ts_ns = 0; // engine clock
  video::Size content;       // the region (0, 0, content) of the slot texture
  Rotation rotation = Rotation::kIdentity;
  CursorState cursor;
};

struct Slot {
  ComPtr<ID3D11Texture2D> texture;
  ComPtr<ID3D11RenderTargetView> rtv;
  ComPtr<ID3D11ShaderResourceView> srv;
  video::Size size;
  uint64_t texture_id = 0;
  FrameMeta meta;
  uint64_t seq = 0;            // the frame's number (Publish's)
  bool cursor_blended = false; // the video thread drew the pointer into it (cursor.h)

  SourceFrame source() const noexcept {
    return SourceFrame{texture.Get(), srv.Get(), texture_id, size, meta.content, meta.rotation};
  }
};

class FrameSlots {
public:
  static constexpr uint32_t kCount = TripleIndex::kSlots;

  // Three slots of size (each side ≥ 1). Failures set the thread's detail (Device::Fail).
  static im_status Create(Device &device, video::Size size, std::unique_ptr<FrameSlots> *out) noexcept;

  FrameSlots(const FrameSlots &) = delete;
  FrameSlots &operator=(const FrameSlots &) = delete;

  // ---- the capture side (one writer thread at a time)
  // The slot to fill next, grown to hold content when it doesn't (an allocation: only on growth). *out stays valid
  // until Publish.
  im_status BeginWrite(video::Size content, Slot **out) noexcept;
  // Publishes the slot BeginWrite returned as the latest frame. Returns its number.
  uint64_t Publish(const FrameMeta &meta) noexcept;

  // ---- the video thread (one reader)
  struct Taken {
    bool fresh = false;   // a newer frame than the last one taken
    uint64_t skipped = 0; // frames published meanwhile that were never taken
    Slot *slot = nullptr; // the reader's frame (null until the first frame was taken)
  };
  Taken TakeLatest() noexcept;
  // The reader's frame without taking a newer one (null until the first one was taken).
  Slot *current() noexcept;

  Device &device() noexcept { return device_; }

private:
  explicit FrameSlots(Device &device) noexcept : device_(device) {}
  im_status Allocate(Slot &slot, video::Size size) noexcept;

  Device &device_;
  std::array<Slot, kCount> slots_;
  TripleIndex index_;
};

} // namespace im::gpu
