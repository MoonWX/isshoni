// Test signals and their measurement for 05's own sound (docs/m2/05-windows-audio.md §11.2, §11.3, §13.2; "05"
// below): the startup probe's tone, the sound check's chime, the test exports' tone and bursts, a Goertzel level meter
// with a Hann window, and the render loop that plays them through a RenderTarget and predicts each burst's
// device-position time from the render client's clock.
//
// Portable C++ (no OS header): render_win.h is Windows' RenderTarget (a shared-mode WASAPI render client with a private
// session), tests use fakes on simulated time (native/windows/tests/audio_engine_control_test.cpp).
#pragma once

#include <atomic>
#include <cstddef>
#include <cstdint>
#include <vector>

#include "isshoni_media.h"

namespace im::audio {

inline constexpr uint32_t kSignalRate = 48000; // every signal here is float32 48 kHz stereo (05 §8.2, §11.2, §13.2)

// The level a Goertzel meter reports for a window without the tone (and for an empty one), dBFS.
inline constexpr double kSignalFloorDb = -200.0;

// The level of a sine at hz in mono[0, n), in dBFS of its peak amplitude (a full-scale sine is 0 dB), with a Hann
// window: the analyzer's toneLevelDB (cmd/isshoni-audiotest/analyze.go), so the DLL and the harness measure alike.
double ToneLevelDb(const float *mono, std::size_t n, double rate, double hz) noexcept;

// The highest ToneLevelDb of hz over windows of `window` samples hopped by `hop` across mono[0, n) (only windows that
// lie fully inside it), so a tone is measured wherever it landed in a capture; kSignalFloorDb when n < window.
double MaxWindowLevelDb(const float *mono, std::size_t n, double rate, double hz, std::size_t window,
                        std::size_t hop) noexcept;

// Appends the mono mix ((L + R) / 2) of frames interleaved stereo frames to out.
void AppendMono(const float *stereo, std::size_t frames, std::vector<float> &out);

// A tone of `length` frames with raised-cosine ramps of `ramp` frames at both ends, made of up to two sine partials
// (hz2 = 0: one), each of linear peak amplitude amp; writes frames [first, first + n) of it into out (interleaved
// stereo, both channels equal). Frames outside [0, length) are zeros.
struct Tone {
  double hz = 1000;
  double hz2 = 0;
  double amp = 0.5;
  uint64_t length = kSignalRate;
  uint32_t ramp = kSignalRate / 100;
};
void ToneFrames(const Tone &tone, uint64_t first, uint32_t n, float *out) noexcept;

// The startup probe's tone (05 §11.2 step 3): 200 ms of 997 Hz at -60 dBFS with 10 ms raised-cosine ramps.
Tone ProbeTone() noexcept;
// The sound check's chime as the DLL plays it itself when helper_pid is 0 (05 §11.3 steps 1-2): 800 ms, partials 880 Hz
// and 1320 Hz, -14 dBFS together (each partial at half that amplitude), 10 ms ramps.
Tone Chime() noexcept;

// The probe's verdict on a measured level (05 §11.2 step 4): [-66, -54] dBFS ok, below -80 failed (silent loopback),
// in between ok with the warning probe_level_mismatch.
enum class ProbeLevel : uint8_t { kOk, kMismatch, kSilent };
ProbeLevel ClassifyProbeLevel(double level_db) noexcept;

// A render client as the signals need it (render_win.h: IAudioClient, IAudioRenderClient, IAudioClock; fakes in
// tests). One thread drives it. HRESULTs as int32_t (0 = S_OK).
class RenderTarget {
public:
  virtual ~RenderTarget() = default;
  // The frames the buffer takes now (its size minus the padding).
  virtual int32_t Writable(uint32_t *frames) noexcept = 0;
  // Appends n interleaved stereo frames (n <= Writable).
  virtual int32_t Write(const float *frames, uint32_t n) noexcept = 0;
  // IAudioClient::Start (after the first write).
  virtual int32_t Start() noexcept = 0;
  // The device position (IAudioClock::GetPosition): the frames of the stream played so far and the engine clock (QPC
  // ns) of that position. False when the clock can't say.
  virtual bool Position(uint64_t *frames, int64_t *qpc_ns) noexcept = 0;
  // Waits about ms milliseconds (the next buffer write).
  virtual void Wait(uint32_t ms) noexcept = 0;
};

// The device-position time of stream frame f from a position sample (pos frames at qpc_ns), 05 §13.2:
// onset = qpc_ns + (f - pos) x 1e9 / 48000 (f >= pos).
int64_t PredictFrameNs(uint64_t f, uint64_t pos, int64_t qpc_ns) noexcept;

// The bursts of im_test_render_bursts (05 §13.2): zeros with `count` bursts of burst_ms of hz (1 ms raised-cosine
// ramps) every period_ms, the first lead_ms after Start.
struct Bursts {
  double hz = 2000;
  double amp = 0.5;
  uint32_t burst_ms = 5;
  uint32_t period_ms = 1000;
  uint32_t count = 1;
  uint32_t lead_ms = 200; // >= 200 ms after Start (05 §13.2)
};

// Plays the bursts on target and writes up to cap predicted onsets (engine clock) into onset_ns: after every write the
// device position is read, and a burst's onset is taken from the last sample whose position is still at or before
// its first frame (05 §13.2). Returns once the last burst has played, with the number of onsets written, or a negative
// status (IM_ERR_INTERNAL with the HRESULT in the thread's detail when the client fails; IM_ERR_CANCELLED when abort
// turns true). Bounded: it gives up (IM_ERR_TIMEOUT) when the device position stops moving for 2 s.
int32_t PlayBursts(RenderTarget &target, const Bursts &b, int64_t *onset_ns, int32_t cap,
                   const std::atomic<bool> *abort) noexcept;

// Plays frames [0, tone.length) of tone on target and returns once they have played (or the position stops moving
// for 2 s: IM_ERR_TIMEOUT). Between writes it calls between(ctx) (the probe and the sound check read their capture
// there); it may be null. lead/tail: zeros before and after the tone.
im_status PlayTone(RenderTarget &target, const Tone &tone, uint32_t lead_frames, uint32_t tail_frames,
                   const std::atomic<bool> *abort, void (*between)(void *ctx) = nullptr, void *ctx = nullptr) noexcept;

} // namespace im::audio
