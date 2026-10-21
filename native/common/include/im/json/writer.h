// A small JSON writer for the engine's documents (docs/m2/04-windows-engine.md §5.6): build info, sources, stats,
// event details, log records and the ABI layout. Output is compact UTF-8. Strings are escaped as Go's encoding/json
// reads them: quote, backslash and control characters escaped, U+2028/U+2029 escaped, invalid UTF-8 and lone
// surrogates replaced by U+FFFD, everything else (emoji included) written as UTF-8.
//
// The writer keeps commas and nesting right; it doesn't check that keys are unique or that every container is closed
// (tests do). Not real-time safe: it allocates. Real-time threads hand their data to a non-real-time thread first
// (04 §6.1).
#pragma once

#include <cstdint>
#include <string>
#include <string_view>
#include <vector>

namespace im::json {

// Appends the escaped body of a JSON string (no quotes).
void AppendEscaped(std::string &out, std::string_view utf8);
void AppendEscaped(std::string &out, std::u16string_view utf16);

// Appends the shortest decimal that reads back as v ("60", "59.8", "1e-07"), or null for NaN and infinities (JSON
// has neither).
void AppendDouble(std::string &out, double v);

class Writer {
public:
  Writer() = default;

  Writer &BeginObject();
  Writer &EndObject();
  Writer &BeginArray();
  Writer &EndArray();

  // The key of the next value inside an object.
  Writer &Key(std::string_view key);

  Writer &String(std::string_view utf8);
  Writer &String(std::u16string_view utf16);
  Writer &String(const char *utf8) { return String(std::string_view(utf8 ? utf8 : "")); }
  Writer &Int(int64_t v);
  Writer &Uint(uint64_t v);
  Writer &Double(double v);              // shortest round-trip form; null when not finite
  Writer &Fixed(double v, int decimals); // fixed decimals (0..9), e.g. Fixed(59.84, 1) = 59.8; null when not finite
  Writer &Bool(bool v);
  Writer &Null();
  Writer &Raw(std::string_view json); // an already serialized value (the caller vouches for it)

  const std::string &str() const noexcept { return out_; }
  std::string Take() noexcept;

  // The document with the snprintf rule of the C ABI (im::abi::CopyOut).
  int32_t CopyOut(char *buf, int32_t cap) const noexcept;

private:
  void BeforeValue();
  void Open(char c);
  void Close(char c);

  std::string out_;
  std::vector<bool> has_items_; // per open container: whether it already holds an item
  bool after_key_ = false;
};

} // namespace im::json
