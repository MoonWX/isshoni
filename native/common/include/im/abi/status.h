// Status codes, detail tokens and the thread-local error detail of the C ABI (docs/m2/04-windows-engine.md §5.1,
// §5.7). Every export clears the detail first (ClearLastError) and, when it fails, returns Fail(...)'s status, so that
// im_last_error on the same OS thread reads "<token>: <detail>". The text is for logs only: the UI maps the status and
// the token, never the text.
#pragma once

#include <cstdint>
#include <span>
#include <string_view>

#include "isshoni_media.h"

#if defined(__GNUC__) || defined(__clang__)
#define IM_PRINTF_FORMAT(fmt, args) __attribute__((format(printf, fmt, args)))
#else
#define IM_PRINTF_FORMAT(fmt, args)
#endif

namespace im::abi {

// The detail tokens of 04 §5.7. They are fixed in v1: never renamed or reused; new ones are additive.
namespace token {
inline constexpr std::string_view kMissing = "missing";
inline constexpr std::string_view kVersion = "version";
inline constexpr std::string_view kBlockedByPolicy = "blocked_by_policy";
inline constexpr std::string_view kDriverTooOld = "driver_too_old";
inline constexpr std::string_view kDisabled = "disabled";
inline constexpr std::string_view kNoMediaFoundation = "no_media_foundation";
inline constexpr std::string_view kSlowOpen = "slow_open";
inline constexpr std::string_view kOpenFailed = "open_failed";
inline constexpr std::string_view kTestEncodeFailed = "test_encode_failed";
inline constexpr std::string_view kLayerFailed = "layer_failed";
inline constexpr std::string_view kForcedFailed = "forced_failed";
inline constexpr std::string_view kDeviceRemoved = "device_removed";
inline constexpr std::string_view kSessionLimit = "session_limit";
inline constexpr std::string_view kPickBusy = "pick_busy";
inline constexpr std::string_view kPickExpired = "pick_expired";
inline constexpr std::string_view kSelfWindow = "self_window";
inline constexpr std::string_view kOsTooOld = "os_too_old";
inline constexpr std::string_view kCaptureDenied = "capture_denied";
inline constexpr std::string_view kDpiUnaware = "dpi_unaware";
inline constexpr std::string_view kEngineTimeout = "engine_timeout";
inline constexpr std::string_view kNotInitialized = "not_initialized";
inline constexpr std::string_view kNotWatching = "not_watching";
inline constexpr std::string_view kAbi = "abi";
} // namespace token

struct TokenInfo {
  std::string_view token;
  im_status status; // the status the token comes with (self_window is Go only: IM_OK here)
};

// Every token of 04 §5.7 in the spec's order.
std::span<const TokenInfo> Tokens() noexcept;

// Whether s is one of the tokens of 04 §5.7.
bool IsToken(std::string_view s) noexcept;

// The constant's name ("IM_ERR_INVALID_ARG"), or "IM_UNKNOWN" for a value the header doesn't define. For logs.
std::string_view StatusName(im_status s) noexcept;

// The longest detail im_last_error returns, in bytes (longer details are cut on a UTF-8 boundary).
inline constexpr std::size_t kMaxErrorBytes = 1023;

// Clears this thread's detail. Every export calls it first.
void ClearLastError() noexcept;

// Records "<token>: <detail>" (or just <detail> when token is empty) as this thread's detail and returns status, so an
// export can `return Fail(IM_ERR_INVALID_ARG, {}, "im_start: cfg is NULL");`. An empty detail is replaced by the
// status's name ("not_initialized: IM_ERR_BAD_STATE"), so a token is always followed by ": ". Control characters in
// the text become spaces, so the detail is always one line.
im_status Fail(im_status status, std::string_view token, std::string_view detail) noexcept;

// Fail with a printf-style detail.
im_status Failf(im_status status, std::string_view token, const char *fmt, ...) noexcept IM_PRINTF_FORMAT(3, 4);

// This thread's detail ("" when the last export succeeded).
std::string_view LastErrorText() noexcept;

// im_last_error: copies this thread's detail with the snprintf rule (CopyOut).
int32_t LastError(char *buf, int32_t cap) noexcept;

// The snprintf rule of every JSON output (04 §5.1): returns the full length n of doc and, when cap > 0, writes
// min(n, cap - 1) bytes and a NUL. A negative cap, or a NULL buf with cap > 0, is IM_ERR_INVALID_ARG; a document longer
// than INT32_MAX is IM_ERR_INTERNAL. Neither sets the thread's detail: the export does.
int32_t CopyOut(std::string_view doc, char *buf, int32_t cap) noexcept;

} // namespace im::abi
