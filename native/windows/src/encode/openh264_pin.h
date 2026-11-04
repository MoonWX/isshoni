// The pinned Cisco OpenH264 release (docs/m2/04-windows-engine.md §9.9, §12.5; "04" below; README WA51): one version
// per isshoni release, compiled into the engine and, with the same values, into the Go binding
// (internal/media/native's OpenH264, whose test reads this file and third_party/openh264/codec_ver.h and fails when
// they disagree). The headers in native/windows/third_party/openh264/ are that release's (codec_ver.h says so; the
// static_assert in openh264_support.h checks it at compile time).
//
// The engine itself checks only the version (WelsGetCodecVersionEx at load, 04 §9.9): Go verifies the file's size and
// SHA-256 before every im_set_openh264_path (VerifyOpenH264). openh264_test downloads the file from kOpenH264Url and
// checks both values here. Portable C++.
//
// Raising the pin: take the newest Cisco release, download <url> over HTTPS, record the decompressed DLL's size and
// SHA-256 here and in internal/media/native/openh264.go, and replace the four headers and LICENSE from the release's
// tag (codec/api/wels/ and the root LICENSE).
#pragma once

#include <cstdint>

// The version string WelsGetCodecVersionEx must match (major.minor.revision; the build number is not compared).
#define IM_OPENH264_VERSION "2.7.0"

namespace im::encode::openh264 {

inline constexpr uint32_t kPinMajor = 2;
inline constexpr uint32_t kPinMinor = 7;
inline constexpr uint32_t kPinRevision = 0;

// Cisco's binary host serves the release over HTTPS as well as HTTP; the hash is pinned either way (04 §9.9).
inline constexpr const char *kOpenH264Url = "https://ciscobinary.openh264.org/openh264-2.7.0-win64.dll.bz2";
// The decompressed DLL: its file name in %LOCALAPPDATA%\isshoni\codecs, byte size and SHA-256 (lower-case hex).
inline constexpr const char *kOpenH264FileName = "openh264-2.7.0-win64.dll";
inline constexpr uint64_t kOpenH264Size = 975352;
inline constexpr const char *kOpenH264Sha256 = "0ef5e7f3b38753b15c13afa0418d4d7805fa228e6adfd3e425bbf758cff1c288";

} // namespace im::encode::openh264
