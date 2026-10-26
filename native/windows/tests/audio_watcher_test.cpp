// The session watcher through the exports (docs/m2/05-windows-audio.md §4.3, §5.6, §7, §11.1, §12, §19.2; README
// WA35): im_init with and without IM_INIT_NO_AUDIO, watch and unwatch, the report and the levels, the probe flags, the
// no-device state of a machine without an audio endpoint (CI's hosted runners), 100 watcher start/stop cycles with the
// process's handle count back to its baseline, the one-shot report's rules and row names, the implicit rule before any
// rules, and mic roots that outlive an unwatch (with child processes of this executable). Runs anywhere: a test that
// needs a render or capture endpoint, or their absence, says so and skips.
#include <windows.h>

#include <algorithm>
#include <chrono>
#include <cstdint>
#include <string>
#include <thread>
#include <vector>

#include <gtest/gtest.h>

#include "audio/mixer.h"
#include "audio/probe_flags.h"
#include "audio/process_snapshot.h"
#include "audio/win_audio.h"
#include "audio_bridge/audio_bridge.h"
#include "im/audiopolicy/text.h"
#include "im/json/writer.h"
#include "isshoni_media.h"

namespace im::audio {
namespace {

std::string LastError() {
  char buf[1024];
  const int32_t n = im_last_error(buf, sizeof buf);
  return n > 0 ? std::string(buf, std::min<std::size_t>(static_cast<std::size_t>(n), sizeof buf - 1)) : std::string();
}

im_status Init(uint32_t flags) {
  im_init_params p{};
  p.struct_size = sizeof p;
  p.abi_version = IM_ABI_VERSION;
  p.flags = flags;
  p.log_level = IM_LOG_DEBUG;
  return im_init(&p);
}

// The rules a share sheet passes, plus a built-in rule on a process every Windows runs (svchost.exe), so the report
// has a "kept out" row without any audio session.
struct Rules {
  im_rule rules[3] = {
      {IM_RULE_APP_EXCLUDE, IM_RULE_SRC_BUILTIN, "svchost.exe", "test-voice", "Test Voice"},
      {IM_RULE_STOP, IM_RULE_SRC_DATA, "explorer.exe", nullptr, nullptr},
      {IM_RULE_SHARED_RUNTIME, IM_RULE_SRC_DATA, "msedgewebview2.exe", nullptr, nullptr},
  };
  im_exclusions ex{};
  explicit Rules(uint32_t count = 3) {
    ex.struct_size = sizeof ex;
    ex.flags = IM_EXCLF_KEEP_OUT_MIC_USERS | IM_EXCLF_FAST_PATH;
    ex.rules = rules;
    ex.rule_count = count;
  }
};

// The report under the snprintf rule (05 §4.3): the length first, then the document. A watcher may publish a new
// report between the two calls (its 1 s poll), so a length that changed is asked again.
std::string Report() {
  for (int attempt = 0; attempt < 5; ++attempt) {
    const int32_t n = im_list_audio_apps(0, nullptr, 0);
    if (n <= 0) {
      ADD_FAILURE() << "im_list_audio_apps: " << n << " " << LastError();
      return {};
    }
    std::string doc(static_cast<std::size_t>(n) + 1, '\0');
    const int32_t m = im_list_audio_apps(0, doc.data(), n + 1);
    if (m == n) {
      doc.resize(static_cast<std::size_t>(n));
      return doc;
    }
  }
  ADD_FAILURE() << "the report changed length five times in a row";
  return {};
}

bool Has(const std::string &doc, const std::string &fragment) { return doc.find(fragment) != std::string::npos; }

// The report's "seq".
uint32_t Seq(const std::string &doc) {
  const auto at = doc.find("\"seq\":");
  return at == std::string::npos ? 0 : static_cast<uint32_t>(std::stoul(doc.substr(at + 6)));
}

// Active endpoints of a flow, counted on a thread of the test's own (COM MTA), independently of the engine.
uint32_t Endpoints(EDataFlow flow) {
  uint32_t n = 0;
  std::thread([&] {
    const win::ComApartment com;
    win::ComPtr<IMMDeviceEnumerator> en;
    if (SUCCEEDED(com.hr()) && SUCCEEDED(CoCreateInstance(__uuidof(MMDeviceEnumerator), nullptr, CLSCTX_ALL,
                                                          __uuidof(IMMDeviceEnumerator), en.PutVoid()))) {
      n = ActiveEndpoints(en.Get(), flow);
    }
  }).join();
  return n;
}

// The verdict and reason of the report row with this id ("kept_out/mic"), "" when there is no such row.
std::string RowVerdict(const std::string &doc, const std::string &id) {
  const auto row = doc.find("{\"id\":\"" + id + "\",");
  const std::string vk = "\"verdict\":\"";
  const std::string rk = "\"reason\":\"";
  const auto v = row == std::string::npos ? row : doc.find(vk, row);
  const auto ve = v == std::string::npos ? v : doc.find('"', v + vk.size());
  const auto r = ve == std::string::npos ? ve : doc.find(rk, ve);
  const auto re = r == std::string::npos ? r : doc.find('"', r + rk.size());
  if (re == std::string::npos) {
    return {};
  }
  return doc.substr(v + vk.size(), ve - v - vk.size()) + "/" + doc.substr(r + rk.size(), re - r - rk.size());
}

// The report's session object of pid (sessions have no nested objects), "" when there is none.
std::string SessionOf(const std::string &doc, uint32_t pid) {
  const auto at = doc.find("{\"pid\":" + std::to_string(pid) + ",");
  const auto end = at == std::string::npos ? at : doc.find('}', at);
  return end == std::string::npos ? std::string() : doc.substr(at, end - at + 1);
}

std::wstring ModulePath() {
  std::wstring buf(32768, L'\0');
  const DWORD n = GetModuleFileNameW(nullptr, buf.data(), static_cast<DWORD>(buf.size()));
  buf.resize(n);
  return buf;
}

// This executable's file name as the report writes it (case-folded).
std::string ExeName() {
  const std::wstring path = ModulePath();
  return audiopolicy::FoldCase(win::Utf8(path.substr(path.find_last_of(L"\\/") + 1)));
}

// A started shared-mode stream on the default endpoint of a flow (a render one plays silence): an audio session of this
// process on that endpoint for as long as it lives. Needs COM on the calling thread.
class Stream {
public:
  Stream() = default;
  Stream(const Stream &) = delete;
  Stream &operator=(const Stream &) = delete;
  ~Stream() {
    if (client_) {
      client_->Stop();
    }
  }

  bool Open(EDataFlow flow) {
    win::ComPtr<IMMDeviceEnumerator> en;
    win::ComPtr<IMMDevice> device;
    WAVEFORMATEX *format = nullptr;
    if (FAILED(CoCreateInstance(__uuidof(MMDeviceEnumerator), nullptr, CLSCTX_ALL, __uuidof(IMMDeviceEnumerator),
                                en.PutVoid())) ||
        FAILED(en->GetDefaultAudioEndpoint(flow, eConsole, device.Put())) ||
        FAILED(device->Activate(__uuidof(IAudioClient), CLSCTX_ALL, nullptr, client_.PutVoid())) ||
        FAILED(client_->GetMixFormat(&format))) {
      client_.Reset();
      return false;
    }
    const HRESULT hr = client_->Initialize(AUDCLNT_SHAREMODE_SHARED, 0, 2'000'000, 0, format, nullptr); // 200 ms
    CoTaskMemFree(format);
    if (FAILED(hr)) {
      client_.Reset();
      return false;
    }
    if (flow == eRender) {
      UINT32 frames = 0;
      BYTE *data = nullptr;
      win::ComPtr<IAudioRenderClient> render;
      if (FAILED(client_->GetBufferSize(&frames)) ||
          FAILED(client_->GetService(__uuidof(IAudioRenderClient), render.PutVoid())) ||
          FAILED(render->GetBuffer(frames, &data)) ||
          FAILED(render->ReleaseBuffer(frames, AUDCLNT_BUFFERFLAGS_SILENT))) {
        client_.Reset();
        return false;
      }
    }
    if (FAILED(client_->Start())) {
      client_.Reset();
      return false;
    }
    return true;
  }

private:
  win::ComPtr<IAudioClient> client_;
};

// MicRootSurvivesUnwatch's children get the prefix of its events' names in this variable.
constexpr wchar_t kChildEnv[] = L"IM_AUDIO_WATCH_CHILD";

std::wstring ChildPrefix() {
  wchar_t name[256] = {};
  const DWORD n = GetEnvironmentVariableW(kChildEnv, name, 256);
  return n > 0 && n < 256 ? std::wstring(name, n) : std::wstring();
}

win::Handle OpenChildEvent(const std::wstring &prefix, const wchar_t *what) {
  return win::Handle(OpenEventW(EVENT_MODIFY_STATE | SYNCHRONIZE, FALSE, (prefix + what).c_str()));
}

// Starts this executable running one test (a child role); hProcess is null when CreateProcessW failed.
PROCESS_INFORMATION SpawnSelf(const wchar_t *test) {
  const std::wstring exe = ModulePath();
  std::wstring cmd = L"\"" + exe + L"\" --gtest_filter=" + test;
  STARTUPINFOW si{};
  si.cb = sizeof si;
  PROCESS_INFORMATION pi{};
  if (!CreateProcessW(exe.c_str(), cmd.data(), nullptr, nullptr, FALSE, CREATE_NO_WINDOW, nullptr, nullptr, &si, &pi)) {
    pi = PROCESS_INFORMATION{};
  }
  return pi;
}

// MicRootSurvivesUnwatch's children (each skips when run on its own). Launch starts Voice and exits at once, so Voice
// is not in this test's process tree (isshoni's own, which mic detection skips) once the watcher looks.
TEST(AudioWatcherChild, Launch) {
  if (ChildPrefix().empty()) {
    GTEST_SKIP() << "runs only as a child of AudioWatcher.MicRootSurvivesUnwatch";
  }
  const PROCESS_INFORMATION pi = SpawnSelf(L"AudioWatcherChild.Voice");
  ASSERT_NE(pi.hProcess, nullptr) << "CreateProcessW " << GetLastError();
  CloseHandle(pi.hThread);
  CloseHandle(pi.hProcess);
}

// A voice app's main process: it plays (a silent render stream) and starts Mic, a helper of the same app (same exe and
// folder) that captures; it stays until the test is done.
TEST(AudioWatcherChild, Voice) {
  const std::wstring prefix = ChildPrefix();
  if (prefix.empty()) {
    GTEST_SKIP() << "runs only as a child of AudioWatcher.MicRootSurvivesUnwatch";
  }
  const win::ComApartment com;
  ASSERT_TRUE(SUCCEEDED(com.hr()));
  const win::Handle voice = OpenChildEvent(prefix, L"-voice");
  const win::Handle done = OpenChildEvent(prefix, L"-done");
  ASSERT_TRUE(voice && done);
  Stream out;
  ASSERT_TRUE(out.Open(eRender));
  const PROCESS_INFORMATION mic = SpawnSelf(L"AudioWatcherChild.Mic");
  ASSERT_NE(mic.hProcess, nullptr) << "CreateProcessW " << GetLastError();
  SetEvent(voice.get());
  WaitForSingleObject(done.get(), 60000);
  if (WaitForSingleObject(mic.hProcess, 10000) != WAIT_OBJECT_0) {
    TerminateProcess(mic.hProcess, 1);
  }
  CloseHandle(mic.hThread);
  CloseHandle(mic.hProcess);
}

// The voice app's helper: it holds a capture stream until the test lets it go, then exits.
TEST(AudioWatcherChild, Mic) {
  const std::wstring prefix = ChildPrefix();
  if (prefix.empty()) {
    GTEST_SKIP() << "runs only as a child of AudioWatcher.MicRootSurvivesUnwatch";
  }
  const win::ComApartment com;
  ASSERT_TRUE(SUCCEEDED(com.hr()));
  const win::Handle on = OpenChildEvent(prefix, L"-mic-on");
  const win::Handle off = OpenChildEvent(prefix, L"-mic-off");
  ASSERT_TRUE(on && off);
  Stream in;
  ASSERT_TRUE(in.Open(eCapture)) << "no capture stream (the microphone privacy setting?)";
  SetEvent(on.get());
  WaitForSingleObject(off.get(), 60000);
}

class AudioWatcher : public ::testing::Test {
protected:
  void TearDown() override { im_shutdown(); }
};

// IM_INIT_NO_AUDIO (05 §4.3): no audio API is touched; the watcher's calls answer IM_ERR_BAD_STATE, im_set_exclusions
// keeps the rules, unwatch is a no-op, and audio_flags is 0.
TEST_F(AudioWatcher, NoAudioInit) {
  ASSERT_EQ(Init(IM_INIT_NO_AUDIO), IM_OK) << LastError();
  Rules r;
  im_audio_levels levels{};
  levels.struct_size = sizeof levels;
  EXPECT_EQ(im_audio_watch(&r.ex), IM_ERR_BAD_STATE);
  EXPECT_NE(LastError().find("IM_INIT_NO_AUDIO"), std::string::npos) << LastError();
  EXPECT_EQ(im_list_audio_apps(0, nullptr, 0), IM_ERR_BAD_STATE);
  EXPECT_EQ(im_audio_levels_get(&levels), IM_ERR_BAD_STATE);
  EXPECT_EQ(im_audio_sound_check(0, 1000, nullptr, 0), IM_ERR_BAD_STATE);
  EXPECT_EQ(im_audio_set_mode(IM_AUDIO_MODE_OFF), IM_ERR_BAD_STATE);
  EXPECT_EQ(im_audio_set_muted(1), IM_ERR_BAD_STATE);
  EXPECT_EQ(im_set_exclusions(&r.ex), IM_OK) << LastError();
  EXPECT_EQ(im_audio_unwatch(), IM_OK) << LastError();
  EXPECT_EQ(audio_bridge::Engine().ProbeFlags(), 0u);
  EXPECT_FALSE(audio_bridge::Engine().NewMixer());
}

// Not watching: the levels are IM_ERR_BAD_STATE (not_watching) and the report is a one-shot snapshot ("stopped");
// watching: both work and the built-in row shows; after unwatch the levels are refused again. Mode and mute need a
// running audio session, which the watcher alone isn't (05 §4.3).
TEST_F(AudioWatcher, WatchAndUnwatch) {
  ASSERT_EQ(Init(0), IM_OK) << LastError();
  im_audio_levels levels{};
  levels.struct_size = sizeof levels;
  EXPECT_EQ(im_audio_levels_get(&levels), IM_ERR_BAD_STATE);
  EXPECT_EQ(LastError().rfind("not_watching: ", 0), 0u) << LastError();
  std::string doc = Report();
  EXPECT_TRUE(Has(doc, "{\"v\":1,\"seq\":")) << doc.substr(0, 200);
  EXPECT_TRUE(Has(doc, "\"state\":\"stopped\""));
  EXPECT_TRUE(Has(doc, "\"engine\":\"none\""));
  EXPECT_TRUE(Has(doc, "\"live\":false"));

  Rules r;
  ASSERT_EQ(im_audio_watch(&r.ex), IM_OK) << LastError();
  ASSERT_EQ(im_audio_watch(&r.ex), IM_OK) << "idempotent: " << LastError();
  doc = Report();
  EXPECT_TRUE(Has(doc, "\"state\":\"ok\"") || Has(doc, "\"state\":\"no-device\"")) << doc.substr(0, 300);
  EXPECT_TRUE(Has(doc, "\"engine\":\"none\""));
  EXPECT_TRUE(Has(doc,
                  "{\"id\":\"test-voice\",\"name\":\"Test Voice\",\"exe\":\"svchost.exe\",\"verdict\":\"kept_out\","
                  "\"reason\":\"builtin\""))
      << "a running process matched by a built-in rule has its row (05 §12.1)";
  EXPECT_FALSE(Has(doc, "\"pids\":0,")) << "every row counts its processes";
  ASSERT_EQ(im_audio_levels_get(&levels), IM_OK) << LastError();
  EXPECT_EQ(levels.struct_size, sizeof levels);
  // The levels carry the report's seq of their sample (published every 100 ms, so they may lag a new report).
  EXPECT_GE(levels.report_seq, 1u);
  EXPECT_LE(levels.report_seq, Seq(Report()));
  EXPECT_FLOAT_EQ(levels.mix_peak_db, -120.0f) << "not live";
  EXPECT_LE(levels.count, IM_MAX_LEVEL_APPS);
  for (uint32_t i = 0; i < levels.count; ++i) {
    EXPECT_EQ(levels.apps[i].source, 1u) << "only activity meters while not live";
  }
  EXPECT_EQ(im_audio_set_mode(IM_AUDIO_MODE_OFF), IM_ERR_BAD_STATE);
  EXPECT_EQ(im_audio_set_muted(1), IM_ERR_BAD_STATE);

  // New rules apply within 50 ms (05 §4.1); the row goes, and seq moves.
  const uint32_t before = Seq(doc);
  Rules fewer(2);
  fewer.rules[0] = fewer.rules[1];
  fewer.rules[1] = fewer.rules[2];
  ASSERT_EQ(im_set_exclusions(&fewer.ex), IM_OK) << LastError();
  bool gone = false;
  for (int i = 0; i < 40 && !gone; ++i) {
    std::this_thread::sleep_for(std::chrono::milliseconds(50));
    doc = Report();
    gone = !Has(doc, "\"id\":\"test-voice\"");
  }
  EXPECT_TRUE(gone) << "the row of a removed rule stays";
  EXPECT_GT(Seq(doc), before);
  EXPECT_TRUE(Has(doc, "\"code\":\"rules_changed\""));

  ASSERT_EQ(im_audio_unwatch(), IM_OK) << LastError();
  ASSERT_EQ(im_audio_unwatch(), IM_OK) << "idempotent: " << LastError();
  EXPECT_EQ(im_audio_levels_get(&levels), IM_ERR_BAD_STATE);
  EXPECT_TRUE(Has(Report(), "\"state\":\"stopped\""));
}

// A machine without an active render endpoint (CI's hosted runners, 05 §7.5, §19.2): state no-device, no crash, the
// probe skipped, the levels empty, and neither loopback nor session-events flag.
TEST_F(AudioWatcher, NoDeviceState) {
  if (Endpoints(eRender) > 0) {
    GTEST_SKIP() << "this machine has an active render endpoint";
  }
  ASSERT_EQ(Init(0), IM_OK) << LastError();
  Rules r;
  ASSERT_EQ(im_audio_watch(&r.ex), IM_OK) << LastError();
  const std::string doc = Report();
  EXPECT_TRUE(Has(doc, "\"state\":\"no-device\"")) << doc.substr(0, 400);
  EXPECT_TRUE(Has(doc, "\"device\":{\"present\":false,\"renderEndpoints\":0,"));
  EXPECT_TRUE(Has(doc, "\"probe\":{\"result\":\"skipped\""));
  EXPECT_TRUE(Has(doc, "\"engine\":\"none\""));
  EXPECT_TRUE(Has(doc, "\"sessions\":[]"));
  im_audio_levels levels{};
  levels.struct_size = sizeof levels;
  ASSERT_EQ(im_audio_levels_get(&levels), IM_OK) << LastError();
  EXPECT_EQ(levels.count, 0u);
  const uint32_t flags = audio_bridge::Engine().ProbeFlags();
  EXPECT_EQ(flags & (IM_AUDIO_PROCESS_LOOPBACK | IM_AUDIO_ENDPOINT_LOOPBACK | IM_AUDIO_SESSION_EVENTS), 0u) << flags;
  EXPECT_EQ((flags & IM_AUDIO_MIC_DETECTION) != 0, Endpoints(eCapture) > 0) << flags;
}

// im_caps.audio_flags (05 §11.1): only the four bits; the mic bit exactly when a capture endpoint is active; without
// a render endpoint no loopback or session-events bit; cached (the second answer is the same and quick).
TEST_F(AudioWatcher, ProbeFlags) {
  ASSERT_EQ(Init(0), IM_OK) << LastError();
  const uint32_t render = Endpoints(eRender);
  const uint32_t capture = Endpoints(eCapture);
  const uint32_t flags = audio_bridge::Engine().ProbeFlags();
  EXPECT_EQ(flags & ~0xFu, 0u) << flags;
  EXPECT_EQ((flags & IM_AUDIO_MIC_DETECTION) != 0, capture > 0) << flags;
  if (render == 0) {
    EXPECT_EQ(flags & (IM_AUDIO_PROCESS_LOOPBACK | IM_AUDIO_ENDPOINT_LOOPBACK | IM_AUDIO_SESSION_EVENTS), 0u);
  } else {
    EXPECT_NE(flags & IM_AUDIO_SESSION_EVENTS, 0u) << "session notifications register on a render endpoint";
  }
  const auto t0 = std::chrono::steady_clock::now();
  EXPECT_EQ(audio_bridge::Engine().ProbeFlags(), flags);
  EXPECT_LT(std::chrono::steady_clock::now() - t0, std::chrono::milliseconds(500)) << "the flags are cached";
  // Watching changes nothing about them.
  Rules r;
  ASSERT_EQ(im_audio_watch(&r.ex), IM_OK) << LastError();
  EXPECT_EQ(audio_bridge::Engine().ProbeFlags(), flags);
}

// The one-shot report (05 §12.1) right after im_set_exclusions has the new rules: the kept one-shot document is
// dropped when rules change. A row without a session is named like any other (05 §5.1): with a built-in rule that has
// no name, after the FileDescription of the process the rule matched. svchost.exe runs on every Windows, so no audio
// device is needed; the row is named after the first svchost.exe of the snapshot (Toolhelp's order, which the table
// keeps), whose description this test reads on its own.
TEST_F(AudioWatcher, OneShotRulesAndRowNames) {
  std::string description;
  {
    ProcessSnapshotter snap;
    std::vector<audiopolicy::ProcessEntry> entries;
    ASSERT_TRUE(snap.Take(entries));
    for (std::size_t i = 0; i < entries.size(); ++i) {
      if (audiopolicy::FoldCase(entries[i].exe) == "svchost.exe") {
        const std::size_t which[] = {i};
        snap.Describe(entries, which);
        description = entries[i].description;
        break;
      }
    }
  }
  ASSERT_EQ(Init(0), IM_OK) << LastError();
  std::string doc = Report();
  EXPECT_TRUE(Has(doc, "\"state\":\"stopped\"")) << doc.substr(0, 300);
  EXPECT_FALSE(Has(doc, "\"id\":\"test-voice\"")) << "no rule names svchost.exe yet";

  Rules r;
  r.rules[0].name = nullptr;
  ASSERT_EQ(im_set_exclusions(&r.ex), IM_OK) << LastError();
  doc = Report();
  EXPECT_TRUE(Has(doc, "{\"id\":\"test-voice\",\"name\":\"")) << "the next one-shot report has the new rules";
  EXPECT_EQ(RowVerdict(doc, "test-voice"), "kept_out/builtin");
  if (description.empty() || description.size() >= 260) {
    GTEST_SKIP() << "the first svchost.exe has no readable FileDescription here";
  }
  std::string name;
  json::AppendEscaped(name, description);
  EXPECT_TRUE(Has(doc, "{\"id\":\"test-voice\",\"name\":\"" + name + "\",\"exe\":\"svchost.exe\","))
      << "want the name \"" << description << "\": " << doc.substr(0, 600);
}

// Before any rules from Go, the watcher has the implicit instance rule on its own process (05 §4.3): a one-shot report
// right after im_init shows this process's render session as isshoni's, kept out. Needs a render endpoint.
TEST_F(AudioWatcher, OwnSessionKeptOutBeforeAnyRules) {
  if (Endpoints(eRender) == 0) {
    GTEST_SKIP() << "needs an active render endpoint";
  }
  const win::ComApartment com;
  ASSERT_TRUE(SUCCEEDED(com.hr()));
  Stream out;
  ASSERT_TRUE(out.Open(eRender));
  ASSERT_EQ(Init(0), IM_OK) << LastError();
  const std::string doc = Report();
  const std::string own = SessionOf(doc, static_cast<uint32_t>(GetCurrentProcessId()));
  ASSERT_FALSE(own.empty()) << doc.substr(0, 600);
  EXPECT_TRUE(Has(own, "\"appId\":\"isshoni\",\"flow\":\"render\"")) << own;
  EXPECT_TRUE(Has(own, "\"verdict\":\"kept_out\",\"reason\":\"isshoni\"")) << own;
}

// Mic roots outlive the share sheet (05 §5.6, §5.7): a voice app whose helper captured stays kept out (mic) after the
// helper's capture ended, while watching, in the one-shot report after unwatch, and after the next watch. The app is
// two children of this executable outside its process tree (AudioWatcherChild). Needs a render and a capture endpoint
// and the microphone (the owner's PC); CI's runners skip it.
TEST_F(AudioWatcher, MicRootSurvivesUnwatch) {
  if (Endpoints(eRender) == 0 || Endpoints(eCapture) == 0) {
    GTEST_SKIP() << "needs an active render and capture endpoint";
  }
  const std::wstring prefix = L"Local\\isshoni-audio-watch-" + std::to_wstring(GetCurrentProcessId()) + L"-" +
                              std::to_wstring(GetTickCount64());
  const auto event = [&](const wchar_t *what) {
    return win::Handle(CreateEventW(nullptr, TRUE, FALSE, (prefix + what).c_str()));
  };
  const win::Handle voice = event(L"-voice");
  const win::Handle mic_on = event(L"-mic-on");
  const win::Handle mic_off = event(L"-mic-off");
  const win::Handle done = event(L"-done");
  ASSERT_TRUE(voice && mic_on && mic_off && done);
  struct LetGo {
    HANDLE off;
    HANDLE end;
    ~LetGo() {
      SetEvent(off);
      SetEvent(end);
      SetEnvironmentVariableW(kChildEnv, nullptr);
    }
  } const let_go{mic_off.get(), done.get()};

  // The children start before the watcher does, and Launch is gone before it first looks: no lineage ties Voice to
  // this process.
  SetEnvironmentVariableW(kChildEnv, prefix.c_str());
  const PROCESS_INFORMATION launch = SpawnSelf(L"AudioWatcherChild.Launch");
  ASSERT_NE(launch.hProcess, nullptr) << "CreateProcessW " << GetLastError();
  const DWORD launched = WaitForSingleObject(launch.hProcess, 10000);
  CloseHandle(launch.hThread);
  CloseHandle(launch.hProcess);
  ASSERT_EQ(launched, WAIT_OBJECT_0);
  if (WaitForSingleObject(voice.get(), 10000) != WAIT_OBJECT_0) {
    GTEST_SKIP() << "the voice child has no render stream";
  }
  if (WaitForSingleObject(mic_on.get(), 10000) != WAIT_OBJECT_0) {
    GTEST_SKIP() << "the helper could not open the microphone (privacy setting?)";
  }

  ASSERT_EQ(Init(0), IM_OK) << LastError();
  Rules r;
  ASSERT_EQ(im_audio_watch(&r.ex), IM_OK) << LastError();
  const std::string app = "exe:" + ExeName();
  std::string doc;
  bool kept = false;
  for (int i = 0; i < 100 && !kept; ++i) {
    doc = Report();
    kept = RowVerdict(doc, app) == "kept_out/mic";
    if (!kept) {
      std::this_thread::sleep_for(std::chrono::milliseconds(100));
    }
  }
  ASSERT_TRUE(kept) << "the voice app while its helper captures: " << RowVerdict(doc, app);

  // The helper ends its capture and exits; the app stays a mic root while its main process lives.
  SetEvent(mic_off.get());
  const std::string capture = "\"appId\":\"" + app + "\",\"flow\":\"capture\"";
  bool gone = false;
  for (int i = 0; i < 100 && !gone; ++i) {
    doc = Report();
    gone = !Has(doc, capture);
    if (!gone) {
      std::this_thread::sleep_for(std::chrono::milliseconds(100));
    }
  }
  ASSERT_TRUE(gone) << "the helper's capture session stays";
  EXPECT_EQ(RowVerdict(doc, app), "kept_out/mic") << "sticky while watching";

  // The sheet closes and opens again.
  ASSERT_EQ(im_audio_unwatch(), IM_OK) << LastError();
  doc = Report();
  EXPECT_TRUE(Has(doc, "\"state\":\"stopped\""));
  EXPECT_EQ(RowVerdict(doc, app), "kept_out/mic") << "the one-shot report after unwatch";
  ASSERT_EQ(im_audio_watch(&r.ex), IM_OK) << LastError();
  EXPECT_EQ(RowVerdict(Report(), app), "kept_out/mic") << "unwatch and watch keep the mic root (05 §5.6)";
}

// 100 watcher start/stop cycles (im_init, watch, report, levels, unwatch, im_shutdown) leave the process's handle
// count where it was: the watcher thread, its events, the enumerator, the notifications and the process handles of
// the table are all released (05 §19.2).
TEST_F(AudioWatcher, StartStopCyclesKeepHandles) {
  auto cycle = [] {
    if (Init(0) != IM_OK) {
      ADD_FAILURE() << "im_init: " << LastError();
      return;
    }
    Rules r;
    EXPECT_EQ(im_audio_watch(&r.ex), IM_OK) << LastError();
    EXPECT_GT(im_list_audio_apps(0, nullptr, 0), 0) << LastError();
    im_audio_levels levels{};
    levels.struct_size = sizeof levels;
    EXPECT_EQ(im_audio_levels_get(&levels), IM_OK) << LastError();
    EXPECT_EQ(im_audio_unwatch(), IM_OK) << LastError();
    im_shutdown();
  };
  auto handles = [] {
    DWORD n = 0;
    GetProcessHandleCount(GetCurrentProcess(), &n);
    return static_cast<int64_t>(n);
  };
  // Warm up: the first cycles load mmdevapi and COM's own lasting state.
  for (int i = 0; i < 3; ++i) {
    cycle();
  }
  const int64_t baseline = handles();
  int64_t at10 = 0;
  for (int i = 0; i < 100; ++i) {
    cycle();
    if (i == 9) {
      at10 = handles();
    }
  }
  const int64_t after = handles();
  // The thread pool's own threads come and go, so a little slack, but no growth with the cycles.
  EXPECT_LE(after, baseline + 32) << "baseline " << baseline << ", after 100 cycles " << after;
  EXPECT_LE(after - at10, 16) << "after 10 cycles " << at10 << ", after 100 " << after;
}

} // namespace
} // namespace im::audio
