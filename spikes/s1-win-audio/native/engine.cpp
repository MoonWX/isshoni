// Spike S1: "all system audio except excluded apps" on Windows.
//
// WASAPI process loopback takes exactly ONE target process tree per stream
// (include or exclude), so excluding several apps (Discord + TeamSpeak + a
// browser + isshoni itself) can't be done with one EXCLUDE stream. Instead the
// engine keeps an "include set": it polls every audio session on every output
// device, classifies each process against the exclusion rules, opens one
// INCLUDE_TARGET_PROCESS_TREE loopback stream per allowed root process, and
// mixes them on a 10 ms clock with fades so streams come and go without clicks.
//
// Two notions of "process tree" are used on purpose:
//  * the OS tree (every descendant, across launchers and folders) is what an
//    INCLUDE stream actually captures, so it decides whether a root is safe to
//    include and whether one root already covers another;
//  * the app tree (ancestors in the same install folder, not past launchers)
//    decides which app a process belongs to, so a browser opened from a
//    Discord link isn't treated as part of Discord.
//
// Threads: poller (sessions → streams, status), mixer (10 ms ticks → output
// queue), one capture thread per stream. See ../README.md.

#define IM_BUILDING 1
#include "isshoni_audio.h"
#include "win_compat.h"

#include <endpointvolume.h>

#include <algorithm>
#include <atomic>
#include <chrono>
#include <cmath>
#include <condition_variable>
#include <cstdio>
#include <cstring>
#include <cwctype>
#include <deque>
#include <iterator>
#include <memory>
#include <mutex>
#include <string>
#include <thread>
#include <unordered_map>
#include <unordered_set>
#include <vector>

#ifndef LOAD_LIBRARY_SEARCH_SYSTEM32
#define LOAD_LIBRARY_SEARCH_SYSTEM32 0x00000800
#endif
#ifndef AUDCLNT_S_NO_SINGLE_PROCESS
#define AUDCLNT_S_NO_SINGLE_PROCESS AUDCLNT_SUCCESS(0x00d)
#endif

namespace im {
namespace {

constexpr int kRate = IM_AUDIO_SAMPLE_RATE;
constexpr int kChunk = IM_AUDIO_CHUNK_FRAMES;           // 10 ms
constexpr size_t kPrerollFrames = 4 * kChunk;           // 40 ms buffered before a stream plays
constexpr size_t kLowWaterFrames = 2 * kChunk;          // below this a running stream fades out, then re-prerolls
constexpr size_t kMaxBufferedFrames = 10 * kChunk;      // above 100 ms…
constexpr size_t kTrimToFrames = 4 * kChunk;            // …fade out, drop back to 40 ms, fade in
constexpr size_t kTailRampFrames = 64;                  // backstop ramp if a chunk ends early
constexpr size_t kOnsetSilenceFrames = kRate / 20;      // 50 ms of digital silence…
constexpr size_t kOnsetRampFrames = kRate / 200;        // …then audio starts: ramp it in over 5 ms
constexpr size_t kRingFrames = kRate;                   // 1 s per stream
constexpr int kMissingPollsBeforeRemoval = 3;           // session gone for 1.5 s (process alive) → drop stream
constexpr int kPollMs = 500;
constexpr int64_t kMicStickyNs = 10'000'000'000;        // a mic user stays excluded 10 s after its mic session
constexpr size_t kMaxQueuedChunks = 200;                // 2 s of output if the host stalls
constexpr REFERENCE_TIME kProcessBuffer = 1'000'000;    // 100 ms: slack only; evented capture latency is the drain
constexpr REFERENCE_TIME kEndpointBuffer = 2'000'000;   // 200 ms

// ---------------------------------------------------------------- utilities

thread_local std::string g_last_error;

void SetError(std::string msg) { g_last_error = std::move(msg); }

std::string HrStr(HRESULT hr) {
  char b[16];
  std::snprintf(b, sizeof b, "0x%08lX", static_cast<unsigned long>(hr));
  return b;
}

std::string Utf8(const std::wstring &w) {
  if (w.empty()) return {};
  int n = WideCharToMultiByte(CP_UTF8, 0, w.data(), static_cast<int>(w.size()), nullptr, 0, nullptr, nullptr);
  std::string s(static_cast<size_t>(n), '\0');
  WideCharToMultiByte(CP_UTF8, 0, w.data(), static_cast<int>(w.size()), s.data(), n, nullptr, nullptr);
  return s;
}

std::wstring Wide(const char *s) {
  if (!s || !*s) return {};
  int n = MultiByteToWideChar(CP_UTF8, 0, s, -1, nullptr, 0);
  std::wstring w(static_cast<size_t>(n), L'\0');
  MultiByteToWideChar(CP_UTF8, 0, s, -1, w.data(), n);
  w.resize(static_cast<size_t>(n > 0 ? n - 1 : 0));
  return w;
}

std::wstring Lower(std::wstring s) {
  for (auto &c : s) c = static_cast<wchar_t>(std::towlower(c));
  return s;
}

void JsonStr(std::string &out, const std::string &s) {
  out += '"';
  for (unsigned char c : s) {
    switch (c) {
      case '"': out += "\\\""; break;
      case '\\': out += "\\\\"; break;
      case '\n': out += "\\n"; break;
      case '\r': out += "\\r"; break;
      case '\t': out += "\\t"; break;
      default:
        if (c < 0x20) {
          char b[8];
          std::snprintf(b, sizeof b, "\\u%04x", c);
          out += b;
        } else {
          out += static_cast<char>(c);
        }
    }
  }
  out += '"';
}

std::string JsonNum(double v) {  // finite numbers only (JSON has no inf/nan)
  if (!std::isfinite(v)) v = 0;
  char b[32];
  std::snprintf(b, sizeof b, "%.4f", v);
  return b;
}

int32_t CopyOut(const std::string &s, char *buf, int32_t cap) {
  if (!buf || cap <= 0 || static_cast<size_t>(cap) <= s.size()) return IM_ERR_BUFFER_TOO_SMALL;
  std::memcpy(buf, s.data(), s.size());
  buf[s.size()] = '\0';
  return static_cast<int32_t>(s.size());
}

int64_t QpcNs() {
  static const int64_t freq = [] {
    LARGE_INTEGER f;
    QueryPerformanceFrequency(&f);
    return static_cast<int64_t>(f.QuadPart);
  }();
  LARGE_INTEGER c;
  QueryPerformanceCounter(&c);
  return static_cast<int64_t>((static_cast<long double>(c.QuadPart) * 1'000'000'000.0L) / freq);
}

DWORD OsBuild() {
  using RtlGetVersionFn = LONG(WINAPI *)(OSVERSIONINFOW *);
  auto fn = reinterpret_cast<RtlGetVersionFn>(
      reinterpret_cast<void *>(GetProcAddress(GetModuleHandleW(L"ntdll.dll"), "RtlGetVersion")));
  OSVERSIONINFOW v{};
  v.dwOSVersionInfoSize = sizeof v;
  if (fn && fn(&v) == 0) return v.dwBuildNumber;
  return 0;
}

// Loads a system DLL from System32 only (never the application folder).
HMODULE LoadSystemDll(const wchar_t *name) { return LoadLibraryExW(name, nullptr, LOAD_LIBRARY_SEARCH_SYSTEM32); }

struct ComInit {
  HRESULT hr;
  ComInit() : hr(CoInitializeEx(nullptr, COINIT_MULTITHREADED)) {}
  ~ComInit() {
    if (SUCCEEDED(hr)) CoUninitialize();
  }
};

// Registers the calling thread with MMCSS ("Pro Audio") so capture and mixing
// keep their 10 ms deadlines under load.
class MmcssScope {
 public:
  MmcssScope() {
    static auto set = reinterpret_cast<HANDLE(WINAPI *)(LPCWSTR, LPDWORD)>(
        reinterpret_cast<void *>(GetProcAddress(LoadSystemDll(L"avrt.dll"), "AvSetMmThreadCharacteristicsW")));
    DWORD task = 0;
    if (set) h_ = set(L"Pro Audio", &task);
  }
  ~MmcssScope() {
    static auto revert = reinterpret_cast<BOOL(WINAPI *)(HANDLE)>(
        reinterpret_cast<void *>(GetProcAddress(LoadSystemDll(L"avrt.dll"), "AvRevertMmThreadCharacteristics")));
    if (h_ && revert) revert(h_);
  }

 private:
  HANDLE h_ = nullptr;
};

template <class T>
class ComPtr {
 public:
  ComPtr() = default;
  ComPtr(const ComPtr &) = delete;
  ComPtr &operator=(const ComPtr &) = delete;
  ~ComPtr() { Reset(); }
  T *operator->() const { return p_; }
  T *Get() const { return p_; }
  T **Put() {
    Reset();
    return &p_;
  }
  void **PutVoid() { return reinterpret_cast<void **>(Put()); }
  void Reset() {
    if (p_) p_->Release();
    p_ = nullptr;
  }
  explicit operator bool() const { return p_ != nullptr; }

 private:
  T *p_ = nullptr;
};

inline const IID kIID_IAudioEndpointVolume = {0x5CDF2C82, 0x841E, 0x4546, {0x97, 0x22, 0x0C, 0xF7, 0x40, 0x78, 0x22, 0x9A}};

// ---------------------------------------------------------------- processes

struct ProcInfo {
  DWORD pid = 0;
  DWORD ppid = 0;
  std::wstring exe;  // lower-case file name
  std::wstring dir;  // lower-case image directory, empty if unknown (e.g. protected process)
  ULONGLONG created = 0;
};

// Snapshot of running processes with parent links. Parent links are only
// trusted if the parent is older than the child (PIDs get reused).
class ProcessTable {
 public:
  void Refresh() {
    std::unordered_map<DWORD, ProcInfo> next;
    HANDLE snap = CreateToolhelp32Snapshot(TH32CS_SNAPPROCESS, 0);
    if (snap == INVALID_HANDLE_VALUE) return;
    PROCESSENTRY32W pe{};
    pe.dwSize = sizeof pe;
    if (Process32FirstW(snap, &pe)) {
      do {
        ProcInfo pi;
        pi.pid = pe.th32ProcessID;
        pi.ppid = pe.th32ParentProcessID;
        pi.exe = Lower(pe.szExeFile);
        auto old = procs_.find(pi.pid);
        if (old != procs_.end() && old->second.ppid == pi.ppid && old->second.exe == pi.exe) {
          pi.created = old->second.created;
          pi.dir = old->second.dir;
        } else {
          QueryDetails(pi);
        }
        next.emplace(pi.pid, std::move(pi));
      } while (Process32NextW(snap, &pe));
    }
    CloseHandle(snap);
    procs_.swap(next);
  }

  const ProcInfo *Get(DWORD pid) const {
    auto it = procs_.find(pid);
    return it == procs_.end() ? nullptr : &it->second;
  }

  bool Alive(DWORD pid, ULONGLONG created) const {
    const ProcInfo *p = Get(pid);
    return p && (created == 0 || p->created == created);
  }

  const ProcInfo *Parent(const ProcInfo &p) const {
    if (p.ppid == 0 || p.ppid == p.pid) return nullptr;
    const ProcInfo *par = Get(p.ppid);
    if (!par) return nullptr;
    if (par->created && p.created && par->created > p.created) return nullptr;  // reused PID
    return par;
  }

  // OS-tree membership (what an INCLUDE/EXCLUDE stream covers).
  bool IsSelfOrDescendant(DWORD pid, DWORD root) const {
    const ProcInfo *p = Get(pid);
    for (int depth = 0; p && depth < 64; ++depth) {
      if (p->pid == root) return true;
      p = Parent(*p);
    }
    return pid == root;
  }

  std::string ExeUtf8(DWORD pid) const {
    const ProcInfo *p = Get(pid);
    return p ? Utf8(p->exe) : std::string("?");
  }

  const std::unordered_map<DWORD, ProcInfo> &All() const { return procs_; }

 private:
  static void QueryDetails(ProcInfo &pi) {
    HANDLE h = OpenProcess(PROCESS_QUERY_LIMITED_INFORMATION, FALSE, pi.pid);
    if (!h) return;
    FILETIME c, e, k, u;
    if (GetProcessTimes(h, &c, &e, &k, &u)) pi.created = (static_cast<ULONGLONG>(c.dwHighDateTime) << 32) | c.dwLowDateTime;
    wchar_t path[MAX_PATH * 2];
    DWORD len = static_cast<DWORD>(std::size(path));
    if (QueryFullProcessImageNameW(h, 0, path, &len)) {
      std::wstring full = Lower(std::wstring(path, len));
      size_t slash = full.find_last_of(L'\\');
      if (slash != std::wstring::npos) pi.dir = full.substr(0, slash);
    }
    CloseHandle(h);
  }

  std::unordered_map<DWORD, ProcInfo> procs_;
};

// Ancestors at or above these end every app walk: a game started from Steam
// isn't part of Steam, and everything started from Explorer isn't one app.
// They are also never used as INCLUDE roots (their OS tree is "everything").
const wchar_t *const kLaunchers[] = {
    L"explorer.exe",    L"steam.exe",       L"epicgameslauncher.exe", L"battle.net.exe",  L"galaxyclient.exe",
    L"eadesktop.exe",   L"upc.exe",         L"riotclientservices.exe", L"heroic.exe",     L"playnite.desktopapp.exe",
    L"services.exe",    L"svchost.exe",     L"wininit.exe",           L"winlogon.exe",    L"userinit.exe",
    L"sihost.exe",      L"cmd.exe",         L"powershell.exe",        L"pwsh.exe",        L"windowsterminal.exe",
    L"openconsole.exe", L"conhost.exe",     L"runtimebroker.exe",     L"dllhost.exe",     L"wsl.exe",
    L"taskhostw.exe",   L"xboxpcapp.exe",   L"overwolf.exe",
};

// Shared runtimes installed outside their host app's folder but still part of it
// (e.g. Teams' WebView2 audio belongs to Teams).
const wchar_t *const kSharedRuntimes[] = {L"msedgewebview2.exe"};

bool IsLauncher(const std::wstring &exe) {
  for (const wchar_t *l : kLaunchers)
    if (exe == l) return true;
  return false;
}

bool IsSharedRuntime(const std::wstring &exe) {
  for (const wchar_t *r : kSharedRuntimes)
    if (exe == r) return true;
  return false;
}

// The parent of p if it belongs to the same app: not a launcher, and installed
// in the same folder (or a parent folder) as p. A browser that Discord opened
// for a link lives elsewhere, so it is not part of Discord.
const ProcInfo *AppParent(const ProcessTable &pt, const ProcInfo &p) {
  const ProcInfo *par = pt.Parent(p);
  if (!par || IsLauncher(par->exe)) return nullptr;
  if (IsSharedRuntime(p.exe)) return par;
  if (p.dir.empty() || par->dir.empty()) return nullptr;  // unknown: treat as a boundary
  if (p.dir == par->dir) return par;
  if (p.dir.size() > par->dir.size() && p.dir.compare(0, par->dir.size(), par->dir) == 0 &&
      p.dir[par->dir.size()] == L'\\')
    return par;
  return nullptr;
}

// The topmost process of the app pid belongs to.
DWORD AppRoot(const ProcessTable &pt, DWORD pid) {
  const ProcInfo *p = pt.Get(pid);
  if (!p) return pid;
  for (int depth = 0; depth < 64; ++depth) {
    const ProcInfo *par = AppParent(pt, *p);
    if (!par) break;
    p = par;
  }
  return p->pid;
}

// ---------------------------------------------------------------- rules

struct Rules {
  std::vector<std::wstring> apps;  // lower-case exe names, in priority order
  std::vector<DWORD> instances;
};

std::mutex g_rules_mu;
Rules g_rules;

Rules CopyRules() {
  std::lock_guard<std::mutex> lk(g_rules_mu);
  return g_rules;
}

struct Verdict {
  bool excluded = false;
  std::string reason;
  int rule_index = -1;  // for app rules: position in Rules::apps
};

Verdict Classify(DWORD pid, const ProcessTable &pt, const Rules &rules, const std::unordered_set<DWORD> &mic_roots,
                 bool exclude_mic_users, DWORD self) {
  if (pt.IsSelfOrDescendant(pid, self)) return {true, "isshoni itself"};
  for (DWORD inst : rules.instances)
    if (pt.IsSelfOrDescendant(pid, inst)) return {true, "instance rule (pid " + std::to_string(inst) + ")"};
  const ProcInfo *p = pt.Get(pid);
  if (!p) return {false, ""};
  const ProcInfo *cur = p;
  for (int depth = 0; cur && depth < 64; ++depth, cur = AppParent(pt, *cur)) {
    for (size_t i = 0; i < rules.apps.size(); ++i) {
      if (cur->exe == rules.apps[i]) {
        std::string r = "app rule " + Utf8(rules.apps[i]);
        if (depth > 0) r += " (" + Utf8(p->exe) + " is part of it)";
        return {true, r, static_cast<int>(i)};
      }
    }
  }
  if (exclude_mic_users) {
    DWORD root = AppRoot(pt, pid);
    if (mic_roots.count(root)) return {true, "uses the microphone (app " + pt.ExeUtf8(root) + ")"};
  }
  return {false, ""};
}

// ---------------------------------------------------------------- sessions

struct SessionInfo {
  DWORD pid;
  int state;  // AudioSessionState
  bool multi_process;
};

// Lists non-expired audio sessions on every active endpoint of one direction
// (render = playing, capture = microphone users). Returns false if any device
// could not be enumerated (the list may then be incomplete).
bool EnumSessions(IMMDeviceEnumerator *en, EDataFlow flow, std::vector<SessionInfo> &out, std::string &err) {
  ComPtr<IMMDeviceCollection> coll;
  HRESULT hr = en->EnumAudioEndpoints(flow, DEVICE_STATE_ACTIVE, coll.Put());
  if (FAILED(hr)) {
    err = "EnumAudioEndpoints " + HrStr(hr);
    return false;
  }
  bool complete = true;
  UINT n = 0;
  coll->GetCount(&n);
  std::unordered_set<DWORD> seen;
  for (UINT i = 0; i < n; ++i) {
    ComPtr<IMMDevice> dev;
    ComPtr<IAudioSessionManager2> mgr;
    ComPtr<IAudioSessionEnumerator> se;
    if (FAILED(coll->Item(i, dev.Put())) ||
        FAILED(hr = dev->Activate(kIID_IAudioSessionManager2, CLSCTX_ALL, nullptr, mgr.PutVoid())) ||
        FAILED(hr = mgr->GetSessionEnumerator(se.Put()))) {
      complete = false;
      err = std::string(flow == eRender ? "output" : "input") + " device " + std::to_string(i) + ": " + HrStr(hr);
      continue;
    }
    int count = 0;
    se->GetCount(&count);
    for (int j = 0; j < count; ++j) {
      ComPtr<IAudioSessionControl> ctl;
      if (FAILED(se->GetSession(j, ctl.Put()))) continue;
      AudioSessionState st = AudioSessionStateExpired;
      ctl->GetState(&st);
      if (st == AudioSessionStateExpired) continue;
      ComPtr<IAudioSessionControl2> ctl2;
      if (FAILED(ctl->QueryInterface(kIID_IAudioSessionControl2, ctl2.PutVoid()))) continue;
      if (ctl2->IsSystemSoundsSession() == S_OK) continue;  // Windows notification sounds
      DWORD pid = 0;
      HRESULT pr = ctl2->GetProcessId(&pid);
      if ((pr != S_OK && pr != AUDCLNT_S_NO_SINGLE_PROCESS) || pid == 0) continue;
      if (seen.insert(pid).second) out.push_back({pid, static_cast<int>(st), pr == AUDCLNT_S_NO_SINGLE_PROCESS});
    }
  }
  return complete;
}

// ---------------------------------------------------------------- activation

class CompletionHandler final : public ActivateCompletionHandler {
 public:
  CompletionHandler() : done(CreateEventW(nullptr, TRUE, FALSE, nullptr)) {}

  HRESULT STDMETHODCALLTYPE QueryInterface(REFIID riid, void **ppv) override {
    if (!ppv) return E_POINTER;
    if (IsEqualIID(riid, kIID_IUnknown) || IsEqualIID(riid, kIID_IActivateCompletionHandler) ||
        IsEqualIID(riid, kIID_IAgileObject)) {
      *ppv = static_cast<ActivateCompletionHandler *>(this);
      AddRef();
      return S_OK;
    }
    *ppv = nullptr;
    return E_NOINTERFACE;
  }
  ULONG STDMETHODCALLTYPE AddRef() override { return ++refs_; }
  ULONG STDMETHODCALLTYPE Release() override {
    ULONG r = --refs_;
    if (r == 0) delete this;
    return r;
  }
  HRESULT STDMETHODCALLTYPE ActivateCompleted(ActivateAsyncOp *op) override {
    HRESULT activate = E_FAIL;
    IUnknown *unk = nullptr;
    HRESULT hr = op->GetActivateResult(&activate, &unk);
    if (SUCCEEDED(hr) && SUCCEEDED(activate) && unk) {
      result = unk->QueryInterface(kIID_IAudioClient, reinterpret_cast<void **>(&client));
    } else {
      result = FAILED(hr) ? hr : FAILED(activate) ? activate : E_NOINTERFACE;
    }
    if (unk) unk->Release();
    SetEvent(done);
    Release();  // drops the reference taken for this callback (see ActivateProcessLoopback)
    return S_OK;
  }

  HANDLE done;
  HRESULT result = E_PENDING;
  IAudioClient *client = nullptr;

 private:
  ~CompletionHandler() {
    if (client) client->Release();
    CloseHandle(done);
  }
  std::atomic<ULONG> refs_{1};
};

ActivateAudioInterfaceAsyncFn ActivateFn() {
  static ActivateAudioInterfaceAsyncFn fn = [] {
    HMODULE m = LoadSystemDll(L"mmdevapi.dll");
    return m ? reinterpret_cast<ActivateAudioInterfaceAsyncFn>(
                   reinterpret_cast<void *>(GetProcAddress(m, "ActivateAudioInterfaceAsync")))
             : nullptr;
  }();
  return fn;
}

// stop_event (optional) aborts the wait early when the engine is stopping.
HRESULT ActivateProcessLoopback(DWORD pid, bool include_tree, HANDLE stop_event, IAudioClient **out) {
  ActivateAudioInterfaceAsyncFn fn = ActivateFn();
  if (!fn) return E_NOTIMPL;
  AUDIOCLIENT_ACTIVATION_PARAMS params{};
  params.ActivationType = AUDIOCLIENT_ACTIVATION_TYPE_PROCESS_LOOPBACK;
  params.ProcessLoopbackParams.TargetProcessId = pid;
  params.ProcessLoopbackParams.ProcessLoopbackMode =
      include_tree ? PROCESS_LOOPBACK_MODE_INCLUDE_TARGET_PROCESS_TREE : PROCESS_LOOPBACK_MODE_EXCLUDE_TARGET_PROCESS_TREE;
  PROPVARIANT pv;
  PropVariantInit(&pv);
  pv.vt = VT_BLOB;
  pv.blob.cbSize = sizeof params;
  pv.blob.pBlobData = reinterpret_cast<BYTE *>(&params);  // not owned: no PropVariantClear

  auto *handler = new CompletionHandler();
  // Keep the handler (and its event) alive until ActivateCompleted runs, even
  // if we stop waiting: Windows' own reference is not documented to suffice.
  handler->AddRef();
  ActivateAsyncOp *op = nullptr;
  HRESULT hr = fn(VIRTUAL_AUDIO_DEVICE_PROCESS_LOOPBACK, kIID_IAudioClient, &pv, handler, &op);
  if (FAILED(hr)) handler->Release();  // the callback will never run
  if (SUCCEEDED(hr)) {
    HANDLE waits[2] = {handler->done, stop_event};
    DWORD w = WaitForMultipleObjects(stop_event ? 2 : 1, waits, FALSE, 5000);
    if (w == WAIT_OBJECT_0 + 1) {
      hr = E_ABORT;
    } else if (w != WAIT_OBJECT_0) {
      hr = HRESULT_FROM_WIN32(ERROR_TIMEOUT);
    } else {
      hr = handler->result;
      if (SUCCEEDED(hr)) {
        *out = handler->client;
        handler->client = nullptr;
      }
    }
  }
  if (op) op->Release();
  handler->Release();
  return hr;
}

// ---------------------------------------------------------------- stream

enum class StreamKind { Include, Exclude, Endpoint };

const char *KindName(StreamKind k) {
  switch (k) {
    case StreamKind::Include: return "include";
    case StreamKind::Exclude: return "exclude";
    case StreamKind::Endpoint: return "endpoint";
  }
  return "?";
}

// One loopback capture (a process tree, or the default output device) with
// its own capture thread feeding a ring buffer of 48 kHz stereo float frames.
class LoopbackStream {
 public:
  static std::unique_ptr<LoopbackStream> Open(StreamKind kind, DWORD pid, HANDLE stop_event, std::string &err) {
    std::unique_ptr<LoopbackStream> s(new LoopbackStream(kind, pid));
    if (!s->Init(stop_event, err)) return nullptr;
    return s;
  }

  ~LoopbackStream() {
    stop_ = true;
    if (thread_.joinable()) thread_.join();  // Run() releases the COM objects on its own (MTA) thread
    ReleaseCom();                            // only if the thread never started
    if (event_) CloseHandle(event_);
  }

  // Pops up to `frames` frames into dst; zero-fills the rest. Returns frames popped.
  size_t Pop(float *dst, size_t frames) {
    std::lock_guard<std::mutex> lk(mu_);
    size_t n = std::min(frames, size_);
    for (size_t i = 0; i < n; ++i) {
      size_t at = ((head_ + i) % kRingFrames) * 2;
      dst[2 * i] = ring_[at];
      dst[2 * i + 1] = ring_[at + 1];
    }
    std::fill(dst + 2 * n, dst + 2 * frames, 0.0f);
    head_ = (head_ + n) % kRingFrames;
    size_ -= n;
    return n;
  }

  size_t Buffered() {
    std::lock_guard<std::mutex> lk(mu_);
    return size_;
  }

  void Trim(size_t keep) {
    std::lock_guard<std::mutex> lk(mu_);
    if (size_ <= keep) return;
    head_ = (head_ + (size_ - keep)) % kRingFrames;
    size_ = keep;
  }

  bool Failed() const { return failed_.load(); }
  std::string FailReason() {
    std::lock_guard<std::mutex> lk(mu_);
    return fail_reason_;
  }

  const StreamKind kind;
  const DWORD pid;
  std::string format;      // which Initialize attempt succeeded
  std::string attempts;    // failed attempts before it
  UINT32 buffer_frames = 0;
  std::atomic<uint64_t> packets{0}, silent_packets{0}, discontinuities{0}, overflows{0};

 private:
  LoopbackStream(StreamKind k, DWORD p) : kind(k), pid(p), ring_(kRingFrames * 2, 0.0f) {}

  void ReleaseCom() {
    if (client_) client_->Stop();
    if (capture_) capture_->Release();
    if (client_) client_->Release();
    capture_ = nullptr;
    client_ = nullptr;
  }

  HRESULT Activate(HANDLE stop_event) {
    if (kind == StreamKind::Endpoint) {
      ComPtr<IMMDeviceEnumerator> en;
      HRESULT hr = CoCreateInstance(kCLSID_MMDeviceEnumerator, nullptr, CLSCTX_ALL, kIID_IMMDeviceEnumerator, en.PutVoid());
      ComPtr<IMMDevice> dev;
      if (SUCCEEDED(hr)) hr = en->GetDefaultAudioEndpoint(eRender, eConsole, dev.Put());
      if (SUCCEEDED(hr)) hr = dev->Activate(kIID_IAudioClient, CLSCTX_ALL, nullptr, reinterpret_cast<void **>(&client_));
      return hr;
    }
    return ActivateProcessLoopback(pid, kind == StreamKind::Include, stop_event, &client_);
  }

  bool Init(HANDLE stop_event, std::string &err) {
    WAVEFORMATEX fmt_float{};
    fmt_float.wFormatTag = WAVE_FORMAT_IEEE_FLOAT;
    fmt_float.nChannels = 2;
    fmt_float.nSamplesPerSec = kRate;
    fmt_float.wBitsPerSample = 32;
    fmt_float.nBlockAlign = 8;
    fmt_float.nAvgBytesPerSec = kRate * 8;
    WAVEFORMATEX fmt_pcm = fmt_float;
    fmt_pcm.wFormatTag = WAVE_FORMAT_PCM;
    fmt_pcm.wBitsPerSample = 16;
    fmt_pcm.nBlockAlign = 4;
    fmt_pcm.nAvgBytesPerSec = kRate * 4;

    // Process loopback: event-driven, and the known-good shape first (plain
    // LOOPBACK|EVENTCALLBACK, as in Microsoft's sample). Endpoint loopback:
    // polled, and needs format conversion from the device mix format.
    const bool evented = kind != StreamKind::Endpoint;
    const DWORD base = AUDCLNT_STREAMFLAGS_LOOPBACK | (evented ? AUDCLNT_STREAMFLAGS_EVENTCALLBACK : 0);
    const DWORD convert = AUDCLNT_STREAMFLAGS_AUTOCONVERTPCM | AUDCLNT_STREAMFLAGS_SRC_DEFAULT_QUALITY;
    struct Attempt {
      const char *name;
      const WAVEFORMATEX *fmt;
      DWORD flags;
      bool is_float;
    };
    const Attempt process_attempts[] = {{"float32", &fmt_float, 0, true},
                                        {"pcm16", &fmt_pcm, 0, false},
                                        {"float32+convert", &fmt_float, convert, true},
                                        {"pcm16+convert", &fmt_pcm, convert, false}};
    const Attempt endpoint_attempts[] = {{"float32+convert", &fmt_float, convert, true},
                                         {"pcm16+convert", &fmt_pcm, convert, false},
                                         {"float32", &fmt_float, 0, true},
                                         {"pcm16", &fmt_pcm, 0, false}};
    const Attempt *list = evented ? process_attempts : endpoint_attempts;

    HRESULT hr = E_FAIL;
    for (int i = 0; i < 4; ++i) {
      // A client whose Initialize failed is not reused: activate a fresh one.
      ReleaseCom();
      hr = Activate(stop_event);
      if (SUCCEEDED(hr) && !client_) hr = E_NOINTERFACE;
      if (FAILED(hr)) {
        err = std::string(evented ? "process loopback activation " : "endpoint activation ") + HrStr(hr);
        if (!attempts.empty()) err += " (after: " + attempts + ")";
        return false;
      }
      hr = client_->Initialize(AUDCLNT_SHAREMODE_SHARED, base | list[i].flags, evented ? kProcessBuffer : kEndpointBuffer,
                               0, list[i].fmt, nullptr);
      if (SUCCEEDED(hr)) {
        is_float_ = list[i].is_float;
        format = list[i].name;
        break;
      }
      if (!attempts.empty()) attempts += ", ";
      attempts += std::string(list[i].name) + ": " + HrStr(hr);
    }
    if (FAILED(hr)) {
      err = "IAudioClient::Initialize failed for every format (" + attempts + ")";
      return false;
    }
    client_->GetBufferSize(&buffer_frames);
    if (evented) {
      event_ = CreateEventW(nullptr, FALSE, FALSE, nullptr);
      hr = client_->SetEventHandle(event_);
      if (FAILED(hr)) {
        err = "SetEventHandle " + HrStr(hr);
        return false;
      }
    }
    hr = client_->GetService(kIID_IAudioCaptureClient, reinterpret_cast<void **>(&capture_));
    if (FAILED(hr)) {
      err = "GetService(IAudioCaptureClient) " + HrStr(hr);
      return false;
    }
    hr = client_->Start();
    if (FAILED(hr)) {
      err = "IAudioClient::Start " + HrStr(hr);
      return false;
    }
    thread_ = std::thread([this] { Run(); });
    return true;
  }

  void Run() {
    ComInit com;
    MmcssScope mmcss;
    Capture();
    ReleaseCom();  // while this thread's MTA membership is still alive
  }

  void Capture() {
    std::vector<float> conv;
    while (!stop_) {
      if (event_) {
        WaitForSingleObject(event_, 20);
      } else {
        Sleep(5);
      }
      UINT32 packet = 0;
      HRESULT hr = capture_->GetNextPacketSize(&packet);
      while (SUCCEEDED(hr) && packet > 0 && !stop_) {
        BYTE *data = nullptr;
        UINT32 frames = 0;
        DWORD flags = 0;
        hr = capture_->GetBuffer(&data, &frames, &flags, nullptr, nullptr);
        if (FAILED(hr) || hr == AUDCLNT_S_BUFFER_EMPTY) break;
        if (flags & AUDCLNT_BUFFERFLAGS_DATA_DISCONTINUITY) ++discontinuities;
        if ((flags & AUDCLNT_BUFFERFLAGS_SILENT) || !data) {
          PushSilence(frames);
          ++silent_packets;
        } else if (is_float_) {
          Push(reinterpret_cast<const float *>(data), frames);
        } else {
          conv.resize(static_cast<size_t>(frames) * 2);
          const int16_t *s = reinterpret_cast<const int16_t *>(data);
          for (size_t i = 0; i < conv.size(); ++i) conv[i] = static_cast<float>(s[i]) / 32768.0f;
          Push(conv.data(), frames);
        }
        capture_->ReleaseBuffer(frames);
        ++packets;
        hr = capture_->GetNextPacketSize(&packet);
      }
      if (FAILED(hr)) {
        std::lock_guard<std::mutex> lk(mu_);
        fail_reason_ = "capture " + HrStr(hr) + (hr == AUDCLNT_E_DEVICE_INVALIDATED ? " (device invalidated)" : "");
        failed_ = true;
        return;
      }
    }
  }

  void Push(const float *src, size_t frames) {
    std::lock_guard<std::mutex> lk(mu_);
    for (size_t i = 0; i < frames; ++i) {
      if (size_ == kRingFrames) {  // full: drop oldest
        head_ = (head_ + 1) % kRingFrames;
        --size_;
        ++overflows;
      }
      size_t at = ((head_ + size_) % kRingFrames) * 2;
      ring_[at] = src[2 * i];
      ring_[at + 1] = src[2 * i + 1];
      ++size_;
    }
  }

  void PushSilence(size_t frames) {
    static const float zeros[2 * kChunk] = {};
    while (frames > 0) {
      size_t n = std::min(frames, static_cast<size_t>(kChunk));
      Push(zeros, n);
      frames -= n;
    }
  }

  IAudioClient *client_ = nullptr;
  IAudioCaptureClient *capture_ = nullptr;
  HANDLE event_ = nullptr;
  bool is_float_ = true;
  std::thread thread_;
  std::atomic<bool> stop_{false};
  std::atomic<bool> failed_{false};

  std::mutex mu_;
  std::vector<float> ring_;
  size_t head_ = 0;  // frame index
  size_t size_ = 0;  // frames
  std::string fail_reason_;
};

// ---------------------------------------------------------------- snapshot

struct SessionEntry {
  DWORD pid = 0;
  ULONGLONG created = 0;
  std::string exe;
  int state = 0;
  bool multi_process = false;
  Verdict verdict;
  std::string note;  // e.g. "captured via ancestor pid N", "NOT captured: …"
};

// Microphone users seen recently, keyed by app root (pid + creation time), so
// a voice app stays excluded while its mic session briefly disappears.
struct MicMemory {
  struct Entry {
    ULONGLONG created;
    int64_t last_seen_ns;
  };
  std::unordered_map<DWORD, Entry> roots;
};

struct Snapshot {
  std::vector<SessionEntry> sessions;
  std::vector<std::pair<DWORD, DWORD>> mic_users;  // pid, app root (this round)
  std::unordered_set<DWORD> mic_roots;             // effective (incl. sticky)
  bool complete = false;                           // output sessions fully enumerated
  bool mic_complete = false;                       // input sessions fully enumerated
  std::vector<std::string> warnings;
};

Snapshot TakeSnapshot(IMMDeviceEnumerator *en, ProcessTable &pt, const Rules &rules, bool exclude_mic_users,
                      MicMemory *memory) {
  Snapshot snap;
  const DWORD self = GetCurrentProcessId();
  std::vector<SessionInfo> render, capture;
  std::string err;
  // Enumerate first, then snapshot processes, so every session's process is known.
  snap.complete = EnumSessions(en, eRender, render, err);
  if (!snap.complete) snap.warnings.push_back(err);
  err.clear();
  snap.mic_complete = EnumSessions(en, eCapture, capture, err);
  if (!snap.mic_complete && exclude_mic_users) snap.warnings.push_back("microphone sessions: " + err);
  pt.Refresh();

  const int64_t now = QpcNs();
  for (const auto &c : capture) {
    if (!pt.Get(c.pid) || pt.IsSelfOrDescendant(c.pid, self)) continue;
    DWORD root = AppRoot(pt, c.pid);
    snap.mic_users.emplace_back(c.pid, root);
    snap.mic_roots.insert(root);
    if (memory) memory->roots[root] = {pt.Get(root) ? pt.Get(root)->created : 0, now};
  }
  if (memory) {
    for (auto it = memory->roots.begin(); it != memory->roots.end();) {
      bool alive = pt.Alive(it->first, it->second.created);
      bool recent = now - it->second.last_seen_ns < kMicStickyNs || !snap.mic_complete;
      if (!alive || !recent) {
        it = memory->roots.erase(it);
      } else {
        snap.mic_roots.insert(it->first);
        ++it;
      }
    }
  }

  for (const auto &r : render) {
    const ProcInfo *p = pt.Get(r.pid);
    if (!p) continue;  // exited between enumeration and snapshot
    SessionEntry e;
    e.pid = r.pid;
    e.state = r.state;
    e.multi_process = r.multi_process;
    e.exe = Utf8(p->exe);
    e.created = p->created;
    e.verdict = Classify(r.pid, pt, rules, snap.mic_roots, exclude_mic_users, self);
    if (r.multi_process)
      snap.warnings.push_back("multi-process audio session from " + e.exe + ": its other processes can't be vetted");
    snap.sessions.push_back(std::move(e));
  }
  return snap;
}

// Chooses the processes to capture in include-set mode and annotates the rest.
//  * A root is an allowed session whose OS tree contains no excluded process
//    (checked against every process, not just those currently playing), and
//    which is not a launcher/shell (Explorer's tree is everything).
//  * A root is skipped if an OS ancestor is also captured (this round's roots,
//    or a still-alive stream), because that stream already includes it.
std::vector<const SessionEntry *> PlanIncludeSet(Snapshot &snap, const ProcessTable &pt, const Rules &rules,
                                                 bool exclude_mic_users, const std::unordered_set<DWORD> &live_roots) {
  const DWORD self = GetCurrentProcessId();
  std::vector<std::pair<DWORD, std::string>> excluded;  // every excluded process, playing or not
  for (const auto &kv : pt.All()) {
    Verdict v = Classify(kv.first, pt, rules, snap.mic_roots, exclude_mic_users, self);
    if (v.excluded) excluded.emplace_back(kv.first, Utf8(kv.second.exe));
  }
  std::unordered_set<DWORD> roots;
  for (auto &e : snap.sessions) {
    if (e.verdict.excluded) continue;
    const ProcInfo *p = pt.Get(e.pid);
    if (p && IsLauncher(p->exe)) {
      e.note = "NOT captured: launcher/shell process (its own UI sounds only)";
      continue;
    }
    bool dirty = false;
    for (const auto &x : excluded) {
      if (x.first != e.pid && pt.IsSelfOrDescendant(x.first, e.pid)) {
        e.note = "NOT captured: its process tree contains excluded " + x.second;
        dirty = true;
        break;
      }
    }
    if (!dirty) roots.insert(e.pid);
  }
  std::vector<const SessionEntry *> plan;
  for (auto &e : snap.sessions) {
    if (!roots.count(e.pid)) continue;
    const ProcInfo *p = pt.Get(e.pid);
    bool covered = false;
    int depth = 0;
    for (const ProcInfo *a = p ? pt.Parent(*p) : nullptr; a && depth < 64; a = pt.Parent(*a), ++depth) {
      if (roots.count(a->pid) || live_roots.count(a->pid)) {
        e.note = "captured via ancestor pid " + std::to_string(a->pid);
        covered = true;
        break;
      }
    }
    if (!covered) plan.push_back(&e);
  }
  return plan;
}

// ---------------------------------------------------------------- engine

struct Channel {
  std::unique_ptr<LoopbackStream> stream;
  DWORD pid = 0;
  ULONGLONG created = 0;
  std::string label;
  enum State { Preroll, Running, Fading };
  std::atomic<State> state{Preroll};  // written by the mixer, read by status
  float gain = 0.0f;                  // mixer thread only
  int missing_polls = 0;              // poller thread only
  std::atomic<float> peak{0.0f};
  std::atomic<float> max_peak{0.0f};     // since the stream opened (proves an app was actually heard)
  uint64_t seen_discontinuities = 0;     // mixer thread only
  // Onset fade (mixer thread only). When a loopback stream attaches to an app
  // that is just starting to play, Windows drops the app's first milliseconds
  // (measured: a 20 ms fade-in never arrived), so the sound begins at full
  // level. Audio that starts after >= 50 ms of digital silence is ramped in.
  size_t silent_frames = kOnsetSilenceFrames;
  size_t onset_pos = kOnsetRampFrames;  // >= kOnsetRampFrames: no ramp in progress
  std::atomic<uint64_t> underruns{0}, trims{0};
  std::atomic<bool> want_removal{false};
};

const char *StateName(Channel::State s) {
  switch (s) {
    case Channel::Preroll: return "preroll";
    case Channel::Running: return "running";
    case Channel::Fading: return "fading";
  }
  return "?";
}

struct Chunk {
  std::vector<float> samples;  // kChunk * 2
  int64_t capture_ns = 0;
};

class Engine {
 public:
  Engine(int mode, uint32_t flags)
      : mode_(mode), flags_(flags), stop_event_(CreateEventW(nullptr, TRUE, FALSE, nullptr)) {}

  // Normally Stop() has joined everything. If the process exits with the engine
  // running, the threads are already gone: detach instead of std::terminate.
  ~Engine() {
    if (poller_.joinable()) poller_.detach();
    if (mixer_.joinable()) mixer_.detach();
    if (stop_event_) CloseHandle(stop_event_);
  }

  // Timestamped engine events (QPC ns, same clock as chunk capture_ns), so a
  // host can line up clicks in the output with what the engine did.
  void Event(const std::string &label, const char *what) {
    std::lock_guard<std::mutex> lk(events_mu_);
    events_.push_back({QpcNs(), label, what});
    if (events_.size() > 300) events_.pop_front();
  }

  bool Start(std::string &err) {
    if (mode_ != IM_AUDIO_MODE_ENDPOINT && !ActivateFn()) {
      err = "ActivateAudioInterfaceAsync not available (Windows too old)";
      return false;
    }
    stop_ = false;
    mixer_ = std::thread([this] { MixerLoop(); });
    poller_ = std::thread([this] { PollerLoop(); });
    return true;
  }

  void Stop() {
    {
      std::lock_guard<std::mutex> lk(stop_mu_);
      std::lock_guard<std::mutex> lk2(out_mu_);  // so a Read() between its check and its wait can't miss this
      stop_ = true;
    }
    SetEvent(stop_event_);  // aborts pending activations
    stop_cv_.notify_all();
    out_cv_.notify_all();
    if (poller_.joinable()) poller_.join();
    if (mixer_.joinable()) mixer_.join();
    std::vector<std::shared_ptr<Channel>> dead;
    {
      std::lock_guard<std::mutex> lk(ch_mu_);
      dead.swap(channels_);
      dead.insert(dead.end(), graveyard_.begin(), graveyard_.end());
      graveyard_.clear();
    }
    ComInit com;
    dead.clear();  // joins capture threads (they release their own COM objects)
  }

  int32_t Read(float *out, int32_t max_frames, int64_t *capture_ns, int32_t timeout_ms) {
    if (max_frames < kChunk) return IM_ERR_INVALID_ARG;
    std::unique_lock<std::mutex> lk(out_mu_);
    bool ok = out_cv_.wait_for(lk, std::chrono::milliseconds(std::max(0, timeout_ms)),
                               [&] { return !out_.empty() || stop_.load(); });
    if (!ok) return IM_ERR_TIMEOUT;
    if (out_.empty()) return IM_ERR_NOT_STARTED;
    Chunk c = std::move(out_.front());
    out_.pop_front();
    lk.unlock();
    std::memcpy(out, c.samples.data(), sizeof(float) * kChunk * 2);
    if (capture_ns) *capture_ns = c.capture_ns;
    return kChunk;
  }

  std::string Status() {
    std::lock_guard<std::mutex> lk(status_mu_);
    return status_;
  }

 private:
  struct Want {
    StreamKind kind;
    DWORD pid;
    ULONGLONG created;
    std::string label;
  };

  // ---- poller: sessions → desired streams → reconcile; build status
  void PollerLoop() {
    ComInit com;
    ComPtr<IMMDeviceEnumerator> en;
    HRESULT hr = CoCreateInstance(kCLSID_MMDeviceEnumerator, nullptr, CLSCTX_ALL, kIID_IMMDeviceEnumerator, en.PutVoid());
    ProcessTable pt;
    MicMemory mic_memory;
    const bool mic = (flags_ & IM_FLAG_EXCLUDE_MIC_USERS) != 0;
    bool first = true;
    while (first || !WaitStop(kPollMs)) {
      first = false;
      Rules rules = CopyRules();
      Snapshot snap;
      if (en) snap = TakeSnapshot(en.Get(), pt, rules, mic, &mic_memory);
      if (FAILED(hr)) snap.warnings.push_back("MMDeviceEnumerator " + HrStr(hr));
      std::unordered_set<DWORD> present;
      for (const auto &e : snap.sessions) present.insert(e.pid);

      if (mode_ == IM_AUDIO_MODE_ENDPOINT) {
        Reconcile({{StreamKind::Endpoint, 0, 0, "default output (everything, no exclusion)"}}, present, pt,
                  snap.warnings);
      } else if (!snap.complete || (mic && !snap.mic_complete)) {
        snap.warnings.push_back("session list incomplete this round; keeping current streams");
      } else if (mode_ == IM_AUDIO_MODE_INCLUDE_SET) {
        auto plan = PlanIncludeSet(snap, pt, rules, mic, LiveIncludeRoots(pt));
        std::vector<Want> want;
        for (const SessionEntry *e : plan) want.push_back({StreamKind::Include, e->pid, e->created, e->exe});
        Reconcile(want, present, pt, snap.warnings);
      } else if (mode_ == IM_AUDIO_MODE_EXCLUDE_ONE) {
        Reconcile({ChooseExcludeTarget(snap, pt)}, present, pt, snap.warnings);
      }
      ReapGraveyard();
      PublishStatus(snap);
    }
  }

  // Include streams that are alive and whose process still exists: they cover
  // their descendants, so the plan doesn't open duplicates during handovers.
  std::unordered_set<DWORD> LiveIncludeRoots(const ProcessTable &pt) {
    std::unordered_set<DWORD> live;
    std::lock_guard<std::mutex> lk(ch_mu_);
    for (auto &ch : channels_)
      if (ch->stream->kind == StreamKind::Include && !ch->want_removal && pt.Alive(ch->pid, ch->created))
        live.insert(ch->pid);
    return live;
  }

  // exclude-one mode: one EXCLUDE stream can drop only one app tree. Keep the
  // current target while it's still excluded; otherwise pick deterministically
  // (playing first, then rule order, then oldest).
  Want ChooseExcludeTarget(Snapshot &snap, const ProcessTable &pt) {
    struct Cand {
      DWORD root;
      bool active;
      int rule;
      ULONGLONG created;
    };
    std::vector<Cand> cands;
    for (const auto &e : snap.sessions) {
      if (!e.verdict.excluded || e.verdict.reason == "isshoni itself") continue;
      DWORD root = AppRoot(pt, e.pid);
      const ProcInfo *rp = pt.Get(root);
      cands.push_back({root, e.state == AudioSessionStateActive, e.verdict.rule_index < 0 ? 1 << 20 : e.verdict.rule_index,
                       rp ? rp->created : 0});
    }
    std::unordered_set<DWORD> distinct;
    for (const auto &c : cands) distinct.insert(c.root);
    bool keep = false;
    for (const auto &c : cands) keep = keep || (c.root == exclude_target_ && pt.Alive(c.root, exclude_created_));
    if (keep) {
      exclude_missing_ = 0;
    } else if (exclude_target_ != 0 && pt.Alive(exclude_target_, exclude_created_) &&
               ++exclude_missing_ < kMissingPollsBeforeRemoval) {
      keep = true;
    } else if (!cands.empty()) {
      auto best = std::min_element(cands.begin(), cands.end(), [](const Cand &a, const Cand &b) {
        if (a.active != b.active) return a.active;
        if (a.rule != b.rule) return a.rule < b.rule;
        return a.created < b.created;
      });
      exclude_target_ = best->root;
      exclude_created_ = best->created;
      exclude_missing_ = 0;
    } else {
      exclude_target_ = GetCurrentProcessId();
      const ProcInfo *sp = pt.Get(exclude_target_);
      exclude_created_ = sp ? sp->created : 0;
    }
    std::string label = exclude_target_ == GetCurrentProcessId()
                            ? "everything except isshoni"
                            : "everything except the tree of " + pt.ExeUtf8(exclude_target_);
    if (distinct.size() > 1)
      snap.warnings.push_back("exclude-one mode can drop only 1 of " + std::to_string(distinct.size()) +
                              " excluded apps; the others are audible");
    if (exclude_target_ != GetCurrentProcessId())
      snap.warnings.push_back("exclude-one mode does not exclude isshoni's own playback");
    return {StreamKind::Exclude, exclude_target_, exclude_created_, label};
  }

  // Streams no longer wanted are removed at once if their app is still playing
  // (e.g. it just got excluded) or their process is gone; a session that merely
  // vanished (enumeration gap) keeps its stream for kMissingPollsBeforeRemoval.
  void Reconcile(const std::vector<Want> &want, const std::unordered_set<DWORD> &present, const ProcessTable &pt,
                 std::vector<std::string> &warnings) {
    std::vector<Want> to_open;
    {
      std::lock_guard<std::mutex> lk(ch_mu_);
      for (auto &ch : channels_) {
        bool keep = false;
        for (const auto &w : want)
          keep = keep || (ch->pid == w.pid && ch->created == w.created && ch->stream->kind == w.kind);
        const bool include = ch->stream->kind == StreamKind::Include;
        if (keep) {
          ch->missing_polls = 0;
        } else if (!include || present.count(ch->pid) || !pt.Alive(ch->pid, ch->created) ||
                   ++ch->missing_polls >= kMissingPollsBeforeRemoval) {
          ch->want_removal = true;
        }
        if (ch->stream->Failed()) ch->want_removal = true;
      }
      for (const auto &w : want) {
        bool have = false;
        for (auto &ch : channels_)
          have = have || (ch->pid == w.pid && ch->created == w.created && ch->stream->kind == w.kind &&
                          !ch->want_removal && !ch->stream->Failed());
        if (!have) to_open.push_back(w);
      }
    }
    for (const auto &w : to_open) {  // activation takes a few ms each: outside the lock
      if (stop_) break;
      std::string err;
      auto s = LoopbackStream::Open(w.kind, w.pid, stop_event_, err);
      if (!s) {
        warnings.push_back("cannot capture " + w.label + " (pid " + std::to_string(w.pid) + "): " + err);
        continue;
      }
      if (!s->attempts.empty()) warnings.push_back(w.label + ": format " + s->format + " after " + s->attempts);
      auto ch = std::make_shared<Channel>();
      ch->stream = std::move(s);
      ch->pid = w.pid;
      ch->created = w.created;
      ch->label = w.label;
      Event(w.label, "opened");
      std::lock_guard<std::mutex> lk(ch_mu_);
      channels_.push_back(std::move(ch));
    }
  }

  void ReapGraveyard() {
    std::vector<std::shared_ptr<Channel>> dead;
    {
      std::lock_guard<std::mutex> lk(ch_mu_);
      dead.swap(graveyard_);
    }
    for (auto &ch : dead) {  // keep lifetime totals for the report
      reaped_discontinuities_ += ch->stream->discontinuities.load();
      reaped_overflows_ += ch->stream->overflows.load();
      reaped_underruns_ += ch->underruns.load();
      reaped_trims_ += ch->trims.load();
    }
    dead.clear();  // joins capture threads outside the lock
  }

  // ---- mixer: every 10 ms pull one chunk from each stream, fade, sum, queue
  void MixerLoop() {
    ComInit com;
    MmcssScope mmcss;
    HANDLE timer = CreateWaitableTimerExW(nullptr, nullptr, CREATE_WAITABLE_TIMER_HIGH_RESOLUTION, TIMER_ALL_ACCESS);
    bool coarse = false;
    if (!timer) {  // before Windows 10 1803
      timer = CreateWaitableTimerW(nullptr, FALSE, nullptr);
      timeBeginPeriod(1);
      coarse = true;
    }
    std::vector<float> mix(kChunk * 2), tmp(kChunk * 2);
    int64_t next = QpcNs();
    while (!stop_) {
      next += 10'000'000;
      int64_t now = QpcNs();
      if (next > now) {
        LARGE_INTEGER due;
        due.QuadPart = -static_cast<LONGLONG>((next - now) / 100);  // relative, 100 ns units
        SetWaitableTimer(timer, &due, 0, nullptr, nullptr, FALSE);
        WaitForSingleObject(timer, 50);
      } else if (now - next > 100'000'000) {
        next = now;  // fell >100 ms behind (e.g. system suspend): resync instead of bursting
        ++mixer_resyncs_;
        Event("mixer", "fell >100 ms behind: resync");
      }

      std::fill(mix.begin(), mix.end(), 0.0f);
      std::vector<std::shared_ptr<Channel>> chans;
      {
        std::lock_guard<std::mutex> lk(ch_mu_);
        chans = channels_;
      }
      for (auto &ch : chans) MixChannel(*ch, mix, tmp);
      chans.clear();  // never hold the last reference here: destruction belongs to the poller
      {
        std::lock_guard<std::mutex> lk(ch_mu_);
        for (auto it = channels_.begin(); it != channels_.end();) {
          Channel &c = **it;
          if (c.state.load() == Channel::Fading && c.gain <= 0.0f) {
            Event(c.label, "removed");
            graveyard_.push_back(*it);
            it = channels_.erase(it);
          } else {
            ++it;
          }
        }
      }
      for (float &v : mix) v = std::clamp(v, -1.0f, 1.0f);

      Chunk c;
      c.samples = mix;
      c.capture_ns = next;
      {
        std::lock_guard<std::mutex> lk(out_mu_);
        out_.push_back(std::move(c));
        while (out_.size() > kMaxQueuedChunks) {  // host not reading: drop oldest
          out_.pop_front();
          ++dropped_chunks_;
        }
      }
      out_cv_.notify_one();
    }
    if (coarse) timeEndPeriod(1);
    CloseHandle(timer);
  }

  // Never changes a channel's contribution abruptly: every stop is a fade over
  // a full chunk that is still buffered (packets arrive in whole 10 ms blocks,
  // so waiting until the ring is empty would leave nothing to fade), and a
  // chunk that ends early anyway gets a short tail ramp.
  void MixChannel(Channel &ch, std::vector<float> &mix, std::vector<float> &tmp) {
    LoopbackStream &s = *ch.stream;
    if ((ch.want_removal || s.Failed()) && ch.state != Channel::Fading) {
      ch.state = Channel::Fading;
      Event(ch.label, s.Failed() ? "failed: fading out" : "fading out");
    }
    if (const uint64_t d = s.discontinuities.load(); d != ch.seen_discontinuities) {
      ch.seen_discontinuities = d;
      Event(ch.label, "capture discontinuity (Windows dropped data)");
    }
    const size_t buffered = s.Buffered();
    if (ch.state == Channel::Preroll) {
      if (buffered < kPrerollFrames) return;
      ch.state = Channel::Running;  // gain is 0: fades in over this chunk
      Event(ch.label, "running (fade in)");
    }
    bool stop_after = false;  // fade out this chunk, then back to preroll
    bool trim_after = false;
    if (ch.state == Channel::Running) {
      if (buffered < kLowWaterFrames) {
        stop_after = true;  // about to run dry
        ++ch.underruns;
        Event(ch.label, "running low: fade out, re-preroll");
      } else if (buffered > kMaxBufferedFrames) {
        stop_after = trim_after = true;  // too far behind: fade, drop the excess, fade back in
        ++ch.trims;
        Event(ch.label, "too much buffered: fade out, trim, re-preroll");
      }
    }
    const size_t got = s.Pop(tmp.data(), kChunk);
    const float target = (ch.state == Channel::Fading || stop_after) ? 0.0f : 1.0f;
    const float step = 1.0f / kChunk;
    float peak = 0.0f, last_l = 0.0f, last_r = 0.0f;
    for (size_t i = 0; i < got; ++i) {
      ch.gain += std::clamp(target - ch.gain, -step, step);
      const float in_l = tmp[2 * i], in_r = tmp[2 * i + 1];
      if (std::fabs(in_l) < 1e-6f && std::fabs(in_r) < 1e-6f) {
        if (ch.silent_frames < kOnsetSilenceFrames) ++ch.silent_frames;
      } else {
        if (ch.silent_frames >= kOnsetSilenceFrames) ch.onset_pos = 0;  // sound starts after silence
        ch.silent_frames = 0;
      }
      float onset = 1.0f;
      if (ch.onset_pos < kOnsetRampFrames) onset = static_cast<float>(++ch.onset_pos) / static_cast<float>(kOnsetRampFrames);
      last_l = in_l * ch.gain * onset;
      last_r = in_r * ch.gain * onset;
      mix[2 * i] += last_l;
      mix[2 * i + 1] += last_r;
      peak = std::max({peak, std::fabs(last_l), std::fabs(last_r)});
    }
    if (got < static_cast<size_t>(kChunk) && ch.gain > 0.0f) {
      // Backstop: the chunk ended early mid-wave. Ramp the last output value to zero.
      const size_t tail = std::min(kTailRampFrames, static_cast<size_t>(kChunk) - got);
      for (size_t j = 0; j < tail; ++j) {
        const float f = 1.0f - static_cast<float>(j + 1) / static_cast<float>(tail);
        mix[2 * (got + j)] += last_l * f;
        mix[2 * (got + j) + 1] += last_r * f;
      }
      ch.gain = 0.0f;
      if (ch.state == Channel::Running) {
        ch.state = Channel::Preroll;
        ++ch.underruns;
        Event(ch.label, "ran dry mid-chunk: tail ramp");
      }
    }
    if (stop_after) {
      ch.gain = 0.0f;
      ch.state = Channel::Preroll;
      if (trim_after) s.Trim(kTrimToFrames);
    }
    ch.peak = peak;
    if (peak > ch.max_peak.load()) ch.max_peak = peak;
  }

  // ---- status JSON ("what friends hear")
  void PublishStatus(const Snapshot &snap) {
    uint64_t disc = reaped_discontinuities_, over = reaped_overflows_, under = reaped_underruns_, trims = reaped_trims_;
    std::string j = "{\"mode\":";
    JsonStr(j, mode_ == IM_AUDIO_MODE_INCLUDE_SET ? "include-set"
               : mode_ == IM_AUDIO_MODE_EXCLUDE_ONE ? "exclude-one"
                                                    : "endpoint");
    j += ",\"streams\":[";
    {
      std::lock_guard<std::mutex> lk(ch_mu_);
      bool first = true;
      for (auto &ch : channels_) {
        if (!first) j += ',';
        first = false;
        LoopbackStream &s = *ch->stream;
        disc += s.discontinuities.load();
        over += s.overflows.load();
        under += ch->underruns.load();
        trims += ch->trims.load();
        j += "{\"label\":";
        JsonStr(j, ch->label);
        j += ",\"kind\":";
        JsonStr(j, KindName(s.kind));
        j += ",\"state\":";
        JsonStr(j, StateName(ch->state.load()));
        j += ",\"format\":";
        JsonStr(j, s.format);
        j += ",\"pid\":" + std::to_string(ch->pid) + ",\"buffered_ms\":" + std::to_string(s.Buffered() * 1000 / kRate) +
             ",\"buffer_ms\":" + std::to_string(s.buffer_frames * 1000 / kRate) + ",\"peak\":" + JsonNum(ch->peak.load()) +
             ",\"max_peak\":" + JsonNum(ch->max_peak.load()) +
             ",\"packets\":" + std::to_string(s.packets.load()) +
             ",\"silent_packets\":" + std::to_string(s.silent_packets.load()) +
             ",\"discontinuities\":" + std::to_string(s.discontinuities.load()) +
             ",\"underruns\":" + std::to_string(ch->underruns.load()) + ",\"trims\":" + std::to_string(ch->trims.load()) +
             ",\"overflows\":" + std::to_string(s.overflows.load()) + ",\"error\":";
        JsonStr(j, s.Failed() ? s.FailReason() : "");
        j += '}';
      }
    }
    j += "],\"totals\":{\"discontinuities\":" + std::to_string(disc) + ",\"overflows\":" + std::to_string(over) +
         ",\"underruns\":" + std::to_string(under) + ",\"trims\":" + std::to_string(trims) +
         ",\"dropped_chunks\":" + std::to_string(dropped_chunks_.load()) +
         ",\"mixer_resyncs\":" + std::to_string(mixer_resyncs_.load()) + "},\"sessions\":";
    AppendSessions(j, snap);
    j += ",\"events\":[";
    {
      std::lock_guard<std::mutex> lk(events_mu_);
      for (size_t i = 0; i < events_.size(); ++i) {
        if (i) j += ',';
        j += "{\"t_ns\":" + std::to_string(events_[i].t_ns) + ",\"label\":";
        JsonStr(j, events_[i].label);
        j += ",\"event\":";
        JsonStr(j, events_[i].what);
        j += '}';
      }
    }
    j += "],\"warnings\":[";
    for (size_t i = 0; i < snap.warnings.size(); ++i) {
      if (i) j += ',';
      JsonStr(j, snap.warnings[i]);
    }
    j += "]}";
    std::lock_guard<std::mutex> lk(status_mu_);
    status_ = std::move(j);
  }

 public:
  static void AppendSessions(std::string &j, const Snapshot &snap) {
    j += '[';
    for (size_t i = 0; i < snap.sessions.size(); ++i) {
      const auto &e = snap.sessions[i];
      if (i) j += ',';
      j += "{\"pid\":" + std::to_string(e.pid) + ",\"exe\":";
      JsonStr(j, e.exe);
      j += ",\"state\":";
      JsonStr(j, e.state == AudioSessionStateActive ? "active" : "inactive");
      j += std::string(",\"multi_process\":") + (e.multi_process ? "true" : "false");
      j += std::string(",\"excluded\":") + (e.verdict.excluded ? "true" : "false") + ",\"reason\":";
      JsonStr(j, e.verdict.reason);
      j += ",\"note\":";
      JsonStr(j, e.note);
      j += '}';
    }
    j += "],\"mic_users\":[";
    for (size_t i = 0; i < snap.mic_users.size(); ++i) {
      if (i) j += ',';
      j += "{\"pid\":" + std::to_string(snap.mic_users[i].first) +
           ",\"app_root_pid\":" + std::to_string(snap.mic_users[i].second) + '}';
    }
    j += ']';
  }

 private:
  bool WaitStop(int ms) {
    std::unique_lock<std::mutex> lk(stop_mu_);
    return stop_cv_.wait_for(lk, std::chrono::milliseconds(ms), [&] { return stop_.load(); });
  }

  const int mode_;
  const uint32_t flags_;
  HANDLE stop_event_;
  std::atomic<bool> stop_{false};
  std::mutex stop_mu_;
  std::condition_variable stop_cv_;
  std::thread poller_, mixer_;

  // exclude-one target (poller thread only)
  DWORD exclude_target_ = 0;
  ULONGLONG exclude_created_ = 0;
  int exclude_missing_ = 0;

  std::mutex ch_mu_;
  std::vector<std::shared_ptr<Channel>> channels_;
  std::vector<std::shared_ptr<Channel>> graveyard_;
  uint64_t reaped_discontinuities_ = 0, reaped_overflows_ = 0, reaped_underruns_ = 0, reaped_trims_ = 0;  // poller

  std::mutex out_mu_;
  std::condition_variable out_cv_;
  std::deque<Chunk> out_;
  std::atomic<uint64_t> dropped_chunks_{0}, mixer_resyncs_{0};

  std::mutex status_mu_;
  std::string status_ = "{}";

  struct EventRec {
    int64_t t_ns;
    std::string label;
    std::string what;
  };
  std::mutex events_mu_;
  std::deque<EventRec> events_;
};

std::mutex g_engine_mu;
// Heap-allocated and never destroyed: no static destructor runs at process exit
// while engine threads are already dead. Callers copy the shared_ptr, so
// im_audio_stop can't free the engine under a concurrent im_audio_read.
std::shared_ptr<Engine> &GEngine() {
  static auto *e = new std::shared_ptr<Engine>();
  return *e;
}

inline const PROPERTYKEY kPKEY_Device_FriendlyName = {
    {0xa45c254e, 0xdf1c, 0x4efd, {0x80, 0x20, 0x67, 0xd1, 0x46, 0xa8, 0x50, 0xe0}}, 14};

// Name of the default output device (e.g. to spot virtual/remote-desktop audio drivers).
std::string DefaultDeviceName() {
  ComPtr<IMMDeviceEnumerator> en;
  ComPtr<IMMDevice> dev;
  ComPtr<IPropertyStore> props;
  std::string name;
  if (SUCCEEDED(CoCreateInstance(kCLSID_MMDeviceEnumerator, nullptr, CLSCTX_ALL, kIID_IMMDeviceEnumerator, en.PutVoid())) &&
      SUCCEEDED(en->GetDefaultAudioEndpoint(eRender, eConsole, dev.Put())) &&
      SUCCEEDED(dev->OpenPropertyStore(STGM_READ, props.Put()))) {
    PROPVARIANT v;
    PropVariantInit(&v);
    if (SUCCEEDED(props->GetValue(kPKEY_Device_FriendlyName, &v)) && v.vt == VT_LPWSTR && v.pwszVal) name = Utf8(v.pwszVal);
    PropVariantClear(&v);
  }
  return name;
}

// Master volume/mute of the default output, for diagnosing quiet recordings.
std::string MasterVolumeJson() {
  ComPtr<IMMDeviceEnumerator> en;
  ComPtr<IMMDevice> dev;
  ComPtr<IAudioEndpointVolume> vol;
  float level = -1;
  BOOL muted = FALSE;
  if (SUCCEEDED(CoCreateInstance(kCLSID_MMDeviceEnumerator, nullptr, CLSCTX_ALL, kIID_IMMDeviceEnumerator, en.PutVoid())) &&
      SUCCEEDED(en->GetDefaultAudioEndpoint(eRender, eConsole, dev.Put())) &&
      SUCCEEDED(dev->Activate(kIID_IAudioEndpointVolume, CLSCTX_ALL, nullptr, vol.PutVoid()))) {
    vol->GetMasterVolumeLevelScalar(&level);
    vol->GetMute(&muted);
  }
  return ",\"master_volume\":" + JsonNum(level) + ",\"master_muted\":" + (muted ? "true" : "false");
}

}  // namespace
}  // namespace im

// ---------------------------------------------------------------- C ABI

using namespace im;

extern "C" {

// Every export clears the thread's last error first, so a later failure never
// reports a stale message.

IM_API int32_t im_version(void) { return IM_ABI_VERSION; }

IM_API int32_t im_audio_probe(char *json, int32_t cap) {
  g_last_error.clear();
  ComInit com;
  std::string err, format;
  bool ok = false;
  if (!ActivateFn()) {
    err = "ActivateAudioInterfaceAsync missing";
  } else {
    // Activating + initializing a loopback of our own process tree proves the OS supports it.
    auto s = LoopbackStream::Open(StreamKind::Include, GetCurrentProcessId(), nullptr, err);
    ok = s != nullptr;
    if (s) {
      format = s->format;
      err = s->attempts;  // formats that were rejected before one worked
    }
  }
  std::string j = "{\"os_build\":" + std::to_string(OsBuild()) + ",\"process_loopback\":" + (ok ? "true" : "false") +
                  ",\"format\":";
  JsonStr(j, format);
  j += MasterVolumeJson() + ",\"device\":";
  JsonStr(j, DefaultDeviceName());
  j += ",\"error\":";
  JsonStr(j, err);
  j += '}';
  int32_t n = CopyOut(j, json, cap);
  if (n < 0) SetError("output buffer too small");
  return n;
}

IM_API int32_t im_audio_clear_rules(void) {
  g_last_error.clear();
  std::lock_guard<std::mutex> lk(g_rules_mu);
  g_rules = Rules{};
  return IM_OK;
}

IM_API int32_t im_audio_add_rule(int32_t kind, const char *value) {
  g_last_error.clear();
  if (!value || !*value) {
    SetError("empty rule value");
    return IM_ERR_INVALID_ARG;
  }
  std::lock_guard<std::mutex> lk(g_rules_mu);
  if (kind == IM_RULE_APP) {
    g_rules.apps.push_back(Lower(Wide(value)));
  } else if (kind == IM_RULE_INSTANCE) {
    char *end = nullptr;
    unsigned long pid = std::strtoul(value, &end, 10);
    if (!end || *end || pid == 0) {
      SetError(std::string("not a process id: ") + value);
      return IM_ERR_INVALID_ARG;
    }
    g_rules.instances.push_back(static_cast<DWORD>(pid));
  } else {
    SetError("unknown rule kind " + std::to_string(kind));
    return IM_ERR_INVALID_ARG;
  }
  return IM_OK;
}

IM_API int32_t im_audio_list_apps(uint32_t flags, char *json, int32_t cap) {
  g_last_error.clear();
  ComInit com;
  ComPtr<IMMDeviceEnumerator> en;
  HRESULT hr = CoCreateInstance(kCLSID_MMDeviceEnumerator, nullptr, CLSCTX_ALL, kIID_IMMDeviceEnumerator, en.PutVoid());
  if (FAILED(hr)) {
    SetError("MMDeviceEnumerator " + HrStr(hr));
    return IM_ERR_INTERNAL;
  }
  ProcessTable pt;
  const bool mic = (flags & IM_FLAG_EXCLUDE_MIC_USERS) != 0;
  Rules rules = CopyRules();
  Snapshot snap = TakeSnapshot(en.Get(), pt, rules, mic, nullptr);
  PlanIncludeSet(snap, pt, rules, mic, {});  // fills notes
  std::string j = "{\"sessions\":";
  Engine::AppendSessions(j, snap);
  j += ",\"warnings\":[";
  for (size_t i = 0; i < snap.warnings.size(); ++i) {
    if (i) j += ',';
    JsonStr(j, snap.warnings[i]);
  }
  j += "]}";
  int32_t n = CopyOut(j, json, cap);
  if (n < 0) SetError("output buffer too small");
  return n;
}

IM_API int32_t im_audio_start(int32_t mode, uint32_t flags) {
  g_last_error.clear();
  if (mode < IM_AUDIO_MODE_INCLUDE_SET || mode > IM_AUDIO_MODE_ENDPOINT) {
    SetError("unknown mode " + std::to_string(mode));
    return IM_ERR_INVALID_ARG;
  }
  std::lock_guard<std::mutex> lk(g_engine_mu);
  if (GEngine()) {
    SetError("engine already started");
    return IM_ERR_ALREADY_STARTED;
  }
  auto e = std::make_shared<Engine>(mode, flags);
  std::string err;
  if (!e->Start(err)) {
    SetError(err);
    return IM_ERR_UNSUPPORTED_OS;
  }
  GEngine() = std::move(e);
  return IM_OK;
}

IM_API int32_t im_audio_read(float *out, int32_t max_frames, int64_t *capture_ns, int32_t timeout_ms) {
  g_last_error.clear();
  std::shared_ptr<Engine> e;
  {
    std::lock_guard<std::mutex> lk(g_engine_mu);
    e = GEngine();
  }
  if (!e) {
    SetError("engine not started");
    return IM_ERR_NOT_STARTED;
  }
  if (!out) {
    SetError("null output buffer");
    return IM_ERR_INVALID_ARG;
  }
  int32_t n = e->Read(out, max_frames, capture_ns, timeout_ms);  // our copy keeps the engine alive
  if (n == IM_ERR_TIMEOUT) SetError("no audio within " + std::to_string(timeout_ms) + " ms");
  else if (n == IM_ERR_NOT_STARTED) SetError("engine stopped");
  else if (n == IM_ERR_INVALID_ARG) SetError("max_frames must be >= " + std::to_string(kChunk));
  return n;
}

IM_API int32_t im_audio_status(char *json, int32_t cap) {
  g_last_error.clear();
  std::shared_ptr<Engine> e;
  {
    std::lock_guard<std::mutex> lk(g_engine_mu);
    e = GEngine();
  }
  if (!e) {
    SetError("engine not started");
    return IM_ERR_NOT_STARTED;
  }
  int32_t n = CopyOut(e->Status(), json, cap);
  if (n < 0) SetError("output buffer too small");
  return n;
}

IM_API void im_audio_stop(void) {
  g_last_error.clear();
  std::shared_ptr<Engine> e;
  {
    std::lock_guard<std::mutex> lk(g_engine_mu);
    e.swap(GEngine());
  }
  if (e) e->Stop();  // freed when the last concurrent reader returns
}

IM_API int32_t im_last_error(char *buf, int32_t cap) { return CopyOut(g_last_error, buf, cap); }

}  // extern "C"
