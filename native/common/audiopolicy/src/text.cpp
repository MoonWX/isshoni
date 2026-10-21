// String rules of the exclusion policy (docs/m2/05-windows-audio.md §4.1, §5.3, §7.4).
#include "im/audiopolicy/text.h"

#include <cstddef>

#include "im/abi/utf8.h"

namespace im::audiopolicy {
namespace {

// One code point to its lower-case form for the blocks FoldCase documents; everything else maps to itself.
char32_t FoldCodePoint(char32_t c) noexcept {
  if (c < 0x80) {
    return (c >= U'A' && c <= U'Z') ? c + 0x20 : c;
  }
  if (c >= 0xC0 && c <= 0xDE && c != 0xD7) { // Latin-1: À..Þ except ×
    return c + 0x20;
  }
  if (c >= 0x100 && c <= 0x17F) { // Latin Extended-A: pairs of upper (first) and lower case
    if ((c >= 0x100 && c <= 0x12F) || (c >= 0x132 && c <= 0x137) || (c >= 0x14A && c <= 0x177)) {
      return (c % 2 == 0) ? c + 1 : c;
    }
    if ((c >= 0x139 && c <= 0x148) || (c >= 0x179 && c <= 0x17E)) {
      return (c % 2 == 1) ? c + 1 : c;
    }
    if (c == 0x178) { // Ÿ
      return 0xFF;
    }
    return c;
  }
  if (c >= 0x391 && c <= 0x3AB && c != 0x3A2) { // Greek capitals (incl. Ϊ, Ϋ)
    return c + 0x20;
  }
  if (c >= 0x410 && c <= 0x42F) { // Cyrillic А..Я
    return c + 0x20;
  }
  if (c >= 0x400 && c <= 0x40F) { // Cyrillic Ѐ..Џ
    return c + 0x50;
  }
  if (c >= 0xFF21 && c <= 0xFF3A) { // fullwidth Ａ..Ｚ
    return c + 0x20;
  }
  return c;
}

bool IsSeparator(char c) noexcept { return c == '\\' || c == '/'; }

bool IsSpace(char c) noexcept { return c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '\v' || c == '\f'; }

} // namespace

std::string FoldCase(std::string_view utf8) {
  std::string out;
  out.reserve(utf8.size());
  std::size_t i = 0;
  while (i < utf8.size()) {
    const unsigned char b = static_cast<unsigned char>(utf8[i]);
    if (b < 0x80) { // the common case: ASCII, byte for byte
      out.push_back(static_cast<char>((b >= 'A' && b <= 'Z') ? b + 0x20 : b));
      ++i;
      continue;
    }
    std::size_t len = 1;
    const char32_t cp = im::abi::DecodeUtf8(utf8, i, len);
    im::abi::AppendUtf8(out, FoldCodePoint(cp));
    i += len;
  }
  return out;
}

std::string NormalizePath(std::string_view path) {
  std::string out;
  out.reserve(path.size());
  std::size_t i = 0;
  // A leading "\\" (UNC "\\server\share", device "\\?\C:\") keeps both separators.
  if (path.size() >= 2 && IsSeparator(path[0]) && IsSeparator(path[1])) {
    out.append("\\\\");
    i = 2;
    while (i < path.size() && IsSeparator(path[i])) {
      ++i;
    }
  }
  for (; i < path.size(); ++i) {
    const char c = path[i];
    if (IsSeparator(c)) {
      if (out.empty() || out.back() != '\\') {
        out.push_back('\\');
      }
      continue;
    }
    out.push_back(c);
  }
  while (!out.empty() && out.back() == '\\') {
    out.pop_back();
  }
  return FoldCase(out);
}

bool PathEqualsOrBelow(std::string_view path, std::string_view dir) {
  const std::string d = NormalizePath(dir);
  if (d.empty()) {
    return false;
  }
  const std::string p = NormalizePath(path);
  if (p.size() < d.size() || p.compare(0, d.size(), d) != 0) {
    return false;
  }
  return p.size() == d.size() || p[d.size()] == '\\';
}

std::vector<std::string> SplitCommandLine(std::string_view s) {
  std::vector<std::string> args;
  std::size_t i = 0;
  const std::size_t n = s.size();
  while (true) {
    while (i < n && IsSpace(s[i])) {
      ++i;
    }
    if (i >= n) {
      break;
    }
    std::string arg;
    bool quoted = false;
    while (i < n) {
      const char c = s[i];
      if (c == '\\') {
        std::size_t slashes = 0;
        while (i < n && s[i] == '\\') {
          ++slashes;
          ++i;
        }
        if (i < n && s[i] == '"') {
          arg.append(slashes / 2, '\\');
          if (slashes % 2 == 1) {
            arg.push_back('"'); // an escaped quote
            ++i;
          }
          // an even count leaves the quote for the next round, where it toggles quoting
        } else {
          arg.append(slashes, '\\');
        }
        continue;
      }
      if (c == '"') {
        if (quoted && i + 1 < n && s[i + 1] == '"') { // "" inside quotes: a literal quote
          arg.push_back('"');
          i += 2;
          continue;
        }
        quoted = !quoted;
        ++i;
        continue;
      }
      if (!quoted && IsSpace(c)) {
        break;
      }
      arg.push_back(c);
      ++i;
    }
    args.push_back(std::move(arg));
  }
  return args;
}

std::string ParseUserDataDir(std::string_view command_line) {
  static constexpr std::string_view kSwitch = "--user-data-dir=";
  std::string value;
  for (const std::string &arg : SplitCommandLine(command_line)) {
    if (arg.size() >= kSwitch.size() && FoldCase(std::string_view(arg).substr(0, kSwitch.size())) == kSwitch) {
      value = arg.substr(kSwitch.size()); // the last one wins, as in Chromium
    }
  }
  return value;
}

std::string_view ExeStem(std::string_view exe) noexcept {
  if (exe.size() > 4) {
    const std::string_view ext = exe.substr(exe.size() - 4);
    if (ext[0] == '.' && (ext[1] == 'e' || ext[1] == 'E') && (ext[2] == 'x' || ext[2] == 'X') &&
        (ext[3] == 'e' || ext[3] == 'E')) {
      return exe.substr(0, exe.size() - 4);
    }
  }
  return exe;
}

} // namespace im::audiopolicy
