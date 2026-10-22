// Adaptive resampling's control loop (docs/m2/05-windows-audio.md §9.3): the PI controller that holds a stream's
// buffer at its target, and the model of how much input speexdsp's resampler consumes at a ratio.
//
// The ratio. speexdsp resamples by num/den input frames per output frame, so a ratio above 1 drains a buffer that is
// too full. §9.3 writes it as (1 000 000 + ppm) / 1 000 000, but speexdsp 1.2.1 (the vcpkg port) can't change a ratio
// with a denominator that large: speex_resampler_set_rate_frac rescales the filter phase with a 32-bit
// phase * new_den / old_den, returns RESAMPLER_ERR_OVERFLOW whenever phase * new_den exceeds 2^32 (almost always at
// 10^6) and leaves the state half updated: ratio fields changed, advance and phase not, and a resampler left so
// crashed (SIGBUS) on its next chunk when probed (mix_resampler_test shows the status). So the ratio is
// (kRatioDen + steps) / kRatioDen with kRatioDen = 62 500 (62 500^2 < 2^32): one step is exactly 16 ppm, and "ppm"
// below is always a multiple of 16. The rest of §9.3 holds unchanged: the resampler is created at a non-unity ratio
// (+1 step) so speexdsp picks its interpolated filter table up front; steps is never 0 (0 becomes +1, or -1 when the
// previous value was negative), because 1:1 would flip speexdsp to its direct table (a recompute, and a reallocation
// at the next non-unity ratio, on the mixer thread) and zero the sub-sample phase; and the filter length stays 80 for
// every |ppm| <= 992, so ratio changes allocate nothing (mix_alloc_test counts it).
//
// The measured value. §9.3 controls the buffer fill sampled at each tick. Sampled at the tick, the fill jumps by a
// whole capture packet (10 ms) whenever a packet's arrival crosses the tick, and with drift that crossing point moves
// slowly, so the sampled fill carries a 10 ms sawtooth with a period of seconds to minutes (20 s at 500 ppm), which a
// PI with a 20 s time constant follows (mix_drift_test's SampledFillControlMissesTheRatioBound shows the ratio's
// spread). The controller therefore holds the head's age instead: d = tick time - the QPC time of the ring's head
// frame, the same d that §10.3 measures for timestamps. d doesn't depend on when packets arrive, only on how many
// frames were consumed against how many were captured; it is the fill plus the age of the newest captured frame.
//
// The target. MixEngine sets it every tick to the larger of two head ages, kept within 40..90 ms. One is the mixer's
// stamping delay L: holding d at L stamps every chunk with the time its audio was mixed (§10.3). The other is the
// stream's fill floor F = 40 ms + the ring's arrival lag + half its packet length (capture_ring.h): the newest frame's
// age averages the packets' lag behind the mix plus half a packet, so at d = F the fill averages §9.3's 40 ms. With
// 10 ms packets that reach the capture thread within 5 ms of their mix, F <= L0 = 50 ms (§10.3's "typical d": 40 ms of
// fill and one packet), so d = L: exact stamps and a fill of 40 ms or a little more. Packets that come later (a capture
// offset S1 never measured, longer packets) would leave less fill at L, down to underruns; d then sits at F instead,
// the fill still averages 40 ms, and the stamps carry the error |L_true - L| that §10.3 accepts for L0 (a re-anchor
// can't raise L). So §9.3's 40 ms is held on average over the packet phase rather than as the tick-sampled value.
#pragma once

#include <cstdint>

#include "im/audiomix/constants.h"

namespace im::audiomix {

inline constexpr uint32_t kRatioDen = 62500;                                        // speexdsp's ratio denominator
inline constexpr int32_t kPpmPerStep = static_cast<int32_t>(1'000'000 / kRatioDen); // 16
inline constexpr int32_t kMaxSteps = 1000 / kPpmPerStep;                            // 62: +-992 ppm (§9.3's +-1000)

// The controller's tuning, set in mix_drift_test's simulation against §17 (§9.3 started from Kp = 50 and Ki = 1, which
// settled slowly and, with the integral limited to 400 ppm, left a 2 ms error at 500 ppm). The stream's head age moves
// by 0.001 ms/s per ppm of mismatch, so Kp = 100 and Ki = 2.5 make a critically damped loop with a 20 s time constant,
// and Ki * integral_limit = 1000 ppm lets the integral alone hold any drift of the range: no steady-state error.
struct ControllerParams {
  int64_t target_age_ns = 50 * kNsPerMs; // the head age held until SetTarget (L0: 40 ms fill + one 10 ms packet)
  double ewma_seconds = 1.0;             // smoothing of the per-tick sample
  uint32_t update_ticks = 10;            // one update every 100 ms
  double kp = 100.0;                     // ppm per ms of error
  double ki = 2.5;                       // ppm per ms*s of integrated error
  double integral_limit = 400.0;         // ms*s
  double slew_ppm = 20.0;                // the most the command moves in one update
  int32_t max_steps = kMaxSteps;         // the ratio's bound
};

// The PI controller of one stream (§9.3). One thread (the mixer's).
class FillController {
public:
  explicit FillController(const ControllerParams &params = {}) noexcept;

  // The stream (re)entered Running with this head age: the average restarts from it. The integral and the last ratio
  // are kept, because they hold the drift estimate of the device.
  void Seed(int64_t age) noexcept;
  // One tick's head age. Every update_ticks ticks it updates the ratio and returns true.
  bool Tick(int64_t age) noexcept;
  // While held (the silent bypass, §9.3) the average still follows every tick, but no update runs.
  void SetHold(bool hold) noexcept { hold_ = hold; }
  // The head age to hold from now on.
  void SetTarget(int64_t age) noexcept { p_.target_age_ns = age; }
  int64_t target_ns() const noexcept { return p_.target_age_ns; }
  bool held() const noexcept { return hold_; }

  int32_t steps() const noexcept { return steps_; }             // the ratio: (kRatioDen + steps) / kRatioDen
  int32_t ppm() const noexcept { return steps_ * kPpmPerStep; } // never 0
  double age_ns() const noexcept { return avg_ns_; }            // the smoothed head age
  double error_ms() const noexcept;                             // smoothed age - target
  double integral() const noexcept { return integral_; }        // ms*s
  double command_ppm() const noexcept { return command_; }      // the unquantized output
  const ControllerParams &params() const noexcept { return p_; }

private:
  void Update() noexcept;

  ControllerParams p_;
  double alpha_;
  double avg_ns_ = 0;
  bool seeded_ = false;
  bool hold_ = false;
  uint32_t ticks_ = 0;
  double integral_ = 0;
  double command_ = kPpmPerStep;
  int32_t steps_ = 1; // the resampler is created at +1 step
};

// How many input frames speexdsp's resampler consumes, exactly as speexdsp 1.2.1 computes it
// (speex_resampler_set_rate_frac's reduction and phase rescale, resampler_basic_interpolate_single's advance per
// output frame, speex_resampler_reset_mem): the stand-in of the simulations and LinearResampler, checked against
// speexdsp itself in mix_resampler_test.
class RateModel {
public:
  RateModel() noexcept; // as speex_resampler_init_frac at +1 step
  // speex_resampler_set_rate_frac(st, kRatioDen + steps, kRatioDen, ...).
  void SetSteps(int32_t steps) noexcept;
  // speex_resampler_reset_mem: position and phase back to 0.
  void Reset() noexcept;
  // Makes up to out_frames output frames from in_frames available input frames. With in and out it also writes the
  // input frame at each output's integer position (a sample-and-hold resampler with speexdsp's consumption); either
  // may be null to count only.
  void Run(const float *in, uint32_t in_frames, float *out, uint32_t out_frames, uint32_t *used,
           uint32_t *made) noexcept;
  // The input frames Run needs to make out_frames output frames.
  uint32_t InputFor(uint32_t out_frames) const noexcept;

  uint32_t num() const noexcept { return num_; }
  uint32_t den() const noexcept { return den_; }

private:
  uint32_t num_ = 0, den_ = 0;          // reduced, as speexdsp stores them
  uint32_t int_adv_ = 0, frac_adv_ = 0; // per output frame
  int64_t last_ = 0;                    // position of the next output relative to the next unconsumed input
  uint32_t frac_ = 0;                   // sub-sample phase, in 1/den_
};

} // namespace im::audiomix
