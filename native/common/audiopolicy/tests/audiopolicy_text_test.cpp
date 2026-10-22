// String rules of the exclusion policy (docs/m2/05-windows-audio.md §5.3, §7.4, §19.1): the case fold, path
// normalization, the WebView2 data-folder path rule and the --user-data-dir parser.
#include <gtest/gtest.h>

#include <string>
#include <vector>

#include "im/audiopolicy/text.h"

namespace {

using im::audiopolicy::ExeStem;
using im::audiopolicy::FoldCase;
using im::audiopolicy::NormalizePath;
using im::audiopolicy::ParseUserDataDir;
using im::audiopolicy::PathEqualsOrBelow;
using im::audiopolicy::SplitCommandLine;

TEST(AudioPolicyText, FoldCaseLowersTheFoldedScripts) {
  struct Case {
    const char *in;
    const char *want;
    const char *what;
  };
  const Case cases[] = {
      {"Discord.EXE", "discord.exe", "ASCII"},
      {"MS-Teams.exe", "ms-teams.exe", "ASCII"},
      {"\xC3\x89lite.exe", "\xC3\xA9lite.exe", "Latin-1: E with acute"},
      {"\xC3\x97", "\xC3\x97", "Latin-1: the multiplication sign has no lower case"},
      {"\xC5\x81\xC3\x93\xC5\xB9", "\xC5\x82\xC3\xB3\xC5\xBA", "Latin Extended-A and Latin-1"},
      {"\xC5\xB8", "\xC3\xBF", "Y with diaeresis folds into Latin-1"},
      {"\xD0\x98\xD0\x93\xD0\xA0\xD0\x90", "\xD0\xB8\xD0\xB3\xD1\x80\xD0\xB0", "Cyrillic"},
      {"\xD0\x81", "\xD1\x91", "Cyrillic IO"},
      {"\xCE\xA3\xCE\x91", "\xCF\x83\xCE\xB1", "Greek"},
      {"\xEF\xBC\xA1", "\xEF\xBD\x81", "fullwidth A"},
      {"\xE6\xB8\xB8\xE6\x88\x8F.exe", "\xE6\xB8\xB8\xE6\x88\x8F.exe", "CJK is unchanged"},
      {"bad\xFF.exe", "bad\xEF\xBF\xBD.exe", "invalid UTF-8 becomes U+FFFD"},
      {"", "", "empty"},
  };
  for (const Case &c : cases) {
    EXPECT_EQ(FoldCase(c.in), c.want) << c.what;
  }
}

TEST(AudioPolicyText, NormalizePath) {
  struct Case {
    const char *in;
    const char *want;
  };
  const Case cases[] = {
      {R"(C:\Users\X\AppData)", R"(c:\users\x\appdata)"},
      {"C:/Users/X/AppData/", R"(c:\users\x\appdata)"},
      {R"(C:\Users\\X\\\AppData\\)", R"(c:\users\x\appdata)"},
      {R"(\\Server\Share\Dir\)", R"(\\server\share\dir)"},
      {"//server//share", R"(\\server\share)"},
      {R"(\\?\C:\Long\Path)", R"(\\?\c:\long\path)"},
      {R"(C:\)", "c:"},
      {R"(\\\)", ""},
      {"", ""},
  };
  for (const Case &c : cases) {
    EXPECT_EQ(NormalizePath(c.in), c.want) << c.in;
  }
}

// 05 §5.3's path rule, §19.1: equal; <dir>\EBWebView below it; '/' and trailing separators normalized; case; <dir>2
// not matching.
TEST(AudioPolicyText, WebViewDataFolderPathRule) {
  const std::string dir = R"(C:\Users\test\AppData\Local\isshoni\WebView2)";
  struct Case {
    const char *path;
    bool want;
  };
  const Case cases[] = {
      {R"(C:\Users\test\AppData\Local\isshoni\WebView2)", true},
      {R"(C:\Users\test\AppData\Local\isshoni\WebView2\EBWebView)", true},
      {"C:/Users/test/AppData/Local/isshoni/WebView2/EBWebView", true},
      {R"(C:\Users\test\AppData\Local\isshoni\WebView2\EBWebView\)", true},
      {R"(c:\users\TEST\appdata\local\ISSHONI\webview2\ebwebview)", true},
      {R"(C:\Users\test\AppData\Local\isshoni\WebView22)", false},
      {R"(C:\Users\test\AppData\Local\isshoni\WebView2 old\EBWebView)", false},
      {R"(C:\Users\test\AppData\Local\isshoni)", false},
      {R"(C:\Users\test\AppData\Local\Other\WebView2\EBWebView)", false},
      {"", false},
  };
  for (const Case &c : cases) {
    EXPECT_EQ(PathEqualsOrBelow(c.path, dir), c.want) << c.path;
  }
  EXPECT_TRUE(PathEqualsOrBelow(R"(C:\a\b)", "C:/a/b/"));
  EXPECT_FALSE(PathEqualsOrBelow(R"(C:\a\b)", ""));
  EXPECT_FALSE(PathEqualsOrBelow(R"(C:\isshoni2\x)", R"(C:\isshoni)"));
}

TEST(AudioPolicyText, SplitCommandLineFollowsTheWindowsRules) {
  struct Case {
    const char *line;
    std::vector<std::string> want;
  };
  const Case cases[] = {
      {R"(a b   c)", {"a", "b", "c"}},
      {R"("C:\Program Files\x\app.exe" --flag)", {R"(C:\Program Files\x\app.exe)", "--flag"}},
      {R"(a"b c"d)", {"ab cd"}},
      {R"(a\\"b c" d)", {R"(a\b c)", "d"}},
      {R"(a\"b c)", {R"(a"b)", "c"}},
      {R"(a\\\"b)", {R"(a\"b)"}},
      {R"(C:\dir\ next)", {R"(C:\dir\)", "next"}},
      {R"("a""b" c)", {R"(a"b)", "c"}},
      {"\t lead and trail \t", {"lead", "and", "trail"}},
      {R"("")", {""}},
      {"", {}},
  };
  for (const Case &c : cases) {
    EXPECT_EQ(SplitCommandLine(c.line), c.want) << c.line;
  }
}

// 05 §7.4, §19.2: quoted and unquoted, forward slashes, and the <dir>\EBWebView form WebView2 really uses.
TEST(AudioPolicyText, ParseUserDataDir) {
  struct Case {
    const char *line;
    const char *want;
  };
  const Case cases[] = {
      {R"("C:\Program Files (x86)\Microsoft\EdgeWebView\Application\130.0\msedgewebview2.exe" --embedded-browser-webview=1 --user-data-dir=C:\Users\test\AppData\Local\isshoni\WebView2\EBWebView --noerrdialogs)",
       R"(C:\Users\test\AppData\Local\isshoni\WebView2\EBWebView)"},
      {R"(msedgewebview2.exe --user-data-dir="C:\Users\test\My Data\isshoni\EBWebView" --type=utility)",
       R"(C:\Users\test\My Data\isshoni\EBWebView)"},
      {R"(msedgewebview2.exe "--user-data-dir=C:\Users\test\My Data\EBWebView")", R"(C:\Users\test\My Data\EBWebView)"},
      {"msedgewebview2.exe --user-data-dir=C:/Users/test/AppData/Local/isshoni/WebView2/EBWebView",
       "C:/Users/test/AppData/Local/isshoni/WebView2/EBWebView"},
      {R"(msedgewebview2.exe --USER-DATA-DIR=C:\x)", R"(C:\x)"},
      {R"(msedgewebview2.exe --user-data-dir=C:\first --user-data-dir=C:\second)", R"(C:\second)"},
      {R"(msedgewebview2.exe --user-data-dir C:\x)", ""},
      {R"(msedgewebview2.exe --other-user-data-dir=C:\x)", ""},
      {R"(msedgewebview2.exe --type=renderer)", ""},
      {"", ""},
  };
  for (const Case &c : cases) {
    EXPECT_EQ(ParseUserDataDir(c.line), c.want) << c.line;
  }
}

TEST(AudioPolicyText, ExeStem) {
  EXPECT_EQ(ExeStem("spotify.exe"), "spotify");
  EXPECT_EQ(ExeStem("Game.EXE"), "Game");
  EXPECT_EQ(ExeStem(".exe"), ".exe");
  EXPECT_EQ(ExeStem("noext"), "noext");
  EXPECT_EQ(ExeStem("a.exe.bak"), "a.exe.bak");
}

} // namespace
