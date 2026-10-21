#include "api/validate.h"

#include <cstdio>
#include <cstring>
#include <string.h>

#include "im/abi/status.h"
#include "im/abi/struct_size.h"
#include "im/abi/utf8.h"

namespace im::api {
namespace {

using abi::Failf;

constexpr std::size_t kMaxPathBytes = 32767; // Windows' longest path, in UTF-16 units, as a bound on the UTF-8 bytes
constexpr std::size_t kSourceIdBytes = sizeof(im_source_info::id);

#if defined(IM_TEST_HOOKS) && IM_TEST_HOOKS
constexpr bool kTestHooks = true;
#else
constexpr bool kTestHooks = false;
#endif

// The length of a caller's NUL-terminated string, reading at most max bytes (max when no NUL came first).
std::size_t BoundedLen(const char *s, std::size_t max) noexcept { return ::strnlen(s, max); }

bool IsAppIdChar(char c) noexcept {
  return (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == ':' || c == '.' || c == '_' || c == '-';
}

bool IsDecimalPid(std::string_view v) noexcept {
  if (v.empty() || v.size() > 10) {
    return false;
  }
  uint64_t n = 0;
  for (const char c : v) {
    if (c < '0' || c > '9') {
      return false;
    }
    n = n * 10 + static_cast<uint64_t>(c - '0');
  }
  return n >= 1 && n <= 0xFFFFFFFFull;
}

im_status CheckRule(const char *fn, const char *what, uint32_t i, const im_rule &r) noexcept {
  const unsigned idx = i;
  if (r.kind > IM_RULE_WEBVIEW_DATA_DIR) {
    return Failf(IM_ERR_INVALID_ARG, {}, "%s: %s->rules[%u].kind: unknown kind %u", fn, what, idx,
                 static_cast<unsigned>(r.kind));
  }
  if (r.source > IM_RULE_SRC_DATA) {
    return Failf(IM_ERR_INVALID_ARG, {}, "%s: %s->rules[%u].source: unknown source %u", fn, what, idx,
                 static_cast<unsigned>(r.source));
  }
  if (r.value == nullptr) {
    return Failf(IM_ERR_INVALID_ARG, {}, "%s: %s->rules[%u].value is NULL", fn, what, idx);
  }
  const std::size_t value_len = BoundedLen(r.value, IM_MAX_RULE_VALUE);
  if (value_len == 0) {
    return Failf(IM_ERR_INVALID_ARG, {}, "%s: %s->rules[%u].value is empty", fn, what, idx);
  }
  if (value_len >= IM_MAX_RULE_VALUE) {
    return Failf(IM_ERR_INVALID_ARG, {}, "%s: %s->rules[%u].value is not shorter than %u bytes", fn, what, idx,
                 IM_MAX_RULE_VALUE);
  }
  const std::string_view value(r.value, value_len);
  if (!abi::IsValidUtf8(value)) {
    return Failf(IM_ERR_INVALID_ARG, {}, "%s: %s->rules[%u].value is not UTF-8", fn, what, idx);
  }
  if (r.kind == IM_RULE_INSTANCE && !IsDecimalPid(value)) {
    return Failf(IM_ERR_INVALID_ARG, {}, "%s: %s->rules[%u].value is not a decimal pid", fn, what, idx);
  }
  if (r.app_id != nullptr) {
    const std::size_t n = BoundedLen(r.app_id, IM_APP_ID_LEN);
    if (n >= IM_APP_ID_LEN) {
      return Failf(IM_ERR_INVALID_ARG, {}, "%s: %s->rules[%u].app_id is not shorter than %u bytes", fn, what, idx,
                   IM_APP_ID_LEN);
    }
    for (std::size_t k = 0; k < n; ++k) {
      if (!IsAppIdChar(r.app_id[k])) {
        return Failf(IM_ERR_INVALID_ARG, {}, "%s: %s->rules[%u].app_id has a character outside [a-z0-9:._-]", fn, what,
                     idx);
      }
    }
  }
  if (r.name != nullptr) {
    const std::size_t n = BoundedLen(r.name, IM_MAX_RULE_VALUE);
    if (n >= IM_MAX_RULE_VALUE || !abi::IsValidUtf8(std::string_view(r.name, n))) {
      return Failf(IM_ERR_INVALID_ARG, {}, "%s: %s->rules[%u].name is not UTF-8 shorter than %u bytes", fn, what, idx,
                   IM_MAX_RULE_VALUE);
    }
  }
  return IM_OK;
}

} // namespace

im_status CheckJsonOut(const char *fn, const char *buf, int32_t cap) noexcept {
  if (cap < 0) {
    return Failf(IM_ERR_INVALID_ARG, {}, "%s: cap %d is negative", fn, static_cast<int>(cap));
  }
  if (cap > 0 && buf == nullptr) {
    return Failf(IM_ERR_INVALID_ARG, {}, "%s: the buffer is NULL with cap %d", fn, static_cast<int>(cap));
  }
  return IM_OK;
}

int32_t JsonOut(const char *fn, std::string_view doc, char *buf, int32_t cap) noexcept {
  const int32_t n = abi::CopyOut(doc, buf, cap);
  if (n < 0) {
    return Failf(n, {}, "%s: the document can't be returned (%zu bytes)", fn, doc.size());
  }
  return n;
}

bool IsAbsoluteWindowsPath(std::string_view p) noexcept {
  const auto sep = [](char c) { return c == '\\' || c == '/'; };
  if (p.size() >= 3 && ((p[0] >= 'A' && p[0] <= 'Z') || (p[0] >= 'a' && p[0] <= 'z')) && p[1] == ':' && sep(p[2])) {
    return true;
  }
  return p.size() >= 3 && sep(p[0]) && sep(p[1]) && !sep(p[2]);
}

im_status ReadAbsolutePath(const char *fn, const char *what, const char *path, std::optional<std::string> &out) {
  out.reset();
  if (path == nullptr || path[0] == '\0') {
    return IM_OK;
  }
  const std::size_t n = BoundedLen(path, kMaxPathBytes);
  if (n >= kMaxPathBytes) {
    return Failf(IM_ERR_INVALID_ARG, {}, "%s: %s is longer than a Windows path", fn, what);
  }
  const std::string_view p(path, n);
  if (!abi::IsValidUtf8(p)) {
    return Failf(IM_ERR_INVALID_ARG, {}, "%s: %s is not UTF-8", fn, what);
  }
  if (!IsAbsoluteWindowsPath(p)) {
    return Failf(IM_ERR_INVALID_ARG, {}, "%s: %s is not an absolute path", fn, what);
  }
  out.emplace(p);
  return IM_OK;
}

im_status CheckExclusions(const char *fn, const char *what, const im_exclusions *ex) noexcept {
  if (ex == nullptr) {
    return Failf(IM_ERR_INVALID_ARG, {}, "%s: %s is NULL", fn, what);
  }
  if (ex->struct_size < abi::V1Size<im_exclusions>::value) {
    return Failf(IM_ERR_ABI, abi::token::kAbi, "%s: %s: struct_size %u is below the v1.0 size %u", fn, what,
                 static_cast<unsigned>(ex->struct_size), static_cast<unsigned>(abi::V1Size<im_exclusions>::value));
  }
  if (ex->rule_count > IM_MAX_RULES) {
    return Failf(IM_ERR_INVALID_ARG, {}, "%s: %s->rule_count %u is above %u", fn, what,
                 static_cast<unsigned>(ex->rule_count), IM_MAX_RULES);
  }
  if (ex->rule_count > 0 && ex->rules == nullptr) {
    return Failf(IM_ERR_INVALID_ARG, {}, "%s: %s->rules is NULL with rule_count %u", fn, what,
                 static_cast<unsigned>(ex->rule_count));
  }
  for (uint32_t i = 0; i < ex->rule_count; ++i) {
    if (const im_status st = CheckRule(fn, what, i, ex->rules[i]); st != IM_OK) {
      return st;
    }
  }
  return IM_OK;
}

im_status CheckAudioConfig(const char *fn, const im_audio_config &a) noexcept {
  switch (a.mode) {
  case IM_AUDIO_MODE_OFF:
  case IM_AUDIO_MODE_EXCLUDE_APPS:
  case IM_AUDIO_MODE_TEST_TONE:
    break;
  case IM_AUDIO_MODE_EVERYTHING:
    return Failf(IM_ERR_INVALID_ARG, {}, "%s: cfg->audio.mode IM_AUDIO_MODE_EVERYTHING is reserved and refused in M2",
                 fn);
  default:
    return Failf(IM_ERR_INVALID_ARG, {}, "%s: cfg->audio.mode: unknown mode %u", fn, static_cast<unsigned>(a.mode));
  }
  if (a.bitrate_bps != 0 && (a.bitrate_bps < 32000 || a.bitrate_bps > 510000)) {
    return Failf(IM_ERR_INVALID_ARG, {}, "%s: cfg->audio.bitrate_bps %u is outside 32000..510000", fn,
                 static_cast<unsigned>(a.bitrate_bps));
  }
  if (a.frame_ms != 0 && a.frame_ms != 20) {
    return Failf(IM_ERR_INVALID_ARG, {}, "%s: cfg->audio.frame_ms %u: only 20 ms frames exist in v1", fn,
                 static_cast<unsigned>(a.frame_ms));
  }
  return IM_OK;
}

im_status CheckLayerConfig(const char *fn, const char *what, const im_layer_config &l) noexcept {
  if (std::memchr(l.rid, '\0', sizeof l.rid) == nullptr || l.rid[0] == '\0') {
    return Failf(IM_ERR_INVALID_ARG, {}, "%s: %s.rid is empty or not NUL-terminated within %zu bytes", fn, what,
                 sizeof l.rid);
  }
  if (l.fps < 1 || l.fps > 240) {
    return Failf(IM_ERR_INVALID_ARG, {}, "%s: %s.fps %u is outside 1..240", fn, what, static_cast<unsigned>(l.fps));
  }
  if (l.gop_ms != 0 && (l.gop_ms < 1000 || l.gop_ms > 10000)) {
    return Failf(IM_ERR_INVALID_ARG, {}, "%s: %s.gop_ms %u is outside 1000..10000", fn, what,
                 static_cast<unsigned>(l.gop_ms));
  }
  return IM_OK;
}

im_status CheckStartConfig(const char *fn, const im_start_config &c) noexcept {
  if (c.layer_count > IM_MAX_LAYERS) {
    return Failf(IM_ERR_INVALID_ARG, {}, "%s: cfg->layer_count %u is above %u", fn,
                 static_cast<unsigned>(c.layer_count), IM_MAX_LAYERS);
  }
  if (c.source_id == nullptr && c.layer_count > 0) {
    return Failf(IM_ERR_INVALID_ARG, {}, "%s: cfg->source_id is NULL with %u video layers", fn,
                 static_cast<unsigned>(c.layer_count));
  }
  if (c.source_id != nullptr) {
    const std::size_t n = BoundedLen(c.source_id, kSourceIdBytes);
    if (n == 0 || n >= kSourceIdBytes || !abi::IsValidUtf8(std::string_view(c.source_id, n))) {
      return Failf(IM_ERR_INVALID_ARG, {}, "%s: cfg->source_id is not a source id (UTF-8, 1..%zu bytes)", fn,
                   kSourceIdBytes - 1);
    }
    if (c.layer_count == 0) {
      return Failf(IM_ERR_INVALID_ARG, {}, "%s: cfg->source_id is set but layer_count is 0", fn);
    }
    if (c.profile != IM_PROFILE_HIGH && c.profile != IM_PROFILE_CB) {
      return Failf(IM_ERR_INVALID_ARG, {}, "%s: cfg->profile 0x%04X is neither IM_PROFILE_HIGH nor IM_PROFILE_CB", fn,
                   static_cast<unsigned>(c.profile));
    }
  }
  if (c.content_hint > IM_CONTENT_DETAIL) {
    return Failf(IM_ERR_INVALID_ARG, {}, "%s: cfg->content_hint: unknown hint %u", fn,
                 static_cast<unsigned>(c.content_hint));
  }
  if (c.encoder == IM_ENC_FAKE) {
    if constexpr (!kTestHooks) {
      return Failf(IM_ERR_INVALID_ARG, {}, "%s: cfg->encoder IM_ENC_FAKE exists in IM_TEST_HOOKS builds only", fn);
    }
  } else if (c.encoder > IM_ENC_GSTREAMER) {
    return Failf(IM_ERR_INVALID_ARG, {}, "%s: cfg->encoder: unknown encoder %u", fn, static_cast<unsigned>(c.encoder));
  }
  for (uint32_t i = 0; i < c.layer_count; ++i) {
    char what[24];
    std::snprintf(what, sizeof what, "cfg->layers[%u]", static_cast<unsigned>(i));
    if (const im_status st = CheckLayerConfig(fn, what, c.layers[i]); st != IM_OK) {
      return st;
    }
    for (uint32_t k = 0; k < i; ++k) {
      if (std::strncmp(c.layers[k].rid, c.layers[i].rid, sizeof c.layers[i].rid) == 0) {
        return Failf(IM_ERR_INVALID_ARG, {}, "%s: cfg->layers[%u] repeats the rid of layers[%u]", fn,
                     static_cast<unsigned>(i), static_cast<unsigned>(k));
      }
    }
  }
  if (const im_status st = CheckAudioConfig(fn, c.audio); st != IM_OK) {
    return st;
  }
  if (c.exclusions != nullptr) {
    return CheckExclusions(fn, "cfg->exclusions", c.exclusions);
  }
  return IM_OK;
}

im_status CheckLayerIndex(const char *fn, uint32_t layer, bool all_ok) noexcept {
  if (layer < IM_MAX_LAYERS || (all_ok && layer == IM_ALL_LAYERS)) {
    return IM_OK;
  }
  return Failf(IM_ERR_INVALID_ARG, {}, "%s: layer %u is not a video layer%s", fn, static_cast<unsigned>(layer),
               all_ok ? " or IM_ALL_LAYERS" : "");
}

im_status CheckBool(const char *fn, const char *what, uint32_t value) noexcept {
  if (value <= 1) {
    return IM_OK;
  }
  return Failf(IM_ERR_INVALID_ARG, {}, "%s: %s is %u, not 0 or 1", fn, what, static_cast<unsigned>(value));
}

} // namespace im::api
