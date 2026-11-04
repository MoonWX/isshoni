// window_match_test (docs/m2/04-windows-engine.md §18.2, README WA50): §7.6's window matcher over a fake window list
// (one match gives `window` and `pid`; two matches or none give 0; cloaked windows are skipped; an
// applicationframehost.exe match takes its Windows.UI.Core.CoreWindow child's pid, and 0 without one), §7.2 step 4's
// kind, the self rule, and the capture area's other plain parts: the UTF-8 copy into im_source_info, the resize
// debounce and minimize edges of §7.7, and WGC's MinUpdateInterval of §7.3.
#include <gtest/gtest.h>

#include <map>
#include <optional>
#include <string>
#include <vector>

#include "capture/source_watch.h"
#include "capture/window_match.h"
#include "isshoni_media.h"

namespace im::capture {
namespace {

struct FakeWindow {
  bool visible = true;
  bool cloaked = false;
  std::wstring title;
  std::optional<PixelSize> frame;
  uint32_t pid = 0;
  WindowHandle core_child = 0; // a Windows.UI.Core.CoreWindow child
};

class FakeWindows final : public WindowSystem {
public:
  // Adds a window (z-order: the order of Add) and returns its handle.
  WindowHandle Add(FakeWindow w) {
    const WindowHandle h = next_;
    next_ += 0x10;
    order_.push_back(h);
    windows_[h] = std::move(w);
    return h;
  }
  // A child window (not top-level) with its own process.
  WindowHandle AddChild(uint32_t pid) {
    const WindowHandle h = next_;
    next_ += 0x10;
    FakeWindow w;
    w.pid = pid;
    windows_[h] = w;
    return h;
  }
  void SetImage(uint32_t pid, std::wstring image) { images_[pid] = std::move(image); }
  FakeWindow &Get(WindowHandle h) { return windows_.at(h); }

  std::vector<WindowHandle> TopLevelWindows() override { return order_; }
  bool Visible(WindowHandle w) override { return Find(w) != nullptr && Find(w)->visible; }
  bool Cloaked(WindowHandle w) override { return Find(w) != nullptr && Find(w)->cloaked; }
  std::wstring Title(WindowHandle w) override { return Find(w) != nullptr ? Find(w)->title : std::wstring(); }
  std::optional<PixelSize> FrameSize(WindowHandle w) override {
    return Find(w) != nullptr ? Find(w)->frame : std::nullopt;
  }
  uint32_t ProcessId(WindowHandle w) override { return Find(w) != nullptr ? Find(w)->pid : 0; }
  std::wstring ImageBaseName(uint32_t pid) override {
    const auto it = images_.find(pid);
    return it != images_.end() ? it->second : std::wstring();
  }
  WindowHandle CoreWindowChild(WindowHandle w) override { return Find(w) != nullptr ? Find(w)->core_child : 0; }

private:
  const FakeWindow *Find(WindowHandle w) const {
    const auto it = windows_.find(w);
    return it != windows_.end() ? &it->second : nullptr;
  }

  WindowHandle next_ = 0x1000;
  std::vector<WindowHandle> order_;
  std::map<WindowHandle, FakeWindow> windows_;
  std::map<uint32_t, std::wstring> images_;
};

FakeWindow Window(std::wstring title, int32_t w, int32_t h, uint32_t pid) {
  FakeWindow f;
  f.title = std::move(title);
  f.frame = PixelSize{w, h};
  f.pid = pid;
  return f;
}

constexpr PixelSize kItem{1280, 720};

// ------------------------------------------------------------------------------------------------- window identity

TEST(WindowMatch, OneMatchGivesWindowAndPid) {
  FakeWindows ws;
  ws.Add(Window(L"Other", 1280, 720, 10));
  const WindowHandle target = ws.Add(Window(L"Docs - Browser", 1280, 720, 42));
  ws.Add(Window(L"Docs - Browser", 800, 600, 43)); // same title, other size
  ws.SetImage(42, L"browser.exe");

  const WindowIdentity id = MatchWindow(ws, L"Docs - Browser", kItem);
  EXPECT_TRUE(id.identified());
  EXPECT_EQ(id.window, target);
  EXPECT_EQ(id.pid, 42u);
}

TEST(WindowMatch, SizeWithinTwoPixels) {
  FakeWindows ws;
  const WindowHandle target = ws.Add(Window(L"Editor", 1282, 718, 7));
  EXPECT_EQ(MatchWindow(ws, L"Editor", kItem).window, target);

  FakeWindows off;
  off.Add(Window(L"Editor", 1283, 720, 7)); // 3 pixels off: not the item
  EXPECT_FALSE(MatchWindow(off, L"Editor", kItem).identified());
  FakeWindows off_h;
  off_h.Add(Window(L"Editor", 1280, 717, 7));
  EXPECT_FALSE(MatchWindow(off_h, L"Editor", kItem).identified());
}

TEST(WindowMatch, TwoMatchesGiveNothing) {
  FakeWindows ws;
  ws.Add(Window(L"Terminal", 1280, 720, 5));
  ws.Add(Window(L"Terminal", 1281, 721, 6));
  const WindowIdentity id = MatchWindow(ws, L"Terminal", kItem);
  EXPECT_FALSE(id.identified());
  EXPECT_EQ(id.window, 0u);
  EXPECT_EQ(id.pid, 0u);
}

TEST(WindowMatch, NoMatchGivesNothing) {
  FakeWindows ws;
  ws.Add(Window(L"Terminal", 1280, 720, 5));
  const WindowIdentity id = MatchWindow(ws, L"terminal", kItem); // titles compare exactly
  EXPECT_EQ(id.window, 0u);
  EXPECT_EQ(id.pid, 0u);

  FakeWindows empty;
  EXPECT_FALSE(MatchWindow(empty, L"Terminal", kItem).identified());
}

TEST(WindowMatch, NoFrameBoundsNeverMatches) {
  FakeWindows ws;
  FakeWindow w = Window(L"Game", 1280, 720, 9);
  w.frame.reset();
  ws.Add(w);
  EXPECT_FALSE(MatchWindow(ws, L"Game", kItem).identified());
}

TEST(WindowMatch, EmptyTitleNeverMatches) {
  FakeWindows ws;
  ws.Add(Window(L"", 1280, 720, 9));
  EXPECT_FALSE(MatchWindow(ws, L"", kItem).identified());
}

TEST(WindowMatch, CloakedWindowsAreSkipped) {
  FakeWindows ws;
  FakeWindow cloaked = Window(L"Mail", 1280, 720, 20);
  cloaked.cloaked = true; // e.g. on another virtual desktop
  ws.Add(cloaked);
  const WindowHandle visible = ws.Add(Window(L"Mail", 1280, 720, 21));
  const WindowIdentity id = MatchWindow(ws, L"Mail", kItem);
  EXPECT_EQ(id.window, visible) << "the cloaked twin must not make the match ambiguous";
  EXPECT_EQ(id.pid, 21u);

  FakeWindows only_cloaked;
  only_cloaked.Add(cloaked);
  EXPECT_FALSE(MatchWindow(only_cloaked, L"Mail", kItem).identified());
}

TEST(WindowMatch, InvisibleWindowsAreSkipped) {
  FakeWindows ws;
  FakeWindow hidden = Window(L"Mail", 1280, 720, 20);
  hidden.visible = false;
  ws.Add(hidden);
  const WindowHandle shown = ws.Add(Window(L"Mail", 1280, 720, 21));
  EXPECT_EQ(MatchWindow(ws, L"Mail", kItem).window, shown);
}

TEST(WindowMatch, FrameHostTakesTheCoreWindowsPid) {
  FakeWindows ws;
  const WindowHandle core = ws.AddChild(77);
  FakeWindow frame = Window(L"Calculator", 1280, 720, 30);
  frame.core_child = core;
  const WindowHandle host = ws.Add(frame);
  ws.SetImage(30, L"ApplicationFrameHost.exe"); // compared without case
  ws.SetImage(77, L"calculatorapp.exe");

  const WindowIdentity id = MatchWindow(ws, L"Calculator", kItem);
  EXPECT_EQ(id.window, host) << "window stays the frame's";
  EXPECT_EQ(id.pid, 77u) << "pid is the CoreWindow child's";
}

TEST(WindowMatch, FrameHostWithoutCoreWindowHasNoPid) {
  FakeWindows ws;
  const WindowHandle host = ws.Add(Window(L"Settings", 1280, 720, 30));
  ws.SetImage(30, L"applicationframehost.exe");

  const WindowIdentity id = MatchWindow(ws, L"Settings", kItem);
  EXPECT_EQ(id.window, host);
  EXPECT_EQ(id.pid, 0u);
}

TEST(WindowMatch, OtherImagesKeepTheirPid) {
  FakeWindows ws;
  const WindowHandle core = ws.AddChild(77);
  FakeWindow w = Window(L"App", 1280, 720, 30);
  w.core_child = core; // a CoreWindow child alone doesn't make a frame host
  ws.Add(w);
  ws.SetImage(30, L"applicationframehost.exe.bak");
  EXPECT_EQ(MatchWindow(ws, L"App", kItem).pid, 30u);
}

TEST(WindowMatch, ImageNames) {
  EXPECT_EQ(BaseName(L"C:\\Windows\\System32\\ApplicationFrameHost.exe"), L"ApplicationFrameHost.exe");
  EXPECT_EQ(BaseName(L"C:/x/y.exe"), L"y.exe");
  EXPECT_EQ(BaseName(L"plain.exe"), L"plain.exe");
  EXPECT_TRUE(SameImageName(L"APPLICATIONFRAMEHOST.EXE", kFrameHostImage));
  EXPECT_TRUE(SameImageName(L"MsEdgeWebView2.exe", kWebView2Image));
  EXPECT_FALSE(SameImageName(L"applicationframehost.ex", kFrameHostImage));
  EXPECT_FALSE(SameImageName(L"\u00c4.exe", L"\u00e4.exe")) << "only ASCII letters fold";
}

// ------------------------------------------------------------------------------------------------------------ kind

TEST(ClassifyPick, WindowMonitorOrUnknownWindow) {
  const std::vector<PixelSize> monitors = {{2560, 1440}, {1920, 1080}};
  WindowIdentity identified;
  identified.window = 0x1000;
  identified.pid = 4;
  EXPECT_EQ(ClassifyPick(identified, monitors, PixelSize{1920, 1080}), IM_SOURCE_WINDOW)
      << "an identified window is a window even at a monitor's size";
  EXPECT_EQ(ClassifyPick(WindowIdentity{}, monitors, PixelSize{1920, 1080}), IM_SOURCE_MONITOR);
  EXPECT_EQ(ClassifyPick(WindowIdentity{}, monitors, PixelSize{2560, 1440}), IM_SOURCE_MONITOR);
  EXPECT_EQ(ClassifyPick(WindowIdentity{}, monitors, PixelSize{1921, 1080}), IM_SOURCE_WINDOW)
      << "monitors compare exactly";
  EXPECT_EQ(ClassifyPick(WindowIdentity{}, {}, PixelSize{1920, 1080}), IM_SOURCE_WINDOW);
}

// ------------------------------------------------------------------------------------------------------------ self

TEST(IsSelfProcess, OwnProcessAndWebView2Children) {
  constexpr uint32_t kSelf = 100;
  const std::vector<ProcessEntry> table = {
      {100, 4, L"isshoni.exe"},          // the host
      {200, 100, L"msedgewebview2.exe"}, // the WebView2 browser process
      {201, 200, L"msedgewebview2.exe"}, // a renderer
      {300, 100, L"notepad.exe"},        // something the app started
      {400, 4, L"msedgewebview2.exe"},   // another app's WebView2
      {500, 501, L"msedgewebview2.exe"}, // a parent loop
      {501, 500, L"msedgewebview2.exe"}, // (the loop's other half)
  };
  EXPECT_TRUE(IsSelfProcess(table, kSelf, kSelf));
  EXPECT_TRUE(IsSelfProcess(table, 200, kSelf));
  EXPECT_TRUE(IsSelfProcess(table, 201, kSelf));
  EXPECT_FALSE(IsSelfProcess(table, 300, kSelf)) << "only WebView2 children count";
  EXPECT_FALSE(IsSelfProcess(table, 400, kSelf));
  EXPECT_FALSE(IsSelfProcess(table, 500, kSelf)) << "a parent loop ends";
  EXPECT_FALSE(IsSelfProcess(table, 0, kSelf));
  EXPECT_FALSE(IsSelfProcess(table, 999, kSelf)) << "unknown process";
}

TEST(IsSelfProcess, DepthIsBounded) {
  constexpr uint32_t kSelf = 1;
  std::vector<ProcessEntry> table;
  // A chain of WebView2 processes, each started by the next: kMaxSelfDepth links reach self, one more doesn't.
  for (uint32_t i = 0; i <= static_cast<uint32_t>(kMaxSelfDepth); ++i) {
    table.push_back(
        ProcessEntry{10 + i, i == static_cast<uint32_t>(kMaxSelfDepth) ? kSelf : 11 + i, L"msedgewebview2.exe"});
  }
  EXPECT_TRUE(IsSelfProcess(table, 11, kSelf));
  EXPECT_FALSE(IsSelfProcess(table, 10, kSelf));
}

// ------------------------------------------------------------------------------------------------------------ text

TEST(Utf8, FromWide) {
  EXPECT_EQ(Utf8FromWide(L"abc"), "abc");
  EXPECT_EQ(Utf8FromWide(L"\u00e9\u20ac"), "\xc3\xa9\xe2\x82\xac");
  std::wstring pair;
  pair.push_back(static_cast<wchar_t>(0xD83D)); // U+1F600 as a surrogate pair
  pair.push_back(static_cast<wchar_t>(0xDE00));
  if (sizeof(wchar_t) == 2) {
    EXPECT_EQ(Utf8FromWide(pair), "\xf0\x9f\x98\x80");
  }
  std::wstring lone;
  lone.push_back(static_cast<wchar_t>(0xD83D));
  lone.push_back(L'x');
  EXPECT_EQ(Utf8FromWide(lone), "\xef\xbf\xbdx") << "an unpaired surrogate becomes U+FFFD";
}

TEST(Utf8, CopyCutsOnACharacterBoundary) {
  char buf[6];
  CopyUtf8("abc", buf, sizeof buf);
  EXPECT_STREQ(buf, "abc");
  CopyUtf8("abcdefgh", buf, sizeof buf);
  EXPECT_STREQ(buf, "abcde");
  CopyUtf8("abcd\xe2\x82\xac", buf, sizeof buf); // the euro sign would need bytes 4..6
  EXPECT_STREQ(buf, "abcd");
  CopyUtf8("\xc3\xa9\xc3\xa9\xc3\xa9", buf, sizeof buf);
  EXPECT_STREQ(buf, "\xc3\xa9\xc3\xa9");
  char one[1];
  CopyUtf8("abc", one, sizeof one);
  EXPECT_STREQ(one, "");
}

// --------------------------------------------------------------------------------------------- source changes

constexpr int64_t kMs = 1'000'000;

TEST(ResizeTracker, OneEventThenSettleAfterOneSecond) {
  ResizeTracker t(video::Size{1280, 720});
  EXPECT_FALSE(t.Observe(video::Size{1280, 720}, 0)) << "the start size is no change";
  EXPECT_TRUE(t.Observe(video::Size{1300, 720}, 100 * kMs)) << "SOURCE_RESIZED when first seen";
  EXPECT_FALSE(t.Observe(video::Size{1320, 730}, 150 * kMs)) << "one event per resize";
  EXPECT_FALSE(t.Observe(video::Size{1400, 800}, 400 * kMs));
  ASSERT_TRUE(t.SettleDueNs().has_value());
  EXPECT_EQ(*t.SettleDueNs(), 1400 * kMs);
  EXPECT_FALSE(t.Settle(1399 * kMs).has_value()) << "not stable for 1 s yet";
  const std::optional<video::Size> settled = t.Settle(1400 * kMs);
  ASSERT_TRUE(settled.has_value());
  EXPECT_EQ(*settled, (video::Size{1400, 800}));
  EXPECT_FALSE(t.resizing());
  EXPECT_FALSE(t.Settle(5000 * kMs).has_value()) << "settled once";
  EXPECT_FALSE(t.Observe(video::Size{1400, 800}, 5000 * kMs));
  EXPECT_TRUE(t.Observe(video::Size{1000, 800}, 6000 * kMs)) << "a new resize sends a new event";
}

TEST(ResizeTracker, BackToTheSettledSizeNeedsNoReinit) {
  ResizeTracker t(video::Size{1280, 720});
  EXPECT_TRUE(t.Observe(video::Size{1290, 720}, 0));
  EXPECT_FALSE(t.Observe(video::Size{1280, 720}, 200 * kMs));
  EXPECT_FALSE(t.Settle(1200 * kMs).has_value());
  EXPECT_FALSE(t.resizing());
  EXPECT_EQ(t.settled(), (video::Size{1280, 720}));
}

TEST(ResizeTracker, EmptyContentIsIgnored) {
  ResizeTracker t(video::Size{1280, 720});
  EXPECT_FALSE(t.Observe(video::Size{0, 0}, 0));
  EXPECT_FALSE(t.Observe(video::Size{1280, 0}, 0));
  EXPECT_FALSE(t.resizing());
  EXPECT_FALSE(t.SettleDueNs().has_value());
}

TEST(MinimizeTracker, Edges) {
  MinimizeTracker m;
  EXPECT_EQ(m.Update(false), MinimizeTracker::Change::kNone);
  EXPECT_EQ(m.Update(true), MinimizeTracker::Change::kMinimized);
  EXPECT_EQ(m.Update(true), MinimizeTracker::Change::kNone);
  EXPECT_TRUE(m.minimized());
  EXPECT_EQ(m.Update(false), MinimizeTracker::Change::kRestored);
  EXPECT_EQ(m.Update(false), MinimizeTracker::Change::kNone);
}

TEST(MinUpdateInterval, HalfTheFrameInterval) {
  EXPECT_EQ(MinUpdateInterval100ns(60), 83'333); // 8.33 ms (§7.3)
  EXPECT_EQ(MinUpdateInterval100ns(30), 166'666);
  EXPECT_EQ(MinUpdateInterval100ns(120), 41'666);
  EXPECT_EQ(MinUpdateInterval100ns(0), 0);
  EXPECT_EQ(kMinimizePollNs, 500 * kMs);
  EXPECT_EQ(kResizeSettleNs, 1000 * kMs);
}

} // namespace
} // namespace im::capture
