// The Windows plumbing of 05's area (docs/m2/05-windows-audio.md §7, §8; "05" below): the audio SDK headers, an
// owning COM pointer, a COM sink base, strings and the few system calls the watcher shares. Only native/windows/src/
// audio/ and its tests include it.
#pragma once

#include <windows.h>

#if defined(_MSC_VER)
#pragma warning(push, 3) // the SDK's own headers at their usual level, whatever /W4 says
#endif
#include <audioclient.h>
#include <audiopolicy.h>
#include <endpointvolume.h> // IAudioMeterInformation
#include <mmdeviceapi.h>
#if defined(_MSC_VER)
#pragma warning(pop)
#endif

#include <atomic>
#include <cstdint>
#include <string>
#include <string_view>
#include <utility>

namespace im::audio::win {

// An owning COM interface pointer: no WRL or ATL, so the DLL links nothing beyond 04 §13.1's libraries.
template <class T> class ComPtr {
public:
  ComPtr() noexcept = default;
  ComPtr(const ComPtr &o) noexcept : p_(o.p_) {
    if (p_ != nullptr) {
      p_->AddRef();
    }
  }
  ComPtr(ComPtr &&o) noexcept : p_(std::exchange(o.p_, nullptr)) {}
  ComPtr &operator=(ComPtr o) noexcept {
    std::swap(p_, o.p_);
    return *this;
  }
  ~ComPtr() { Reset(); }

  // Takes over a reference the caller owns (a new object's first one).
  static ComPtr Attach(T *p) noexcept {
    ComPtr c;
    c.p_ = p;
    return c;
  }

  T *Get() const noexcept { return p_; }
  T *operator->() const noexcept { return p_; }
  explicit operator bool() const noexcept { return p_ != nullptr; }

  // The address for an out parameter; releases what was held.
  T **Put() noexcept {
    Reset();
    return &p_;
  }
  void **PutVoid() noexcept { return reinterpret_cast<void **>(Put()); }
  void Reset() noexcept {
    if (T *p = std::exchange(p_, nullptr)) {
      p->Release();
    }
  }

  // QueryInterface for U.
  template <class U> HRESULT As(ComPtr<U> &out) const noexcept {
    if (p_ == nullptr) {
      out.Reset();
      return E_POINTER;
    }
    return p_->QueryInterface(__uuidof(U), out.PutVoid());
  }

private:
  T *p_ = nullptr;
};

// The IUnknown half of the watcher's COM sinks (IMMNotificationClient, IAudioSessionNotification,
// IAudioSessionEvents, the activation's completion handler): a thread-safe reference count, deleting at zero.
// Windows calls sinks on its own threads, also after they were unregistered, so a sink only touches state it owns
// through shared pointers (05 §7.1: callbacks only enqueue).
template <class Iface> class Sink : public Iface {
public:
  Sink() = default;
  Sink(const Sink &) = delete;
  Sink &operator=(const Sink &) = delete;

  HRESULT STDMETHODCALLTYPE QueryInterface(REFIID riid, void **ppv) override {
    if (ppv == nullptr) {
      return E_POINTER;
    }
    if (IsEqualIID(riid, __uuidof(IUnknown)) || IsEqualIID(riid, __uuidof(Iface)) || Agile(riid)) {
      *ppv = static_cast<Iface *>(this);
      this->AddRef();
      return S_OK;
    }
    *ppv = nullptr;
    return E_NOINTERFACE;
  }
  ULONG STDMETHODCALLTYPE AddRef() override { return refs_.fetch_add(1, std::memory_order_relaxed) + 1; }
  ULONG STDMETHODCALLTYPE Release() override {
    const ULONG left = refs_.fetch_sub(1, std::memory_order_acq_rel) - 1;
    if (left == 0) {
      delete this;
    }
    return left;
  }

protected:
  virtual ~Sink() = default;
  // Whether the sink also answers for riid (IAgileObject, for the activation handler).
  virtual bool Agile(REFIID) const noexcept { return false; }

private:
  std::atomic<ULONG> refs_{1};
};

// UTF-16 → UTF-8 (a lone surrogate becomes U+FFFD) and UTF-8 → UTF-16 (invalid sequences become U+FFFD).
std::string Utf8(std::wstring_view w);
std::wstring Wide(std::string_view s);

// "0x88890004": an HRESULT for details and logs.
std::string Hr(HRESULT hr);

// A FILETIME as one number (100 ns units since 1601), the creation time of 05 §5.1's process keys.
constexpr uint64_t FiletimeValue(const FILETIME &ft) noexcept {
  return (static_cast<uint64_t>(ft.dwHighDateTime) << 32) | ft.dwLowDateTime;
}

// Whether two UTF-16 strings are equal ignoring case (CompareStringOrdinal, as 05 §5.3's path rule compares).
bool EqualsIgnoreCase(std::wstring_view a, std::wstring_view b) noexcept;

// An export of a system DLL loaded from System32 only (never the application folder, 04 §13.3), or nullptr. The DLL
// stays loaded. No import-table entry: deploy/windows/dll-imports.txt is unchanged by these.
FARPROC SystemProc(const wchar_t *dll, const char *name) noexcept;

// The OS build number (RtlGetVersion: not subject to manifest lies), 0 when unknown.
uint32_t OsBuild() noexcept;

// COM in the multithreaded apartment for the calling thread, for as long as it lives (05 §4.3: the DLL never calls
// CoInitializeEx on a caller's thread, only on its own).
class ComApartment {
public:
  ComApartment() noexcept : hr_(CoInitializeEx(nullptr, COINIT_MULTITHREADED)) {}
  ~ComApartment() {
    if (SUCCEEDED(hr_)) {
      CoUninitialize();
    }
  }
  ComApartment(const ComApartment &) = delete;
  ComApartment &operator=(const ComApartment &) = delete;
  HRESULT hr() const noexcept { return hr_; }

private:
  HRESULT hr_;
};

// A kernel handle closed with CloseHandle.
class Handle {
public:
  Handle() noexcept = default;
  explicit Handle(HANDLE h) noexcept : h_(h == INVALID_HANDLE_VALUE ? nullptr : h) {}
  Handle(Handle &&o) noexcept : h_(std::exchange(o.h_, nullptr)) {}
  Handle &operator=(Handle &&o) noexcept {
    if (this != &o) {
      Reset();
      h_ = std::exchange(o.h_, nullptr);
    }
    return *this;
  }
  Handle(const Handle &) = delete;
  Handle &operator=(const Handle &) = delete;
  ~Handle() { Reset(); }
  HANDLE get() const noexcept { return h_; }
  explicit operator bool() const noexcept { return h_ != nullptr; }
  void Reset() noexcept {
    if (h_ != nullptr) {
      CloseHandle(h_);
      h_ = nullptr;
    }
  }

private:
  HANDLE h_ = nullptr;
};

} // namespace im::audio::win
