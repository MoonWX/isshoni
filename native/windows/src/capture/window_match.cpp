#include "capture/window_match.h"

#include <cstdlib>

#include "isshoni_media.h"

namespace im::capture {
namespace {

wchar_t AsciiLower(wchar_t c) noexcept { return (c >= L'A' && c <= L'Z') ? static_cast<wchar_t>(c - L'A' + L'a') : c; }

void AppendUtf8(std::string &out, uint32_t cp) {
  if (cp < 0x80) {
    out.push_back(static_cast<char>(cp));
  } else if (cp < 0x800) {
    out.push_back(static_cast<char>(0xC0 | (cp >> 6)));
    out.push_back(static_cast<char>(0x80 | (cp & 0x3F)));
  } else if (cp < 0x10000) {
    out.push_back(static_cast<char>(0xE0 | (cp >> 12)));
    out.push_back(static_cast<char>(0x80 | ((cp >> 6) & 0x3F)));
    out.push_back(static_cast<char>(0x80 | (cp & 0x3F)));
  } else {
    out.push_back(static_cast<char>(0xF0 | (cp >> 18)));
    out.push_back(static_cast<char>(0x80 | ((cp >> 12) & 0x3F)));
    out.push_back(static_cast<char>(0x80 | ((cp >> 6) & 0x3F)));
    out.push_back(static_cast<char>(0x80 | (cp & 0x3F)));
  }
}

} // namespace

bool SizeMatches(PixelSize a, PixelSize b) noexcept {
  return std::abs(a.width - b.width) <= kSizeTolerance && std::abs(a.height - b.height) <= kSizeTolerance;
}

std::wstring_view BaseName(std::wstring_view path) noexcept {
  const std::size_t slash = path.find_last_of(L"\\/");
  return slash == std::wstring_view::npos ? path : path.substr(slash + 1);
}

bool SameImageName(std::wstring_view a, std::wstring_view b) noexcept {
  if (a.size() != b.size()) {
    return false;
  }
  for (std::size_t i = 0; i < a.size(); ++i) {
    if (AsciiLower(a[i]) != AsciiLower(b[i])) {
      return false;
    }
  }
  return true;
}

WindowIdentity MatchWindow(WindowSystem &ws, std::wstring_view title, PixelSize size) {
  WindowIdentity id;
  if (title.empty()) {
    return id; // a picked window always has a title; an empty one would match every untitled window
  }
  WindowHandle match = 0;
  for (const WindowHandle w : ws.TopLevelWindows()) {
    if (w == 0 || !ws.Visible(w) || ws.Title(w) != title || ws.Cloaked(w)) {
      continue;
    }
    const std::optional<PixelSize> frame = ws.FrameSize(w);
    if (!frame || !SizeMatches(*frame, size)) {
      continue;
    }
    if (match != 0) {
      return id; // two matches: unidentified
    }
    match = w;
  }
  if (match == 0) {
    return id;
  }
  id.window = match;
  id.pid = ws.ProcessId(match);
  if (id.pid != 0 && SameImageName(BaseName(ws.ImageBaseName(id.pid)), kFrameHostImage)) {
    // A frame-hosted app: the frame belongs to ApplicationFrameHost, the app's process owns the CoreWindow inside it.
    const WindowHandle core = ws.CoreWindowChild(match);
    id.pid = core != 0 ? ws.ProcessId(core) : 0;
  }
  return id;
}

uint32_t ClassifyPick(const WindowIdentity &id, std::span<const PixelSize> monitors, PixelSize item) noexcept {
  if (id.identified()) {
    return IM_SOURCE_WINDOW;
  }
  for (const PixelSize &m : monitors) {
    if (m == item) {
      return IM_SOURCE_MONITOR;
    }
  }
  return IM_SOURCE_WINDOW;
}

bool IsSelfProcess(std::span<const ProcessEntry> table, uint32_t pid, uint32_t self) noexcept {
  if (pid == 0) {
    return false;
  }
  if (pid == self) {
    return true;
  }
  const auto find = [&](uint32_t p) -> const ProcessEntry * {
    for (const ProcessEntry &e : table) {
      if (e.pid == p) {
        return &e;
      }
    }
    return nullptr;
  };
  const ProcessEntry *e = find(pid);
  if (e == nullptr || !SameImageName(BaseName(e->image), kWebView2Image)) {
    return false;
  }
  for (int depth = 0; depth < kMaxSelfDepth && e != nullptr; ++depth) {
    if (e->parent == self) {
      return true;
    }
    if (e->parent == 0 || e->parent == e->pid) {
      return false;
    }
    e = find(e->parent);
  }
  return false;
}

std::string Utf8FromWide(std::wstring_view s) {
  std::string out;
  out.reserve(s.size());
  for (std::size_t i = 0; i < s.size(); ++i) {
    uint32_t cp = static_cast<uint32_t>(s[i]);
    if (cp >= 0xD800 && cp <= 0xDBFF) {
      if (i + 1 < s.size()) {
        const auto lo = static_cast<uint32_t>(s[i + 1]);
        if (lo >= 0xDC00 && lo <= 0xDFFF) {
          cp = 0x10000 + ((cp - 0xD800) << 10) + (lo - 0xDC00);
          ++i;
          AppendUtf8(out, cp);
          continue;
        }
      }
      cp = 0xFFFD;
    } else if ((cp >= 0xDC00 && cp <= 0xDFFF) || cp > 0x10FFFF) {
      cp = 0xFFFD;
    }
    AppendUtf8(out, cp);
  }
  return out;
}

void CopyUtf8(std::string_view s, char *dst, std::size_t cap) noexcept {
  if (dst == nullptr || cap == 0) {
    return;
  }
  std::size_t n = s.size();
  if (n > cap - 1) {
    n = cap - 1;
    // Back up over continuation bytes so the cut never splits a character.
    while (n > 0 && (static_cast<unsigned char>(s[n]) & 0xC0) == 0x80) {
      --n;
    }
  }
  for (std::size_t i = 0; i < n; ++i) {
    dst[i] = s[i];
  }
  dst[n] = '\0';
}

} // namespace im::capture
