<#
.SYNOPSIS
The engine's automated hardware run on the owner's PC (docs/m2/04-windows-engine.md, "04" below, sections 14 and
18.4; README WA76): one command gives a result bundle to attach to a pull request.

.DESCRIPTION
Runs isshoni-enginetest.exe from the tools folder (isshoni-native-tools.zip, unpacked: the tool, isshoni-media.dll
and the FFmpeg DLLs, flat) in this order, each step in a folder of its own:

  1. probe: the full probe (< 2 s, every encoder has a status), probe.json;
  2. for every encoder the probe lists as available and hardware, two capture runs of test:pattern at 1080p60:
     5 s with -keyframes 500ms (and -dump-aus 12: q's first access units, the Windows fixtures of 04 section 18.1),
     and 25 s with -bitrate-steps 8M,2M,8M (04 section 14's rate and keyframe rules);
  3. bench (30 s): with a hardware encoder on layer f, capture-to-packet p95 <= 25 ms and the engine's encode p95
     <= 16 ms (04 section 18.4 asks for encode p99 <= 16 ms; bench reports the stats' p95 only, which is a necessary
     condition, so the summary says "p95");
  4. sessions -count 5 (every session starts; fallbacks allowed);
  5. protect-check (the pattern is absent while protected, present after).

soak -hours 2 and avsync run separately (04 section 18.4, 05 section 19.3).

Every step's report (the tool's -json output, report.json, probe.json) and its console output go into the bundle,
with summary.json (each step's exit code and verdict). The video and audio streams are deleted (the bundle is for a
public pull request: reports only, plus the small access-unit fixtures). Before zipping, every text file is cleaned
of this PC's name, the user's name and domain, and the paths of the tools, the work folder, the user profile and
TEMP; a file that still names one of them stops the run without a bundle.

The bundle is hwcheck-<yyyyMMdd-HHmmss>.zip in -Out (default: the current folder).

Exit status: 0 when every step passed, 1 when a step failed (or a report still named the PC), 2 when the tools are
missing. Windows PowerShell 5.1 and PowerShell 7.

.PARAMETER Out
The folder for the bundle (default: the current folder).

.PARAMETER Tools
The folder with isshoni-enginetest.exe and the DLLs (default: this script's folder).

.PARAMETER KeepWork
Keep the work folder (under TEMP) after zipping, for a closer look.

.EXAMPLE
powershell -NoProfile -ExecutionPolicy Bypass -File hwcheck.ps1 -Out C:\results
#>
[CmdletBinding()]
param(
    [string]$Out = '',
    [string]$Tools = '',
    [switch]$KeepWork
)

Set-StrictMode -Version 3.0
$ErrorActionPreference = 'Stop'
$ProgressPreference = 'SilentlyContinue'
$InformationPreference = 'Continue'

# The thresholds of 04 section 18.4 that hwcheck applies to bench's numbers.
$script:BenchLatencyP95Ms = 25.0
$script:BenchEncodeP95Ms = 16.0
# Per-step time limits, in seconds (the runs themselves take 5, 25 and 30 s).
$script:LimitShort = 120
$script:LimitLong = 300

if ($Tools -eq '') { $Tools = $PSScriptRoot }
$script:ToolsDir = [IO.Path]::GetFullPath($Tools)
$script:Exe = Join-Path -Path $script:ToolsDir -ChildPath 'isshoni-enginetest.exe'
if ($Out -eq '') { $Out = (Get-Location).Path }
$script:OutDir = [IO.Path]::GetFullPath($Out)
$script:Steps = New-Object -TypeName System.Collections.ArrayList

function Write-Step {
    param([string]$Message)
    Write-Information -MessageData "hwcheck: $Message"
}

# One command-line argument, quoted for CreateProcess when it needs it (the arguments here have no quotes).
function ConvertTo-QuotedArgument {
    param([string]$Value)
    if ($Value -eq '' -or $Value -match '[\s"]') { return '"' + ($Value -replace '"', '\"') + '"' }
    return $Value
}

# Runs isshoni-enginetest.exe with -json and the arguments in the step's own folder (so -out . keeps every path out
# of the arguments), writes its output to stdout.txt and stderr.txt there and records the step.
function Invoke-EngineTest {
    param(
        [string]$Name,
        [string[]]$Arguments,
        [int]$LimitSeconds
    )
    $dir = Join-Path -Path $script:Work -ChildPath $Name
    $null = New-Item -ItemType Directory -Force -Path $dir
    $all = @('-json') + $Arguments
    Write-Step -Message ("{0}: isshoni-enginetest {1}" -f $Name, ($all -join ' '))
    $utf8 = New-Object -TypeName System.Text.UTF8Encoding -ArgumentList $false
    $psi = New-Object -TypeName System.Diagnostics.ProcessStartInfo
    $psi.FileName = $script:Exe
    $psi.Arguments = (($all | ForEach-Object { ConvertTo-QuotedArgument -Value $_ }) -join ' ')
    $psi.WorkingDirectory = $dir
    $psi.UseShellExecute = $false
    $psi.CreateNoWindow = $true
    $psi.RedirectStandardOutput = $true
    $psi.RedirectStandardError = $true
    # Go writes UTF-8 (bench's durations have a micro sign); the default would be the console's code page.
    $psi.StandardOutputEncoding = $utf8
    $psi.StandardErrorEncoding = $utf8
    $started = Get-Date
    $proc = [System.Diagnostics.Process]::Start($psi)
    $stdoutTask = $proc.StandardOutput.ReadToEndAsync()
    $stderrTask = $proc.StandardError.ReadToEndAsync()
    $timedOut = -not $proc.WaitForExit($LimitSeconds * 1000)
    if ($timedOut) {
        $proc.Kill()
        $proc.WaitForExit()
    }
    $code = $proc.ExitCode
    [IO.File]::WriteAllText((Join-Path -Path $dir -ChildPath 'stdout.txt'), $stdoutTask.Result, $utf8)
    [IO.File]::WriteAllText((Join-Path -Path $dir -ChildPath 'stderr.txt'), $stderrTask.Result, $utf8)
    $step = [ordered]@{
        name     = $Name
        args     = ($all -join ' ')
        exitCode = $code
        seconds  = [math]::Round(((Get-Date) - $started).TotalSeconds, 1)
        pass     = ($code -eq 0 -and -not $timedOut)
        notes    = @()
    }
    if ($timedOut) { $step.notes += "killed after $LimitSeconds s" }
    $null = $script:Steps.Add($step)
    Write-Step -Message ("{0}: exit code {1}" -f $Name, $code)
    return $step
}

# The JSON a step printed (its -json output), or $null.
function Get-StepJson {
    param([string]$Name)
    $file = Join-Path -Path (Join-Path -Path $script:Work -ChildPath $Name) -ChildPath 'stdout.txt'
    if (-not (Test-Path -LiteralPath $file -PathType Leaf)) { return $null }
    $text = [IO.File]::ReadAllText($file)
    if ($text.Trim() -eq '') { return $null }
    try { return ($text | ConvertFrom-Json) } catch { return $null }
}

# A property of a JSON object, or $null when it has none (Set-StrictMode refuses a missing property).
function Get-JsonValue {
    param($Object, [string]$Name)
    if ($null -eq $Object) { return $null }
    $prop = $Object.PSObject.Properties[$Name]
    if ($null -eq $prop) { return $null }
    return $prop.Value
}

# The available hardware encoders of probe.json, by token.
function Get-HardwareEncoder {
    param([string]$ProbeFile)
    $found = @()
    if (-not (Test-Path -LiteralPath $ProbeFile -PathType Leaf)) { return $found }
    $doc = [IO.File]::ReadAllText($ProbeFile) | ConvertFrom-Json
    foreach ($enc in @(Get-JsonValue -Object $doc -Name 'encoders')) {
        if ($null -eq $enc) { continue }
        $flags = @(Get-JsonValue -Object $enc -Name 'flags')
        if ((Get-JsonValue -Object $enc -Name 'available') -and ($flags -contains 'hardware')) {
            $name = [string](Get-JsonValue -Object $enc -Name 'name')
            if ($found -notcontains $name) { $found += $name }
        }
    }
    return $found
}

# A Go time.Duration string (as bench prints it: 850us, 12.5ms, 1.2s, 1m2s) in milliseconds, or $null.
function ConvertFrom-GoDuration {
    param([string]$Text)
    # Go prints microseconds with the micro sign (U+00B5); U+03BC is accepted too. Both become 'u' first: as keys of a
    # hash literal (case-insensitive) the two can collide ("Duplicate keys") because they share an uppercase form.
    $Text = $Text.Replace([string][char]0x00b5, 'u').Replace([string][char]0x03bc, 'u')
    if ($Text -cnotmatch '^([0-9.]+(ns|us|ms|s|m|h))+$') { return $null }
    $unit = @{ 'ns' = 1e-6; 'us' = 1e-3; 'ms' = 1.0; 's' = 1e3; 'm' = 6e4; 'h' = 3.6e6 }
    $total = 0.0
    foreach ($m in [regex]::Matches($Text, '([0-9.]+)([a-z]+)')) {
        $total += [double]::Parse($m.Groups[1].Value, [Globalization.CultureInfo]::InvariantCulture) * $unit[$m.Groups[2].Value]
    }
    return $total
}

# bench's checks of 04 section 18.4 on layer f, when its encoder is hardware.
function Test-Bench {
    param($Step)
    $rep = Get-StepJson -Name $Step.name
    if ($null -eq $rep) {
        $Step.pass = $false
        $Step.notes += 'no report'
        return
    }
    $f = @(Get-JsonValue -Object $rep -Name 'layers') | Where-Object { $null -ne $_ -and (Get-JsonValue -Object $_ -Name 'rid') -eq 'f' }
    if ($null -eq $f) {
        $Step.pass = $false
        $Step.notes += 'no layer f in the report'
        return
    }
    if (-not (Get-JsonValue -Object $f -Name 'hw')) {
        $Step.notes += ('layer f ran on {0} (software): the latency checks apply to a hardware encoder' -f (Get-JsonValue -Object $f -Name 'encoder'))
        return
    }
    $lat = ConvertFrom-GoDuration -Text ([string](Get-JsonValue -Object $f -Name 'captureToPacketP95'))
    $encMax = Get-JsonValue -Object $rep -Name 'encodeP95Max'
    $enc = ConvertFrom-GoDuration -Text ([string](Get-JsonValue -Object $encMax -Name 'f'))
    if ($null -eq $lat -or $lat -gt $script:BenchLatencyP95Ms) {
        $Step.pass = $false
        $Step.notes += ('capture-to-packet p95 {0} ms > {1} ms' -f $lat, $script:BenchLatencyP95Ms)
    } else {
        $Step.notes += ('capture-to-packet p95 {0} ms <= {1} ms' -f $lat, $script:BenchLatencyP95Ms)
    }
    if ($null -eq $enc -or $enc -gt $script:BenchEncodeP95Ms) {
        $Step.pass = $false
        $Step.notes += ('encode p95 {0} ms > {1} ms (04 section 18.4: p99 <= 16 ms)' -f $enc, $script:BenchEncodeP95Ms)
    } else {
        $Step.notes += ('encode p95 {0} ms <= {1} ms (bench reports no p99)' -f $enc, $script:BenchEncodeP95Ms)
    }
}

# What must not appear in the bundle, longest first: (pattern, replacement) pairs. Paths in their plain, JSON-escaped
# and forward-slash spellings; names of 3 characters or more as whole words.
function Get-PrivateText {
    $paths = @()
    foreach ($pair in @(@($script:Work, '<redacted:work>'), @($script:ToolsDir, '<redacted:tools>'), @($script:OutDir, '<redacted:out>'),
            @($env:USERPROFILE, '<redacted:profile>'), @([IO.Path]::GetTempPath(), '<redacted:temp>'))) {
        $p = [string]$pair[0]
        if ($p.Length -lt 4) { continue }
        $p = $p.TrimEnd('\')
        foreach ($spelling in @($p, ($p -replace '\\', '\\'), ($p -replace '\\', '/'))) {
            $paths += , @([regex]::Escape($spelling), $pair[1])
        }
    }
    $paths = @($paths | Sort-Object -Property { $_[0].Length } -Descending)
    $names = @()
    foreach ($pair in @(@($env:COMPUTERNAME, '<redacted:host>'), @($env:USERNAME, '<redacted:user>'), @($env:USERDOMAIN, '<redacted:domain>'))) {
        $n = [string]$pair[0]
        if ($n.Length -lt 3) { continue }
        $names += , @(('(?<![A-Za-z0-9])' + [regex]::Escape($n) + '(?![A-Za-z0-9])'), $pair[1])
    }
    return @($paths + $names)
}

# Cleans every text file of the work folder of Get-PrivateText's patterns, then checks that none is left; returns the
# files that still have one.
function Protect-Report {
    $patterns = Get-PrivateText
    $utf8 = New-Object -TypeName System.Text.UTF8Encoding -ArgumentList $false
    $opts = [Text.RegularExpressions.RegexOptions]::IgnoreCase
    $left = @()
    foreach ($file in @(Get-ChildItem -LiteralPath $script:Work -Recurse -File | Where-Object { $_.Extension -in @('.json', '.txt') })) {
        $text = [IO.File]::ReadAllText($file.FullName)
        foreach ($p in $patterns) { $text = [regex]::Replace($text, $p[0], $p[1], $opts) }
        [IO.File]::WriteAllText($file.FullName, $text, $utf8)
        # The check runs on the text without the replacement tokens: a name such as "User" or "host" would otherwise
        # match the token "<redacted:user>" itself (its ':' and '>' are word boundaries) and stop every run.
        # A space keeps the word boundary the token was.
        $check = [regex]::Replace($text, '<redacted:[a-z]+>', ' ')
        foreach ($p in $patterns) {
            if ([regex]::IsMatch($check, $p[0], $opts)) { $left += $file.FullName.Substring($script:Work.Length + 1) }
        }
    }
    return @($left | Select-Object -Unique)
}

if (-not (Test-Path -LiteralPath $script:Exe -PathType Leaf)) {
    Write-Step -Message 'isshoni-enginetest.exe is not in the tools folder (-Tools; default: the script''s folder)'
    exit 2
}
$script:Work = Join-Path -Path ([IO.Path]::GetTempPath()) -ChildPath ('isshoni-hwcheck-' + [guid]::NewGuid().ToString('N').Substring(0, 12))
$null = New-Item -ItemType Directory -Force -Path $script:Work
$null = New-Item -ItemType Directory -Force -Path $script:OutDir

# 1. The probe.
$null = Invoke-EngineTest -Name 'probe' -Arguments @('probe', '-out', '.') -LimitSeconds $script:LimitShort
$encoders = @(Get-HardwareEncoder -ProbeFile (Join-Path -Path (Join-Path -Path $script:Work -ChildPath 'probe') -ChildPath 'probe.json'))

# 2. capture per available hardware encoder.
if ($encoders.Count -eq 0) {
    Write-Step -Message 'the probe lists no available hardware encoder: no capture runs (04 section 18.4 runs them per hardware encoder)'
} else {
    Write-Step -Message ('hardware encoders: {0}' -f ($encoders -join ', '))
}
foreach ($enc in $encoders) {
    $null = Invoke-EngineTest -Name "capture-$enc-keyframes" -LimitSeconds $script:LimitShort -Arguments @(
        'capture', '-encoder', $enc, '-seconds', '5', '-keyframes', '500ms', '-dump-aus', '12', '-out', '.')
    $null = Invoke-EngineTest -Name "capture-$enc-steps" -LimitSeconds $script:LimitLong -Arguments @(
        'capture', '-encoder', $enc, '-seconds', '25', '-bitrate-steps', '8M,2M,8M', '-out', '.')
}

# 3. bench.
$bench = Invoke-EngineTest -Name 'bench' -Arguments @('bench') -LimitSeconds $script:LimitLong
Test-Bench -Step $bench

# 4. sessions -count 5.
$null = Invoke-EngineTest -Name 'sessions' -Arguments @('sessions', '-count', '5') -LimitSeconds $script:LimitShort

# 5. protect-check.
$null = Invoke-EngineTest -Name 'protect-check' -Arguments @('protect-check') -LimitSeconds $script:LimitShort

# The streams stay on the PC; the access-unit fixtures (aus\) are small and go with the reports.
Get-ChildItem -LiteralPath $script:Work -Recurse -File | Where-Object { $_.Extension -in @('.h264', '.ogg') -and $_.Directory.Name -ne 'aus' -and $_.Directory.Parent.Name -ne 'aus' } |
    Remove-Item -Force

$failed = @($script:Steps | Where-Object { -not $_.pass })
$summary = [ordered]@{
    tool             = 'hwcheck'
    encoders         = $encoders
    steps            = @($script:Steps)
    pass             = ($failed.Count -eq 0)
    powershell       = $PSVersionTable.PSVersion.ToString()
    osVersion        = [Environment]::OSVersion.Version.ToString()
}
$utf8 = New-Object -TypeName System.Text.UTF8Encoding -ArgumentList $false
[IO.File]::WriteAllText((Join-Path -Path $script:Work -ChildPath 'summary.json'), ($summary | ConvertTo-Json -Depth 6), $utf8)

$left = @(Protect-Report)
if ($left.Count -gt 0) {
    Write-Step -Message ('these files still name this PC, its user or a local path, so no bundle was made: {0}' -f ($left -join ', '))
    Write-Step -Message 'the work folder is kept for a look'
    exit 1
}

$zip = Join-Path -Path $script:OutDir -ChildPath ('hwcheck-{0}.zip' -f (Get-Date -Format 'yyyyMMdd-HHmmss'))
try { Add-Type -AssemblyName System.IO.Compression.FileSystem } catch { Write-Verbose -Message 'ZipFile: already loaded' }
if (Test-Path -LiteralPath $zip) { Remove-Item -LiteralPath $zip -Force }
[IO.Compression.ZipFile]::CreateFromDirectory($script:Work, $zip)
if (-not $KeepWork) { Remove-Item -LiteralPath $script:Work -Recurse -Force }

foreach ($s in $script:Steps) {
    $verdict = 'FAIL'
    if ($s.pass) { $verdict = 'pass' }
    Write-Step -Message ('{0,-34} {1,-4} (exit {2}, {3} s) {4}' -f $s.name, $verdict, $s.exitCode, $s.seconds, ($s.notes -join '; '))
}
Write-Step -Message ('bundle: {0}' -f (Split-Path -Path $zip -Leaf))
if ($failed.Count -gt 0) { exit 1 }
exit 0
