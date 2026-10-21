#include "im/abi/status.h"

#include <array>
#include <cstdarg>
#include <cstdio>
#include <cstring>
#include <functional>
#include <limits>

#include "im/abi/utf8.h"

namespace im::abi {
namespace {

constexpr std::array<TokenInfo, 23> kTokens{{
    {token::kMissing, IM_ERR_DEPENDENCY},
    {token::kVersion, IM_ERR_DEPENDENCY},
    {token::kBlockedByPolicy, IM_ERR_DEPENDENCY},
    {token::kDriverTooOld, IM_ERR_DEPENDENCY},
    {token::kDisabled, IM_ERR_DEPENDENCY},
    {token::kNoMediaFoundation, IM_ERR_DEPENDENCY},
    {token::kSlowOpen, IM_ERR_NO_ENCODER},
    {token::kOpenFailed, IM_ERR_NO_ENCODER},
    {token::kTestEncodeFailed, IM_ERR_NO_ENCODER},
    {token::kLayerFailed, IM_ERR_NO_ENCODER},
    {token::kForcedFailed, IM_ERR_NO_ENCODER},
    {token::kDeviceRemoved, IM_ERR_ENCODER_LOST},
    {token::kSessionLimit, IM_ERR_LIMIT},
    {token::kPickBusy, IM_ERR_LIMIT},
    {token::kPickExpired, IM_ERR_SOURCE_GONE},
    {token::kSelfWindow, IM_OK},
    {token::kOsTooOld, IM_ERR_UNSUPPORTED_OS},
    {token::kCaptureDenied, IM_ERR_PERMISSION_DENIED},
    {token::kDpiUnaware, IM_ERR_INTERNAL},
    {token::kEngineTimeout, IM_ERR_TIMEOUT},
    {token::kNotInitialized, IM_ERR_BAD_STATE},
    {token::kNotWatching, IM_ERR_BAD_STATE},
    {token::kAbi, IM_ERR_ABI},
}};

// The detail lives in a fixed thread-local buffer: no allocation and no destructor on thread exit.
struct ErrorBuffer {
  char text[kMaxErrorBytes + 1];
  std::size_t len;
};
thread_local ErrorBuffer t_error{};

// Appends s to the buffer, cut on a UTF-8 boundary when it doesn't fit; returns false once the buffer is full.
bool Append(ErrorBuffer &b, std::string_view s) noexcept {
  const std::size_t room = kMaxErrorBytes - b.len;
  const std::size_t n = Utf8PrefixLen(s, room);
  std::memcpy(b.text + b.len, s.data(), n);
  b.len += n;
  b.text[b.len] = '\0';
  return n == s.size();
}

// Starts a detail with "<token>: " ("<token>" without a detail to follow; nothing for an empty token).
void Begin(ErrorBuffer &b, std::string_view tok, bool more) noexcept {
  b.len = 0;
  b.text[0] = '\0';
  if (!tok.empty()) {
    Append(b, tok);
    if (more) {
      Append(b, ": ");
    }
  }
}

// One line only: control characters (CR, LF, tab, ...) become spaces.
void Flatten(ErrorBuffer &b) noexcept {
  for (std::size_t i = 0; i < b.len; ++i) {
    const auto c = static_cast<unsigned char>(b.text[i]);
    if (c < 0x20u || c == 0x7Fu) {
      b.text[i] = ' ';
    }
  }
}

} // namespace

std::span<const TokenInfo> Tokens() noexcept { return kTokens; }

bool IsToken(std::string_view s) noexcept {
  for (const auto &t : kTokens) {
    if (t.token == s) {
      return true;
    }
  }
  return false;
}

std::string_view StatusName(im_status s) noexcept {
  switch (s) {
  case IM_OK:
    return "IM_OK";
  case IM_TIMEOUT:
    return "IM_TIMEOUT";
  case IM_ENDED:
    return "IM_ENDED";
  case IM_ERR_INVALID_ARG:
    return "IM_ERR_INVALID_ARG";
  case IM_ERR_BAD_STATE:
    return "IM_ERR_BAD_STATE";
  case IM_ERR_ABI:
    return "IM_ERR_ABI";
  case IM_ERR_UNSUPPORTED_OS:
    return "IM_ERR_UNSUPPORTED_OS";
  case IM_ERR_PERMISSION_DENIED:
    return "IM_ERR_PERMISSION_DENIED";
  case IM_ERR_AUDIO_DEVICE_CHANGED:
    return "IM_ERR_AUDIO_DEVICE_CHANGED";
  case IM_ERR_SOURCE_GONE:
    return "IM_ERR_SOURCE_GONE";
  case IM_ERR_ENCODER_LOST:
    return "IM_ERR_ENCODER_LOST";
  case IM_ERR_NO_ENCODER:
    return "IM_ERR_NO_ENCODER";
  case IM_ERR_CANCELLED:
    return "IM_ERR_CANCELLED";
  case IM_ERR_LIMIT:
    return "IM_ERR_LIMIT";
  case IM_ERR_DEPENDENCY:
    return "IM_ERR_DEPENDENCY";
  case IM_ERR_AUDIO_UNAVAILABLE:
    return "IM_ERR_AUDIO_UNAVAILABLE";
  case IM_ERR_NOT_IMPLEMENTED:
    return "IM_ERR_NOT_IMPLEMENTED";
  case IM_ERR_TIMEOUT:
    return "IM_ERR_TIMEOUT";
  case IM_ERR_INTERNAL:
    return "IM_ERR_INTERNAL";
  default:
    return "IM_UNKNOWN";
  }
}

void ClearLastError() noexcept {
  t_error.len = 0;
  t_error.text[0] = '\0';
}

im_status Fail(im_status status, std::string_view tok, std::string_view detail) noexcept {
  ErrorBuffer &b = t_error;
  // The detail may be (part of) the current one, e.g. LastErrorText() wrapped by an outer call: copy it first.
  char copy[kMaxErrorBytes + 1];
  const std::less<const char *> before;
  if (!detail.empty() && !before(detail.data(), b.text) && before(detail.data(), b.text + sizeof b.text)) {
    const std::size_t n = detail.size() < kMaxErrorBytes ? detail.size() : kMaxErrorBytes;
    std::memmove(copy, detail.data(), n);
    detail = std::string_view(copy, n);
  }
  Begin(b, tok, !detail.empty());
  Append(b, detail);
  Flatten(b);
  return status;
}

im_status Failf(im_status status, std::string_view tok, const char *fmt, ...) noexcept {
  // Format first: an argument may point into the current detail, which Begin overwrites.
  char tmp[kMaxErrorBytes + 1];
  std::size_t len = 0;
  if (fmt != nullptr) {
    va_list ap;
    va_start(ap, fmt);
    const int n = std::vsnprintf(tmp, sizeof tmp, fmt, ap);
    va_end(ap);
    if (n > 0) {
      len = static_cast<std::size_t>(n) < sizeof tmp ? static_cast<std::size_t>(n) : sizeof tmp - 1;
    }
  }
  ErrorBuffer &b = t_error;
  Begin(b, tok, len > 0);
  Append(b, std::string_view(tmp, len));
  Flatten(b);
  return status;
}

std::string_view LastErrorText() noexcept { return std::string_view(t_error.text, t_error.len); }

int32_t LastError(char *buf, int32_t cap) noexcept { return CopyOut(LastErrorText(), buf, cap); }

int32_t CopyOut(std::string_view doc, char *buf, int32_t cap) noexcept {
  if (cap < 0 || (cap > 0 && buf == nullptr)) {
    return IM_ERR_INVALID_ARG;
  }
  if (doc.size() > static_cast<std::size_t>(std::numeric_limits<int32_t>::max())) {
    return IM_ERR_INTERNAL;
  }
  const auto n = static_cast<int32_t>(doc.size());
  if (cap > 0) {
    const int32_t w = n < cap ? n : cap - 1;
    std::memcpy(buf, doc.data(), static_cast<std::size_t>(w));
    buf[w] = '\0';
  }
  return n;
}

} // namespace im::abi
