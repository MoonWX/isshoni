// The Windows pieces of a share's mixer (live_mix.h, docs/m2/05-windows-audio.md §9.1, §10.5): the PcmSink adapter,
// the high-resolution waitable timer clock and MMCSS on the mixer thread.
#include "audio/live_mix.h"

#include <windows.h>

#include <atomic>
#include <memory>

#include "engine/os.h"
#include "im/audio/pcm_sink.h"
#include "im/log/ring.h"

namespace im::audio {
namespace {

static_assert(audiomix::kChunkFrames == kChunkFrames && audiomix::kRate == kRate && audiomix::kChannels == kChannels,
              "im_audiomix's chunks are 04's PCM chunks");

class PcmSinkAdapter final : public audiomix::ChunkSink {
public:
  explicit PcmSinkAdapter(PcmSink *sink) : sink_(sink) {}
  void OnChunk(const float *frames, int64_t capture_ns) noexcept override { sink_->OnChunk(frames, capture_ns); }
  void OnGap(int64_t capture_ns) noexcept override { sink_->OnGap(capture_ns); }

private:
  PcmSink *const sink_;
};

// The engine clock (QPC ns) and a high-resolution waitable timer (05 §9.1; available from build 17134, below the
// product floor of 19045), with an auto-reset event that Interrupt sets. Falls back to a plain waitable timer, whose
// waits end on the system tick, if the high-resolution one can't be made.
class WindowsTickClock final : public audiomix::TickClock {
public:
  WindowsTickClock() {
    timer_ = CreateWaitableTimerExW(nullptr, nullptr, CREATE_WAITABLE_TIMER_HIGH_RESOLUTION, TIMER_ALL_ACCESS);
    if (timer_ == nullptr) {
      timer_ = CreateWaitableTimerExW(nullptr, nullptr, 0, TIMER_ALL_ACCESS);
      log::Warn("audio", "no high-resolution timer for the audio mixer: waits end on the system tick");
    }
    interrupt_ = CreateEventW(nullptr, FALSE, FALSE, nullptr);
  }
  ~WindowsTickClock() override {
    if (timer_ != nullptr) {
      CloseHandle(timer_);
    }
    if (interrupt_ != nullptr) {
      CloseHandle(interrupt_);
    }
  }
  WindowsTickClock(const WindowsTickClock &) = delete;
  WindowsTickClock &operator=(const WindowsTickClock &) = delete;

  int64_t NowNs() noexcept override { return engine::os::QpcNs(); }

  void WaitUntil(int64_t deadline_ns, const std::atomic<bool> &stop) noexcept override {
    if (stop.load(std::memory_order_acquire)) {
      return;
    }
    const int64_t left = deadline_ns - NowNs();
    if (left <= 0) {
      return;
    }
    if (timer_ == nullptr || interrupt_ == nullptr) {
      Sleep(static_cast<DWORD>(left / 1'000'000 + 1));
      return;
    }
    LARGE_INTEGER due;
    due.QuadPart = -(left / 100 > 0 ? left / 100 : 1); // relative, in 100 ns units
    if (!SetWaitableTimerEx(timer_, &due, 0, nullptr, nullptr, nullptr, 0)) {
      Sleep(static_cast<DWORD>(left / 1'000'000 + 1));
      return;
    }
    const HANDLE handles[2] = {timer_, interrupt_};
    WaitForMultipleObjects(2, handles, FALSE, static_cast<DWORD>(left / 1'000'000 + 100));
  }

  void Interrupt() noexcept override {
    if (interrupt_ != nullptr) {
      SetEvent(interrupt_);
    }
  }

private:
  HANDLE timer_ = nullptr;
  HANDLE interrupt_ = nullptr;
};

} // namespace

std::unique_ptr<audiomix::ChunkSink> NewPcmSinkAdapter(PcmSink *sink) { return std::make_unique<PcmSinkAdapter>(sink); }

std::unique_ptr<audiomix::TickClock> NewWindowsTickClock() { return std::make_unique<WindowsTickClock>(); }

void MixerThreadHook(void *, bool starting) {
  // The mixer thread's name and MMCSS "Pro Audio" (05 §9.1), undone on the same thread.
  thread_local uintptr_t token = 0;
  if (starting) {
    token = engine::os::EnterThread("isshoni audio mixer", engine::os::ThreadClass::kProAudio);
  } else {
    engine::os::LeaveThread(token);
    token = 0;
  }
}

} // namespace im::audio
