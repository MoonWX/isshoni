// im_build_info (docs/m2/04-windows-engine.md §5.6): what was built, from what, and what is loaded.
#pragma once

#include <string>
#include <string_view>

namespace im::engine {

// The release SemVer (IM_VERSION), the short commit (IM_COMMIT), the FFmpeg build tag this engine was compiled
// against (native/ffmpeg/lock.json's tag; "" before the first published build) and whether test hooks are compiled in.
std::string_view EngineVersion() noexcept;
std::string_view EngineCommit() noexcept;
std::string_view FfmpegTag() noexcept;
bool TestHooks() noexcept;

// The im_build_info document:
//   {"engine":"0.4.0","abi":65536,"commit":"0affed6","testHooks":false,"os":"windows","arch":"amd64",
//    "compiler":"MSVC 19.44","ffmpeg":{"loaded":false,"dir":".","tag":"deps-ffmpeg-9.0.2-r1","avcodec":null,
//    "avutil":null,"license":null},"opus":null,"openh264":{"loaded":false,"version":null}}
// Values that aren't known (no commit, no FFmpeg tag, a library not loaded yet) are null. ffmpeg.dir is relative to the
// engine DLL's folder ("." when the FFmpeg DLLs sit next to it), null when it isn't below it: never an absolute path.
std::string BuildInfoJson();

} // namespace im::engine
