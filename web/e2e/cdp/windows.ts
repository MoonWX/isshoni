// What the real-app CDP spec (desktop.cdp.ts, M2 03 §22.4 B) asks Windows itself, through Windows PowerShell 5.1:
// the app's main window and its state, the visible top-level windows of the session (W21: no Settings or search
// window opened), the app's process tree with the command lines (W19), the environment variable names of a process
// (W19: the WEBVIEW2_* scrub), and the private working sets that 03 §20's memory budget is about.
//
// Nothing here prints a window title, a variable's value, a user name or a path: the spec's annotations go into the
// PR's "Wails checklist", which is public. Every script is ASCII and runs under Windows PowerShell 5.1 (C# 5 for
// Add-Type), as the runner's and the owner's PCs have it.
import { execFile } from 'node:child_process';
import { promisify } from 'node:util';

const execFileAsync = promisify(execFile);

/** One PowerShell call may compile a type first (Add-Type): a few seconds on a cold runner. */
const PS_TIMEOUT_MS = 60_000;

/**
 * Runs a PowerShell script and returns its trimmed stdout. -EncodedCommand (base64 of UTF-16LE), so the C# in a
 * script keeps its double quotes through Windows' argument quoting.
 */
export async function ps(script: string): Promise<string> {
  const { stdout } = await execFileAsync(
    'powershell.exe',
    ['-NoProfile', '-NonInteractive', '-EncodedCommand', Buffer.from(script, 'utf16le').toString('base64')],
    { timeout: PS_TIMEOUT_MS, maxBuffer: 16 * 1024 * 1024 },
  );
  return stdout.trim();
}

/** Parses a ConvertTo-Json array that PowerShell printed (an empty one prints nothing). */
function jsonArray<T>(out: string): T[] {
  if (out === '') return [];
  const v = JSON.parse(out) as T | T[];
  return Array.isArray(v) ? v : [v];
}

/** The window class of the app's main window: Wails' default WndClass at beta.34, which the app doesn't change. */
export const MAIN_WINDOW_CLASS = 'WailsWebviewWindow';
/** The window class of the Sharing pill (internal/desktop/winos/pill_windows.go, 03 §11.4). */
export const PILL_WINDOW_CLASS = 'isshoni.pill';

/** A top-level window of a process, shown or not. */
export interface ProcessWindow {
  readonly hwnd: string;
  readonly visible: boolean;
  /** The window class (GetClassNameW); a class name is the app's own, never a title. */
  readonly cls: string;
}

const PROCESS_WINDOWS_TYPE = `
Add-Type -TypeDefinition @'
using System;
using System.Collections.Generic;
using System.Runtime.InteropServices;
using System.Text;
public static class IsshoniCdpOwn {
  delegate bool EnumProc(IntPtr h, IntPtr p);
  [DllImport("user32.dll")] static extern bool EnumWindows(EnumProc cb, IntPtr p);
  [DllImport("user32.dll")] static extern bool IsWindowVisible(IntPtr h);
  [DllImport("user32.dll")] static extern uint GetWindowThreadProcessId(IntPtr h, out uint pid);
  [DllImport("user32.dll", EntryPoint = "GetClassNameW", CharSet = CharSet.Unicode)]
  static extern int GetClassNameW(IntPtr h, StringBuilder name, int max);
  public static string[] Of(int target) {
    List<string> list = new List<string>();
    EnumWindows(delegate (IntPtr h, IntPtr p) {
      uint pid;
      GetWindowThreadProcessId(h, out pid);
      if (pid == (uint)target) {
        StringBuilder cls = new StringBuilder(256);
        GetClassNameW(h, cls, cls.Capacity);
        list.Add(h.ToInt64().ToString() + " " + (IsWindowVisible(h) ? "1" : "0") + " " + cls.ToString());
      }
      return true;
    }, IntPtr.Zero);
    return list.ToArray();
  }
}
'@
`;

/** Every top-level window of a process (EnumWindows), hidden ones included, with its class. */
export async function processWindows(pid: number): Promise<ProcessWindow[]> {
  const out = await ps(`${PROCESS_WINDOWS_TYPE}
ConvertTo-Json -Compress -InputObject @([IsshoniCdpOwn]::Of(${String(pid)}))`);
  // "<hwnd> <0|1> <class>": the class is the rest of the line.
  return jsonArray<string>(out).map((line) => {
    const [hwnd = '', visible = '', ...cls] = line.split(' ');
    return { hwnd, visible: visible === '1', cls: cls.join(' ') };
  });
}

/**
 * The main window's HWND of a process as a decimal string, "0" while it has none: its one top-level window of the
 * Wails class, shown or hidden. Not .NET's MainWindowHandle: that is the first visible unowned window in Z order,
 * which while a share is live is the topmost Sharing pill (03 §11.4), not the main window.
 */
export async function mainWindow(pid: number): Promise<string> {
  const main = (await processWindows(pid)).filter((w) => w.cls === MAIN_WINDOW_CLASS);
  if (main.length > 1)
    throw new Error(`process ${String(pid)} has ${String(main.length)} ${MAIN_WINDOW_CLASS} windows`);
  return main[0]?.hwnd ?? '0';
}

const USER32 =
  "Add-Type -Namespace IsshoniCdp -Name U -MemberDefinition '" +
  '[DllImport("user32.dll")] public static extern bool IsWindow(IntPtr h); ' +
  '[DllImport("user32.dll")] public static extern bool IsWindowVisible(IntPtr h); ' +
  '[DllImport("user32.dll")] public static extern bool PostMessageW(IntPtr h, uint msg, IntPtr w, IntPtr l);' +
  "'; ";

/**
 * Posts WM_CLOSE to a window (an HWND as a decimal string), as its close button sends it; false when nothing was
 * posted. What Process.CloseMainWindow does, but to the window named, not to whatever .NET takes for the main one.
 */
export async function postClose(hwnd: string): Promise<boolean> {
  return (
    (await ps(`${USER32}[IsshoniCdp.U]::PostMessageW([IntPtr]${hwnd}, 0x10, [IntPtr]::Zero, [IntPtr]::Zero)`)) ===
    'True'
  );
}

/** IsWindow and IsWindowVisible of an HWND (decimal), independent of Chromium's own idea of visibility. */
export async function windowState(hwnd: string): Promise<{ exists: boolean; visible: boolean }> {
  const out = await ps(
    `${USER32}$h = [IntPtr]${hwnd}; '{0} {1}' -f [IsshoniCdp.U]::IsWindow($h), [IsshoniCdp.U]::IsWindowVisible($h)`,
  );
  const [exists, visible] = out.split(' ');
  return { exists: exists === 'True', visible: visible === 'True' };
}

const KEYS_TYPE = `
Add-Type -TypeDefinition @'
using System;
using System.Runtime.InteropServices;
using System.Threading;
public static class IsshoniCdpKeys {
  [StructLayout(LayoutKind.Sequential)]
  struct MouseInput { public int Dx; public int Dy; public uint Data; public uint Flags; public uint Time; public IntPtr Extra; }
  [StructLayout(LayoutKind.Sequential)]
  struct KeyInput { public ushort Vk; public ushort Scan; public uint Flags; public uint Time; public IntPtr Extra; }
  // The union is as large as its largest member (MOUSEINPUT), so cbSize is sizeof(INPUT) on either bitness.
  [StructLayout(LayoutKind.Explicit)]
  struct InputUnion { [FieldOffset(0)] public MouseInput Mouse; [FieldOffset(0)] public KeyInput Key; }
  [StructLayout(LayoutKind.Sequential)]
  struct Input { public uint Type; public InputUnion U; }
  [DllImport("user32.dll", SetLastError = true)] static extern uint SendInput(uint count, Input[] inputs, int size);
  [DllImport("user32.dll")] static extern IntPtr GetForegroundWindow();
  [DllImport("user32.dll")] static extern bool SetForegroundWindow(IntPtr h);
  [DllImport("user32.dll")] static extern bool BringWindowToTop(IntPtr h);
  [DllImport("user32.dll")] static extern uint GetWindowThreadProcessId(IntPtr h, IntPtr pid);
  [DllImport("user32.dll")] static extern bool AttachThreadInput(uint from, uint to, bool attach);
  [DllImport("user32.dll", EntryPoint = "MapVirtualKeyW")] static extern uint MapVirtualKeyW(uint code, uint type);
  [DllImport("kernel32.dll")] static extern uint GetCurrentThreadId();
  // Brings h to the foreground: this thread's input is attached to the foreground window's thread and to h's for
  // the call, which lets SetForegroundWindow through. True once h is the foreground window.
  public static bool Front(IntPtr h) {
    uint me = GetCurrentThreadId();
    for (int i = 0; i < 20; i++) {
      IntPtr fg = GetForegroundWindow();
      if (fg == h) { return true; }
      uint fgThread = GetWindowThreadProcessId(fg, IntPtr.Zero);
      uint target = GetWindowThreadProcessId(h, IntPtr.Zero);
      bool toFg = fgThread != 0 && fgThread != me && AttachThreadInput(me, fgThread, true);
      bool toTarget = target != 0 && target != me && target != fgThread && AttachThreadInput(me, target, true);
      try {
        BringWindowToTop(h);
        SetForegroundWindow(h);
      } finally {
        if (toTarget) { AttachThreadInput(me, target, false); }
        if (toFg) { AttachThreadInput(me, fgThread, false); }
      }
      Thread.Sleep(100);
    }
    return GetForegroundWindow() == h;
  }
  // A key press and release of the virtual key vk through SendInput (the keyboard's own path: WM_KEYDOWN and
  // WM_KEYUP reach the focused window's thread); the number of events Windows took, 2 when both went in.
  public static uint Press(ushort vk) {
    Input[] inputs = new Input[2];
    ushort scan = (ushort)MapVirtualKeyW(vk, 0); // MAPVK_VK_TO_VSC
    inputs[0].Type = 1; // INPUT_KEYBOARD
    inputs[0].U.Key.Vk = vk;
    inputs[0].U.Key.Scan = scan;
    inputs[1].Type = 1;
    inputs[1].U.Key.Vk = vk;
    inputs[1].U.Key.Scan = scan;
    inputs[1].U.Key.Flags = 2; // KEYEVENTF_KEYUP
    return SendInput(2, inputs, Marshal.SizeOf(typeof(Input)));
  }
}
'@
`;

/**
 * Presses Esc on the real keyboard path (SendInput) with the window hwnd (decimal) in the foreground. Not CDP's
 * Input.dispatchKeyEvent: that enters at the renderer, below WebView2's own key handling, which is what ends element
 * fullscreen on Esc (03 §3.2 W16). foreground: the window was the foreground window before the key went in (nothing
 * is sent otherwise); sent: how many of the key's two events SendInput took.
 */
export async function pressEscape(hwnd: string): Promise<{ foreground: boolean; sent: number }> {
  const out = await ps(`${KEYS_TYPE}
$h = [IntPtr]${hwnd}
if ([IsshoniCdpKeys]::Front($h)) { 'True {0}' -f [IsshoniCdpKeys]::Press(0x1B) } else { 'False 0' }`);
  const [foreground, sent] = out.split(' ');
  return { foreground: foreground === 'True', sent: Number(sent) };
}

/** A visible, unowned top-level window of the session: its HWND, its process id and the process's image name. */
export interface TopWindow {
  readonly hwnd: string;
  readonly pid: number;
  /** The process's name without ".exe" ("?" when the process is gone or can't be read). */
  readonly process: string;
}

const TOP_WINDOWS_TYPE = `
Add-Type -TypeDefinition @'
using System;
using System.Collections.Generic;
using System.Runtime.InteropServices;
public static class IsshoniCdpWindows {
  delegate bool EnumProc(IntPtr h, IntPtr p);
  [DllImport("user32.dll")] static extern bool EnumWindows(EnumProc cb, IntPtr p);
  [DllImport("user32.dll")] static extern bool IsWindowVisible(IntPtr h);
  [DllImport("user32.dll")] static extern IntPtr GetWindow(IntPtr h, uint cmd);
  [DllImport("user32.dll")] static extern uint GetWindowThreadProcessId(IntPtr h, out uint pid);
  public static string[] Visible() {
    List<string> list = new List<string>();
    EnumWindows(delegate (IntPtr h, IntPtr p) {
      // GW_OWNER = 4: owned windows (dialogs, tool windows) belong to a top-level window already listed.
      if (IsWindowVisible(h) && GetWindow(h, 4) == IntPtr.Zero) {
        uint pid;
        GetWindowThreadProcessId(h, out pid);
        list.Add(h.ToInt64().ToString() + " " + pid.ToString());
      }
      return true;
    }, IntPtr.Zero);
    return list.ToArray();
  }
}
'@
`;

/** Every visible, unowned top-level window of the session (EnumWindows), with its process's name. */
export async function topWindows(): Promise<TopWindow[]> {
  const out = await ps(`${TOP_WINDOWS_TYPE}
$rows = @(foreach ($line in [IsshoniCdpWindows]::Visible()) {
  $hwnd, $id = $line.Split(' ')
  $p = Get-Process -Id ([int]$id) -ErrorAction SilentlyContinue
  $name = '?'
  if ($null -ne $p) { $name = $p.ProcessName }
  [pscustomobject]@{ hwnd = $hwnd; pid = [int]$id; process = $name }
})
ConvertTo-Json -Compress -InputObject $rows`);
  return jsonArray<TopWindow>(out);
}

/** A process below the app (Win32_Process): the WebView2 processes and anything else the app started. */
export interface ChildProcessInfo {
  readonly pid: number;
  readonly parent: number;
  readonly name: string;
  /** The full command line; read it, never print it (it holds the user data folder's path). */
  readonly commandLine: string;
}

/** The descendants of a process, by Win32_Process's ParentProcessId links. */
export async function descendants(pid: number): Promise<ChildProcessInfo[]> {
  const out = await ps(`
$all = @(Get-CimInstance -ClassName Win32_Process | Select-Object ProcessId, ParentProcessId, Name, CommandLine)
$seen = @{ ${String(pid)} = $true }
$found = New-Object System.Collections.ArrayList
do {
  $added = 0
  foreach ($p in $all) {
    if ($seen.ContainsKey([int]$p.ParentProcessId) -and -not $seen.ContainsKey([int]$p.ProcessId)) {
      $seen[[int]$p.ProcessId] = $true
      $cl = ''
      if ($null -ne $p.CommandLine) { $cl = [string]$p.CommandLine }
      [void]$found.Add([pscustomobject]@{ pid = [int]$p.ProcessId; parent = [int]$p.ParentProcessId; name = [string]$p.Name; commandLine = $cl })
      $added++
    }
  }
} while ($added -gt 0)
ConvertTo-Json -Compress -InputObject @($found)`);
  return jsonArray<ChildProcessInfo>(out);
}

/** The WebView2 processes among a process's descendants (msedgewebview2.exe). */
export function webviewProcesses(all: readonly ChildProcessInfo[]): ChildProcessInfo[] {
  return all.filter((p) => p.name.toLowerCase() === 'msedgewebview2.exe');
}

const PEB_TYPE = `
Add-Type -TypeDefinition @'
using System;
using System.Collections.Generic;
using System.ComponentModel;
using System.Runtime.InteropServices;
using System.Text;
public static class IsshoniCdpPeb {
  [StructLayout(LayoutKind.Sequential)]
  struct BasicInfo { public IntPtr ExitStatus; public IntPtr Peb; public IntPtr Affinity; public IntPtr Priority; public IntPtr Pid; public IntPtr ParentPid; }
  [DllImport("ntdll.dll")] static extern int NtQueryInformationProcess(IntPtr h, int cls, ref BasicInfo info, int len, out int ret);
  [DllImport("kernel32.dll", SetLastError = true)] static extern IntPtr OpenProcess(uint access, bool inherit, uint pid);
  [DllImport("kernel32.dll", SetLastError = true)] static extern bool ReadProcessMemory(IntPtr h, IntPtr addr, byte[] buf, IntPtr size, out IntPtr read);
  [DllImport("kernel32.dll")] static extern bool CloseHandle(IntPtr h);
  static long ReadInt64(IntPtr h, IntPtr addr) {
    byte[] b = new byte[8];
    IntPtr n;
    if (!ReadProcessMemory(h, addr, b, (IntPtr)8, out n)) { throw new Win32Exception(); }
    return BitConverter.ToInt64(b, 0);
  }
  // The names in a 64-bit process's environment block: PEB+0x20 is ProcessParameters, whose Environment is at +0x80
  // and EnvironmentSize at +0x3F0 (x64). SetEnvironmentVariableW, which Go's os.Unsetenv calls, edits this block.
  public static string[] Names(int pid) {
    IntPtr h = OpenProcess(0x0410, false, (uint)pid); // PROCESS_QUERY_INFORMATION | PROCESS_VM_READ
    if (h == IntPtr.Zero) { throw new Win32Exception(); }
    try {
      BasicInfo info = new BasicInfo();
      int ret;
      int status = NtQueryInformationProcess(h, 0, ref info, Marshal.SizeOf(typeof(BasicInfo)), out ret);
      if (status != 0) { throw new Exception("NtQueryInformationProcess: 0x" + status.ToString("X8")); }
      IntPtr parameters = new IntPtr(ReadInt64(h, IntPtr.Add(info.Peb, 0x20)));
      IntPtr env = new IntPtr(ReadInt64(h, IntPtr.Add(parameters, 0x80)));
      long size = ReadInt64(h, IntPtr.Add(parameters, 0x3F0));
      if (size <= 0 || size > (1 << 22)) { throw new Exception("environment size " + size.ToString()); }
      byte[] buf = new byte[size];
      IntPtr n;
      if (!ReadProcessMemory(h, env, buf, (IntPtr)size, out n)) { throw new Win32Exception(); }
      string all = Encoding.Unicode.GetString(buf, 0, (int)n.ToInt64());
      List<string> names = new List<string>();
      foreach (string kv in all.Split((char)0)) {
        if (kv.Length == 0) { continue; }
        int eq = kv.IndexOf('=', 1);
        names.Add(eq > 0 ? kv.Substring(0, eq) : kv);
      }
      return names.ToArray();
    } finally {
      CloseHandle(h);
    }
  }
}
'@
`;

/**
 * The names (never the values) of the variables in a running process's environment, read from its PEB; null when
 * the process can't be opened or read (a sandboxed WebView2 child, a process that is gone).
 */
export async function environmentNames(pid: number): Promise<string[] | null> {
  const out = await ps(`${PEB_TYPE}
try { ConvertTo-Json -Compress -InputObject @([IsshoniCdpPeb]::Names(${String(pid)})) } catch { 'null' }`);
  if (out === 'null') return null;
  return jsonArray<string>(out);
}

/** Private working sets in bytes (Task Manager's "Memory (private working set)"): the app's and its WebView2's. */
export interface Memory {
  /** null when the perf provider had no row for the app (not picked up yet, or gone). */
  readonly app: number | null;
  /** The sum over the app's msedgewebview2.exe processes; null unless every one of them had a row. */
  readonly webview: number | null;
  readonly webviewProcesses: number;
  /** How many of the WebView2 processes had a row. */
  readonly webviewRead: number;
}

/** The private working set of the app and the sum of its WebView2 processes' (Win32_PerfFormattedData_PerfProc_Process). */
export async function memory(pid: number): Promise<Memory> {
  const tree = webviewProcesses(await descendants(pid));
  const ids = [pid, ...tree.map((p) => p.pid)];
  const out = await ps(`
$ids = @(${ids.map(String).join(', ')})
$rows = @(Get-CimInstance -ClassName Win32_PerfFormattedData_PerfProc_Process |
  Where-Object { $ids -contains [int]$_.IDProcess } |
  ForEach-Object { [pscustomobject]@{ pid = [int]$_.IDProcess; ws = [int64]$_.WorkingSetPrivate } })
ConvertTo-Json -Compress -InputObject $rows`);
  const rows = jsonArray<{ pid: number; ws: number }>(out);
  // A pid without a row is unreadable, never 0 bytes: the annotation must not record it as a measurement.
  const of = (id: number): number | null => rows.find((r) => r.pid === id)?.ws ?? null;
  const webview = tree.map((p) => of(p.pid));
  const read = webview.filter((ws): ws is number => ws !== null);
  return {
    app: of(pid),
    webview: tree.length > 0 && read.length === tree.length ? read.reduce((sum, ws) => sum + ws, 0) : null,
    webviewProcesses: tree.length,
    webviewRead: read.length,
  };
}

/** Bytes as MiB with one decimal, for the annotations. */
export function mib(bytes: number): string {
  return `${(bytes / (1024 * 1024)).toFixed(1)} MiB`;
}
