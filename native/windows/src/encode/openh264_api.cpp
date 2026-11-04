#include "encode/openh264_api.h"

#include <windows.h>

#include <cstdio>
#include <mutex>
#include <string>
#include <utility>

#include "encode/encode.h"
#include "engine/engine.h"
#include "im/abi/status.h"
#include "im/log/ring.h"

namespace im::encode::openh264 {
namespace {

// Not defined in every SDK's winerror.h (ffmpeg_api.cpp has the same list, 04 §5.7's blocked_by_policy).
constexpr DWORD kErrorSystemIntegrityPolicyViolation = 4551; // ERROR_SYSTEM_INTEGRITY_POLICY_VIOLATION
constexpr DWORD kErrorInvalidImageHash = 577;                // ERROR_INVALID_IMAGE_HASH (STATUS_INVALID_IMAGE_HASH)

bool BlockedByPolicy(DWORD err) noexcept {
  return err == ERROR_ACCESS_DISABLED_BY_POLICY || err == kErrorInvalidImageHash ||
         err == kErrorSystemIntegrityPolicyViolation;
}

std::wstring Widen(const std::string &s) {
  if (s.empty()) {
    return {};
  }
  const int n = MultiByteToWideChar(CP_UTF8, MB_ERR_INVALID_CHARS, s.data(), static_cast<int>(s.size()), nullptr, 0);
  if (n <= 0) {
    return {};
  }
  std::wstring w(static_cast<std::size_t>(n), L'\0');
  MultiByteToWideChar(CP_UTF8, MB_ERR_INVALID_CHARS, s.data(), static_cast<int>(s.size()), w.data(), n);
  return w;
}

std::string VersionText(const OpenH264Version &v) {
  char buf[48];
  std::snprintf(buf, sizeof buf, "%u.%u.%u", v.uMajor, v.uMinor, v.uRevision);
  return buf;
}

struct Expected {
  uint32_t major = kPinMajor;
  uint32_t minor = kPinMinor;
  uint32_t revision = kPinRevision;
};

struct State {
  std::mutex mu;
  // im_set_openh264_path's setting (see openh264_api.h): set means it overrides im_init's openh264_path, as long as
  // that is still init_path (the value in force when the setting was made).
  bool overridden = false;
  std::optional<std::string> override_path;
  std::optional<std::string> init_path;
  // The loaded library of the setting in force and the path it came from.
  std::shared_ptr<const Library> lib;
  std::string lib_path;
  TestRecord record; // lib's
  Expected expected;
};

State &TheState() noexcept {
  static State *const s = new State(); // never destroyed, like the engine
  return *s;
}

// The path in force; mu held.
std::optional<std::string> EffectivePathLocked(State &s) {
  std::optional<std::string> init = engine::CurrentOptions().openh264_path;
  if (s.overridden && s.init_path == init) {
    return s.override_path;
  }
  s.overridden = false; // another im_init's configuration
  s.override_path.reset();
  return init;
}

template <class Fn> bool Resolve(HMODULE m, const char *name, Fn &out) noexcept {
  out = reinterpret_cast<Fn>(reinterpret_cast<void *>(GetProcAddress(m, name)));
  return out != nullptr;
}

// Loads and checks the DLL at path (absolute, UTF-8); mu held (for expected).
im_status OpenAt(const State &s, const std::string &path, std::shared_ptr<const Library> *out) noexcept {
  try {
    const std::wstring wide = Widen(path);
    if (wide.empty()) {
      return abi::Fail(IM_ERR_DEPENDENCY, abi::token::kMissing, "openh264: the path is not valid UTF-8");
    }
    if (GetFileAttributesW(wide.c_str()) == INVALID_FILE_ATTRIBUTES) {
      return abi::Fail(IM_ERR_DEPENDENCY, abi::token::kMissing, "openh264: the OpenH264 file is not there");
    }
    const HMODULE m =
        LoadLibraryExW(wide.c_str(), nullptr, LOAD_LIBRARY_SEARCH_DLL_LOAD_DIR | LOAD_LIBRARY_SEARCH_SYSTEM32);
    if (m == nullptr) {
      const DWORD err = GetLastError();
      return abi::Failf(IM_ERR_DEPENDENCY, BlockedByPolicy(err) ? abi::token::kBlockedByPolicy : abi::token::kMissing,
                        "openh264: the OpenH264 file failed to load (error %lu)", static_cast<unsigned long>(err));
    }
    auto lib = std::make_shared<Library>();
    lib->module = m; // FreeLibrary from here on, in ~Library
    decltype(&::WelsGetCodecVersionEx) version_ex = nullptr;
    decltype(&::WelsGetCodecVersion) version_fn = nullptr;
    if (Resolve(m, "WelsGetCodecVersionEx", version_ex)) {
      version_ex(&lib->version);
    } else if (Resolve(m, "WelsGetCodecVersion", version_fn)) {
      lib->version = version_fn();
    } else {
      return abi::Fail(IM_ERR_DEPENDENCY, abi::token::kVersion,
                       "openh264: the file has no WelsGetCodecVersion (not an OpenH264 library)");
    }
    lib->version_text = VersionText(lib->version);
    if (lib->version.uMajor != s.expected.major || lib->version.uMinor != s.expected.minor ||
        lib->version.uRevision != s.expected.revision) {
      return abi::Failf(IM_ERR_DEPENDENCY, abi::token::kVersion, "openh264: OpenH264 %s, want %u.%u.%u",
                        lib->version_text.c_str(), s.expected.major, s.expected.minor, s.expected.revision);
    }
    if (!Resolve(m, "WelsCreateSVCEncoder", lib->create_encoder) ||
        !Resolve(m, "WelsDestroySVCEncoder", lib->destroy_encoder)) {
      return abi::Fail(IM_ERR_DEPENDENCY, abi::token::kVersion, "openh264: the encoder functions are missing");
    }
    if (!Resolve(m, "WelsCreateDecoder", lib->create_decoder) ||
        !Resolve(m, "WelsDestroyDecoder", lib->destroy_decoder)) {
      lib->create_decoder = nullptr;
      lib->destroy_decoder = nullptr;
    }
    *out = std::move(lib);
    return IM_OK;
  } catch (...) {
    return abi::Fail(IM_ERR_INTERNAL, {}, "openh264: out of memory");
  }
}

} // namespace

Library::~Library() {
  if (module != nullptr) {
    FreeLibrary(static_cast<HMODULE>(module));
  }
}

im_status Acquire(std::shared_ptr<const Library> *out) noexcept {
  out->reset();
  State &s = TheState();
  try {
    std::lock_guard<std::mutex> lock(s.mu);
    const std::optional<std::string> path = EffectivePathLocked(s);
    if (!path) {
      if (s.lib) {
        s.lib.reset(); // a new im_init without OpenH264: the next encoder close unloads it
        s.lib_path.clear();
        s.record = TestRecord{};
      }
      return abi::Fail(IM_ERR_DEPENDENCY, abi::token::kDisabled, "openh264: OpenH264 is not turned on");
    }
    if (s.lib && s.lib_path == *path) {
      *out = s.lib;
      return IM_OK;
    }
    std::shared_ptr<const Library> lib;
    if (const im_status st = OpenAt(s, *path, &lib); st != IM_OK) {
      return st;
    }
    s.lib = lib;
    s.lib_path = *path;
    s.record = TestRecord{};
    log::Info("encode", "openh264 loaded", {{"version", std::string_view(lib->version_text)}});
    *out = std::move(lib);
    return IM_OK;
  } catch (...) {
    return abi::Fail(IM_ERR_INTERNAL, {}, "openh264: out of memory");
  }
}

TestRecord TestRecordOf(const Library *lib) noexcept {
  State &s = TheState();
  std::lock_guard<std::mutex> lock(s.mu);
  if (lib == nullptr || s.lib.get() != lib) {
    return {};
  }
  return s.record;
}

void StoreTestRecord(const Library *lib, const TestRecord &r) noexcept {
  State &s = TheState();
  std::lock_guard<std::mutex> lock(s.mu);
  if (lib != nullptr && s.lib.get() == lib) {
    s.record = r;
    s.record.tested = true;
  }
}

#if defined(IM_TEST_HOOKS) && IM_TEST_HOOKS
void SetExpectedVersionForTest(uint32_t major, uint32_t minor, uint32_t revision) noexcept {
  State &s = TheState();
  std::lock_guard<std::mutex> lock(s.mu);
  s.expected = Expected{major, minor, revision};
}

void ResetForTest() noexcept {
  State &s = TheState();
  std::lock_guard<std::mutex> lock(s.mu);
  s.expected = Expected{};
  s.overridden = false;
  s.override_path.reset();
  s.init_path.reset();
  s.lib.reset();
  s.lib_path.clear();
  s.record = TestRecord{};
}
#endif

namespace {

// im_set_openh264_path's work, behind encode::SetOpenH264Path's exception guard.
im_status SetPath(const std::optional<std::string> &path) {
  State &s = TheState();
  std::lock_guard<std::mutex> lock(s.mu);
  std::optional<std::string> init = engine::CurrentOptions().openh264_path;
  if (!path) {
    s.overridden = true;
    s.override_path.reset();
    s.init_path = std::move(init);
    if (s.lib) {
      log::Info("encode", "openh264 turned off");
    }
    s.lib.reset(); // unloaded when the last encoder using it closes
    s.lib_path.clear();
    s.record = TestRecord{};
    return IM_OK;
  }
  std::shared_ptr<const Library> lib;
  if (s.lib && s.lib_path == *path) {
    lib = s.lib; // the same file again: keep the loaded library and its probe record
  } else if (const im_status st = OpenAt(s, *path, &lib); st != IM_OK) {
    return st; // nothing changes
  }
  s.overridden = true;
  s.override_path = *path;
  s.init_path = std::move(init);
  if (s.lib != lib) {
    s.lib = lib;
    s.lib_path = *path;
    s.record = TestRecord{};
    log::Info("encode", "openh264 loaded", {{"version", std::string_view(lib->version_text)}});
  }
  return IM_OK;
}

} // namespace

} // namespace im::encode::openh264

namespace im::encode {

im_status SetOpenH264Path(const std::optional<std::string> &path) {
  try {
    return openh264::SetPath(path);
  } catch (...) {
    return abi::Fail(IM_ERR_INTERNAL, {}, "im_set_openh264_path: out of memory");
  }
}

OpenH264Info GetOpenH264Info() {
  openh264::State &s = openh264::TheState();
  OpenH264Info info;
  try {
    std::lock_guard<std::mutex> lock(s.mu);
    if (s.lib) {
      info.loaded = true;
      info.version = s.lib->version_text;
    }
  } catch (...) {
    return {};
  }
  return info;
}

} // namespace im::encode
