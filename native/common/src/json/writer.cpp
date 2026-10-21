#include "im/json/writer.h"

#include <charconv>
#include <cmath>
#include <system_error>

#include "im/abi/status.h"
#include "im/abi/utf8.h"

namespace im::json {
namespace {

constexpr char kHex[] = "0123456789abcdef";

void AppendCodePoint(std::string &out, char32_t cp) {
  switch (cp) {
  case U'"':
    out += "\\\"";
    return;
  case U'\\':
    out += "\\\\";
    return;
  case U'\b':
    out += "\\b";
    return;
  case U'\f':
    out += "\\f";
    return;
  case U'\n':
    out += "\\n";
    return;
  case U'\r':
    out += "\\r";
    return;
  case U'\t':
    out += "\\t";
    return;
  case U' ':
    out += "\\u2028";
    return;
  case U' ':
    out += "\\u2029";
    return;
  default:
    break;
  }
  if (cp < 0x20u) {
    out += "\\u00";
    out.push_back(kHex[(cp >> 4) & 0xFu]);
    out.push_back(kHex[cp & 0xFu]);
    return;
  }
  abi::AppendUtf8(out, cp);
}

} // namespace

void AppendEscaped(std::string &out, std::string_view utf8) {
  std::size_t i = 0;
  while (i < utf8.size()) {
    const auto c = static_cast<unsigned char>(utf8[i]);
    // Fast path: printable ASCII that needs no escape.
    if (c >= 0x20u && c < 0x80u && c != '"' && c != '\\') {
      out.push_back(static_cast<char>(c));
      ++i;
      continue;
    }
    std::size_t len = 0;
    const char32_t cp = abi::DecodeUtf8(utf8, i, len);
    AppendCodePoint(out, cp); // an invalid sequence decodes as U+FFFD
    i += len;
  }
}

void AppendEscaped(std::string &out, std::u16string_view utf16) { AppendEscaped(out, abi::Utf16ToUtf8(utf16)); }

void AppendDouble(std::string &out, double v) {
  if (!std::isfinite(v)) {
    out += "null";
    return;
  }
  if (v == 0) {
    out += "0"; // also -0, which Go would print as 0
    return;
  }
  char buf[32];
  const auto r = std::to_chars(buf, buf + sizeof buf, v);
  if (r.ec != std::errc{}) {
    out += "null";
    return;
  }
  out.append(buf, r.ptr);
}

Writer &Writer::BeginObject() {
  Open('{');
  return *this;
}

Writer &Writer::EndObject() {
  Close('}');
  return *this;
}

Writer &Writer::BeginArray() {
  Open('[');
  return *this;
}

Writer &Writer::EndArray() {
  Close(']');
  return *this;
}

Writer &Writer::Key(std::string_view key) {
  if (!has_items_.empty()) {
    if (has_items_.back()) {
      out_.push_back(',');
    }
    has_items_.back() = true;
  }
  out_.push_back('"');
  AppendEscaped(out_, key);
  out_ += "\":";
  after_key_ = true;
  return *this;
}

Writer &Writer::String(std::string_view utf8) {
  BeforeValue();
  out_.push_back('"');
  AppendEscaped(out_, utf8);
  out_.push_back('"');
  return *this;
}

Writer &Writer::String(std::u16string_view utf16) {
  BeforeValue();
  out_.push_back('"');
  AppendEscaped(out_, utf16);
  out_.push_back('"');
  return *this;
}

Writer &Writer::Int(int64_t v) {
  BeforeValue();
  char buf[24];
  const auto r = std::to_chars(buf, buf + sizeof buf, v);
  out_.append(buf, r.ptr);
  return *this;
}

Writer &Writer::Uint(uint64_t v) {
  BeforeValue();
  char buf[24];
  const auto r = std::to_chars(buf, buf + sizeof buf, v);
  out_.append(buf, r.ptr);
  return *this;
}

Writer &Writer::Double(double v) {
  BeforeValue();
  AppendDouble(out_, v);
  return *this;
}

Writer &Writer::Fixed(double v, int decimals) {
  BeforeValue();
  if (!std::isfinite(v)) {
    out_ += "null";
    return *this;
  }
  decimals = decimals < 0 ? 0 : (decimals > 9 ? 9 : decimals);
  char buf[400]; // the longest finite double in fixed form has 309 integer digits
  const auto r = std::to_chars(buf, buf + sizeof buf, v, std::chars_format::fixed, decimals);
  if (r.ec != std::errc{}) {
    out_ += "null";
    return *this;
  }
  std::string_view s(buf, static_cast<std::size_t>(r.ptr - buf));
  if (s.front() == '-' && s.find_first_not_of("-0.") == std::string_view::npos) {
    s.remove_prefix(1); // a value that rounds to zero is written without the sign: "0.0", not "-0.0"
  }
  out_ += s;
  return *this;
}

Writer &Writer::Bool(bool v) {
  BeforeValue();
  out_ += v ? "true" : "false";
  return *this;
}

Writer &Writer::Null() {
  BeforeValue();
  out_ += "null";
  return *this;
}

Writer &Writer::Raw(std::string_view json) {
  BeforeValue();
  out_ += json;
  return *this;
}

std::string Writer::Take() noexcept {
  std::string s = std::move(out_);
  out_.clear();
  has_items_.clear();
  after_key_ = false;
  return s;
}

int32_t Writer::CopyOut(char *buf, int32_t cap) const noexcept { return abi::CopyOut(out_, buf, cap); }

void Writer::BeforeValue() {
  if (after_key_) {
    after_key_ = false; // the key already wrote the comma
    return;
  }
  if (!has_items_.empty()) {
    if (has_items_.back()) {
      out_.push_back(',');
    }
    has_items_.back() = true;
  }
}

void Writer::Open(char c) {
  BeforeValue();
  out_.push_back(c);
  has_items_.push_back(false);
}

void Writer::Close(char c) {
  out_.push_back(c);
  if (!has_items_.empty()) {
    has_items_.pop_back();
  }
  after_key_ = false;
}

} // namespace im::json
