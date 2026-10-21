// Encoder selection (docs/m2/04-windows-engine.md §9.1, §9.6–§9.8): the candidate order, opening one encoder per layer
// with one profile for the whole session, and what happens when an encoder fails mid-session. Pure policy: the
// engine supplies the probe's encoder list and an Opener that really opens encoders (the selector in native/windows
// wires it; tests use a fake).
//
// Order (IM_ENC_AUTO; the probe's encoders[] lists it the same way):
//   1. the vendor encoder (NVENC, QSV, AMF) on the capture adapter (DD: the output's adapter; WGC: the adapter of the
//      monitor that holds most of the source, the primary when unknown);
//   2. MF hardware on that adapter;
//   3. vendor encoders, then MF hardware, on the other adapters, in adapter order (for DD these need a GPU -> CPU ->
//      GPU copy, Candidate::cross_adapter_copy, and come after 1-2 only);
//   4. MF software;
//   5. OpenH264, if enabled (its probe status is IM_OK only then).
// Only encoders whose probe status is IM_OK take part. A forced kind (im_start_config.encoder) keeps only that kind,
// and its failure is IM_ERR_NO_ENCODER with forced_failed, never a silent fallback. Under test hooks a test: source
// puts IM_ENC_FAKE first (04 §18.2).
#pragma once

#include <array>
#include <cstddef>
#include <cstdint>
#include <span>
#include <string_view>

#include "isshoni_media.h"

namespace im::encpolicy {

inline constexpr uint32_t kNoAdapter = 0xFFFFFFFFu; // im_encoder_info.adapter_index of software encoders

struct Candidate {
  uint32_t kind = IM_ENC_AUTO;
  uint32_t adapter_index = kNoAdapter;
  uint32_t flags = 0;              // IM_ENCF_* from the probe
  bool cross_adapter_copy = false; // DD with an encoder on another adapter: frames go through the CPU
  uint32_t probe_index = 0;        // index into the probe's encoders[]
};

struct CandidateQuery {
  uint32_t capture_path = IM_CAPTURE_WGC; // IM_CAPTURE_*
  uint32_t capture_adapter = kNoAdapter;  // see the order above; kNoAdapter: every adapter counts as "other"
  uint32_t forced = IM_ENC_AUTO;          // im_start_config.encoder
  bool test_source = false;               // a test: source: IM_ENC_FAKE first (test hooks)
};

// Writes the candidates for a session in order into out and returns how many (at most out.size()).
std::size_t BuildCandidates(std::span<const im_encoder_info> probe, const CandidateQuery &query,
                            std::span<Candidate> out) noexcept;

// Whether the candidate can encode the profile (IM_PROFILE_HIGH or IM_PROFILE_CB): the probe's IM_ENCF_HIGH/CB flags,
// or the kind's traits when the probe recorded neither.
bool Supports(const Candidate &c, uint32_t profile) noexcept;

// Opens and closes encoders for the policy. Implemented by the engine's selector (and by tests).
class Opener {
public:
  virtual ~Opener() = default;
  // Opens c's encoder for the layer with the session profile; replaces whatever the layer had. IM_OK, IM_ERR_LIMIT for
  // a session limit (NVENC's), or another error status.
  virtual im_status Open(uint32_t layer, const Candidate &c, uint32_t profile) = 0;
  // Closes the layer's encoder (a switch of the whole session to CB re-opens every layer).
  virtual void Close(uint32_t layer) = 0;
  // Waits before the one retry of a further layer (500 ms in the engine).
  virtual void Wait(uint32_t ms) = 0;
};

inline constexpr uint32_t kLayerRetryWaitMs = 500;

struct LayerChoice {
  std::size_t candidate = 0; // index into the candidate list
  uint32_t kind = IM_ENC_AUTO;
};

struct Selection {
  im_status status = IM_OK; // IM_OK or IM_ERR_NO_ENCODER
  std::string_view token;   // "layer_failed" or "forced_failed" when status != IM_OK
  uint32_t failed_layer = IM_NO_LAYER;
  uint32_t profile = 0; // the session profile every layer runs (CB when only a CB encoder opened somewhere)
  uint32_t layer_count = 0;
  std::array<LayerChoice, IM_MAX_LAYERS> layers{};
};

// Opens every layer (layer 0 first): each takes the first candidate in order that supports the session profile and
// opens. A further layer whose candidates all fail is retried once after kLayerRetryWaitMs. If a layer can't open
// with High, every opened layer is closed and the whole session tries again with CB (im_start never returns a mixed
// session; OpenH264 is CB only). Nothing opened stays open on failure. forced: the candidates are one forced kind.
Selection SelectEncoders(std::span<const Candidate> candidates, uint32_t profile, uint32_t layer_count, bool forced,
                         Opener &opener);

// Why a running layer's encoder is given up (§9.4, §9.8).
enum class Failure : uint8_t {
  kEncodeError, // avcodec_send_frame/receive_packet failed, the device is fine: re-open the same encoder once first
  kBadStream,   // the output broke the session's rules (a profile mismatch): straight to the next candidate
  kReopenFailed // a re-init's re-open failed (cause says why): straight to the next candidate
};

struct Recovery {
  enum class Outcome : uint8_t {
    kSame,     // the same encoder re-opened: REINIT|KEYFRAME, no event
    kChanged,  // another candidate opened: ENCODER_CHANGED (when kind_changed) before its first REINIT|KEYFRAME
    kExhausted // nothing left for the layer: the session ends with IM_ERR_ENCODER_LOST
  };
  Outcome outcome = Outcome::kExhausted;
  std::size_t candidate = 0; // the layer's candidate now
  uint32_t kind = IM_ENC_AUTO;
  bool kind_changed = false;
  std::string_view reason; // ENCODER_CHANGED's reason: "encode_error", "open_failed" or "session_limit"
};

// Recovers a layer whose encoder failed (the failed encoder is already closed). A device removal is not a Failure: it
// ends the session (§9.8). cause: the status of the failed re-open for kReopenFailed (IM_ERR_LIMIT: session_limit).
Recovery RecoverLayer(std::span<const Candidate> candidates, std::size_t current, uint32_t layer, uint32_t profile,
                      Failure why, im_status cause, Opener &opener);

} // namespace im::encpolicy
