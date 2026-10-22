// Validation and copy of im_exclusions (docs/m2/05-windows-audio.md §4.3).
#include "im/audiopolicy/exclusions.h"

#include <string_view>

#include "im/abi/status.h"
#include "im/abi/struct_size.h"
#include "im/abi/utf8.h"
#include "im/audiopolicy/text.h"

namespace im::audiopolicy {
namespace {

constexpr uint32_t kKnownFlags = IM_EXCLF_KEEP_OUT_MIC_USERS | IM_EXCLF_FAST_PATH;

// strnlen without the CRT's "unsafe" warnings: the length of s, or limit when s has no NUL in its first limit bytes.
std::size_t BoundedLen(const char *s, std::size_t limit) noexcept {
  std::size_t n = 0;
  while (n < limit && s[n] != '\0') {
    ++n;
  }
  return n;
}

bool ParsePid(std::string_view v, uint32_t &pid) noexcept {
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
  if (n == 0 || n > 0xFFFFFFFFull) {
    return false;
  }
  pid = static_cast<uint32_t>(n);
  return true;
}

} // namespace

bool IsValidAppId(const char *s) noexcept {
  if (s == nullptr) {
    return false;
  }
  const std::size_t n = BoundedLen(s, IM_APP_ID_LEN);
  if (n == 0 || n >= IM_APP_ID_LEN) {
    return false;
  }
  for (std::size_t i = 0; i < n; ++i) {
    const char c = s[i];
    const bool ok = (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == ':' || c == '.' || c == '_' || c == '-';
    if (!ok) {
      return false;
    }
  }
  return true;
}

im_status ParseExclusions(const im_exclusions *ex, uint32_t self_pid, Exclusions &out) {
  im_exclusions in{};
  if (const im_status st = im::abi::ReadIn(ex, in, "im_exclusions"); st != IM_OK) {
    return st;
  }
  if (in.rule_count > IM_MAX_RULES) {
    return im::abi::Failf(IM_ERR_INVALID_ARG, {}, "im_exclusions: rule_count %u is above %u",
                          static_cast<unsigned>(in.rule_count), static_cast<unsigned>(IM_MAX_RULES));
  }
  if (in.rule_count > 0 && in.rules == nullptr) {
    return im::abi::Failf(IM_ERR_INVALID_ARG, {}, "im_exclusions: rules is NULL with rule_count %u",
                          static_cast<unsigned>(in.rule_count));
  }

  Exclusions result;
  result.flags = in.flags & kKnownFlags;
  result.shared_pid = in.shared_pid;
  result.rules.reserve(in.rule_count + 1);
  for (uint32_t i = 0; i < in.rule_count; ++i) {
    const im_rule &r = in.rules[i];
    const unsigned idx = static_cast<unsigned>(i);
    if (r.kind > IM_RULE_WEBVIEW_DATA_DIR) {
      return im::abi::Failf(IM_ERR_INVALID_ARG, {}, "im_exclusions.rules[%u]: unknown kind %u", idx,
                            static_cast<unsigned>(r.kind));
    }
    if (r.source > IM_RULE_SRC_DATA) {
      return im::abi::Failf(IM_ERR_INVALID_ARG, {}, "im_exclusions.rules[%u]: unknown source %u", idx,
                            static_cast<unsigned>(r.source));
    }
    if (r.value == nullptr) {
      return im::abi::Failf(IM_ERR_INVALID_ARG, {}, "im_exclusions.rules[%u]: value is NULL", idx);
    }
    const std::size_t vlen = BoundedLen(r.value, IM_MAX_RULE_VALUE);
    if (vlen == 0) {
      return im::abi::Failf(IM_ERR_INVALID_ARG, {}, "im_exclusions.rules[%u]: value is empty", idx);
    }
    if (vlen >= IM_MAX_RULE_VALUE) {
      return im::abi::Failf(IM_ERR_INVALID_ARG, {}, "im_exclusions.rules[%u]: value is %u bytes or longer", idx,
                            static_cast<unsigned>(IM_MAX_RULE_VALUE));
    }
    const std::string_view value(r.value, vlen);
    if (!im::abi::IsValidUtf8(value)) {
      return im::abi::Failf(IM_ERR_INVALID_ARG, {}, "im_exclusions.rules[%u]: value is not valid UTF-8", idx);
    }
    Rule rule;
    rule.kind = r.kind;
    rule.source = r.source;
    switch (r.kind) {
    case IM_RULE_INSTANCE:
      if (!ParsePid(value, rule.pid)) {
        return im::abi::Failf(IM_ERR_INVALID_ARG, {}, "im_exclusions.rules[%u]: malformed pid", idx);
      }
      rule.value = std::string(value);
      break;
    case IM_RULE_WEBVIEW_DATA_DIR:
      rule.value = NormalizePath(value);
      if (rule.value.empty()) {
        return im::abi::Failf(IM_ERR_INVALID_ARG, {}, "im_exclusions.rules[%u]: the folder is only separators", idx);
      }
      break;
    default:
      rule.value = FoldCase(value);
      break;
    }
    if (r.app_id != nullptr && r.app_id[0] != '\0') {
      if (!IsValidAppId(r.app_id)) {
        return im::abi::Failf(IM_ERR_INVALID_ARG, {}, "im_exclusions.rules[%u]: app_id is not [a-z0-9:._-]{1,63}", idx);
      }
      rule.app_id = r.app_id;
    } else if (r.kind == IM_RULE_INSTANCE || r.kind == IM_RULE_WEBVIEW_DATA_DIR) {
      rule.app_id = kSelfAppId;
    } else {
      rule.app_id = "exe:" + rule.value;
    }
    if (r.name != nullptr) {
      // Only the kept prefix is read and checked: IM_MAX_RULE_VALUE - 1 bytes plus enough to find a UTF-8 boundary.
      const std::string_view whole(r.name, BoundedLen(r.name, IM_MAX_RULE_VALUE + 4));
      const std::string_view name = whole.substr(0, im::abi::Utf8PrefixLen(whole, IM_MAX_RULE_VALUE - 1));
      if (!im::abi::IsValidUtf8(name)) {
        return im::abi::Failf(IM_ERR_INVALID_ARG, {}, "im_exclusions.rules[%u]: name is not valid UTF-8", idx);
      }
      rule.name = std::string(name);
    }
    result.rules.push_back(std::move(rule));
  }
  if (self_pid != 0) {
    Rule self;
    self.kind = IM_RULE_INSTANCE;
    self.source = IM_RULE_SRC_SELF;
    self.value = std::to_string(self_pid);
    self.pid = self_pid;
    self.app_id = kSelfAppId;
    self.name = "isshoni";
    result.rules.push_back(std::move(self));
  }
  out = std::move(result);
  return IM_OK;
}

} // namespace im::audiopolicy
