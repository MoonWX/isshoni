<#
.SYNOPSIS
Sets up the MSVC build environment of every Windows build of isshoni (docs/m2/06-windows-release.md section 3).

.DESCRIPTION
One script for ffmpeg.yml's build, every windows-native job, windows-asan, audiotest.yml's build, and
"task native:build" / "task ffmpeg:build" on a Windows PC. It uses no third-party action. It reads the "msvc" entry of
deploy/windows/tools.json ({"vs": "[17.0,19.0)", "toolset": "14.44", "windowsSdk": "10.0.26100.0"}) and:

1. finds the newest Visual Studio 2022 or 2026 instance in the "vs" version range that has the C++ x64 tools
   (vswhere -latest -products * -version <vs> -requires Microsoft.VisualStudio.Component.VC.Tools.x86.x64);
2. fails fast unless that instance has the MSVC toolset <toolset>.* (an exact minor such as 14.44; the newest build,
   such as 14.44.35207, wins) and the Windows SDK <windowsSdk> is installed; the message names what is missing and
   what it found;
3. runs vcvars64.bat -vcvars_ver=<toolset> <windowsSdk> and takes over every variable it set or changed (PATH,
   INCLUDE, LIB, LIBPATH, VCToolsVersion, WindowsSDKVersion, VSINSTALLDIR and the rest): into this process, and in
   GitHub Actions also into GITHUB_ENV for the later steps;
4. prints one line, also to the job summary: the instance's displayName and installationVersion, the toolset build
   (VCToolsVersion) and the SDK (WindowsSDKVersion). The same values go to GITHUB_OUTPUT (vs-name, vs-version,
   toolset, windows-sdk) and to MSVC_ENV_VS_NAME and MSVC_ENV_VS_VERSION, for the build manifests.

Usage (run it, don't dot-source it):

    msvc-env.ps1 [-Toolset <minor>] [-WindowsSdk <version>] [-ToolsJson <path>] [--] [<program> [<argument>...]]

    -Toolset     use this MSVC toolset (an exact minor, such as 14.44) instead of tools.json's
    -WindowsSdk  use this Windows SDK (such as 10.0.26100.0) instead of tools.json's
    -ToolsJson   read the pins from this file instead of tools.json next to this script

Without a program it sets up the PowerShell window it runs in. With one it runs the program in the environment and
exits with its exit code. Options are read only before the program (or up to "--"): every argument after the program
reaches it as given, its own options (build.sh -w, sh -c, cmake -D) included. PowerShell's -File itself passes an
argument of the form -name:value as two (-name and value), so give a program such an option without the colon
(cmake -DX=ON, not -DX:BOOL=ON).

Why the range [17.0,19.0) and not the versioned component as the only selector: GitHub's windows-2025 image has
Visual Studio 2026 (18.x) and no VS 2022 instance, with 14.44 only as the component
Microsoft.VisualStudio.Component.VC.14.44.17.14.x86.x64; VS 2022 17.14 ships 14.44 as its default VC.Tools.x86.x64
and has no such component. A PC needs VS 2022 17.14 Build Tools ("Desktop development with C++") or VS 2026 Build
Tools with "MSVC v143 - VS 2022 C++ x64/x86 build tools (v14.44)", plus the Windows 11 SDK 10.0.26100.

Exit status: 0 when the environment is set up, or the program's exit code; 1 when something is missing; 2 for a bad
command line; 127 when the program cannot be started.

.EXAMPLE
powershell -NoProfile -File deploy\windows\msvc-env.ps1 C:\msys64\usr\bin\bash.exe native/ffmpeg/build.sh -j 4

.EXAMPLE
.\deploy\windows\msvc-env.ps1 -Toolset 14.99
#>

Set-StrictMode -Version 3.0
$ErrorActionPreference = 'Stop'

# The command line is read by hand, not by a param block: PowerShell's parameter binder would take the program's own
# options (build.sh's -w as -WindowsSdk, sh's -c as -Command, by prefix, wherever they stand). Only the leading
# option pairs and an optional "--" are this script's; the rest is the program and its arguments. [string] keeps a
# number typed in PowerShell as written (14.50, not 14.5).
$Toolset = ''
$WindowsSdk = ''
$ToolsJson = ''
$next = 0
while ($next -lt $args.Count) {
    $opt = [string]$args[$next]
    if ($opt -eq '--') {
        $next++
        break
    }
    if (-not $opt.StartsWith('-')) { break }
    if ($opt -notin '-Toolset', '-WindowsSdk', '-ToolsJson') {
        [Console]::Error.WriteLine("msvc-env: unknown option $opt (the options are -Toolset, -WindowsSdk and " +
            "-ToolsJson; put -- before a program whose name starts with -)")
        exit 2
    }
    if ($next + 1 -ge $args.Count) {
        [Console]::Error.WriteLine("msvc-env: $opt needs a value")
        exit 2
    }
    $value = [string]$args[$next + 1]
    switch ($opt) {
        '-Toolset' { $Toolset = $value }
        '-WindowsSdk' { $WindowsSdk = $value }
        default { $ToolsJson = $value }
    }
    $next += 2
}
$Command = @()
if ($next -lt $args.Count) { $Command = @(foreach ($a in $args[$next..($args.Count - 1)]) { [string]$a }) }

$VcToolsComponent = 'Microsoft.VisualStudio.Component.VC.Tools.x86.x64'

function Write-MsvcLog([string]$Message) {
    [Console]::Out.WriteLine("msvc-env: $Message")
}

function Join-Found([string[]]$Items) {
    if ($null -eq $Items -or $Items.Count -eq 0) { return 'none' }
    return ($Items -join ', ')
}

function Read-MsvcPin([string]$Path) {
    if (-not (Test-Path -LiteralPath $Path -PathType Leaf)) { throw "msvc-env: $Path not found" }
    $json = Get-Content -LiteralPath $Path -Raw | ConvertFrom-Json
    if (-not ($json.PSObject.Properties.Name -contains 'msvc')) { throw "msvc-env: $Path has no ""msvc"" entry" }
    $m = $json.msvc
    $pin = @{}
    foreach ($key in 'vs', 'toolset', 'windowsSdk') {
        if (-not ($m.PSObject.Properties.Name -contains $key) -or [string]::IsNullOrEmpty([string]$m.$key)) {
            throw "msvc-env: $Path has no msvc.$key"
        }
        $pin[$key] = [string]$m.$key
    }
    return $pin
}

function Get-VsInstance([string]$Vswhere, [string]$Range) {
    $out = & $Vswhere -latest -products '*' -version $Range -requires $VcToolsComponent -format json
    if ($LASTEXITCODE -ne 0) { throw "msvc-env: vswhere failed with exit code $LASTEXITCODE" }
    # Windows PowerShell 5.1 returns a JSON array as one object: enumerate it.
    $found = @(($out | Out-String | ConvertFrom-Json) | ForEach-Object { $_ })
    if ($found.Count -gt 0) { return $found[0] }

    $all = & $Vswhere -all -products '*' -format json
    $list = @(($all | Out-String | ConvertFrom-Json) | ForEach-Object {
            "$($_.displayName) $($_.installationVersion) ($($_.installationPath))"
        })
    throw ("msvc-env: no VS 2022/2026 instance with the C++ x64 tools (vswhere -version $Range -requires " +
        "$VcToolsComponent); instances found: $(Join-Found $list)")
}

function Find-ToolsetBuild($Instance, [string]$Minor) {
    $root = Join-Path $Instance.installationPath 'VC\Tools\MSVC'
    $builds = @()
    if (Test-Path -LiteralPath $root -PathType Container) {
        $builds = @(Get-ChildItem -LiteralPath $root -Directory |
                ForEach-Object { $_.Name } |
                Where-Object { $_ -match '^\d+\.\d+\.\d+$' } |
                Sort-Object { [version]$_ })
    }
    $matching = @($builds | Where-Object { $_.StartsWith("$Minor.") })
    if ($matching.Count -eq 0) {
        $hint = 'install the MSVC build tools of that toolset'
        if ($Minor -eq '14.44') {
            $hint = "install the component Microsoft.VisualStudio.Component.VC.14.44.17.14.x86.x64 (VS 2026) or use VS 2022 17.14"
        }
        throw ("msvc-env: the MSVC toolset $Minor is not installed in $($Instance.displayName) " +
            "$($Instance.installationVersion) ($root); toolsets found: $(Join-Found $builds); $hint")
    }
    return $matching[-1]
}

function Test-WindowsSdk([string]$Version) {
    $root = Join-Path ${env:ProgramFiles(x86)} 'Windows Kits\10\Include'
    if (Test-Path -LiteralPath (Join-Path $root $Version) -PathType Container) { return }
    $have = @()
    if (Test-Path -LiteralPath $root -PathType Container) {
        $have = @(Get-ChildItem -LiteralPath $root -Directory | ForEach-Object { $_.Name } |
                Where-Object { $_ -match '^10\.0\.\d+\.\d+$' } | Sort-Object { [version]$_ })
    }
    throw "msvc-env: the Windows SDK $Version is not installed ($root\$Version); SDKs found: $(Join-Found $have)"
}

function Invoke-VcvarsBatch($Instance, [string]$Minor, [string]$Sdk) {
    $vcvars = Join-Path $Instance.installationPath 'VC\Auxiliary\Build\vcvars64.bat'
    if (-not (Test-Path -LiteralPath $vcvars -PathType Leaf)) { throw "msvc-env: $vcvars not found" }
    $marker = '__MSVC_ENV_SET__'
    # A batch file of its own, so no quoting passes through PowerShell's native argument rules; "set" after the
    # marker prints the environment vcvars64.bat left.
    $cmdFile = Join-Path ([IO.Path]::GetTempPath()) ("msvc-env-" + [guid]::NewGuid().ToString('N') + '.cmd')
    $lines = @(
        '@echo off',
        "call ""$vcvars"" -vcvars_ver=$Minor $Sdk",
        'if errorlevel 1 exit /b 1',
        "echo $marker",
        'set'
    )
    [IO.File]::WriteAllText($cmdFile, ($lines -join "`r`n") + "`r`n", (New-Object Text.ASCIIEncoding))
    try {
        $out = @(& cmd.exe /d /c $cmdFile)
        $code = $LASTEXITCODE
    } finally {
        Remove-Item -LiteralPath $cmdFile -Force -ErrorAction SilentlyContinue
    }
    $start = [array]::IndexOf($out, $marker)
    if ($code -ne 0 -or $start -lt 0 -or $start -ge $out.Count - 1) {
        $tail = ($out | Select-Object -Last 15) -join "`n"
        throw "msvc-env: vcvars64.bat -vcvars_ver=$Minor $Sdk failed (exit code $code):`n$tail"
    }
    $vars = @{}
    foreach ($line in $out[($start + 1)..($out.Count - 1)]) {
        $i = $line.IndexOf('=')
        if ($i -gt 0) { $vars[$line.Substring(0, $i)] = $line.Substring($i + 1) }
    }
    return $vars
}

function Export-MsvcEnvironment([hashtable]$After) {
    $before = @{}
    Get-ChildItem Env: | ForEach-Object { $before[$_.Name] = $_.Value }
    $names = @($After.Keys | Where-Object { -not $before.ContainsKey($_) -or $before[$_] -cne $After[$_] } | Sort-Object)
    $utf8 = New-Object Text.UTF8Encoding($false)
    foreach ($name in $names) {
        $value = $After[$name]
        [Environment]::SetEnvironmentVariable($name, $value, 'Process')
        if ($env:GITHUB_ENV) {
            $delimiter = 'ghadelimiter_' + [guid]::NewGuid().ToString('N')
            [IO.File]::AppendAllText($env:GITHUB_ENV, "$name<<$delimiter`n$value`n$delimiter`n", $utf8)
        }
    }
    return $names
}

function Write-Result([hashtable]$Values, [string]$Line) {
    $utf8 = New-Object Text.UTF8Encoding($false)
    foreach ($name in 'MSVC_ENV_VS_NAME', 'MSVC_ENV_VS_VERSION') {
        $value = $Values[$name]
        [Environment]::SetEnvironmentVariable($name, $value, 'Process')
        if ($env:GITHUB_ENV) { [IO.File]::AppendAllText($env:GITHUB_ENV, "$name=$value`n", $utf8) }
    }
    if ($env:GITHUB_OUTPUT) {
        $text = "vs-name=$($Values['MSVC_ENV_VS_NAME'])`nvs-version=$($Values['MSVC_ENV_VS_VERSION'])`n" +
        "toolset=$($Values['toolset'])`nwindows-sdk=$($Values['windows-sdk'])`n"
        [IO.File]::AppendAllText($env:GITHUB_OUTPUT, $text, $utf8)
    }
    if ($env:GITHUB_STEP_SUMMARY) {
        [IO.File]::AppendAllText($env:GITHUB_STEP_SUMMARY, "MSVC: $Line`n`n", $utf8)
    }
    Write-MsvcLog $Line
}

function Invoke-Main([string]$ToolsetOverride, [string]$SdkOverride, [string]$ToolsPath) {
    if ([string]::IsNullOrEmpty($ToolsPath)) { $ToolsPath = Join-Path $PSScriptRoot 'tools.json' }
    $pin = Read-MsvcPin $ToolsPath
    $minor = $pin['toolset']
    if ($ToolsetOverride) { $minor = $ToolsetOverride }
    $sdk = $pin['windowsSdk']
    if ($SdkOverride) { $sdk = $SdkOverride }
    if ($minor -notmatch '^\d+\.\d+$') { throw "msvc-env: toolset ""$minor"" is not an exact minor such as 14.44" }
    if ($sdk -notmatch '^10\.0\.\d+\.0$') { throw "msvc-env: Windows SDK ""$sdk"" is not a version such as 10.0.26100.0" }

    $vswhere = Join-Path ${env:ProgramFiles(x86)} 'Microsoft Visual Studio\Installer\vswhere.exe'
    if (-not (Test-Path -LiteralPath $vswhere -PathType Leaf)) {
        throw "msvc-env: no VS 2022/2026 instance with the C++ x64 tools: vswhere.exe is missing ($vswhere)"
    }
    $instance = Get-VsInstance $vswhere $pin['vs']
    $build = Find-ToolsetBuild $instance $minor
    Test-WindowsSdk $sdk

    $after = Invoke-VcvarsBatch -Instance $instance -Minor $minor -Sdk $sdk
    $vcTools = [string]$after['VCToolsVersion']
    $sdkSet = ([string]$after['WindowsSDKVersion']).TrimEnd('\')
    if (-not $vcTools.StartsWith("$minor.")) {
        throw "msvc-env: vcvars64.bat selected the toolset ""$vcTools"", not $minor ($build is installed)"
    }
    if ($sdkSet -ne $sdk) { throw "msvc-env: vcvars64.bat selected the Windows SDK ""$sdkSet"", not $sdk" }
    $names = Export-MsvcEnvironment $after

    $line = "$($instance.displayName) $($instance.installationVersion), toolset $vcTools, Windows SDK $sdkSet"
    Write-Result @{
        MSVC_ENV_VS_NAME    = [string]$instance.displayName
        MSVC_ENV_VS_VERSION = [string]$instance.installationVersion
        'toolset'           = $vcTools
        'windows-sdk'       = $sdkSet
    } $line
    Write-MsvcLog "set $($names.Count) variables: $($names -join ' ')"
}

try {
    Invoke-Main -ToolsetOverride $Toolset -SdkOverride $WindowsSdk -ToolsPath $ToolsJson
} catch {
    $message = $_.Exception.Message
    if ($env:GITHUB_ACTIONS -eq 'true') {
        $escaped = $message.Replace('%', '%25').Replace("`r", '%0D').Replace("`n", '%0A')
        [Console]::Out.WriteLine("::error::$escaped")
    }
    [Console]::Error.WriteLine($message)
    exit 1
}

if ($Command.Count -gt 0) {
    $program = $Command[0]
    $arguments = @($Command | Select-Object -Skip 1)
    # The program's output on stderr is not an error of this script (Windows PowerShell 5.1 turns it into error
    # records when the output is redirected, which 'Stop' would make fatal); its exit code says how it went.
    $ErrorActionPreference = 'Continue'
    try {
        & $program @arguments
    } catch {
        [Console]::Error.WriteLine("msvc-env: cannot run ${program}: $($_.Exception.Message)")
        exit 127
    }
    exit $LASTEXITCODE
}
exit 0
