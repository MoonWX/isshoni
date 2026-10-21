// The process-wide engine state (docs/m2/04-windows-engine.md §5.3, §6.1): im_init and im_shutdown, the options
// im_init copied, and the engine worker. Every export except the few callable before im_init (im_version,
// im_build_info, im_clock, im_last_error, im_read_logs, im_abi_layout, im_test_opus_decode) checks RequireInit first.
//
// The engine object is created once and never destroyed: threads may still run (or be abandoned) at process exit, and
// the DLL is never unloaded (04 §5.3 im_shutdown: FreeLibrary is not called).
#pragma once

#include <chrono>
#include <cstdint>
#include <optional>
#include <string>

#include "engine/worker.h"
#include "isshoni_media.h"

namespace im::engine {

// What im_init copied from im_init_params (04 §5.1: the engine keeps no pointer into caller memory).
struct Options {
  uint32_t init_flags = 0;                  // IM_INIT_*
  uint32_t log_level = IM_LOG_INFO;         // IM_LOG_*
  uint32_t caller_abi = 0;                  // the caller's IM_ABI_VERSION (04 §5.5: a minor mismatch is logged)
  std::optional<std::string> ffmpeg_dir;    // absolute, UTF-8; none = the engine DLL's own folder
  std::optional<std::string> openh264_path; // absolute, UTF-8; none = OpenH264 off
};

// How long an export waits for its posted job: 04 §5.3's "Blocks" value + 1 s (04 §6.1, §6.4).
namespace bound {
inline constexpr std::chrono::milliseconds kInit{500 + 1000};
inline constexpr std::chrono::milliseconds kShutdown{3000 + 1000};
inline constexpr std::chrono::milliseconds kSetOpenH264Path{50 + 1000};
inline constexpr std::chrono::milliseconds kProbeQuick{100 + 1000};
inline constexpr std::chrono::milliseconds kProbeFull{2000 + 1000};
inline constexpr std::chrono::milliseconds kListSources{50 + 1000};
inline constexpr std::chrono::milliseconds kStart{3000 + 1000};
// How long im_shutdown waits for the worker thread to exit after its last job.
inline constexpr std::chrono::milliseconds kWorkerExit{1000};
} // namespace bound

// im_init after its argument checks: sets the log level, clears libvpl's search paths (04 §13.3), starts the worker
// and runs 05's AudioEngine::Init on it. A second im_init without im_shutdown is IM_ERR_BAD_STATE.
im_status Init(Options options);

// im_shutdown: stops every session, cancels a pick, runs AudioEngine::Shutdown on the worker, then stops the worker.
// A no-op when not initialized; im_init may follow.
void Shutdown() noexcept;

bool Initialized() noexcept;

// IM_OK when initialized, else IM_ERR_BAD_STATE with "not_initialized: <fn>: call im_init first".
im_status RequireInit(const char *fn) noexcept;

// A copy of the options of the current im_init (defaults when not initialized).
Options CurrentOptions();

// The engine worker. Posting to it before im_init (or after im_shutdown) gives IM_ERR_BAD_STATE not_initialized.
Worker &TheWorker() noexcept;

} // namespace im::engine
