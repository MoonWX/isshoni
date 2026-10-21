// String rules of the exclusion policy (docs/m2/05-windows-audio.md §4.1, §5.1, §5.3, §7.4): case-insensitive exe
// names, the WebView2 data-folder path rule and the --user-data-dir parser the process table uses.
#pragma once

#include <string>
#include <string_view>
#include <vector>

namespace im::audiopolicy {

// The case fold every comparison of exe names, folders and rule values goes through (rules compare
// case-insensitively, 05 §4.1). Windows compares with CompareStringOrdinal(bIgnoreCase = TRUE), which upper-cases
// each UTF-16 unit with the OS table; this portable fold lower-cases ASCII, Latin-1, Latin Extended-A, Greek, Cyrillic
// and the fullwidth Latin letters, one code point to one, and leaves every other code point as it is. Both sides of a
// comparison are folded the same way, so a script it doesn't fold compares exactly. Invalid UTF-8 becomes U+FFFD.
std::string FoldCase(std::string_view utf8);

// A folder path normalized for the path rule (05 §5.3): '/' becomes '\', runs of separators collapse to one (a
// leading "\\" of a UNC or device path stays), trailing separators are stripped and the result is case-folded.
// "C:/Users/X/" and "c:\users\x" normalize to the same string. "" stays "".
std::string NormalizePath(std::string_view path);

// The WebView2 data-folder rule (05 §5.3 row 1): whether path equals dir or lies below it, at a path-separator
// boundary ("...\isshoni\EBWebView" is below "...\isshoni"; "...\isshoni2" is not). Both are normalized first, so
// callers may pass raw paths. An empty dir matches nothing.
bool PathEqualsOrBelow(std::string_view path, std::string_view dir);

// A command line split the way CommandLineToArgvW and the MSVC runtime split one: whitespace separates arguments
// outside quotes, 2n backslashes before a quote give n backslashes and toggle quoting, 2n+1 give n and a literal
// quote, "" inside quotes is a literal quote, and other backslashes are literal (Windows paths keep theirs).
std::vector<std::string> SplitCommandLine(std::string_view command_line);

// The value of the last --user-data-dir=<dir> argument of a WebView2 (msedgewebview2.exe) command line, quoted or not
// ("--user-data-dir=C:\x\EBWebView", "--user-data-dir=\"C:\Program Files\x\"", "\"--user-data-dir=C:\a b\""), or ""
// when there is none (05 §7.4). The switch name compares case-insensitively; the value is returned as written (the
// process table normalizes it).
std::string ParseUserDataDir(std::string_view command_line);

// The exe name without a trailing ".exe" (any case): the display name of last resort (05 §5.1).
std::string_view ExeStem(std::string_view exe) noexcept;

} // namespace im::audiopolicy
