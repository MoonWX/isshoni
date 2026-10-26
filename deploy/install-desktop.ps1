# isshoni desktop installer for Windows. Usage: irm https://<your isshoni server>/install-desktop.ps1 | iex
# Docs: https://moonwx.github.io/isshoni/download#windows-powershell
# >>> isshoni-prefill (the server rewrites this block; see docs/m2/06-windows-release.md 7.4)
$IsshoniServer  = ''
$IsshoniVersion = '@VERSION@'
# <<< isshoni-prefill
$AllowedSigners = @'
# BEGIN ALLOWED SIGNERS (generated from deploy/keys/allowed_signers; do not edit here)
# END ALLOWED SIGNERS
'@
& {
    # docs/m2/06-windows-release.md 7.1-7.3. What the lines above hold:
    #   $IsshoniServer   the server whose Download page showed the command (written by that server, 7.4); the app
    #                    then opens set up for it (/SERVER=). Empty in the release asset.
    #   $IsshoniVersion  the version to install: stamped by `task release:prepare` (the release asset) or written by
    #                    the server (its own version). Unstamped or empty: the latest release.
    #   $AllowedSigners  the release keys that may sign checksums.txt (`task release:keys`, checked by `task
    #                    lint:keys`). Without a key this copy can verify nothing, so it installs nothing.
    #
    # Rules: Windows PowerShell 5.1 compatible, ASCII only, LF line ends. The whole body is this one block, so its
    # variables stay out of the user's session, and a download that was cut off doesn't parse and so runs nothing.
    # It never calls exit (under `irm | iex` that would close the user's window): every error is a line that starts
    # with "isshoni:", then the block returns. It writes only to a new folder $env:TEMP\isshoni-install-<guid>,
    # removed at the end.
    #
    # Environment (7.3): ISSHONI_VERSION and ISSHONI_DOWNLOAD_BASE as for install.sh; ISSHONI_NO_UPDATE_CHECKS=1,
    # ISSHONI_NO_LAUNCH=1 and ISSHONI_ALLOW_DOWNGRADE=1 pass /NOUPDATECHECKS, skip /LAUNCH and pass /ALLOWDOWNGRADE.
    Set-StrictMode -Version 3
    $ErrorActionPreference = 'Stop'
    # Windows PowerShell's progress bar makes Invoke-WebRequest many times slower.
    $ProgressPreference = 'SilentlyContinue'

    # Every line the script prints goes through here (the user reads them; they are not output objects).
    function Show-IsshoniLine {
        param([string]$Text)
        Write-Host $Text
    }

    # Test-IsshoniVersion: a version as the releases name them, 0.2.0 or 0.2.0-rc.1 (7.1 step 3). \z, not $: .NET's
    # $ also matches before a final newline.
    function Test-IsshoniVersion {
        param([string]$Version)
        return $Version -cmatch '^[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.]+)?\z'
    }

    # Get-IsshoniVersionFromUrl: the version of a release page URL (.../releases/tag/v0.2.0 is 0.2.0), or ''.
    function Get-IsshoniVersionFromUrl {
        param([string]$Url)
        $m = [regex]::Match($Url, '/tag/v([^/?#]+)')
        if (-not $m.Success) { return '' }
        $found = $m.Groups[1].Value
        if (-not (Test-IsshoniVersion -Version $found)) { return '' }
        return $found
    }

    # Get-IsshoniChecksum: the SHA-256 that a checksums.txt lists for Name, or '' unless exactly one line is
    # "<64 hex>  <Name>" or "<64 hex> *<Name>" for the whole name (7.1 step 6).
    function Get-IsshoniChecksum {
        param([string]$Text, [string]$Name)
        $found = @()
        foreach ($line in ($Text -split "`n")) {
            $m = [regex]::Match($line.TrimEnd([char]13), '^([0-9A-Fa-f]{64}) [ *](.*)\z')
            if ($m.Success -and $m.Groups[2].Value -ceq $Name) { $found += $m.Groups[1].Value }
        }
        if ($found.Count -ne 1) { return '' }
        return $found[0]
    }

    # ConvertTo-IsshoniArgument: one argument for Start-Process -ArgumentList, which joins its arguments with spaces
    # and quotes nothing, while $env:TEMP holds the user's name, which may have spaces. The value goes into double
    # quotes, by the rules the C runtime parses a command line with: a quote inside becomes \", and backslashes
    # before a quote or at the end are doubled.
    function ConvertTo-IsshoniArgument {
        param([string]$Value)
        $escaped = $Value -replace '(\\*)"', '$1$1\"'
        $escaped = $escaped -replace '(\\+)\z', '$1$1'
        return '"' + $escaped + '"'
    }

    # Find-IsshoniSshKeygen: Windows' inbox OpenSSH Client (an optional feature that Windows 10 and 11 install by
    # default; Sysnative is the same folder seen from a 32-bit PowerShell), else an ssh-keygen.exe on the PATH (Git
    # for Windows), else '' (7.2).
    function Find-IsshoniSshKeygen {
        if ($env:SystemRoot) {
            foreach ($folder in @('System32', 'Sysnative')) {
                $candidate = "$($env:SystemRoot)\$folder\OpenSSH\ssh-keygen.exe"
                if (Test-Path -LiteralPath $candidate -PathType Leaf) { return $candidate }
            }
        }
        $found = @(Get-Command -Name 'ssh-keygen.exe' -CommandType Application -ErrorAction SilentlyContinue)
        if ($found.Count -gt 0) { return [string]$found[0].Path }
        return ''
    }

    # Get-IsshoniSmartAppControl: the state of Smart App Control (1 on, 2 evaluation, 0 off), or $null when the value
    # is missing, as on Windows 10, which means off (7.1 step 1).
    function Get-IsshoniSmartAppControl {
        $key = 'HKLM:\SYSTEM\CurrentControlSet\Control\CI\Policy'
        try {
            $policy = Get-ItemProperty -Path $key -Name 'VerifiedAndReputablePolicyState' -ErrorAction SilentlyContinue
        } catch {
            return $null
        }
        if ($null -eq $policy) { return $null }
        $value = $policy.PSObject.Properties['VerifiedAndReputablePolicyState']
        if ($null -eq $value) { return $null }
        return $value.Value
    }

    # Get-IsshoniSystem: what the checks of 7.1 step 1 look at. PROCESSOR_ARCHITEW6432 is the machine's architecture
    # when a 32-bit PowerShell runs on 64-bit Windows.
    function Get-IsshoniSystem {
        $windows = [Environment]::OSVersion.Platform -eq [PlatformID]::Win32NT
        $arch = [string]$env:PROCESSOR_ARCHITEW6432
        if (-not $arch) { $arch = [string]$env:PROCESSOR_ARCHITECTURE }
        $elevated = $false
        if ($windows) {
            $identity = [Security.Principal.WindowsIdentity]::GetCurrent()
            $principal = New-Object -TypeName Security.Principal.WindowsPrincipal -ArgumentList $identity
            $elevated = $principal.IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)
        }
        return [pscustomobject]@{
            Windows   = $windows
            PSVersion = $PSVersionTable.PSVersion
            Build     = [Environment]::OSVersion.Version.Build
            Is64Bit   = [Environment]::Is64BitOperatingSystem
            Arch      = $arch
            Elevated  = $elevated
        }
    }

    # Get-IsshoniAllowedSigner: the key lines of the key block, without blank and comment lines.
    function Get-IsshoniAllowedSigner {
        param([string]$Text)
        foreach ($line in ($Text -split "`n")) {
            $trimmed = $line.Trim()
            if ($trimmed -and -not $trimmed.StartsWith('#')) { $trimmed }
        }
    }

    # Invoke-IsshoniDownload: Uri into OutFile, 3 attempts 2 s apart (7.1 step 4). Returns '' or the last error.
    function Invoke-IsshoniDownload {
        param([string]$Uri, [string]$OutFile)
        $problem = ''
        for ($attempt = 1; $attempt -le 3; $attempt++) {
            try {
                Invoke-WebRequest -Uri $Uri -OutFile $OutFile -UseBasicParsing
                return ''
            } catch {
                $problem = $_.Exception.Message
                if ($attempt -lt 3) { Start-Sleep -Seconds 2 }
            }
        }
        return $problem
    }

    # Get-IsshoniLatestVersion: the version of the latest release, from where Base/latest redirects to
    # (.../releases/tag/v<version>), as install.sh finds it; the GitHub API is not used (it is rate-limited). Throws
    # with the reason when there is none.
    function Get-IsshoniLatestVersion {
        param([string]$Base)
        $problem = ''
        for ($attempt = 1; $attempt -le 3; $attempt++) {
            try {
                $response = Invoke-WebRequest -Uri "$Base/latest" -Method Head -UseBasicParsing
                $problem = ''
                break
            } catch {
                $problem = $_.Exception.Message
                if ($attempt -lt 3) { Start-Sleep -Seconds 2 }
            }
        }
        if ($problem) { throw $problem }
        # Windows PowerShell 5.1 has an HttpWebResponse (ResponseUri), PowerShell 7 an HttpResponseMessage
        # (RequestMessage.RequestUri); both name the URL after the redirects.
        $final = ''
        $inner = $response.BaseResponse
        if ($null -ne $inner) {
            if ($inner.PSObject.Properties['ResponseUri']) {
                $final = [string]$inner.ResponseUri
            } elseif ($inner.PSObject.Properties['RequestMessage']) {
                $final = [string]$inner.RequestMessage.RequestUri
            }
        }
        $found = Get-IsshoniVersionFromUrl -Url $final
        if (-not $found) { throw "it leads to '$final', which names no release" }
        return $found
    }

    # Test-IsshoniSignature: runs `ssh-keygen -Y verify` on checksums.txt and checksums.txt.sig in Directory with the
    # Signer lines as its allowed_signers file (UTF-8 without BOM, LF), principal isshoni-release and namespace
    # isshoni-checksums (7.2). -RedirectStandardInput hands ssh-keygen the file's bytes unchanged, which a
    # PowerShell pipe would not (it re-encodes text). Returns ExitCode, Output and Error.
    function Test-IsshoniSignature {
        param([string]$SshKeygen, [string]$Directory, [string[]]$Signer)
        $allowed = Join-Path -Path $Directory -ChildPath 'allowed_signers'
        $outFile = Join-Path -Path $Directory -ChildPath 'ssh-keygen.out'
        $errFile = Join-Path -Path $Directory -ChildPath 'ssh-keygen.err'
        $utf8 = New-Object -TypeName Text.UTF8Encoding -ArgumentList $false
        [IO.File]::WriteAllText($allowed, (($Signer -join "`n") + "`n"), $utf8)
        $start = @{
            FilePath               = $SshKeygen
            ArgumentList           = @('-Y', 'verify', '-f', (ConvertTo-IsshoniArgument -Value $allowed),
                '-I', 'isshoni-release', '-n', 'isshoni-checksums',
                '-s', (ConvertTo-IsshoniArgument -Value (Join-Path -Path $Directory -ChildPath 'checksums.txt.sig')))
            RedirectStandardInput  = Join-Path -Path $Directory -ChildPath 'checksums.txt'
            RedirectStandardOutput = $outFile
            RedirectStandardError  = $errFile
            NoNewWindow            = $true
            Wait                   = $true
            PassThru               = $true
        }
        $process = Start-Process @start
        $output = ''
        if (Test-Path -LiteralPath $outFile -PathType Leaf) { $output = [IO.File]::ReadAllText($outFile) }
        $errors = ''
        if (Test-Path -LiteralPath $errFile -PathType Leaf) { $errors = [IO.File]::ReadAllText($errFile) }
        return [pscustomobject]@{ ExitCode = $process.ExitCode; Output = $output; Error = $errors }
    }

    # Get-IsshoniExitMessage: the line for an exit code of the installer other than 0 (06 6.4).
    function Get-IsshoniExitMessage {
        param([int]$Code)
        switch ($Code) {
            1 { return 'isshoni: The setup was cancelled.' }
            3 { return 'isshoni: Quit isshoni from its tray icon and run the command again.' }
            4 { return 'isshoni: A newer isshoni is installed (set ISSHONI_ALLOW_DOWNGRADE=1 to replace it).' }
            5 { return 'isshoni: This Windows version is not supported.' }
            7 {
                return 'isshoni: WebView2 could not be installed: ' +
                'https://moonwx.github.io/isshoni/troubleshooting#desktop-webview2'
            }
            8 { return 'isshoni: Another isshoni setup is running; run the command again when it has finished.' }
        }
        return "isshoni: The setup failed (exit code $Code)."
    }

    # Show-IsshoniFailure: the line of a failure before the installer ran, then "Nothing was installed.".
    function Show-IsshoniFailure {
        param([string]$Text)
        Show-IsshoniLine $Text
        Show-IsshoniLine 'Nothing was installed.'
    }

    # Install-IsshoniDesktop: the flow of 7.1. Every failure prints its "isshoni:" line (and "Nothing was installed."
    # while that is true) and returns; nothing is thrown out of it.
    function Install-IsshoniDesktop {
        param([string]$Server, [string]$StampedVersion, [string]$AllowedSigners)
        $site = 'https://moonwx.github.io/isshoni'
        $dir = ''
        $started = $false
        try {
            # 1. Checks, and Smart App Control before anything is downloaded.
            $sys = Get-IsshoniSystem
            if (-not $sys.Windows) {
                Show-IsshoniFailure ('isshoni: This script installs the isshoni app on Windows; run it in PowerShell ' +
                    'on a Windows 10 or 11 PC.')
                return
            }
            $ps = $sys.PSVersion
            if ($ps.Major -lt 5 -or ($ps.Major -eq 5 -and $ps.Minor -lt 1)) {
                Show-IsshoniFailure "isshoni: This script needs Windows PowerShell 5.1 or newer; this is $ps."
                return
            }
            if (-not $sys.Is64Bit) {
                Show-IsshoniFailure 'isshoni: isshoni needs 64-bit Windows; this PC runs 32-bit Windows.'
                return
            }
            $build = $sys.Build
            if ($build -lt 19041) {
                Show-IsshoniFailure ('isshoni: isshoni needs Windows 10 version 2004 (build 19041) or newer; this PC ' +
                    "has build $build.")
                return
            }
            if ($build -lt 19045) {
                Show-IsshoniLine ('isshoni: Note: isshoni is tested on Windows 10 22H2 (build 19045) and newer; this ' +
                    "PC has build $build.")
            }
            if ($sys.Arch -eq 'ARM64') {
                Show-IsshoniLine ('isshoni: Note: this PC has an ARM64 processor, where the x64 app runs under ' +
                    "Windows' emulation, which may be slower.")
            } elseif ($sys.Arch -ne 'AMD64') {
                Show-IsshoniFailure "isshoni: isshoni needs an x64 PC; this one reports the processor '$($sys.Arch)'."
                return
            }
            if ((Get-IsshoniSmartAppControl) -eq 1) {
                Show-IsshoniFailure ("isshoni: Smart App Control is on, and this version of the app can't run on " +
                    'your PC yet: share from Chrome or Edge instead. More: ' +
                    "$site/download#windows-smart-app-control")
                return
            }

            # What the verification needs, also before anything is downloaded (7.2).
            $signers = @(Get-IsshoniAllowedSigner -Text $AllowedSigners)
            if ($signers.Count -eq 0) {
                Show-IsshoniFailure ('isshoni: This copy of the script carries no release signing key (a development ' +
                    "copy), so it can't check a download. Use the command on your server's Download page.")
                return
            }
            $sshKeygen = Find-IsshoniSshKeygen
            if (-not $sshKeygen) {
                Show-IsshoniFailure ("isshoni: Windows' OpenSSH Client is needed to check the download (Settings > " +
                    'System > Optional features > OpenSSH Client), or download the installer from your ' +
                    "server's Download page. More: $site/download#windows-verify")
                return
            }

            # 2. TLS 1.2 for the downloads (the user's own irm has already reached the isshoni server).
            $tls12 = [Net.SecurityProtocolType]::Tls12
            [Net.ServicePointManager]::SecurityProtocol = [Net.ServicePointManager]::SecurityProtocol -bor $tls12

            # 3. The version: ISSHONI_VERSION (0.2.0, v0.2.0 or latest), else the one this copy carries, else the
            # latest release.
            $base = 'https://github.com/MoonWX/isshoni/releases'
            if ($env:ISSHONI_DOWNLOAD_BASE) { $base = ([string]$env:ISSHONI_DOWNLOAD_BASE).TrimEnd('/') }
            $version = ([string]$env:ISSHONI_VERSION).Trim()
            if (-not $version -and -not $StampedVersion.StartsWith('@')) { $version = $StampedVersion.Trim() }
            if ($version -ceq 'latest') { $version = '' }
            if ($version.StartsWith('v')) { $version = $version.Substring(1) }
            if ($version) {
                if (-not (Test-IsshoniVersion -Version $version)) {
                    Show-IsshoniFailure "isshoni: '$version' is not a version of isshoni (like 0.2.0 or latest)."
                    return
                }
            } else {
                try {
                    $version = Get-IsshoniLatestVersion -Base $base
                } catch {
                    Show-IsshoniFailure ("isshoni: Could not find out the latest version of isshoni from " +
                        "$base/latest ($($_.Exception.Message)).")
                    return
                }
            }
            $serverArgument = ''
            if ($Server) {
                if ($Server -cmatch '^https?://[!#-~]+\z') {
                    $serverArgument = "/SERVER=$Server"
                } else {
                    Show-IsshoniLine 'isshoni: Note: the server address in this script is not usable; the app will ask.'
                }
            }

            # 4. Download, into a folder of its own.
            $tempRoot = [string]$env:TEMP
            if (-not $tempRoot) { $tempRoot = [IO.Path]::GetTempPath() }
            $dir = Join-Path -Path $tempRoot -ChildPath ('isshoni-install-' + [guid]::NewGuid().ToString())
            $null = New-Item -ItemType Directory -Path $dir
            $name = "isshoni-desktop_$($version)_windows_amd64_setup.exe"
            $sums = Join-Path -Path $dir -ChildPath 'checksums.txt'
            $setup = Join-Path -Path $dir -ChildPath $name
            Show-IsshoniLine "Downloading isshoni $version"
            foreach ($file in @('checksums.txt', 'checksums.txt.sig', $name)) {
                $url = "$base/download/v$version/$file"
                $problem = Invoke-IsshoniDownload -Uri $url -OutFile (Join-Path -Path $dir -ChildPath $file)
                if ($problem) {
                    Show-IsshoniFailure "isshoni: Could not download $url ($problem)"
                    return
                }
            }

            # 5. The signature of checksums.txt.
            try {
                $verify = Test-IsshoniSignature -SshKeygen $sshKeygen -Directory $dir -Signer $signers
            } catch {
                Show-IsshoniFailure "isshoni: Could not run $sshKeygen ($($_.Exception.Message))."
                return
            }
            if ($verify.ExitCode -ne 0) {
                if ($verify.Error -match 'unknown option -- Y|illegal option|usage: ssh-keygen') {
                    Show-IsshoniFailure ("isshoni: Your ssh-keygen is older than OpenSSH 8.1, which can't check the " +
                        "download ($sshKeygen). Update Windows, or download the installer from your server's " +
                        "Download page. More: $site/download#windows-verify")
                    return
                }
                Show-IsshoniLine ('isshoni: The signature of checksums.txt is not valid: this is not a release the ' +
                    'isshoni project signed.')
                $said = @(($verify.Error + "`n" + $verify.Output) -split "`n" |
                        ForEach-Object { $_.Trim() } | Where-Object { $_ })
                if ($said.Count -gt 0) { Show-IsshoniLine "ssh-keygen says: $($said[0])" }
                Show-IsshoniFailure "If this keeps happening, see $site/security#verify-desktop"
                return
            }
            $good = @($verify.Output -split "`n" | ForEach-Object { $_.Trim() } |
                    Where-Object { $_.StartsWith('Good ') })
            if ($good.Count -gt 0) { Show-IsshoniLine $good[0] }

            # 6. The installer's SHA-256 as checksums.txt lists it.
            $want = Get-IsshoniChecksum -Text ([IO.File]::ReadAllText($sums)) -Name $name
            if (-not $want) {
                Show-IsshoniFailure "isshoni: checksums.txt does not list $name exactly once."
                return
            }
            $got = (Get-FileHash -LiteralPath $setup -Algorithm SHA256).Hash
            if (-not [string]::Equals($got, $want, [StringComparison]::OrdinalIgnoreCase)) {
                Show-IsshoniFailure ("isshoni: The checksum of $name does not match checksums.txt: the download is " +
                    'damaged or was changed.')
                return
            }

            # 7. Install. Start-Process -Wait would also wait for every process the installer starts, the app that
            # /LAUNCH opens included, so the script waits for the installer's own process only. Reading Handle right
            # away keeps the exit code readable in Windows PowerShell 5.1.
            $arguments = @('/S')
            if ($serverArgument) { $arguments += $serverArgument }
            if ($env:ISSHONI_NO_UPDATE_CHECKS -eq '1') { $arguments += '/NOUPDATECHECKS' }
            if ($env:ISSHONI_ALLOW_DOWNGRADE -eq '1') { $arguments += '/ALLOWDOWNGRADE' }
            $launch = (-not $sys.Elevated) -and ($env:ISSHONI_NO_LAUNCH -ne '1')
            if ($launch) { $arguments += '/LAUNCH' }
            Show-IsshoniLine "Installing isshoni $version"
            try {
                $process = Start-Process -FilePath $setup -ArgumentList $arguments -PassThru
            } catch {
                # Smart App Control, WDAC or AppLocker blocking the file, an antivirus quarantine, or a file that is
                # not a valid executable.
                Show-IsshoniLine "isshoni: Windows blocked the installer ($($_.Exception.Message))"
                Show-IsshoniFailure "More: $site/download#windows-smart-app-control"
                return
            }
            $started = $true
            $null = $process.Handle
            $process.WaitForExit()
            $code = $process.ExitCode
            if ($code -ne 0) {
                Show-IsshoniLine (Get-IsshoniExitMessage -Code $code)
                return
            }

            # 8. Done.
            Show-IsshoniLine "isshoni $version is installed."
            if ($launch) {
                Show-IsshoniLine 'It is starting now.'
            } else {
                Show-IsshoniLine 'Start isshoni from the Start menu.'
            }
        } catch {
            # Anything unexpected still ends as a line, not as an error record in the user's window.
            if ($started) {
                Show-IsshoniLine "isshoni: $($_.Exception.Message)"
            } else {
                Show-IsshoniFailure "isshoni: $($_.Exception.Message)"
            }
        } finally {
            if ($dir -and (Test-Path -LiteralPath $dir)) {
                Remove-Item -LiteralPath $dir -Recurse -Force -ErrorAction SilentlyContinue
            }
        }
    }

    Install-IsshoniDesktop -Server $IsshoniServer -StampedVersion $IsshoniVersion -AllowedSigners $AllowedSigners
}
