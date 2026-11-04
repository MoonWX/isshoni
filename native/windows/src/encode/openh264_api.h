// Cisco's OpenH264 DLL, loaded at run time (docs/m2/04-windows-engine.md §9.9, §5.3 im_set_openh264_path; "04" below;
// README WA51).
//
// - The file: an absolute path that Go's VerifyOpenH264 checked (size and SHA-256 of the pinned release) right before
//   it called im_set_openh264_path, or im_init_params.openh264_path (which must have passed the same check). The
//   engine never downloads or hashes it; it loads it by full path with LOAD_LIBRARY_SEARCH_DLL_LOAD_DIR |
//   LOAD_LIBRARY_SEARCH_SYSTEM32 (04 §13.3 layer 1), resolves the encoder (and, for openh264_test, the decoder)
//   functions, and requires WelsGetCodecVersionEx (WelsGetCodecVersion when the Ex form is missing) to report the
//   pinned version (openh264_pin.h).
// - The setting in force: the last successful im_set_openh264_path of this im_init, else im_init's openh264_path.
//   im_set_openh264_path(NULL) turns OpenH264 off. A failed im_set_openh264_path changes nothing. im_shutdown forgets
//   the setting and drops the library (encode.h ForgetOpenH264Setting), so each im_init starts from its own
//   openh264_path.
// - Lifetime: the loaded library is shared by the setting and by every open encoder (std::shared_ptr). Turning
//   OpenH264 off or switching files drops the setting's reference; the DLL is unloaded (FreeLibrary) when the last
//   encoder using it closes, so 03 can delete the file after turning it off (04 §9.9: "turning it off deletes the
//   file"; while a share still encodes with it, the delete fails until that share ends).
// - Failures are IM_ERR_DEPENDENCY with 04 §5.7's token: disabled (no file set), missing (the file is absent or
//   doesn't load), version (another release, or not an OpenH264 DLL: a function is missing), blocked_by_policy
//   (Smart App Control or WDAC refused it). Details never name the path (logs and reports stay free of local paths).
//
// Threads: every function is thread-safe (one mutex); loading happens under it, on the engine worker (im_probe,
// im_start, im_set_openh264_path) or the video thread (a re-init or fallback open).
#pragma once

#include <cstdint>
#include <memory>
#include <optional>
#include <string>
#include <string_view>

#include "encode/openh264_support.h"
#include "isshoni_media.h"

namespace im::encode::openh264 {

// One loaded OpenH264 DLL.
struct Library {
  Library() = default;
  Library(const Library &) = delete;
  Library &operator=(const Library &) = delete;
  ~Library(); // FreeLibrary

  void *module = nullptr; // HMODULE
  decltype(&::WelsCreateSVCEncoder) create_encoder = nullptr;
  decltype(&::WelsDestroySVCEncoder) destroy_encoder = nullptr;
  // The decoder is resolved when present (openh264_test decodes the engine's output with it); the engine never uses it.
  decltype(&::WelsCreateDecoder) create_decoder = nullptr;
  decltype(&::WelsDestroyDecoder) destroy_decoder = nullptr;
  OpenH264Version version{};
  std::string version_text; // "2.7.0"
};

// The library of the setting in force, loaded now if it isn't yet. IM_OK with *out set, or IM_ERR_DEPENDENCY with
// the token (disabled, missing, version, blocked_by_policy) and the thread's detail set.
im_status Acquire(std::shared_ptr<const Library> *out) noexcept;

// What the probe learned about a library (its test encode, 04 §9.10), kept with the setting so that later probes
// and the catalog's refresh (selector.cpp) don't encode again. tested false: no full probe ran since it was loaded.
struct TestRecord {
  bool tested = false;
  im_status status = IM_OK;    // IM_OK or the test's failure
  std::string_view token;      // 04 §5.7's token of a failure
  bool live_bitrate = true;    // the live-bitrate check saw no IDR (04 §9.5, §9.10)
  uint32_t test_encode_us = 0; // the test's wall time
};
// The record of lib (an empty one for a library that is no longer the setting's).
TestRecord TestRecordOf(const Library *lib) noexcept;
// Stores the full probe's result for lib, if it is still the setting's library.
void StoreTestRecord(const Library *lib, const TestRecord &r) noexcept;

#if defined(IM_TEST_HOOKS) && IM_TEST_HOOKS
// openh264_test: the version a library must report, instead of the pin (another release's DLL is the pinned one with
// the expectation moved, as ffmpeg_load_test does for FFmpeg's majors); ResetForTest restores the pin and forgets the
// setting and the loaded library.
void SetExpectedVersionForTest(uint32_t major, uint32_t minor, uint32_t revision) noexcept;
void ResetForTest() noexcept;
#endif

} // namespace im::encode::openh264
