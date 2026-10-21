#include "im/abi/utf8.h"

#include <cstring>

namespace im::abi {
namespace {

bool IsCont(unsigned char c) { return (c & 0xC0u) == 0x80u; }

} // namespace

char32_t DecodeUtf8(std::string_view s, std::size_t i, std::size_t &len) noexcept {
  const auto b0 = static_cast<unsigned char>(s[i]);
  len = 1;
  if (b0 < 0x80u) {
    return b0;
  }
  // The valid range of the second byte depends on the first (Unicode Table 3-7); later bytes are 80..BF.
  std::size_t need = 0;
  unsigned char lo = 0x80u;
  unsigned char hi = 0xBFu;
  char32_t cp = 0;
  if (b0 >= 0xC2u && b0 <= 0xDFu) {
    need = 1;
    cp = b0 & 0x1Fu;
  } else if (b0 >= 0xE0u && b0 <= 0xEFu) {
    need = 2;
    cp = b0 & 0x0Fu;
    if (b0 == 0xE0u) {
      lo = 0xA0u; // no overlongs
    } else if (b0 == 0xEDu) {
      hi = 0x9Fu; // no surrogates
    }
  } else if (b0 >= 0xF0u && b0 <= 0xF4u) {
    need = 3;
    cp = b0 & 0x07u;
    if (b0 == 0xF0u) {
      lo = 0x90u; // no overlongs
    } else if (b0 == 0xF4u) {
      hi = 0x8Fu; // nothing above U+10FFFF
    }
  } else {
    return kReplacementChar; // 80..C1, F5..FF never start a sequence
  }
  for (std::size_t k = 1; k <= need; ++k) {
    if (i + k >= s.size()) {
      len = k;
      return kReplacementChar;
    }
    const auto b = static_cast<unsigned char>(s[i + k]);
    const unsigned char min = (k == 1) ? lo : 0x80u;
    const unsigned char max = (k == 1) ? hi : 0xBFu;
    if (b < min || b > max) {
      len = k;
      return kReplacementChar;
    }
    cp = (cp << 6) | (b & 0x3Fu);
  }
  len = need + 1;
  return cp;
}

void AppendUtf8(std::string &out, char32_t cp) {
  if ((cp >= 0xD800u && cp <= 0xDFFFu) || cp > 0x10FFFFu) {
    cp = kReplacementChar;
  }
  if (cp < 0x80u) {
    out.push_back(static_cast<char>(cp));
  } else if (cp < 0x800u) {
    out.push_back(static_cast<char>(0xC0u | (cp >> 6)));
    out.push_back(static_cast<char>(0x80u | (cp & 0x3Fu)));
  } else if (cp < 0x10000u) {
    out.push_back(static_cast<char>(0xE0u | (cp >> 12)));
    out.push_back(static_cast<char>(0x80u | ((cp >> 6) & 0x3Fu)));
    out.push_back(static_cast<char>(0x80u | (cp & 0x3Fu)));
  } else {
    out.push_back(static_cast<char>(0xF0u | (cp >> 18)));
    out.push_back(static_cast<char>(0x80u | ((cp >> 12) & 0x3Fu)));
    out.push_back(static_cast<char>(0x80u | ((cp >> 6) & 0x3Fu)));
    out.push_back(static_cast<char>(0x80u | (cp & 0x3Fu)));
  }
}

bool IsValidUtf8(std::string_view s) noexcept {
  std::size_t i = 0;
  while (i < s.size()) {
    std::size_t len = 0;
    if (DecodeUtf8(s, i, len) == kReplacementChar) {
      // U+FFFD itself is valid (EF BF BD); a replacement for an invalid sequence is not.
      if (len != 3 || s.compare(i, 3, "\xEF\xBF\xBD") != 0) {
        return false;
      }
    }
    i += len;
  }
  return true;
}

std::string SanitizeUtf8(std::string_view s) {
  std::string out;
  out.reserve(s.size());
  std::size_t i = 0;
  while (i < s.size()) {
    std::size_t len = 0;
    const char32_t cp = DecodeUtf8(s, i, len);
    if (cp == kReplacementChar) {
      AppendUtf8(out, kReplacementChar);
    } else {
      out.append(s.substr(i, len));
    }
    i += len;
  }
  return out;
}

std::string Utf16ToUtf8(std::u16string_view s) {
  std::string out;
  out.reserve(s.size());
  for (std::size_t i = 0; i < s.size(); ++i) {
    const char16_t c = s[i];
    if (c >= 0xD800u && c <= 0xDBFFu && i + 1 < s.size() && s[i + 1] >= 0xDC00u && s[i + 1] <= 0xDFFFu) {
      const char32_t cp = 0x10000u + ((static_cast<char32_t>(c) - 0xD800u) << 10) + (s[i + 1] - 0xDC00u);
      AppendUtf8(out, cp);
      ++i;
    } else {
      AppendUtf8(out, c); // a lone surrogate becomes U+FFFD in AppendUtf8
    }
  }
  return out;
}

std::size_t Utf8PrefixLen(std::string_view s, std::size_t max_bytes) noexcept {
  if (s.size() <= max_bytes) {
    return s.size();
  }
  // s[max_bytes] is the first byte left out. Back up over at most 3 continuation bytes to the byte that would start
  // its sequence; cut before that byte only when it is a lead byte whose sequence reaches past max_bytes. Stray
  // continuation bytes (invalid input) are cut where asked.
  std::size_t n = max_bytes;
  for (int back = 0; back < 3 && n > 0 && IsCont(static_cast<unsigned char>(s[n])); ++back) {
    --n;
  }
  const auto lead = static_cast<unsigned char>(s[n]);
  std::size_t len = 1;
  if (lead >= 0xF0u && lead <= 0xF4u) {
    len = 4;
  } else if (lead >= 0xE0u && lead <= 0xEFu) {
    len = 3;
  } else if (lead >= 0xC2u && lead <= 0xDFu) {
    len = 2;
  }
  return n + len > max_bytes ? n : max_bytes;
}

std::size_t CopyFixed(char *dst, std::size_t cap, std::string_view s) noexcept {
  if (cap == 0) {
    return 0;
  }
  const std::size_t n = Utf8PrefixLen(s, cap - 1);
  std::memcpy(dst, s.data(), n);
  std::memset(dst + n, 0, cap - n);
  return n;
}

} // namespace im::abi
