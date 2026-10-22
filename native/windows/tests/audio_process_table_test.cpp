// The process table's Windows half against real processes (docs/m2/05-windows-audio.md §5.1, §7.4, §19.2; README
// WA35): this executable spawns copies of itself as children that wait (AudioProcessChild.Wait), in folders of its
// choosing and named as it likes, and checks what the snapshot says about them: parent pids, creation times, image
// folders, and msedgewebview2.exe's --user-data-dir, quoted and unquoted, with forward slashes and in WebView2's own
// "<dir>\EBWebView" form. No audio device needed.
#include <windows.h>

#include <cstdint>
#include <deque>
#include <string>
#include <vector>

#include <gtest/gtest.h>

#include "audio/process_snapshot.h"
#include "audio/win_audio.h"
#include "im/audiopolicy/process_table.h"
#include "im/audiopolicy/text.h"

namespace im::audio {
namespace {

// A child gets the name of an event to wait on (at most 30 s) in this variable.
constexpr wchar_t kChildEnv[] = L"IM_AUDIO_TEST_CHILD";

// Runs only as a spawned child: waits until the parent lets it go.
TEST(AudioProcessChild, Wait) {
  wchar_t name[256] = {};
  const DWORD n = GetEnvironmentVariableW(kChildEnv, name, 256);
  if (n == 0 || n >= 256) {
    GTEST_SKIP() << "runs only as a child of the other tests";
  }
  const win::Handle ev(OpenEventW(SYNCHRONIZE, FALSE, name));
  ASSERT_TRUE(ev);
  WaitForSingleObject(ev.get(), 30000);
}

std::wstring ModulePath() {
  std::wstring buf(32768, L'\0');
  const DWORD n = GetModuleFileNameW(nullptr, buf.data(), static_cast<DWORD>(buf.size()));
  buf.resize(n);
  return buf;
}

std::wstring FolderOf(const std::wstring &path) { return path.substr(0, path.find_last_of(L"\\/")); }

uint64_t CreationTime(HANDLE process) {
  FILETIME c{}, e{}, k{}, u{};
  return GetProcessTimes(process, &c, &e, &k, &u) ? win::FiletimeValue(c) : 0;
}

// A temporary folder with copies of this executable, removed at the end.
class TempDir {
public:
  TempDir() {
    wchar_t tmp[MAX_PATH + 1] = {};
    GetTempPathW(MAX_PATH + 1, tmp);
    root_ = std::wstring(tmp) + L"isshoni-audio-test-" + std::to_wstring(GetCurrentProcessId()) + L"-" +
            std::to_wstring(GetTickCount64());
    CreateDirectoryW(root_.c_str(), nullptr);
  }
  ~TempDir() {
    for (auto it = files_.rbegin(); it != files_.rend(); ++it) {
      DeleteFileW(it->c_str());
    }
    for (auto it = dirs_.rbegin(); it != dirs_.rend(); ++it) {
      RemoveDirectoryW(it->c_str());
    }
    RemoveDirectoryW(root_.c_str());
  }
  TempDir(const TempDir &) = delete;
  TempDir &operator=(const TempDir &) = delete;

  const std::wstring &root() const { return root_; }

  // Copies this executable to <root>\<sub>\<name> and returns the copy's path.
  std::wstring Copy(const std::wstring &sub, const std::wstring &name) {
    const std::wstring dir = root_ + L"\\" + sub;
    CreateDirectoryW(dir.c_str(), nullptr);
    dirs_.push_back(dir);
    const std::wstring path = dir + L"\\" + name;
    EXPECT_TRUE(CopyFileW(ModulePath().c_str(), path.c_str(), FALSE)) << "CopyFileW " << GetLastError();
    files_.push_back(path);
    return path;
  }

private:
  std::wstring root_;
  std::vector<std::wstring> dirs_;
  std::vector<std::wstring> files_;
};

// Children that wait on one event until the set is destroyed (then they exit).
class Children {
public:
  Children() {
    name_ = L"Local\\isshoni-audio-test-" + std::to_wstring(GetCurrentProcessId()) + L"-" +
            std::to_wstring(GetTickCount64());
    ev_ = win::Handle(CreateEventW(nullptr, TRUE, FALSE, name_.c_str()));
    SetEnvironmentVariableW(kChildEnv, name_.c_str());
  }
  ~Children() {
    SetEvent(ev_.get());
    for (PROCESS_INFORMATION &pi : kids_) {
      if (WaitForSingleObject(pi.hProcess, 10000) != WAIT_OBJECT_0) {
        TerminateProcess(pi.hProcess, 1);
        WaitForSingleObject(pi.hProcess, 5000);
      }
      CloseHandle(pi.hThread);
      CloseHandle(pi.hProcess);
    }
    SetEnvironmentVariableW(kChildEnv, nullptr);
  }
  Children(const Children &) = delete;
  Children &operator=(const Children &) = delete;

  // Starts exe with extra arguments after the filter that makes it a waiting child.
  const PROCESS_INFORMATION &Spawn(const std::wstring &exe, const std::wstring &extra) {
    std::wstring cmd = L"\"" + exe + L"\" --gtest_filter=AudioProcessChild.Wait";
    if (!extra.empty()) {
      cmd += L" " + extra;
    }
    STARTUPINFOW si{};
    si.cb = sizeof si;
    PROCESS_INFORMATION pi{};
    const BOOL ok =
        CreateProcessW(exe.c_str(), cmd.data(), nullptr, nullptr, FALSE, CREATE_NO_WINDOW, nullptr, nullptr, &si, &pi);
    EXPECT_TRUE(ok) << "CreateProcessW " << GetLastError();
    kids_.push_back(pi);
    return kids_.back();
  }

  // Ends one child now and waits until it has exited.
  void End(const PROCESS_INFORMATION &pi) {
    TerminateProcess(pi.hProcess, 0);
    WaitForSingleObject(pi.hProcess, 10000);
  }

private:
  std::wstring name_;
  win::Handle ev_;
  std::deque<PROCESS_INFORMATION> kids_; // Spawn's references stay valid
};

const audiopolicy::ProcessEntry *FindPid(const std::vector<audiopolicy::ProcessEntry> &entries, DWORD pid) {
  for (const auto &e : entries) {
    if (e.pid == pid) {
      return &e;
    }
  }
  return nullptr;
}

bool SamePath(const std::string &a, const std::wstring &b) {
  return audiopolicy::NormalizePath(a) == audiopolicy::NormalizePath(win::Utf8(b));
}

TEST(AudioProcessTable, SpawnedChildren) {
  TempDir dir;
  Children kids;
  const std::wstring self = ModulePath();
  const std::wstring copy = dir.Copy(L"App Folder", L"Child.exe");
  const PROCESS_INFORMATION &a = kids.Spawn(self, L"");
  const PROCESS_INFORMATION &b = kids.Spawn(copy, L"");

  ProcessSnapshotter snap;
  std::vector<audiopolicy::ProcessEntry> entries;
  ASSERT_TRUE(snap.Take(entries));
  EXPECT_GT(entries.size(), 10u);
  const auto *me = FindPid(entries, GetCurrentProcessId());
  const auto *ea = FindPid(entries, a.dwProcessId);
  const auto *eb = FindPid(entries, b.dwProcessId);
  ASSERT_NE(me, nullptr);
  ASSERT_NE(ea, nullptr);
  ASSERT_NE(eb, nullptr);

  EXPECT_EQ(ea->parent_pid, GetCurrentProcessId());
  EXPECT_EQ(eb->parent_pid, GetCurrentProcessId());
  EXPECT_EQ(ea->created, CreationTime(a.hProcess));
  EXPECT_EQ(eb->created, CreationTime(b.hProcess));
  EXPECT_EQ(me->created, CreationTime(GetCurrentProcess()));
  EXPECT_NE(ea->created, 0u);
  EXPECT_GE(ea->created, me->created) << "a child is created after its parent";
  EXPECT_TRUE(SamePath(me->folder, FolderOf(self))) << me->folder;
  EXPECT_TRUE(SamePath(ea->folder, FolderOf(self))) << ea->folder;
  EXPECT_TRUE(SamePath(eb->folder, dir.root() + L"\\App Folder")) << eb->folder;
  EXPECT_EQ(audiopolicy::FoldCase(eb->exe), "child.exe");
  EXPECT_TRUE(ea->user_data_dir.empty());
  EXPECT_TRUE(eb->user_data_dir.empty());

  // The policy's table links them to this process strictly (the parent is older).
  const audiopolicy::ProcessTable table(entries);
  const auto ime = table.Find(GetCurrentProcessId());
  const auto ib = table.Find(b.dwProcessId);
  ASSERT_TRUE(ime && ib);
  EXPECT_EQ(table.Parent(*ib, false), static_cast<int32_t>(*ime));
  EXPECT_TRUE(table.InTree(*ib, *ime, false));
  EXPECT_EQ(table.at(*ib).exe, "child.exe");

  // A second snapshot keeps the details (cached per process) and drops a child that exited.
  kids.End(a);
  ASSERT_TRUE(snap.Take(entries));
  const auto *eb2 = FindPid(entries, b.dwProcessId);
  ASSERT_NE(eb2, nullptr);
  EXPECT_EQ(eb2->created, CreationTime(b.hProcess));
  EXPECT_TRUE(SamePath(eb2->folder, dir.root() + L"\\App Folder"));
  const auto *ea2 = FindPid(entries, a.dwProcessId);
  EXPECT_TRUE(ea2 == nullptr || ea2->created != CreationTime(a.hProcess)) << "an exited child is gone";
}

TEST(AudioProcessTable, WebView2UserDataDir) {
  TempDir dir;
  Children kids;
  const std::wstring wv = dir.Copy(L"EdgeWebView", L"msedgewebview2.exe");
  const std::wstring other = dir.Copy(L"Other", L"notwebview.exe");
  const std::wstring host = dir.root() + L"\\Data Dir\\isshoni";
  const std::wstring quoted_whole = host + L"\\EBWebView";
  const std::wstring quoted_value = dir.root() + L"\\Quoted Value\\EBWebView";
  const std::wstring forward = L"C:/isshoni-test/plain/EBWebView";
  // As WebView2 passes it (the whole switch quoted), a quoted value, forward slashes unquoted, a later switch.
  const PROCESS_INFORMATION &p1 = kids.Spawn(wv, L"--type=renderer \"--user-data-dir=" + quoted_whole + L"\"");
  const PROCESS_INFORMATION &p2 = kids.Spawn(wv, L"--user-data-dir=\"" + quoted_value + L"\" --lang=en");
  const PROCESS_INFORMATION &p3 = kids.Spawn(wv, L"--user-data-dir=C:/old --user-data-dir=" + forward);
  const PROCESS_INFORMATION &p4 = kids.Spawn(wv, L"--no-sandbox");
  const PROCESS_INFORMATION &p5 = kids.Spawn(other, L"\"--user-data-dir=" + quoted_whole + L"\"");

  ProcessSnapshotter snap;
  std::vector<audiopolicy::ProcessEntry> entries;
  ASSERT_TRUE(snap.Take(entries));
  const auto *e1 = FindPid(entries, p1.dwProcessId);
  const auto *e2 = FindPid(entries, p2.dwProcessId);
  const auto *e3 = FindPid(entries, p3.dwProcessId);
  const auto *e4 = FindPid(entries, p4.dwProcessId);
  const auto *e5 = FindPid(entries, p5.dwProcessId);
  ASSERT_TRUE(e1 && e2 && e3 && e4 && e5);
  EXPECT_EQ(e1->user_data_dir, win::Utf8(quoted_whole));
  EXPECT_EQ(e2->user_data_dir, win::Utf8(quoted_value));
  EXPECT_EQ(e3->user_data_dir, win::Utf8(forward)) << "the last switch counts";
  EXPECT_EQ(e4->user_data_dir, "");
  EXPECT_EQ(e5->user_data_dir, "") << "only msedgewebview2.exe's command line is read (05 §7.4)";
  EXPECT_EQ(audiopolicy::FoldCase(e1->exe), "msedgewebview2.exe");

  // 05 §5.3's path rule on what the table read: "<host>\EBWebView" lies below the folder 03 passes; "<host>2" is no
  // match; forward slashes and case are normalized.
  EXPECT_TRUE(audiopolicy::PathEqualsOrBelow(e1->user_data_dir, win::Utf8(host)));
  EXPECT_TRUE(audiopolicy::PathEqualsOrBelow(e1->user_data_dir, win::Utf8(host) + "\\"));
  EXPECT_FALSE(audiopolicy::PathEqualsOrBelow(e1->user_data_dir, win::Utf8(host) + "2"));
  EXPECT_TRUE(audiopolicy::PathEqualsOrBelow(e3->user_data_dir, "c:\\ISSHONI-TEST\\plain"));
  const audiopolicy::ProcessTable table(entries);
  const auto i1 = table.Find(p1.dwProcessId);
  ASSERT_TRUE(i1);
  EXPECT_EQ(table.at(*i1).user_data_dir, audiopolicy::NormalizePath(win::Utf8(quoted_whole)));

  // The one-process query reads the same.
  const ProcessDetails d = QueryProcessDetails(p2.dwProcessId, true);
  EXPECT_TRUE(d.opened);
  EXPECT_EQ(d.user_data_dir, win::Utf8(quoted_value));
  EXPECT_TRUE(SamePath(d.folder, dir.root() + L"\\EdgeWebView"));
  EXPECT_EQ(d.created, CreationTime(p2.hProcess));
}

TEST(AudioProcessTable, CommandLineAndDescription) {
  // This process's command line as Windows keeps it.
  EXPECT_EQ(ReadCommandLine(GetCurrentProcess()), std::wstring(GetCommandLineW()));
  EXPECT_EQ(ReadCommandLine(nullptr), L"");

  // A system DLL has a FileDescription (05 §5.1's display name); this test executable has no version resource.
  wchar_t sys[MAX_PATH] = {};
  ASSERT_GT(GetSystemDirectoryW(sys, MAX_PATH), 0u);
  EXPECT_FALSE(FileDescription(std::wstring(sys) + L"\\kernel32.dll").empty());
  EXPECT_EQ(FileDescription(ModulePath()), "");
  EXPECT_EQ(FileDescription(L""), "");

  // The idle process has no details; this one has all of them.
  EXPECT_FALSE(QueryProcessDetails(0, false).opened);
  const ProcessDetails me = QueryProcessDetails(GetCurrentProcessId(), false);
  EXPECT_TRUE(me.opened);
  EXPECT_EQ(me.created, CreationTime(GetCurrentProcess()));
  EXPECT_TRUE(SamePath(win::Utf8(me.image), ModulePath()));
  EXPECT_TRUE(me.user_data_dir.empty());
  EXPECT_TRUE(IsWebView2Exe(L"MSEdgeWebView2.EXE"));
  EXPECT_FALSE(IsWebView2Exe(L"msedge.exe"));
}

// Describe fills the FileDescription of the processes asked for, from a cache per image path.
TEST(AudioProcessTable, Describe) {
  ProcessSnapshotter snap;
  std::vector<audiopolicy::ProcessEntry> entries;
  ASSERT_TRUE(snap.Take(entries));
  std::vector<std::size_t> which;
  for (std::size_t i = 0; i < entries.size(); ++i) {
    if (entries[i].pid == GetCurrentProcessId()) {
      which.push_back(i);
    }
    EXPECT_TRUE(entries[i].description.empty()) << "Take leaves descriptions to Describe";
  }
  ASSERT_EQ(which.size(), 1u);
  which.push_back(entries.size() + 5); // out of range: ignored
  snap.Describe(entries, which);
  EXPECT_EQ(entries[which[0]].description, "") << "the test executable has no version resource";
  EXPECT_GT(snap.cached(), 10u);
  snap.Clear();
  EXPECT_EQ(snap.cached(), 0u);
}

} // namespace
} // namespace im::audio
