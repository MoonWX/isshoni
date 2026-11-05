<#
.SYNOPSIS
The installer's install, upgrade and uninstall assertions (docs/m2/06-windows-release.md, "06" below, sections 6
and 16).

.DESCRIPTION
-Mode ci (CI's windows-desktop job, README WA67) runs every case of 06 section 16's smoke row on a runner where
isshoni is not installed, in this order:

  1. a fresh silent install of -OldSetup (an older build whose install-files.txt lists an extra avcodec-00.dll),
     with /DESKTOPSHORTCUT=1;
  2. the upgrade to -Setup with /S /UPDATE while a helper process holds the running mutex and keeps
     isshoni-media.dll and an FFmpeg DLL mapped for 3 s after it released the mutex (the .old rename loop, 06
     section 6.7): avcodec-00.dll is gone;
  3. the installed tree against -Stage, the registry (06 section 2.3), the protocol command exactly
     "<InstallDir>\isshoni.exe" --open-url "%1", the shortcuts' AppUserModelID, --version (engineAbi 1) and
     --self-check (ok, ffmpegLoaded, testHooks false);
  4. a same-version reinstall (repair), with a /D= that an existing install ignores, which leaves no .old file;
  5. while a helper holds Local\io.isshoni.desktop.installing: a second setup exits 8 and changes no file or
     registry value, and a start through the registered protocol command exits 0 within 10 s, leaves no isshoni.exe
     running, never creates Local\io.isshoni.desktop.running and writes PendingServer and PendingRoom;
  6. during a /S /UPDATE run the installer's own process holds the installing mutex, and after it exits it is gone;
  7. while a helper keeps the list's last PE file open without FILE_SHARE_DELETE (so it can't be moved aside),
     /S /UPDATE exits 6 and puts back the files it had already renamed to .old: the tree is the stage's, with no
     .old file, and --version and --self-check still work;
  8. /SERVER=https://... writes PendingServer and drops a stale PendingRoom; a /SERVER=http:// that isn't loopback
     is ignored and the install still exits 0;
  9. a helper holding the running mutex makes /S /UPDATE exit 3 after 60 s;
 10. -NewerSetup over -Setup, then -Setup exits 4, then -Setup /ALLOWDOWNGRADE installs;
 11. the keep-data uninstall and 12. the /PURGE uninstall, each after seeding a Credential Manager entry, the toast
     keys the app writes (an AppUserModelId key whose CustomActivator names a CLSID key with LocalServer32 in
     InstallDir, and a foreign CLSID key), a NotificationCategories subkey and a Run value: no isshoni:v1: credential,
     no AUMID key, no matching CLSID key, no Software\isshoni, the foreign CLSID key kept; the data root kept by the
     first and removed by the second, which leaves nothing of 06 sections 2.2 and 2.3;
 13. /D= into a folder with a foreign file, then an uninstall: the foreign file and the folder stay.

-Mode release (verify-windows, README WA81) and -Mode update (the updater e2e, README WA88) are declared here and
filled by those slices.

Exit status 0 when every case passed; a failed case throws (exit 1). Windows PowerShell 5.1 and PowerShell 7.

.EXAMPLE
pwsh -File deploy/test/windows/smoke.ps1 -Mode ci -Setup setup.exe -Version 0.0.0-ci.5 -OldSetup old.exe -NewerSetup newer.exe -Stage dist-desktop/stage
#>
[CmdletBinding()]
param(
    [Parameter(Mandatory = $true)]
    [ValidateSet('ci', 'release', 'update')]
    [string]$Mode,
    # The installer under test and its version.
    [string]$Setup = '',
    [string]$Version = '',
    # -Mode ci: an older build whose install-files.txt lists an extra avcodec-00.dll, a newer build, and the staged
    # tree the installer under test was built from.
    [string]$OldSetup = '',
    [string]$NewerSetup = '',
    [string]$Stage = '',
    # A scratch folder (default: RUNNER_TEMP or TEMP).
    [string]$WorkDir = ''
)

Set-StrictMode -Version 3.0
$ErrorActionPreference = 'Stop'
$ProgressPreference = 'SilentlyContinue'

$script:AppKey = 'Software\isshoni'
$script:UninstallKey = 'Software\Microsoft\Windows\CurrentVersion\Uninstall\isshoni'
$script:ProtocolKey = 'Software\Classes\isshoni'
$script:RunKey = 'Software\Microsoft\Windows\CurrentVersion\Run'
$script:AumidKey = 'Software\Classes\AppUserModelId\io.isshoni.desktop'
$script:ClsidKey = 'Software\Classes\CLSID'
$script:RunningMutex = 'Local\io.isshoni.desktop.running'
$script:InstallingMutex = 'Local\io.isshoni.desktop.installing'
$script:DefaultDir = Join-Path -Path $env:LOCALAPPDATA -ChildPath 'Programs\isshoni'
$script:DataRoot = Join-Path -Path $env:LOCALAPPDATA -ChildPath 'isshoni'
$script:SetupTimeoutSec = 300
$script:Helpers = @()
# -WorkDir as the functions read it (Get-WorkDir): through $script: rather than the caller's scope, which
# PSScriptAnalyzer's PSReviewUnusedParameter doesn't follow into functions.
$script:WorkRoot = $WorkDir

function Write-Step {
    param([string]$Message)
    Write-Output "smoke: $Message"
}

function Confirm-That {
    param([bool]$Condition, [string]$Message)
    if (-not $Condition) { throw "smoke: FAILED: $Message" }
}

# ---- registry ----

function Get-RegValue {
    param([string]$Key, [string]$Name)
    $k = [Microsoft.Win32.Registry]::CurrentUser.OpenSubKey($Key)
    if ($null -eq $k) { return $null }
    try {
        return $k.GetValue($Name, $null, [Microsoft.Win32.RegistryValueOptions]::DoNotExpandEnvironmentNames)
    } finally {
        $k.Close()
    }
}

function Test-RegKey {
    param([string]$Key)
    $k = [Microsoft.Win32.Registry]::CurrentUser.OpenSubKey($Key)
    if ($null -eq $k) { return $false }
    $k.Close()
    return $true
}

function Test-RegValue {
    param([string]$Key, [string]$Name)
    $k = [Microsoft.Win32.Registry]::CurrentUser.OpenSubKey($Key)
    if ($null -eq $k) { return $false }
    try { return ($k.GetValueNames() -contains $Name) } finally { $k.Close() }
}

function Write-RegString {
    param([string]$Key, [string]$Name, [string]$Value)
    $k = [Microsoft.Win32.Registry]::CurrentUser.CreateSubKey($Key)
    try { $k.SetValue($Name, $Value, [Microsoft.Win32.RegistryValueKind]::String) } finally { $k.Close() }
}

function Clear-RegKey {
    param([string]$Key)
    [Microsoft.Win32.Registry]::CurrentUser.DeleteSubKeyTree($Key, $false)
}

function Clear-RegValue {
    param([string]$Key, [string]$Name)
    $k = [Microsoft.Win32.Registry]::CurrentUser.OpenSubKey($Key, $true)
    if ($null -eq $k) { return }
    try { $k.DeleteValue($Name, $false) } finally { $k.Close() }
}

# A text form of every value under the installer's keys, for "nothing changed" (case 5).
function Get-RegSnapshot {
    $lines = @()
    foreach ($key in @($script:AppKey, $script:UninstallKey, "$($script:ProtocolKey)\shell\open\command")) {
        $k = [Microsoft.Win32.Registry]::CurrentUser.OpenSubKey($key)
        if ($null -eq $k) { $lines += "$key <missing>"; continue }
        try {
            foreach ($name in ($k.GetValueNames() | Sort-Object)) { $lines += "$key|$name|$($k.GetValue($name))" }
        } finally {
            $k.Close()
        }
    }
    return ($lines -join "`n")
}

# ---- mutexes and processes ----

function Test-Mutex {
    param([string]$Name)
    $m = $null
    if ([System.Threading.Mutex]::TryOpenExisting($Name, [ref]$m)) {
        $m.Dispose()
        return $true
    }
    return $false
}

function Get-WorkDir {
    if ($script:WorkRoot) { $dir = $script:WorkRoot }
    elseif ($env:RUNNER_TEMP) { $dir = Join-Path -Path $env:RUNNER_TEMP -ChildPath 'smoke' }
    else { $dir = Join-Path -Path $env:TEMP -ChildPath 'isshoni-smoke' }
    $null = New-Item -ItemType Directory -Force -Path $dir
    return $dir
}

# The helper process: holds a named mutex (and optionally maps DLLs as images, the way an exiting app still maps
# them, or keeps a file open without FILE_SHARE_DELETE, which no rename gets past) until its release file appears,
# then keeps the DLLs mapped for -KeepMappedSec more seconds.
$script:HelperSource = @'
param([string]$Name, [string]$Ready, [string]$Release, [string]$Dlls, [int]$KeepMappedSec, [string]$Lock)
$ErrorActionPreference = 'Stop'
Add-Type -TypeDefinition @"
using System;
using System.Runtime.InteropServices;
public static class SmokeImage {
    [DllImport("kernel32.dll", CharSet = CharSet.Unicode, SetLastError = true)]
    public static extern IntPtr LoadLibraryExW(string path, IntPtr file, uint flags);
    [DllImport("kernel32.dll", SetLastError = true)]
    public static extern bool FreeLibrary(IntPtr module);
}
"@
$mapped = @()
if ($Dlls) {
    foreach ($dll in ($Dlls -split '\|')) {
        # DONT_RESOLVE_DLL_REFERENCES: mapped as an image, no DllMain, no imports.
        $h = [SmokeImage]::LoadLibraryExW($dll, [IntPtr]::Zero, 1)
        if ($h -eq [IntPtr]::Zero) { throw "LoadLibraryExW($dll) failed: $([Runtime.InteropServices.Marshal]::GetLastWin32Error())" }
        $mapped += $h
    }
}
$locked = $null
if ($Lock) {
    $locked = [IO.File]::Open($Lock, [IO.FileMode]::Open, [IO.FileAccess]::Read, [IO.FileShare]::ReadWrite)
}
$mutex = New-Object -TypeName System.Threading.Mutex -ArgumentList $false, $Name
Set-Content -LiteralPath $Ready -Value 'ready'
$deadline = (Get-Date).AddMinutes(10)
while (-not (Test-Path -LiteralPath $Release) -and (Get-Date) -lt $deadline) { Start-Sleep -Milliseconds 100 }
$mutex.Dispose()
if ($null -ne $locked) { $locked.Dispose() }
if ($mapped.Count -gt 0) { Start-Sleep -Seconds $KeepMappedSec }
foreach ($h in $mapped) { $null = [SmokeImage]::FreeLibrary($h) }
'@

function Invoke-Helper {
    param([string]$Name, [string[]]$Dlls = @(), [int]$KeepMappedSec = 0, [string]$Lock = '')
    $dir = Get-WorkDir
    $id = [guid]::NewGuid().ToString('N')
    $helperFile = Join-Path -Path $dir -ChildPath 'mutex-helper.ps1'
    [IO.File]::WriteAllText($helperFile, $script:HelperSource)
    $ready = Join-Path -Path $dir -ChildPath "$id.ready"
    $release = Join-Path -Path $dir -ChildPath "$id.release"
    $shell = (Get-Process -Id $PID).Path
    $arguments = @('-NoProfile', '-NonInteractive', '-ExecutionPolicy', 'Bypass', '-File', "`"$helperFile`"",
        '-Name', $Name, '-Ready', "`"$ready`"", '-Release', "`"$release`"", '-KeepMappedSec', $KeepMappedSec)
    if ($Dlls.Count -gt 0) { $arguments += @('-Dlls', "`"$($Dlls -join '|')`"") }
    if ($Lock) { $arguments += @('-Lock', "`"$Lock`"") }
    $p = Start-Process -FilePath $shell -ArgumentList $arguments -PassThru -WindowStyle Hidden
    $deadline = (Get-Date).AddSeconds(60)
    while (-not (Test-Path -LiteralPath $ready)) {
        if ($p.HasExited) { throw "smoke: the mutex helper exited with $($p.ExitCode) before it was ready" }
        if ((Get-Date) -gt $deadline) { throw 'smoke: the mutex helper was not ready within 60 s' }
        Start-Sleep -Milliseconds 100
    }
    $helper = [pscustomobject]@{ Process = $p; Release = $release; Name = $Name }
    $script:Helpers += $helper
    return $helper
}

function Stop-Helper {
    [CmdletBinding(SupportsShouldProcess = $true)]
    param($Helper, [switch]$NoWait)
    if (-not $PSCmdlet.ShouldProcess($Helper.Name, 'release')) { return }
    Set-Content -LiteralPath $Helper.Release -Value 'release'
    if ($NoWait) { return }
    if (-not $Helper.Process.WaitForExit(60000)) {
        $Helper.Process.Kill()
        throw 'smoke: the mutex helper did not exit'
    }
}

# Invoke-Setup runs an installer or uninstaller and returns its exit code; -Elapsed receives the seconds it took.
function Invoke-Setup {
    param([string]$Path, [string[]]$Arguments, [ref]$Elapsed)
    $start = Get-Date
    $p = Start-Process -FilePath $Path -ArgumentList $Arguments -PassThru
    $null = $p.Handle
    if (-not $p.WaitForExit($script:SetupTimeoutSec * 1000)) {
        $p.Kill()
        throw "smoke: $Path $($Arguments -join ' ') did not exit within $($script:SetupTimeoutSec) s"
    }
    if ($Elapsed) { $Elapsed.Value = ((Get-Date) - $start).TotalSeconds }
    return $p.ExitCode
}

function Invoke-Install {
    param([string]$Path, [string[]]$Arguments = @(), [int]$Expect = 0)
    $all = @('/S') + $Arguments
    $code = Invoke-Setup -Path $Path -Arguments $all
    Confirm-That ($code -eq $Expect) "$(Split-Path -Leaf $Path) $($all -join ' ') exited $code, not $Expect"
}

# Invoke-Uninstall runs uninstall.exe /S [...]: it copies itself into a ~nsu*.tmp folder under TEMP and exits; the
# copy does the work, so this waits until no process runs from such a folder and uninstall.exe is gone.
function Invoke-Uninstall {
    param([string]$InstallDir, [string[]]$Arguments = @())
    $uninstaller = Join-Path -Path $InstallDir -ChildPath 'uninstall.exe'
    Confirm-That (Test-Path -LiteralPath $uninstaller -PathType Leaf) "$uninstaller is missing"
    $code = Invoke-Setup -Path $uninstaller -Arguments (@('/S') + $Arguments)
    Confirm-That ($code -eq 0) "uninstall.exe exited $code"
    $deadline = (Get-Date).AddSeconds(180)
    do {
        Start-Sleep -Milliseconds 250
        $running = @(Get-CimInstance -ClassName Win32_Process | Where-Object { $_.ExecutablePath -like '*\~nsu*.tmp\*' })
        if ((Get-Date) -gt $deadline) { throw 'smoke: the uninstaller did not finish within 180 s' }
    } while ($running.Count -gt 0 -or (Test-Path -LiteralPath $uninstaller))
}

function Invoke-Captured {
    param([string]$Path, [string]$Arguments, [int]$TimeoutSec = 120)
    $psi = New-Object -TypeName System.Diagnostics.ProcessStartInfo
    $psi.FileName = $Path
    $psi.Arguments = $Arguments
    $psi.UseShellExecute = $false
    $psi.RedirectStandardOutput = $true
    $psi.CreateNoWindow = $true
    $p = [System.Diagnostics.Process]::Start($psi)
    $out = $p.StandardOutput.ReadToEndAsync()
    if (-not $p.WaitForExit($TimeoutSec * 1000)) {
        $p.Kill()
        throw "smoke: $Path $Arguments did not exit within $TimeoutSec s"
    }
    return [pscustomobject]@{ ExitCode = $p.ExitCode; Output = $out.Result }
}

# ---- what the installer wrote ----

# Get-WinVer is 06 section 5.3's four-part version (tools/desktoppkg winver).
function Get-WinVer {
    param([string]$SemVer)
    $m = [regex]::Match($SemVer, '^(\d+)\.(\d+)\.(\d+)(?:-([0-9A-Za-z.-]+))?(?:\+[0-9A-Za-z.-]+)?$')
    Confirm-That $m.Success "$SemVer is not X.Y.Z[-pre][+build]"
    $w = 65000
    if ($m.Groups[4].Success) {
        $last = ($m.Groups[4].Value -split '\.')[-1]
        if ($last -match '^\d+$') { $w = [int]([decimal]$last % 65000) } else { $w = 0 }
    }
    return '{0}.{1}.{2}.{3}' -f $m.Groups[1].Value, $m.Groups[2].Value, $m.Groups[3].Value, $w
}

function Get-FileList {
    param([string]$Dir)
    $file = Join-Path -Path $Dir -ChildPath 'install-files.txt'
    return @((Get-Content -LiteralPath $file) | Where-Object { $_ -ne '' })
}

function Get-TreeFile {
    param([string]$Dir)
    $prefix = (Resolve-Path -LiteralPath $Dir).Path.TrimEnd('\') + '\'
    return @(Get-ChildItem -LiteralPath $Dir -Recurse -File -Force | ForEach-Object { $_.FullName.Substring($prefix.Length) })
}

function Get-Sha256 {
    param([string]$Path)
    return (Get-FileHash -Algorithm SHA256 -LiteralPath $Path).Hash
}

# Test-InstalledTree: install-files.txt equals the stage's, every file it lists matches the stage's bytes, and the
# folder holds nothing else of isshoni's but install-files.txt and uninstall.exe (and, with -AllowOld, .old files).
function Test-InstalledTree {
    param([string]$InstallDir, [string]$Stage, [switch]$AllowOld)
    $list = Get-FileList -Dir $InstallDir
    $want = Get-FileList -Dir $Stage
    Confirm-That ((Compare-Object -ReferenceObject $want -DifferenceObject $list -CaseSensitive | Measure-Object).Count -eq 0) `
        "install-files.txt is not the stage's: $($list -join ', ')"
    foreach ($rel in $list) {
        $file = Join-Path -Path $InstallDir -ChildPath $rel
        Confirm-That (Test-Path -LiteralPath $file -PathType Leaf) "$rel is not installed"
        Confirm-That ((Get-Sha256 $file) -eq (Get-Sha256 (Join-Path -Path $Stage -ChildPath $rel))) "$rel differs from the stage's"
    }
    $allowed = @($list) + @('install-files.txt', 'uninstall.exe')
    foreach ($rel in (Get-TreeFile -Dir $InstallDir)) {
        if ($allowed -contains $rel) { continue }
        if ($AllowOld -and $rel.EndsWith('.old')) { continue }
        throw "smoke: FAILED: $InstallDir holds $rel, which the install doesn't list"
    }
}

function Test-InstalledRegistry {
    param([string]$InstallDir, [string]$Ver, [int]$DesktopShortcut = 1, [int]$UpdateChecks = 1)
    $exe = Join-Path -Path $InstallDir -ChildPath 'isshoni.exe'
    $checks = @(
        @($script:AppKey, 'InstallDir', $InstallDir),
        @($script:AppKey, 'Version', $Ver),
        @($script:AppKey, 'WinVer', (Get-WinVer $Ver)),
        @($script:AppKey, 'DesktopShortcut', $DesktopShortcut),
        @($script:AppKey, 'UpdateChecks', $UpdateChecks),
        @($script:UninstallKey, 'DisplayName', 'isshoni'),
        @($script:UninstallKey, 'DisplayVersion', $Ver),
        @($script:UninstallKey, 'Publisher', 'isshoni project'),
        @($script:UninstallKey, 'DisplayIcon', "`"$exe`",0"),
        @($script:UninstallKey, 'InstallLocation', $InstallDir),
        @($script:UninstallKey, 'UninstallString', "`"$InstallDir\uninstall.exe`""),
        @($script:UninstallKey, 'QuietUninstallString', "`"$InstallDir\uninstall.exe`" /S"),
        @($script:UninstallKey, 'URLInfoAbout', 'https://moonwx.github.io/isshoni/'),
        @($script:UninstallKey, 'HelpLink', 'https://moonwx.github.io/isshoni/download#windows'),
        @($script:UninstallKey, 'NoModify', 1),
        @($script:UninstallKey, 'NoRepair', 1),
        @($script:ProtocolKey, '', 'URL:isshoni'),
        @($script:ProtocolKey, 'URL Protocol', ''),
        @("$($script:ProtocolKey)\DefaultIcon", '', "`"$exe`",0"),
        @("$($script:ProtocolKey)\shell\open\command", '', "`"$exe`" --open-url `"%1`"")
    )
    foreach ($c in $checks) {
        $got = Get-RegValue -Key $c[0] -Name $c[1]
        Confirm-That ($null -ne $got -and [string]$got -ceq [string]$c[2]) "HKCU\$($c[0]) '$($c[1])' is '$got', not '$($c[2])'"
    }
    $size = Get-RegValue -Key $script:UninstallKey -Name 'EstimatedSize'
    Confirm-That ($null -ne $size -and [int]$size -gt 1000) "EstimatedSize is '$size'"
}

function Get-ShortcutAumid {
    param([string]$Lnk)
    $shell = New-Object -ComObject Shell.Application
    $folder = $shell.Namespace((Split-Path -Parent $Lnk))
    $item = $folder.ParseName((Split-Path -Leaf $Lnk))
    return [string]$item.ExtendedProperty('System.AppUserModel.ID')
}

function Test-InstalledShortcut {
    param([string]$InstallDir, [bool]$Desktop)
    $links = @(Join-Path -Path ([Environment]::GetFolderPath('Programs')) -ChildPath 'isshoni.lnk')
    $desktopLnk = Join-Path -Path ([Environment]::GetFolderPath('Desktop')) -ChildPath 'isshoni.lnk'
    if ($Desktop) { $links += $desktopLnk } else { Confirm-That (-not (Test-Path -LiteralPath $desktopLnk)) 'a desktop shortcut exists' }
    $wsh = New-Object -ComObject WScript.Shell
    foreach ($lnk in $links) {
        Confirm-That (Test-Path -LiteralPath $lnk -PathType Leaf) "$lnk is missing"
        $target = $wsh.CreateShortcut($lnk).TargetPath
        Confirm-That ($target -eq (Join-Path -Path $InstallDir -ChildPath 'isshoni.exe')) "$lnk points to $target"
        $aumid = Get-ShortcutAumid -Lnk $lnk
        Confirm-That ($aumid -ceq 'io.isshoni.desktop') "$lnk has the AppUserModelID '$aumid'"
    }
}

function Test-AppMode {
    param([string]$InstallDir, [string]$Ver)
    $exe = Join-Path -Path $InstallDir -ChildPath 'isshoni.exe'
    $r = Invoke-Captured -Path $exe -Arguments '--version'
    Confirm-That ($r.ExitCode -eq 0) "isshoni.exe --version exited $($r.ExitCode)"
    $line = @($r.Output -split "`r?`n" | Where-Object { $_ -ne '' })
    Confirm-That ($line.Count -eq 1) "--version printed $($line.Count) lines: $($r.Output)"
    $v = $line[0] | ConvertFrom-Json
    Confirm-That ($v.version -ceq $Ver) "--version says $($v.version), not $Ver"
    Confirm-That ($v.engineAbi -eq 1) "--version's engineAbi is $($v.engineAbi), not 1"

    $report = Join-Path -Path (Get-WorkDir) -ChildPath 'self-check.json'
    if (Test-Path -LiteralPath $report) { Remove-Item -LiteralPath $report -Force }
    $r = Invoke-Captured -Path $exe -Arguments "--self-check=`"$report`""
    Confirm-That (Test-Path -LiteralPath $report -PathType Leaf) "--self-check wrote no report (exit $($r.ExitCode))"
    $text = Get-Content -Raw -LiteralPath $report
    $sc = $text | ConvertFrom-Json
    Confirm-That ($sc.ok -eq $true) "--self-check is not ok: $text"
    Confirm-That ($sc.engine.ffmpegLoaded -eq $true) "--self-check: ffmpegLoaded is not true: $text"
    Confirm-That ($sc.engine.testHooks -eq $false) "--self-check: testHooks is not false: $text"
    Confirm-That ($r.ExitCode -eq 0) "--self-check exited $($r.ExitCode)"
}

# The protocol command as the registry holds it, split into the program and its arguments with %1 replaced.
function Get-ProtocolStart {
    param([string]$Link)
    $command = [string](Get-RegValue -Key "$($script:ProtocolKey)\shell\open\command" -Name '')
    $m = [regex]::Match($command, '^"([^"]+)" (.*)$')
    Confirm-That $m.Success "the protocol command '$command' is not `"<exe>`" <arguments>"
    return [pscustomobject]@{ Path = $m.Groups[1].Value; Arguments = $m.Groups[2].Value.Replace('%1', $Link) }
}

function Get-AppProcess {
    param([string]$InstallDir)
    $exe = Join-Path -Path $InstallDir -ChildPath 'isshoni.exe'
    return @(Get-CimInstance -ClassName Win32_Process -Filter "Name = 'isshoni.exe'" | Where-Object { $_.ExecutablePath -eq $exe })
}

# ---- seeds for the uninstall cases ----

$script:ToastGuid = '{6F1D7A43-2E5B-4C1E-9C55-5A1B7E6D0001}'
$script:ForeignGuid = '{6F1D7A43-2E5B-4C1E-9C55-5A1B7E6D0002}'
$script:CredTarget = 'isshoni:v1:https://smoke.invalid'

function Initialize-UninstallSeed {
    param([string]$InstallDir)
    & cmdkey.exe "/generic:$($script:CredTarget)" '/user:t' '/pass:x' | Out-Null
    Confirm-That ($LASTEXITCODE -eq 0) 'cmdkey /generic failed'
    Write-RegString -Key $script:AumidKey -Name 'DisplayName' -Value 'isshoni'
    Write-RegString -Key $script:AumidKey -Name 'CustomActivator' -Value $script:ToastGuid
    Write-RegString -Key "$($script:ClsidKey)\$($script:ToastGuid)\LocalServer32" -Name '' -Value "`"$InstallDir\isshoni.exe`" -Embedding"
    Write-RegString -Key "$($script:ClsidKey)\$($script:ForeignGuid)\LocalServer32" -Name '' -Value 'C:\elsewhere\other.exe'
    Write-RegString -Key "$($script:AppKey)\NotificationCategories" -Name 'smoke' -Value 'x'
    Write-RegString -Key $script:RunKey -Name 'isshoni' -Value "`"$InstallDir\isshoni.exe`" --autostart"
    $null = New-Item -ItemType Directory -Force -Path $script:DataRoot
    Set-Content -LiteralPath (Join-Path -Path $script:DataRoot -ChildPath 'settings.json') -Value '{"schema":1}'
}

function Test-Uninstalled {
    param([string]$InstallDir, [bool]$Purged)
    $creds = (& cmdkey.exe /list) -join "`n"
    Confirm-That (-not $creds.Contains('isshoni:v1:')) 'an isshoni:v1: credential is left in Credential Manager'
    Confirm-That (-not (Test-RegKey $script:AumidKey)) 'the AppUserModelId key is left'
    Confirm-That (-not (Test-RegKey "$($script:ClsidKey)\$($script:ToastGuid)")) 'the toast activator CLSID key is left'
    Confirm-That (Test-RegKey "$($script:ClsidKey)\$($script:ForeignGuid)") 'the foreign CLSID key was deleted'
    Clear-RegKey "$($script:ClsidKey)\$($script:ForeignGuid)"
    Confirm-That (-not (Test-RegKey $script:AppKey)) 'HKCU\Software\isshoni is left'
    Confirm-That (-not (Test-RegKey $script:UninstallKey)) 'the Uninstall key is left'
    Confirm-That (-not (Test-RegKey $script:ProtocolKey)) 'the isshoni: protocol key is left'
    Confirm-That (-not (Test-RegValue $script:RunKey 'isshoni')) 'the Run value is left'
    foreach ($lnk in @((Join-Path -Path ([Environment]::GetFolderPath('Programs')) -ChildPath 'isshoni.lnk'),
            (Join-Path -Path ([Environment]::GetFolderPath('Desktop')) -ChildPath 'isshoni.lnk'))) {
        Confirm-That (-not (Test-Path -LiteralPath $lnk)) "$lnk is left"
    }
    Confirm-That (-not (Test-Path -LiteralPath $InstallDir)) "$InstallDir is left"
    if ($Purged) {
        Confirm-That (-not (Test-Path -LiteralPath $script:DataRoot)) "the data root $($script:DataRoot) is left after /PURGE"
    } else {
        Confirm-That (Test-Path -LiteralPath (Join-Path -Path $script:DataRoot -ChildPath 'settings.json')) 'the data root was removed without /PURGE'
    }
}

# ---- the modes ----

# The parameters come in explicitly (not through the script's scope), so each function says what it reads.
function Invoke-CiMode {
    param([string]$Setup, [string]$Version, [string]$OldSetup, [string]$NewerSetup, [string]$Stage)
    foreach ($p in @(@('-Setup', $Setup), @('-Version', $Version), @('-OldSetup', $OldSetup), @('-NewerSetup', $NewerSetup), @('-Stage', $Stage))) {
        if (-not $p[1]) { throw "smoke: -Mode ci needs $($p[0])" }
    }
    foreach ($f in @($Setup, $OldSetup, $NewerSetup)) { Confirm-That (Test-Path -LiteralPath $f -PathType Leaf) "$f is missing" }
    $script:SetupPath = (Resolve-Path -LiteralPath $Setup).Path
    $old = (Resolve-Path -LiteralPath $OldSetup).Path
    $newer = (Resolve-Path -LiteralPath $NewerSetup).Path
    $dir = $script:DefaultDir
    Confirm-That (-not (Test-RegKey $script:AppKey)) 'isshoni is installed already: -Mode ci needs a PC without it'
    Confirm-That (-not (Test-Path -LiteralPath $dir)) "$dir exists already"

    Write-Step '1. fresh silent install of the older build'
    Invoke-Install -Path $old -Arguments @('/DESKTOPSHORTCUT=1')
    Confirm-That ((Get-RegValue $script:AppKey 'InstallDir') -eq $dir) "InstallDir is not $dir"
    Confirm-That ((Get-FileList -Dir $dir) -contains 'avcodec-00.dll') "the older build's install-files.txt doesn't list avcodec-00.dll"
    Confirm-That (Test-Path -LiteralPath (Join-Path -Path $dir -ChildPath 'avcodec-00.dll')) 'avcodec-00.dll is not installed'

    Write-Step '2. upgrade while the exiting app still maps isshoni-media.dll and an FFmpeg DLL'
    $ffmpeg = @(Get-FileList -Dir $Stage | Where-Object { $_ -like 'avcodec-*.dll' })[0]
    $helper = Invoke-Helper -Name $script:RunningMutex -KeepMappedSec 3 `
        -Dlls @((Join-Path -Path $dir -ChildPath 'isshoni-media.dll'), (Join-Path -Path $dir -ChildPath $ffmpeg))
    $setupProcess = Start-Process -FilePath $script:SetupPath -ArgumentList @('/S', '/UPDATE') -PassThru
    $null = $setupProcess.Handle
    Start-Sleep -Seconds 2
    Confirm-That (-not $setupProcess.HasExited) 'the upgrade did not wait for the running mutex'
    Stop-Helper -Helper $helper -NoWait
    Confirm-That ($setupProcess.WaitForExit($script:SetupTimeoutSec * 1000)) 'the upgrade did not finish'
    Confirm-That ($setupProcess.ExitCode -eq 0) "the upgrade over mapped DLLs exited $($setupProcess.ExitCode), not 0"
    Stop-Helper -Helper $helper
    Confirm-That (-not (Test-Path -LiteralPath (Join-Path -Path $dir -ChildPath 'avcodec-00.dll'))) 'the stale avcodec-00.dll is left'
    Confirm-That (-not ((Get-FileList -Dir $dir) -contains 'avcodec-00.dll')) 'install-files.txt still lists avcodec-00.dll'

    Write-Step '3. files, registry, protocol command, shortcuts, --version and --self-check'
    Test-InstalledTree -InstallDir $dir -Stage $Stage -AllowOld
    Test-InstalledRegistry -InstallDir $dir -Ver $Version
    Test-InstalledShortcut -InstallDir $dir -Desktop $true
    Test-AppMode -InstallDir $dir -Ver $Version

    Write-Step '4. same-version reinstall (repair); /D= is ignored while an install exists'
    $other = Join-Path -Path (Get-WorkDir) -ChildPath 'ignored dir'
    Invoke-Install -Path $script:SetupPath -Arguments @("/D=$other")
    Confirm-That (-not (Test-Path -LiteralPath $other)) '/D= was not ignored on a reinstall'
    Test-InstalledTree -InstallDir $dir -Stage $Stage
    Test-InstalledRegistry -InstallDir $dir -Ver $Version

    Write-Step '5. another setup and a link start while the installing mutex is held'
    $files = (Get-TreeFile -Dir $dir | Sort-Object | ForEach-Object {
            $i = Get-Item -LiteralPath (Join-Path -Path $dir -ChildPath $_); "$_|$($i.Length)|$($i.LastWriteTimeUtc.Ticks)" }) -join "`n"
    $reg = Get-RegSnapshot
    $helper = Invoke-Helper -Name $script:InstallingMutex
    Invoke-Install -Path $script:SetupPath -Expect 8
    $files2 = (Get-TreeFile -Dir $dir | Sort-Object | ForEach-Object {
            $i = Get-Item -LiteralPath (Join-Path -Path $dir -ChildPath $_); "$_|$($i.Length)|$($i.LastWriteTimeUtc.Ticks)" }) -join "`n"
    Confirm-That ($files2 -ceq $files) 'a setup that exited 8 changed files'
    Confirm-That ((Get-RegSnapshot) -ceq $reg) 'a setup that exited 8 changed the registry'
    $start = Get-ProtocolStart -Link 'isshoni://smoke.invalid/r/abc'
    $psi = New-Object -TypeName System.Diagnostics.ProcessStartInfo
    $psi.FileName = $start.Path
    $psi.Arguments = $start.Arguments
    $psi.UseShellExecute = $false
    $app = [System.Diagnostics.Process]::Start($psi)
    $sawRunning = $false
    $deadline = (Get-Date).AddSeconds(10)
    while (-not $app.HasExited -and (Get-Date) -lt $deadline) {
        if (Test-Mutex $script:RunningMutex) { $sawRunning = $true }
        Start-Sleep -Milliseconds 100
    }
    if (-not $app.HasExited) {
        $app.Kill()
        throw 'smoke: FAILED: a link start during an install did not exit within 10 s'
    }
    Confirm-That ($app.ExitCode -eq 0) "a link start during an install exited $($app.ExitCode), not 0"
    Confirm-That (-not $sawRunning) 'a link start during an install created the running mutex'
    Confirm-That (-not (Test-Mutex $script:RunningMutex)) 'the running mutex exists after the link start'
    Confirm-That ((Get-AppProcess -InstallDir $dir).Count -eq 0) 'an isshoni.exe is left running'
    Confirm-That ((Get-RegValue $script:AppKey 'PendingServer') -ceq 'https://smoke.invalid') "PendingServer is '$(Get-RegValue $script:AppKey 'PendingServer')'"
    Confirm-That ((Get-RegValue $script:AppKey 'PendingRoom') -ceq 'abc') "PendingRoom is '$(Get-RegValue $script:AppKey 'PendingRoom')'"
    Clear-RegValue $script:AppKey 'PendingServer'
    Clear-RegValue $script:AppKey 'PendingRoom'
    Stop-Helper -Helper $helper

    Write-Step "6. a /S /UPDATE run holds the installing mutex for its whole run"
    Confirm-That (-not (Test-Mutex $script:InstallingMutex)) 'the installing mutex exists before the run'
    $setupProcess = Start-Process -FilePath $script:SetupPath -ArgumentList @('/S', '/UPDATE') -PassThru
    $null = $setupProcess.Handle
    $held = $false
    while (-not $setupProcess.HasExited) {
        if (Test-Mutex $script:InstallingMutex) { $held = $true }
        Start-Sleep -Milliseconds 20
    }
    Confirm-That ($setupProcess.ExitCode -eq 0) "the /S /UPDATE run exited $($setupProcess.ExitCode)"
    Confirm-That $held 'the installer did not hold Local\io.isshoni.desktop.installing'
    Confirm-That (-not (Test-Mutex $script:InstallingMutex)) 'the installing mutex is left after the installer exited'

    Write-Step '7. a PE file that cannot be moved aside: exit 6, and the files already moved are put back'
    $images = @(Get-FileList -Dir $dir | Where-Object { $_ -like '*.dll' -or $_ -like '*.exe' })
    Confirm-That ($images.Count -ge 2) "install-files.txt lists $($images.Count) PE files, too few for this case"
    $locked = Join-Path -Path $dir -ChildPath $images[-1]
    $helper = Invoke-Helper -Name 'Local\io.isshoni.smoke.lock' -Lock $locked
    $elapsed = 0.0
    $code = Invoke-Setup -Path $script:SetupPath -Arguments @('/S', '/UPDATE') -Elapsed ([ref]$elapsed)
    Stop-Helper -Helper $helper
    Confirm-That ($code -eq 6) "/S /UPDATE with $($images[-1]) locked exited $code, not 6"
    Confirm-That ($elapsed -ge 9) "/S /UPDATE gave up on the locked file after $([int]$elapsed) s, not 10"
    Test-InstalledTree -InstallDir $dir -Stage $Stage
    Test-InstalledRegistry -InstallDir $dir -Ver $Version
    Test-AppMode -InstallDir $dir -Ver $Version

    Write-Step '8. /SERVER=: PendingServer written, a stale PendingRoom dropped; a non-loopback http:// ignored'
    Write-RegString -Key $script:AppKey -Name 'PendingRoom' -Value 'stale'
    Invoke-Install -Path $script:SetupPath -Arguments @('/SERVER=https://smoke.invalid')
    Confirm-That ((Get-RegValue $script:AppKey 'PendingServer') -ceq 'https://smoke.invalid') "/SERVER=: PendingServer is '$(Get-RegValue $script:AppKey 'PendingServer')'"
    Confirm-That (-not (Test-RegValue $script:AppKey 'PendingRoom')) '/SERVER= left the stale PendingRoom'
    Clear-RegValue $script:AppKey 'PendingServer'
    Invoke-Install -Path $script:SetupPath -Arguments @('/SERVER=http://smoke.invalid')
    Confirm-That (-not (Test-RegValue $script:AppKey 'PendingServer')) 'a non-loopback /SERVER=http:// was written'
    Test-InstalledRegistry -InstallDir $dir -Ver $Version

    Write-Step '9. a running app makes /S /UPDATE exit 3 after 60 s'
    $helper = Invoke-Helper -Name $script:RunningMutex
    $elapsed = 0.0
    $code = Invoke-Setup -Path $script:SetupPath -Arguments @('/S', '/UPDATE') -Elapsed ([ref]$elapsed)
    Stop-Helper -Helper $helper
    Confirm-That ($code -eq 3) "/S /UPDATE with isshoni running exited $code, not 3"
    Confirm-That ($elapsed -ge 55) "/S /UPDATE gave up after $([int]$elapsed) s, not 60"

    Write-Step '10. downgrade: exit 4, then /ALLOWDOWNGRADE'
    Invoke-Install -Path $newer
    $newerVersion = [string](Get-RegValue $script:AppKey 'Version')
    Confirm-That ($newerVersion -cne $Version) 'the newer build did not change Version'
    Invoke-Install -Path $script:SetupPath -Expect 4
    Confirm-That ((Get-RegValue $script:AppKey 'Version') -ceq $newerVersion) 'a refused downgrade changed Version'
    Invoke-Install -Path $script:SetupPath -Arguments @('/ALLOWDOWNGRADE')
    Test-InstalledRegistry -InstallDir $dir -Ver $Version
    Test-InstalledTree -InstallDir $dir -Stage $Stage

    Write-Step '11. uninstall keeping the data: sign-ins and toast keys gone, the data root kept'
    Initialize-UninstallSeed -InstallDir $dir
    Invoke-Uninstall -InstallDir $dir
    Test-Uninstalled -InstallDir $dir -Purged $false

    Write-Step '12. uninstall with /PURGE: the data root gone too, nothing of 06 sections 2.2 and 2.3 left'
    Invoke-Install -Path $script:SetupPath
    Initialize-UninstallSeed -InstallDir $dir
    Invoke-Uninstall -InstallDir $dir -Arguments @('/PURGE')
    Test-Uninstalled -InstallDir $dir -Purged $true

    Write-Step '13. /D= into a folder with a foreign file, then uninstall: the foreign file and the folder stay'
    $custom = Join-Path -Path (Get-WorkDir) -ChildPath 'custom dir'
    $null = New-Item -ItemType Directory -Force -Path $custom
    $foreign = Join-Path -Path $custom -ChildPath 'foreign.txt'
    Set-Content -LiteralPath $foreign -Value 'not isshoni'
    Invoke-Install -Path $script:SetupPath -Arguments @("/D=$custom")
    Confirm-That ((Get-RegValue $script:AppKey 'InstallDir') -eq $custom) "/D= installed into $(Get-RegValue $script:AppKey 'InstallDir')"
    foreach ($rel in (Get-FileList -Dir $Stage)) {
        Confirm-That (Test-Path -LiteralPath (Join-Path -Path $custom -ChildPath $rel) -PathType Leaf) "/D=: $rel is not installed"
    }
    Invoke-Uninstall -InstallDir $custom -Arguments @('/PURGE')
    Confirm-That (Test-Path -LiteralPath $foreign -PathType Leaf) 'the foreign file was deleted'
    $left = @(Get-ChildItem -LiteralPath $custom -Recurse -Force)
    Confirm-That ($left.Count -eq 1) "the custom folder holds more than the foreign file: $($left.Name -join ', ')"
    Confirm-That (-not (Test-RegKey $script:AppKey)) 'HKCU\Software\isshoni is left'

    Write-Step 'every -Mode ci case passed'
}

try {
    switch ($Mode) {
        'ci' { Invoke-CiMode -Setup $Setup -Version $Version -OldSetup $OldSetup -NewerSetup $NewerSetup -Stage $Stage }
        'release' { throw 'smoke: -Mode release is not implemented yet (README WA81, verify-windows)' }
        'update' { throw 'smoke: -Mode update is not implemented yet (README WA88, the updater e2e)' }
    }
} finally {
    foreach ($h in $script:Helpers) {
        if (-not $h.Process.HasExited) {
            Set-Content -LiteralPath $h.Release -Value 'release'
            if (-not $h.Process.WaitForExit(10000)) { $h.Process.Kill() }
        }
    }
}
