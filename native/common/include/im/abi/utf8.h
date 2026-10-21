// UTF-8 helpers for the C ABI's string rules (docs/m2/04-windows-engine.md §5.1): strings are UTF-8, text from
// Windows (UTF-16) is converted with invalid surrogates as U+FFFD, and fixed char[N] fields are NUL-terminated and cut
// on a UTF-8 boundary.
#pragma once

#include <cstddef>
#include <cstdint>
#include <string>
#include <string_view>

namespace im::abi {

inline constexpr char32_t kReplacementChar = U'�';

// Decodes the code point at s[i]. On success it returns the code point and sets len to its byte length (1..4). An
// invalid sequence (a stray continuation byte, an overlong form, an encoded surrogate, a value above U+10FFFF, a cut
// sequence) gives kReplacementChar with len = the length of its maximal subpart (at least 1), as the WHATWG decoder
// does, so each maximal subpart becomes one U+FFFD. i must be < s.size().
char32_t DecodeUtf8(std::string_view s, std::size_t i, std::size_t &len) noexcept;

// Appends the UTF-8 encoding of cp; a surrogate or a value above U+10FFFF is appended as U+FFFD.
void AppendUtf8(std::string &out, char32_t cp);

// Whether s is valid UTF-8.
bool IsValidUtf8(std::string_view s) noexcept;

// s with every invalid sequence replaced by U+FFFD.
std::string SanitizeUtf8(std::string_view s);

// UTF-16 to UTF-8; a lone surrogate becomes U+FFFD.
std::string Utf16ToUtf8(std::u16string_view s);

// The longest prefix of s that has at most max_bytes bytes and doesn't end inside a UTF-8 sequence. On invalid input
// it only avoids cutting before a continuation byte.
std::size_t Utf8PrefixLen(std::string_view s, std::size_t max_bytes) noexcept;

// Copies s into the fixed field dst[cap] (cap >= 1): at most cap - 1 bytes, cut on a UTF-8 boundary, NUL-terminated,
// the rest of the field zeroed so no stale bytes leak. Returns the bytes copied.
std::size_t CopyFixed(char *dst, std::size_t cap, std::string_view s) noexcept;

template <std::size_t N> std::size_t CopyFixed(char (&dst)[N], std::string_view s) noexcept {
  static_assert(N >= 1);
  return CopyFixed(dst, N, s);
}

} // namespace im::abi
