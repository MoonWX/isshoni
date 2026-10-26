# Pester 5 tests of deploy/install-desktop.ps1 (docs/m2/06-windows-release.md 7.1, 7.2 and 16). `task test:ps` runs
# them under pwsh (CI ubuntu); CI's windows-desktop job runs them under Windows PowerShell 5.1 as well, so this file
# keeps to 5.1 too and is ASCII only.
#
# Nothing here uses the network or installs anything. BeforeAll defines every function of the script's body in this
# file's scope (they are taken from its syntax tree), so each one is called and mocked on its own: the system probes,
# the downloads, ssh-keygen and the installer are mocks. Two tests run the whole script as `irm | iex` does. The
# ssh-keygen tests run the real ssh-keygen (OpenSSH 8.1 or newer) when there is one, on a checksums.txt signed once
# with a throwaway key whose private half was deleted right after; only its public key and the signature are here.

BeforeAll {
    $ScriptPath = [IO.Path]::GetFullPath([IO.Path]::Combine($PSScriptRoot, '..', 'install-desktop.ps1'))
    $ScriptText = [IO.File]::ReadAllText($ScriptPath)
    $ScriptLines = @($ScriptText -split "`n" | ForEach-Object { $_.TrimEnd([char]13) })
    $parseTokens = $null
    $parseErrors = $null
    $ScriptAst = [System.Management.Automation.Language.Parser]::ParseFile($ScriptPath, [ref]$parseTokens,
        [ref]$parseErrors)
    $ScriptParseErrors = @($parseErrors)
    $definitions = $ScriptAst.FindAll({
            param($node)
            $node -is [System.Management.Automation.Language.FunctionDefinitionAst]
        }, $true)
    foreach ($definition in $definitions) {
        . ([scriptblock]::Create($definition.Extent.Text))
    }

    $OnWindows = [Environment]::OSVersion.Platform -eq [PlatformID]::Win32NT
    $Site = 'https://moonwx.github.io/isshoni'
    $Nothing = 'Nothing was installed.'
    $InboxSshKeygen = 'C:\Windows\System32\OpenSSH\ssh-keygen.exe'
    $EnvNames = @('ISSHONI_VERSION', 'ISSHONI_DOWNLOAD_BASE', 'ISSHONI_NO_UPDATE_CHECKS', 'ISSHONI_NO_LAUNCH',
        'ISSHONI_ALLOW_DOWNGRADE', 'TEMP', 'SystemRoot')

    # The throwaway key (comment pester-test-key) and another one that signed nothing. ssh-keygen prints the first
    # one's fingerprint as SHA256:qVOf8BPK4qDyoPqg3Hb+8X3Xn+s0pieT/YdeO6tz0sY.
    $TestKey = 'ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIDPtbfD0H9PP3t1PxsryhB+JOFH+JgKTRYj3EFSLGM06'
    $OtherKey = 'ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIKMYB7rw3KSnonCgKziAUhHMDxxwliPhONRwhdTpxJ6u'
    $TestSignerLine = 'isshoni-release namespaces="isshoni-checksums" ' + $TestKey + ' pester-test-key'
    $OtherSignerLine = 'isshoni-release namespaces="isshoni-checksums" ' + $OtherKey + ' pester-other-key'
    # A key block as the script carries it, markers and a blank line included, with CRLF line ends as a Windows
    # checkout would have them.
    $TestSigners = "# BEGIN ALLOWED SIGNERS (test)`r`n`r`n$TestSignerLine`r`n# END ALLOWED SIGNERS"
    # What the throwaway key signed (namespace isshoni-checksums), built here so that the line ends of this file's
    # checkout don't matter. The installer line's hash is the SHA-256 of "isshoni test installer`n".
    $TestChecksums = '10fcf1268efece33a7dee0de91cb59a8c37232952686ca63bfc6a5bbc63d489c  ' +
    "isshoni_0.2.0_linux_amd64.tar.gz`n" +
    'bad14730fda9afb8c653c763a69e6c05b788cb1d08acda627848f8864152c67b  ' +
    "isshoni-desktop_0.2.0_windows_amd64_setup.exe`n"
    $TestSignature = (@(
            '-----BEGIN SSH SIGNATURE-----',
            'U1NIU0lHAAAAAQAAADMAAAALc3NoLWVkMjU1MTkAAAAgM+1t8PQf08/e3U/GyvKEH4k4Uf',
            '4mApNFiPcQVIsYzToAAAARaXNzaG9uaS1jaGVja3N1bXMAAAAAAAAABnNoYTUxMgAAAFMA',
            'AAALc3NoLWVkMjU1MTkAAABAZIgbOwL3/X2BEvsTLcuTrAh2f28v5RKafamThujW0y9+nS',
            'coKfJvAfNzCqsRz2gUtydOE3pqjd4eD1F3DUBIDQ==',
            '-----END SSH SIGNATURE-----'
        ) -join "`n") + "`n"
    $GoodLine = 'Good "isshoni-checksums" signature for isshoni-release with ED25519 key ' +
    'SHA256:qVOf8BPK4qDyoPqg3Hb+8X3Xn+s0pieT/YdeO6tz0sY'

    function Get-TestSha256 {
        param([byte[]]$Bytes)
        $sha = [Security.Cryptography.SHA256]::Create()
        try {
            return ([BitConverter]::ToString($sha.ComputeHash($Bytes)) -replace '-', '').ToLowerInvariant()
        } finally {
            $sha.Dispose()
        }
    }

    # New-FakeRelease: the three files of a release as the download mock serves them, by file name.
    function New-FakeRelease {
        param([string]$Version = '0.2.0')
        $setupName = "isshoni-desktop_$($Version)_windows_amd64_setup.exe"
        $setupBytes = [Text.Encoding]::ASCII.GetBytes("isshoni test installer $Version`n")
        $sums = "0000000000000000000000000000000000000000000000000000000000000000  isshoni_$($Version)_linux_amd64.tar.gz`n" +
        "$(Get-TestSha256 -Bytes $setupBytes)  $setupName`n"
        return @{
            'checksums.txt'     = [Text.Encoding]::ASCII.GetBytes($sums)
            'checksums.txt.sig' = [Text.Encoding]::ASCII.GetBytes("-----BEGIN SSH SIGNATURE-----`nAAAA`n-----END SSH SIGNATURE-----`n")
            $setupName          = $setupBytes
        }
    }

    # New-FakeProcess: what Start-Process -PassThru returns, as far as the script uses it.
    function New-FakeProcess {
        param([int]$ExitCode)
        $fake = New-Object -TypeName PSObject -Property @{ ExitCode = $ExitCode; Handle = [IntPtr]::Zero }
        Add-Member -InputObject $fake -MemberType ScriptMethod -Name WaitForExit -Value { }
        return $fake
    }

    # Invoke-Install: Install-IsshoniDesktop with the preferences the script sets, returning the lines it printed.
    # Anything else it emits, an error record above all, fails the test.
    function Invoke-Install {
        param([string]$Server = '', [string]$StampedVersion = '0.2.0', [string]$Signers = $TestSigners)
        $ErrorActionPreference = 'Stop'
        $ProgressPreference = 'SilentlyContinue'
        $records = @(Install-IsshoniDesktop -Server $Server -StampedVersion $StampedVersion -AllowedSigners $Signers 2>&1 6>&1)
        foreach ($record in $records) {
            if ($record -is [System.Management.Automation.ErrorRecord]) { throw "an error record escaped: $record" }
            if ($record -isnot [System.Management.Automation.InformationRecord]) { throw "unexpected output: $record" }
            [string]$record
        }
    }

    # Invoke-WholeScript: the script's text through Invoke-Expression in a scope of its own, as `irm ... | iex`
    # runs it, returning the lines it printed.
    function Invoke-WholeScript {
        $records = @(& { Invoke-Expression $ScriptText } 2>&1 6>&1)
        foreach ($record in $records) {
            if ($record -is [System.Management.Automation.ErrorRecord]) { throw "an error record escaped: $record" }
            if ($record -isnot [System.Management.Automation.InformationRecord]) { throw "unexpected output: $record" }
            [string]$record
        }
    }

    # Get-TestSshKeygen: a real ssh-keygen, or '' (the tests that need one are then skipped).
    function Get-TestSshKeygen {
        $inbox = ''
        if ($env:SystemRoot) { $inbox = "$($env:SystemRoot)\System32\OpenSSH\ssh-keygen.exe" }
        if ($inbox -and (Test-Path -LiteralPath $inbox -PathType Leaf)) { return $inbox }
        $found = @(Get-Command -Name 'ssh-keygen' -CommandType Application -ErrorAction SilentlyContinue)
        if ($found.Count -gt 0) { return [string]$found[0].Path }
        return ''
    }

    function Save-TestEnvironment {
        $saved = @{}
        foreach ($n in $EnvNames) { $saved[$n] = [Environment]::GetEnvironmentVariable($n) }
        foreach ($n in $EnvNames) {
            if ($n.StartsWith('ISSHONI_')) { [Environment]::SetEnvironmentVariable($n, $null) }
        }
        return $saved
    }

    function Restore-TestEnvironment {
        param([hashtable]$Saved)
        foreach ($n in $EnvNames) { [Environment]::SetEnvironmentVariable($n, $Saved[$n]) }
    }

    # New-TestTemp: an empty folder that stands in for $env:TEMP.
    function New-TestTemp {
        $path = Join-Path -Path $TestDrive -ChildPath ('temp-' + [guid]::NewGuid().ToString())
        $null = New-Item -ItemType Directory -Path $path
        [Environment]::SetEnvironmentVariable('TEMP', $path)
        return $path
    }

    function Get-LeftoverFolder {
        param([string]$Temp)
        return @(Get-ChildItem -LiteralPath $Temp -Filter 'isshoni-install-*' -Force)
    }
}

Describe 'install-desktop.ps1 itself' {
    It 'parses without errors' {
        ($ScriptParseErrors | ForEach-Object { $_.Message }) -join "`n" | Should -BeNullOrEmpty
    }

    It 'is ASCII only' {
        $bytes = [IO.File]::ReadAllBytes($ScriptPath)
        $bad = @(for ($i = 0; $i -lt $bytes.Length; $i++) {
                $b = $bytes[$i]
                if ($b -gt 126 -or ($b -lt 32 -and $b -ne 9 -and $b -ne 10 -and $b -ne 13)) { $i }
            })
        $bad.Count | Should -Be 0 -Because "byte offsets $($bad -join ', ') are not ASCII text"
    }

    It 'never calls exit (under irm | iex it would close the window)' {
        $exits = @($ScriptAst.FindAll({
                    param($node)
                    $node -is [System.Management.Automation.Language.ExitStatementAst]
                }, $true))
        $exits.Count | Should -Be 0
        $ScriptText | Should -Not -Match '\[(System\.)?Environment\]::Exit|SetShouldExit|Exit-PS'
    }

    It 'starts with the usage line and the docs link' {
        $ScriptLines[0] | Should -BeExactly ('# isshoni desktop installer for Windows. Usage: irm ' +
            'https://<your isshoni server>/install-desktop.ps1 | iex')
        $ScriptLines[1] | Should -BeExactly '# Docs: https://moonwx.github.io/isshoni/download#windows-powershell'
    }

    It 'has the prefill block that the server rewrites and release:prepare stamps' {
        $open = @($ScriptLines | Where-Object { $_.StartsWith('# >>> isshoni-prefill') })
        $close = @($ScriptLines | Where-Object { $_.StartsWith('# <<< isshoni-prefill') })
        $open.Count | Should -Be 1
        $close.Count | Should -Be 1
        $at = [array]::IndexOf($ScriptLines, $open[0])
        $ScriptLines[$at + 1] | Should -BeExactly "`$IsshoniServer  = ''"
        $ScriptLines[$at + 2] | Should -BeExactly "`$IsshoniVersion = '@VERSION@'"
        $ScriptLines[$at + 3] | Should -BeExactly $close[0]
    }

    It 'has the key block that task lint:keys compares with deploy/keys/allowed_signers' {
        $begin = @($ScriptLines | Where-Object { $_.StartsWith('# BEGIN ALLOWED SIGNERS') })
        $end = @($ScriptLines | Where-Object { $_.StartsWith('# END ALLOWED SIGNERS') })
        $begin.Count | Should -Be 1
        $end.Count | Should -Be 1
        $ScriptLines[[array]::IndexOf($ScriptLines, $begin[0]) - 1] | Should -BeExactly "`$AllowedSigners = @'"
        $ScriptLines[[array]::IndexOf($ScriptLines, $end[0]) + 1] | Should -BeExactly "'@"
    }

    It 'has its whole body in one & { } block after the three assignments' {
        $statements = @($ScriptAst.EndBlock.Statements)
        $statements.Count | Should -Be 4
        $statements[0].Left.VariablePath.UserPath | Should -Be 'IsshoniServer'
        $statements[1].Left.VariablePath.UserPath | Should -Be 'IsshoniVersion'
        $statements[2].Left.VariablePath.UserPath | Should -Be 'AllowedSigners'
        $statements[3].PipelineElements[0].InvocationOperator | Should -Be 'Ampersand'
        $ScriptAst.ParamBlock | Should -BeNullOrEmpty
    }
}

Describe 'Get-IsshoniChecksum (7.1 step 6)' {
    BeforeAll {
        $Setup = 'isshoni-desktop_0.2.0_windows_amd64_setup.exe'
        $H1 = 'bad14730fda9afb8c653c763a69e6c05b788cb1d08acda627848f8864152c67b'
        $H2 = '10fcf1268efece33a7dee0de91cb59a8c37232952686ca63bfc6a5bbc63d489c'
    }

    It 'finds the line "<hex>  <name>"' {
        Get-IsshoniChecksum -Text "$H2  isshoni_0.2.0_linux_amd64.tar.gz`n$H1  $Setup`n" -Name $Setup | Should -BeExactly $H1
    }

    It 'finds the line "<hex> *<name>"' {
        Get-IsshoniChecksum -Text "$H1 *$Setup`n" -Name $Setup | Should -BeExactly $H1
    }

    It 'reads CRLF line ends and upper-case hex' {
        $upper = $H1.ToUpperInvariant()
        Get-IsshoniChecksum -Text "$H2  other`r`n$upper  $Setup`r`n" -Name $Setup | Should -BeExactly $upper
    }

    It 'matches the whole name only' {
        Get-IsshoniChecksum -Text "$H1  $Setup.sig`n$H2  x$Setup`n" -Name $Setup | Should -BeExactly ''
        Get-IsshoniChecksum -Text "$H1  $Setup `n" -Name $Setup | Should -BeExactly ''
        Get-IsshoniChecksum -Text "$H1  $($Setup.ToUpperInvariant())`n" -Name $Setup | Should -BeExactly ''
    }

    It 'refuses a name listed twice, even with the same hash' {
        Get-IsshoniChecksum -Text "$H1  $Setup`n$H1  $Setup`n" -Name $Setup | Should -BeExactly ''
        Get-IsshoniChecksum -Text "$H1  $Setup`n$H2 *$Setup`n" -Name $Setup | Should -BeExactly ''
    }

    It 'refuses lines of another shape' {
        Get-IsshoniChecksum -Text "$H1 $Setup`n" -Name $Setup | Should -BeExactly ''
        Get-IsshoniChecksum -Text "$($H1.Substring(1))  $Setup`n" -Name $Setup | Should -BeExactly ''
        Get-IsshoniChecksum -Text "$($H1)0  $Setup`n" -Name $Setup | Should -BeExactly ''
        Get-IsshoniChecksum -Text " $H1  $Setup`n" -Name $Setup | Should -BeExactly ''
        Get-IsshoniChecksum -Text '' -Name $Setup | Should -BeExactly ''
    }
}

Describe 'Test-IsshoniVersion (7.1 step 3)' {
    It 'accepts <Version>' -TestCases @(
        @{ Version = '0.2.0' }, @{ Version = '10.20.30' }, @{ Version = '0.2.0-rc.1' }, @{ Version = '0.0.1-ci.2' },
        @{ Version = '0.0.0-dryrun.17' }, @{ Version = '1.2.3-Alpha.Beta' }
    ) {
        Test-IsshoniVersion -Version $Version | Should -BeTrue
    }

    It 'refuses "<Version>"' -TestCases @(
        @{ Version = '' }, @{ Version = 'v0.2.0' }, @{ Version = '0.2' }, @{ Version = '0.2.0.1' },
        @{ Version = '0.2.0-' }, @{ Version = '0.2.0+abc' }, @{ Version = "0.2.0`n" }, @{ Version = '0.2.0 ' },
        @{ Version = 'latest' }, @{ Version = '0.2.0-rc_1' }, @{ Version = '@VERSION@' }
    ) {
        Test-IsshoniVersion -Version $Version | Should -BeFalse
    }
}

Describe 'Get-IsshoniVersionFromUrl' {
    It 'reads <Url>' -TestCases @(
        @{ Url = 'https://github.com/MoonWX/isshoni/releases/tag/v0.2.0'; Want = '0.2.0' },
        @{ Url = 'https://github.com/MoonWX/isshoni/releases/tag/v0.2.0-rc.1'; Want = '0.2.0-rc.1' },
        @{ Url = 'https://mirror.example.com/isshoni/tag/v1.0.0?x=1'; Want = '1.0.0' },
        @{ Url = 'https://github.com/MoonWX/isshoni/releases'; Want = '' },
        @{ Url = 'https://github.com/MoonWX/isshoni/releases/tag/0.2.0'; Want = '' },
        @{ Url = 'https://github.com/MoonWX/isshoni/releases/tag/vnext'; Want = '' },
        @{ Url = ''; Want = '' }
    ) {
        Get-IsshoniVersionFromUrl -Url $Url | Should -BeExactly $Want
    }
}

Describe 'Get-IsshoniLatestVersion' {
    BeforeEach {
        Mock Start-Sleep { }
    }

    It 'follows the redirect of .../latest in Windows PowerShell 5.1' {
        Mock Invoke-WebRequest {
            New-Object -TypeName PSObject -Property @{
                BaseResponse = New-Object -TypeName PSObject -Property @{
                    ResponseUri = [Uri]'https://github.com/MoonWX/isshoni/releases/tag/v0.2.1'
                }
            }
        }
        Get-IsshoniLatestVersion -Base 'https://github.com/MoonWX/isshoni/releases' | Should -BeExactly '0.2.1'
        Should -Invoke Invoke-WebRequest -Times 1 -Exactly -ParameterFilter {
            [string]$Uri -eq 'https://github.com/MoonWX/isshoni/releases/latest' -and $Method -eq 'Head'
        }
    }

    It 'follows the redirect in PowerShell 7' {
        Mock Invoke-WebRequest {
            New-Object -TypeName PSObject -Property @{
                BaseResponse = New-Object -TypeName PSObject -Property @{
                    RequestMessage = New-Object -TypeName PSObject -Property @{
                        RequestUri = [Uri]'https://github.com/MoonWX/isshoni/releases/tag/v0.3.0-rc.2'
                    }
                }
            }
        }
        Get-IsshoniLatestVersion -Base 'https://github.com/MoonWX/isshoni/releases' | Should -BeExactly '0.3.0-rc.2'
    }

    It 'throws when the redirect names no release' {
        Mock Invoke-WebRequest {
            New-Object -TypeName PSObject -Property @{
                BaseResponse = New-Object -TypeName PSObject -Property @{
                    ResponseUri = [Uri]'https://github.com/MoonWX/isshoni/releases'
                }
            }
        }
        { Get-IsshoniLatestVersion -Base 'https://github.com/MoonWX/isshoni/releases' } |
            Should -Throw -ExpectedMessage '*names no release*'
    }

    It 'tries 3 times, 2 s apart, then throws the last error' {
        Mock Invoke-WebRequest { throw 'The remote name could not be resolved' }
        { Get-IsshoniLatestVersion -Base 'https://github.com/MoonWX/isshoni/releases' } |
            Should -Throw -ExpectedMessage 'The remote name could not be resolved'
        Should -Invoke Invoke-WebRequest -Times 3 -Exactly
        Should -Invoke Start-Sleep -Times 2 -Exactly -ParameterFilter { $Seconds -eq 2 }
    }
}

Describe 'Invoke-IsshoniDownload (7.1 step 4)' {
    BeforeEach {
        Mock Start-Sleep { }
        $FakeFailures = New-Object -TypeName 'System.Collections.Generic.List[string]'
    }

    It 'retries a failed download' {
        Mock Invoke-WebRequest {
            if ($FakeFailures.Count -lt 2) {
                $FakeFailures.Add('x')
                throw 'The operation has timed out.'
            }
        }
        Invoke-IsshoniDownload -Uri 'https://example.com/a' -OutFile (Join-Path -Path $TestDrive -ChildPath 'a') |
            Should -BeExactly ''
        Should -Invoke Invoke-WebRequest -Times 3 -Exactly -ParameterFilter { $UseBasicParsing -and $OutFile }
        Should -Invoke Start-Sleep -Times 2 -Exactly
    }

    It 'gives up after 3 attempts with the last error' {
        Mock Invoke-WebRequest { throw 'The remote server returned an error: (404) Not Found.' }
        Invoke-IsshoniDownload -Uri 'https://example.com/a' -OutFile (Join-Path -Path $TestDrive -ChildPath 'a') |
            Should -BeExactly 'The remote server returned an error: (404) Not Found.'
        Should -Invoke Invoke-WebRequest -Times 3 -Exactly
        Should -Invoke Start-Sleep -Times 2 -Exactly
    }
}

Describe 'ConvertTo-IsshoniArgument (the quoting helper of 7.1)' {
    It 'quotes <Value>' -TestCases @(
        @{ Value = 'C:\Users\Jane Doe\AppData\Local\Temp\isshoni-install-1\allowed_signers'
            Want = '"C:\Users\Jane Doe\AppData\Local\Temp\isshoni-install-1\allowed_signers"' },
        @{ Value = 'C:\plain\path'; Want = '"C:\plain\path"' },
        @{ Value = 'C:\folder with space\'; Want = '"C:\folder with space\\"' },
        @{ Value = 'say "hi"'; Want = '"say \"hi\""' },
        @{ Value = 'a\"b'; Want = '"a\\\"b"' },
        @{ Value = ''; Want = '""' }
    ) {
        ConvertTo-IsshoniArgument -Value $Value | Should -BeExactly $Want
    }
}

Describe 'Get-IsshoniExitMessage (the installer exit codes of 06 6.4)' {
    It 'exit code <Code>' -TestCases @(
        @{ Code = 1; Want = 'isshoni: The setup was cancelled.' },
        @{ Code = 2; Want = 'isshoni: The setup failed (exit code 2).' },
        @{ Code = 3; Want = 'isshoni: Quit isshoni from its tray icon and run the command again.' },
        @{ Code = 4; Want = 'isshoni: A newer isshoni is installed (set ISSHONI_ALLOW_DOWNGRADE=1 to replace it).' },
        @{ Code = 5; Want = 'isshoni: This Windows version is not supported.' },
        @{ Code = 6; Want = 'isshoni: The setup failed (exit code 6).' },
        @{ Code = 7; Want = 'isshoni: WebView2 could not be installed: https://moonwx.github.io/isshoni/troubleshooting#desktop-webview2' },
        @{ Code = 8; Want = 'isshoni: Another isshoni setup is running; run the command again when it has finished.' },
        @{ Code = -1073741819; Want = 'isshoni: The setup failed (exit code -1073741819).' }
    ) {
        Get-IsshoniExitMessage -Code $Code | Should -BeExactly $Want
    }
}

Describe 'Get-IsshoniAllowedSigner' {
    It 'keeps the key lines of the block only' {
        $block = "# BEGIN ALLOWED SIGNERS (x)`r`n`r`n  $TestSignerLine  `r`n# a comment`n$OtherSignerLine`n# END ALLOWED SIGNERS"
        $lines = @(Get-IsshoniAllowedSigner -Text $block)
        $lines.Count | Should -Be 2
        $lines[0] | Should -BeExactly $TestSignerLine
        $lines[1] | Should -BeExactly $OtherSignerLine
    }

    It 'finds no key in an empty block' {
        @(Get-IsshoniAllowedSigner -Text "# BEGIN ALLOWED SIGNERS (x)`n# END ALLOWED SIGNERS").Count | Should -Be 0
    }
}

Describe 'Find-IsshoniSshKeygen (7.2, mocked)' {
    BeforeEach {
        $SavedEnv = Save-TestEnvironment
        [Environment]::SetEnvironmentVariable('SystemRoot', 'C:\Windows')
        Mock Test-Path { $false }
        Mock Get-Command { }
    }

    AfterEach {
        Restore-TestEnvironment -Saved $SavedEnv
    }

    It 'takes the inbox OpenSSH Client first' {
        Mock Test-Path { $true } -ParameterFilter { $LiteralPath -eq 'C:\Windows\System32\OpenSSH\ssh-keygen.exe' }
        Find-IsshoniSshKeygen | Should -BeExactly 'C:\Windows\System32\OpenSSH\ssh-keygen.exe'
        Should -Invoke Get-Command -Times 0 -Exactly
    }

    It 'takes the inbox client through Sysnative from a 32-bit PowerShell' {
        Mock Test-Path { $true } -ParameterFilter { $LiteralPath -eq 'C:\Windows\Sysnative\OpenSSH\ssh-keygen.exe' }
        Find-IsshoniSshKeygen | Should -BeExactly 'C:\Windows\Sysnative\OpenSSH\ssh-keygen.exe'
    }

    It 'falls back to an ssh-keygen.exe on the PATH (Git for Windows)' {
        Mock Get-Command { New-Object -TypeName PSObject -Property @{ Path = 'C:\Program Files\Git\usr\bin\ssh-keygen.exe' } } -ParameterFilter {
            $Name -contains 'ssh-keygen.exe'
        }
        Find-IsshoniSshKeygen | Should -BeExactly 'C:\Program Files\Git\usr\bin\ssh-keygen.exe'
    }

    It 'gives an empty string when there is none' {
        Find-IsshoniSshKeygen | Should -BeExactly ''
    }
}

Describe 'Test-IsshoniSignature (7.2)' {
    BeforeEach {
        $FakeDir = Join-Path -Path $TestDrive -ChildPath ('A Folder With Spaces ' + [guid]::NewGuid().ToString())
        $null = New-Item -ItemType Directory -Path $FakeDir
        $utf8 = New-Object -TypeName Text.UTF8Encoding -ArgumentList $false
        [IO.File]::WriteAllText((Join-Path -Path $FakeDir -ChildPath 'checksums.txt'), $TestChecksums, $utf8)
        [IO.File]::WriteAllText((Join-Path -Path $FakeDir -ChildPath 'checksums.txt.sig'), $TestSignature, $utf8)
    }

    It 'runs ssh-keygen -Y verify with quoted paths, checksums.txt as its standard input' {
        Mock Start-Process { New-FakeProcess -ExitCode 0 }
        $result = Test-IsshoniSignature -SshKeygen $InboxSshKeygen -Directory $FakeDir -Signer @('one', 'two')
        $result.ExitCode | Should -Be 0
        $wantArguments = '-Y verify -f "' + (Join-Path -Path $FakeDir -ChildPath 'allowed_signers') +
        '" -I isshoni-release -n isshoni-checksums -s "' + (Join-Path -Path $FakeDir -ChildPath 'checksums.txt.sig') + '"'
        Should -Invoke Start-Process -Times 1 -Exactly -ParameterFilter {
            $FilePath -eq $InboxSshKeygen -and ($ArgumentList -join ' ') -ceq $wantArguments -and
            $RedirectStandardInput -eq (Join-Path -Path $FakeDir -ChildPath 'checksums.txt') -and
            $NoNewWindow -and $Wait -and $PassThru
        }
    }

    It 'writes the allowed_signers file as UTF-8 without BOM, with LF line ends' {
        Mock Start-Process { New-FakeProcess -ExitCode 0 }
        $null = Test-IsshoniSignature -SshKeygen $InboxSshKeygen -Directory $FakeDir -Signer @($TestSignerLine, $OtherSignerLine)
        $written = [IO.File]::ReadAllBytes((Join-Path -Path $FakeDir -ChildPath 'allowed_signers'))
        $want = [Text.Encoding]::ASCII.GetBytes("$TestSignerLine`n$OtherSignerLine`n")
        ($written -join ',') | Should -BeExactly ($want -join ',')
    }

    It 'verifies the committed signature with the real ssh-keygen' {
        $sk = Get-TestSshKeygen
        if (-not $sk) {
            Set-ItResult -Skipped -Because 'there is no ssh-keygen here'
            return
        }
        $result = Test-IsshoniSignature -SshKeygen $sk -Directory $FakeDir -Signer @($TestSignerLine)
        $result.ExitCode | Should -Be 0 -Because "ssh-keygen said: $($result.Error)"
        $result.Output.Trim() | Should -BeExactly $GoodLine
    }

    It 'refuses a changed checksums.txt with the real ssh-keygen' {
        $sk = Get-TestSshKeygen
        if (-not $sk) {
            Set-ItResult -Skipped -Because 'there is no ssh-keygen here'
            return
        }
        [IO.File]::WriteAllText((Join-Path -Path $FakeDir -ChildPath 'checksums.txt'), $TestChecksums.Replace('bad1', 'bad2'))
        $result = Test-IsshoniSignature -SshKeygen $sk -Directory $FakeDir -Signer @($TestSignerLine)
        $result.ExitCode | Should -Not -Be 0
    }

    It 'refuses a signature by a key that is not in the block with the real ssh-keygen' {
        $sk = Get-TestSshKeygen
        if (-not $sk) {
            Set-ItResult -Skipped -Because 'there is no ssh-keygen here'
            return
        }
        $result = Test-IsshoniSignature -SshKeygen $sk -Directory $FakeDir -Signer @($OtherSignerLine)
        $result.ExitCode | Should -Not -Be 0
    }
}

Describe 'Smart App Control (7.1 step 1, mocked Get-ItemProperty)' {
    BeforeEach {
        $SavedEnv = Save-TestEnvironment
        $FakeTemp = New-TestTemp
        $FakeRelease = New-FakeRelease
        $FakeSystem = @{ Windows = $true; PSVersion = [version]'5.1.19041.4648'; Build = 19045; Is64Bit = $true
            Arch = 'AMD64'; Elevated = $false
        }
        Mock Get-IsshoniSystem { New-Object -TypeName PSObject -Property $FakeSystem }
        Mock Find-IsshoniSshKeygen { $InboxSshKeygen }
        Mock Test-IsshoniSignature { New-Object -TypeName PSObject -Property @{ ExitCode = 0; Output = "$GoodLine`n"; Error = '' } }
        Mock Invoke-WebRequest {
            $leaf = ([string]$Uri).Substring(([string]$Uri).LastIndexOf('/') + 1)
            if (-not $FakeRelease.ContainsKey($leaf)) { throw "(404) Not Found: $leaf" }
            [IO.File]::WriteAllBytes($OutFile, [byte[]]$FakeRelease[$leaf])
        }
        Mock Start-Process { New-FakeProcess -ExitCode 0 }
        Mock Start-Sleep { }
    }

    AfterEach {
        Restore-TestEnvironment -Saved $SavedEnv
    }

    It 'state 1 (on): the Smart App Control line, "Nothing was installed." and no download' {
        Mock Get-ItemProperty { New-Object -TypeName PSObject -Property @{ VerifiedAndReputablePolicyState = 1 } }
        $lines = @(Invoke-Install)
        ($lines -join "`n") | Should -BeExactly (@(
                ("isshoni: Smart App Control is on, and this version of the app can't run on your PC yet: share " +
                "from Chrome or Edge instead. More: $Site/download#windows-smart-app-control"),
                $Nothing) -join "`n")
        Should -Invoke Get-ItemProperty -Times 1 -Exactly -ParameterFilter {
            $Path -contains 'HKLM:\SYSTEM\CurrentControlSet\Control\CI\Policy' -and
            $Name -contains 'VerifiedAndReputablePolicyState'
        }
        Should -Invoke Invoke-WebRequest -Times 0 -Exactly
        Should -Invoke Start-Process -Times 0 -Exactly
        (Get-LeftoverFolder -Temp $FakeTemp).Count | Should -Be 0
    }

    It 'state <State> proceeds to the download and the install' -TestCases @(@{ State = 2 }, @{ State = 0 }) {
        Mock Get-ItemProperty { New-Object -TypeName PSObject -Property @{ VerifiedAndReputablePolicyState = $State } }
        $lines = @(Invoke-Install)
        $lines | Should -Contain 'isshoni 0.2.0 is installed.'
        Should -Invoke Invoke-WebRequest -Times 3 -Exactly
        Should -Invoke Start-Process -Times 1 -Exactly
    }

    It 'a missing key or value (Windows 10) proceeds' {
        Mock Get-ItemProperty { }
        $lines = @(Invoke-Install)
        $lines | Should -Contain 'isshoni 0.2.0 is installed.'
        Should -Invoke Invoke-WebRequest -Times 3 -Exactly
    }

    It 'a failing read proceeds' {
        Mock Get-ItemProperty { throw 'Requested registry access is not allowed.' }
        $lines = @(Invoke-Install)
        $lines | Should -Contain 'isshoni 0.2.0 is installed.'
    }
}

Describe 'Install-IsshoniDesktop (the flow of 7.1)' {
    BeforeEach {
        $SavedEnv = Save-TestEnvironment
        $FakeTemp = New-TestTemp
        $FakeRelease = New-FakeRelease
        $FakeExitCode = 0
        $FakeSystem = @{ Windows = $true; PSVersion = [version]'5.1.19041.4648'; Build = 19045; Is64Bit = $true
            Arch = 'AMD64'; Elevated = $false
        }
        $FakeVerify = New-Object -TypeName PSObject -Property @{ ExitCode = 0; Output = "$GoodLine`n"; Error = '' }
        $DownloadLog = New-Object -TypeName 'System.Collections.Generic.List[string]'
        Mock Get-IsshoniSystem { New-Object -TypeName PSObject -Property $FakeSystem }
        Mock Get-IsshoniSmartAppControl { $null }
        Mock Find-IsshoniSshKeygen { $InboxSshKeygen }
        Mock Get-IsshoniLatestVersion { throw 'the latest version must not be looked up here' }
        Mock Test-IsshoniSignature { $FakeVerify }
        Mock Invoke-WebRequest {
            $DownloadLog.Add([string]$Uri)
            $leaf = ([string]$Uri).Substring(([string]$Uri).LastIndexOf('/') + 1)
            if (-not $FakeRelease.ContainsKey($leaf)) { throw "(404) Not Found: $leaf" }
            [IO.File]::WriteAllBytes($OutFile, [byte[]]$FakeRelease[$leaf])
        }
        Mock Start-Process { New-FakeProcess -ExitCode $FakeExitCode }
        Mock Start-Sleep { }
        $Release = 'https://github.com/MoonWX/isshoni/releases/download'
    }

    AfterEach {
        Restore-TestEnvironment -Saved $SavedEnv
        (Get-LeftoverFolder -Temp $FakeTemp).Count | Should -Be 0 -Because 'the download folder is removed at the end'
    }

    Context 'a successful install' {
        It 'downloads, verifies and installs the stamped version, and starts the app' {
            $lines = @(Invoke-Install)
            ($lines -join "`n") | Should -BeExactly (@(
                    'Downloading isshoni 0.2.0',
                    $GoodLine,
                    'Installing isshoni 0.2.0',
                    'isshoni 0.2.0 is installed.',
                    'It is starting now.') -join "`n")
            ($DownloadLog -join "`n") | Should -BeExactly (@(
                    "$Release/v0.2.0/checksums.txt",
                    "$Release/v0.2.0/checksums.txt.sig",
                    "$Release/v0.2.0/isshoni-desktop_0.2.0_windows_amd64_setup.exe") -join "`n")
            Should -Invoke Test-IsshoniSignature -Times 1 -Exactly -ParameterFilter {
                $SshKeygen -eq $InboxSshKeygen -and @($Signer).Count -eq 1 -and $Signer[0] -ceq $TestSignerLine
            }
            Should -Invoke Start-Process -Times 1 -Exactly -ParameterFilter {
                (Split-Path -Path $FilePath -Leaf) -eq 'isshoni-desktop_0.2.0_windows_amd64_setup.exe' -and
                ($ArgumentList -join ' ') -ceq '/S /LAUNCH' -and $PassThru -and -not $Wait
            }
        }

        It 'passes the server that wrote the script as /SERVER=' {
            $null = Invoke-Install -Server 'https://watch.example.com'
            Should -Invoke Start-Process -Times 1 -Exactly -ParameterFilter {
                ($ArgumentList -join ' ') -ceq '/S /SERVER=https://watch.example.com /LAUNCH'
            }
        }

        It 'drops a server address it cannot pass on' {
            $lines = @(Invoke-Install -Server 'https://watch.example.com/a b')
            $lines | Should -Contain 'isshoni: Note: the server address in this script is not usable; the app will ask.'
            Should -Invoke Start-Process -Times 1 -Exactly -ParameterFilter { ($ArgumentList -join ' ') -ceq '/S /LAUNCH' }
        }

        It 'maps ISSHONI_NO_UPDATE_CHECKS, ISSHONI_ALLOW_DOWNGRADE and ISSHONI_NO_LAUNCH to the installer flags' {
            [Environment]::SetEnvironmentVariable('ISSHONI_NO_UPDATE_CHECKS', '1')
            [Environment]::SetEnvironmentVariable('ISSHONI_ALLOW_DOWNGRADE', '1')
            [Environment]::SetEnvironmentVariable('ISSHONI_NO_LAUNCH', '1')
            $lines = @(Invoke-Install -Server 'https://watch.example.com')
            $lines[-1] | Should -BeExactly 'Start isshoni from the Start menu.'
            Should -Invoke Start-Process -Times 1 -Exactly -ParameterFilter {
                ($ArgumentList -join ' ') -ceq '/S /SERVER=https://watch.example.com /NOUPDATECHECKS /ALLOWDOWNGRADE'
            }
        }

        It 'ignores the flags unless they are 1' {
            [Environment]::SetEnvironmentVariable('ISSHONI_NO_UPDATE_CHECKS', 'yes')
            [Environment]::SetEnvironmentVariable('ISSHONI_NO_LAUNCH', '0')
            $null = Invoke-Install
            Should -Invoke Start-Process -Times 1 -Exactly -ParameterFilter { ($ArgumentList -join ' ') -ceq '/S /LAUNCH' }
        }

        It 'does not start the app when it runs elevated' {
            $FakeSystem.Elevated = $true
            $lines = @(Invoke-Install)
            $lines[-2] | Should -BeExactly 'isshoni 0.2.0 is installed.'
            $lines[-1] | Should -BeExactly 'Start isshoni from the Start menu.'
            Should -Invoke Start-Process -Times 1 -Exactly -ParameterFilter { ($ArgumentList -join ' ') -ceq '/S' }
        }

        It 'installs ISSHONI_VERSION, also written with a v' {
            [Environment]::SetEnvironmentVariable('ISSHONI_VERSION', 'v0.2.1-rc.1')
            $FakeRelease = New-FakeRelease -Version '0.2.1-rc.1'
            $lines = @(Invoke-Install)
            $lines | Should -Contain 'isshoni 0.2.1-rc.1 is installed.'
            $DownloadLog[2] | Should -BeExactly "$Release/v0.2.1-rc.1/isshoni-desktop_0.2.1-rc.1_windows_amd64_setup.exe"
        }

        It 'downloads from ISSHONI_DOWNLOAD_BASE' {
            [Environment]::SetEnvironmentVariable('ISSHONI_DOWNLOAD_BASE', 'https://127.0.0.1:8443/mirror/')
            $null = Invoke-Install
            $DownloadLog[0] | Should -BeExactly 'https://127.0.0.1:8443/mirror/download/v0.2.0/checksums.txt'
        }

        It 'looks up the latest release when the script carries no version (<Stamped>)' -TestCases @(
            @{ Stamped = '@VERSION@' }, @{ Stamped = '' }
        ) {
            Mock Get-IsshoniLatestVersion { '0.2.3' }
            $FakeRelease = New-FakeRelease -Version '0.2.3'
            $lines = @(Invoke-Install -StampedVersion $Stamped)
            $lines | Should -Contain 'isshoni 0.2.3 is installed.'
            Should -Invoke Get-IsshoniLatestVersion -Times 1 -Exactly -ParameterFilter {
                $Base -eq 'https://github.com/MoonWX/isshoni/releases'
            }
        }

        It 'looks up the latest release for ISSHONI_VERSION=latest' {
            [Environment]::SetEnvironmentVariable('ISSHONI_VERSION', 'latest')
            Mock Get-IsshoniLatestVersion { '0.2.3' }
            $FakeRelease = New-FakeRelease -Version '0.2.3'
            $lines = @(Invoke-Install)
            $lines | Should -Contain 'isshoni 0.2.3 is installed.'
        }

        It 'notes a Windows build below 19045 and goes on' {
            $FakeSystem.Build = 19044
            $lines = @(Invoke-Install)
            $lines[0] | Should -BeExactly ('isshoni: Note: isshoni is tested on Windows 10 22H2 (build 19045) and newer; ' +
                'this PC has build 19044.')
            $lines | Should -Contain 'isshoni 0.2.0 is installed.'
        }

        It 'notes the emulation on ARM64 and goes on' {
            $FakeSystem.Arch = 'ARM64'
            $lines = @(Invoke-Install)
            $lines[0] | Should -BeLike 'isshoni: Note: this PC has an ARM64 processor*'
            $lines | Should -Contain 'isshoni 0.2.0 is installed.'
        }
    }

    Context 'stops before downloading anything' {
        It 'outside Windows' {
            $FakeSystem.Windows = $false
            $lines = @(Invoke-Install)
            ($lines -join "`n") | Should -BeExactly (('isshoni: This script installs the isshoni app on Windows; run it ' +
                    "in PowerShell on a Windows 10 or 11 PC.`n") + $Nothing)
            Should -Invoke Invoke-WebRequest -Times 0 -Exactly
        }

        It 'in a PowerShell older than 5.1' {
            $FakeSystem.PSVersion = [version]'5.0.10586.117'
            $lines = @(Invoke-Install)
            $lines[0] | Should -BeExactly 'isshoni: This script needs Windows PowerShell 5.1 or newer; this is 5.0.10586.117.'
            $lines[-1] | Should -BeExactly $Nothing
            Should -Invoke Invoke-WebRequest -Times 0 -Exactly
        }

        It 'on 32-bit Windows' {
            $FakeSystem.Is64Bit = $false
            $lines = @(Invoke-Install)
            $lines[0] | Should -BeExactly 'isshoni: isshoni needs 64-bit Windows; this PC runs 32-bit Windows.'
            $lines[-1] | Should -BeExactly $Nothing
            Should -Invoke Invoke-WebRequest -Times 0 -Exactly
        }

        It 'below build 19041' {
            $FakeSystem.Build = 18363
            $lines = @(Invoke-Install)
            $lines[0] | Should -BeExactly ('isshoni: isshoni needs Windows 10 version 2004 (build 19041) or newer; this ' +
                'PC has build 18363.')
            $lines[-1] | Should -BeExactly $Nothing
            Should -Invoke Invoke-WebRequest -Times 0 -Exactly
        }

        It 'on a processor other than x64 and ARM64' {
            $FakeSystem.Arch = 'x86'
            $lines = @(Invoke-Install)
            $lines[0] | Should -BeExactly "isshoni: isshoni needs an x64 PC; this one reports the processor 'x86'."
            $lines[-1] | Should -BeExactly $Nothing
            Should -Invoke Invoke-WebRequest -Times 0 -Exactly
        }

        It 'when Smart App Control is on' {
            Mock Get-IsshoniSmartAppControl { 1 }
            $lines = @(Invoke-Install)
            $lines[0] | Should -BeLike 'isshoni: Smart App Control is on, *'
            $lines[-1] | Should -BeExactly $Nothing
            Should -Invoke Invoke-WebRequest -Times 0 -Exactly
        }

        It 'when the script carries no key (a development copy)' {
            $lines = @(Invoke-Install -Signers "# BEGIN ALLOWED SIGNERS (x)`n# END ALLOWED SIGNERS")
            $lines[0] | Should -BeLike 'isshoni: This copy of the script carries no release signing key*'
            $lines[-1] | Should -BeExactly $Nothing
            Should -Invoke Invoke-WebRequest -Times 0 -Exactly
        }

        It 'without ssh-keygen' {
            Mock Find-IsshoniSshKeygen { '' }
            $lines = @(Invoke-Install)
            ($lines -join "`n") | Should -BeExactly (@(
                    ("isshoni: Windows' OpenSSH Client is needed to check the download (Settings > System > Optional " +
                    "features > OpenSSH Client), or download the installer from your server's Download page. More: " +
                    "$Site/download#windows-verify"),
                    $Nothing) -join "`n")
            Should -Invoke Invoke-WebRequest -Times 0 -Exactly
        }

        It 'for an ISSHONI_VERSION that is no version' {
            [Environment]::SetEnvironmentVariable('ISSHONI_VERSION', '0.2')
            $lines = @(Invoke-Install)
            ($lines -join "`n") | Should -BeExactly ("isshoni: '0.2' is not a version of isshoni (like 0.2.0 or latest).`n" +
                $Nothing)
            Should -Invoke Invoke-WebRequest -Times 0 -Exactly
        }

        It 'when the latest version cannot be found out' {
            Mock Get-IsshoniLatestVersion { throw 'The remote name could not be resolved' }
            $lines = @(Invoke-Install -StampedVersion '@VERSION@')
            $lines[0] | Should -BeExactly ('isshoni: Could not find out the latest version of isshoni from ' +
                'https://github.com/MoonWX/isshoni/releases/latest (The remote name could not be resolved).')
            $lines[-1] | Should -BeExactly $Nothing
            Should -Invoke Invoke-WebRequest -Times 0 -Exactly
        }
    }

    Context 'stops before installing anything' {
        It 'when a download fails' {
            $FakeRelease.Remove('isshoni-desktop_0.2.0_windows_amd64_setup.exe')
            $lines = @(Invoke-Install)
            $lines[-2] | Should -BeExactly ("isshoni: Could not download $Release/v0.2.0/" +
                'isshoni-desktop_0.2.0_windows_amd64_setup.exe ((404) Not Found: isshoni-desktop_0.2.0_windows_amd64_setup.exe)')
            $lines[-1] | Should -BeExactly $Nothing
            Should -Invoke Start-Sleep -Times 2 -Exactly
            Should -Invoke Test-IsshoniSignature -Times 0 -Exactly
            Should -Invoke Start-Process -Times 0 -Exactly
        }

        It 'when the signature is not valid' {
            $FakeVerify = New-Object -TypeName PSObject -Property @{ ExitCode = 255; Output = ''
                Error = "Signature verification failed: incorrect signature`nCould not verify signature.`n"
            }
            $lines = @(Invoke-Install)
            ($lines -join "`n") | Should -BeExactly (@(
                    'Downloading isshoni 0.2.0',
                    'isshoni: The signature of checksums.txt is not valid: this is not a release the isshoni project signed.',
                    'ssh-keygen says: Signature verification failed: incorrect signature',
                    "If this keeps happening, see $Site/security#verify-desktop",
                    $Nothing) -join "`n")
            Should -Invoke Start-Process -Times 0 -Exactly
        }

        It 'when ssh-keygen is older than OpenSSH 8.1' {
            $FakeVerify = New-Object -TypeName PSObject -Property @{ ExitCode = 1; Output = ''
                Error = "unknown option -- Y`nusage: ssh-keygen [-q] [-b bits] [-C comment] [-f output_keyfile]`n"
            }
            $lines = @(Invoke-Install)
            $lines[-2] | Should -BeExactly ("isshoni: Your ssh-keygen is older than OpenSSH 8.1, which can't check the " +
                "download ($InboxSshKeygen). Update Windows, or download the installer from your server's Download " +
                "page. More: $Site/download#windows-verify")
            $lines[-1] | Should -BeExactly $Nothing
            Should -Invoke Start-Process -Times 0 -Exactly
        }

        It 'when ssh-keygen cannot be run' {
            Mock Test-IsshoniSignature { throw 'Access is denied' }
            $lines = @(Invoke-Install)
            $lines[-2] | Should -BeExactly "isshoni: Could not run $InboxSshKeygen (Access is denied)."
            $lines[-1] | Should -BeExactly $Nothing
            Should -Invoke Start-Process -Times 0 -Exactly
        }

        It 'when the installer does not have the listed checksum' {
            $FakeRelease['isshoni-desktop_0.2.0_windows_amd64_setup.exe'] = [Text.Encoding]::ASCII.GetBytes('changed')
            $lines = @(Invoke-Install)
            $lines[-2] | Should -BeExactly ('isshoni: The checksum of isshoni-desktop_0.2.0_windows_amd64_setup.exe does ' +
                'not match checksums.txt: the download is damaged or was changed.')
            $lines[-1] | Should -BeExactly $Nothing
            Should -Invoke Start-Process -Times 0 -Exactly
        }

        It 'when checksums.txt lists the installer <Case>' -TestCases @(@{ Case = 'never' }, @{ Case = 'twice' }) {
            $sums = [Text.Encoding]::ASCII.GetString($FakeRelease['checksums.txt'])
            if ($Case -eq 'never') {
                $sums = $sums.Replace('isshoni-desktop_0.2.0_windows_amd64_setup.exe', 'isshoni-desktop_0.2.0_windows_arm64_setup.exe')
            } else {
                $sums = $sums + $sums
            }
            $FakeRelease['checksums.txt'] = [Text.Encoding]::ASCII.GetBytes($sums)
            $lines = @(Invoke-Install)
            $lines[-2] | Should -BeExactly 'isshoni: checksums.txt does not list isshoni-desktop_0.2.0_windows_amd64_setup.exe exactly once.'
            $lines[-1] | Should -BeExactly $Nothing
            Should -Invoke Start-Process -Times 0 -Exactly
        }

        It 'when Windows blocks the installer: one isshoni: line, and no error record escapes' {
            Mock Start-Process { throw 'This command cannot be run due to the error: An Application Control policy has blocked this file.' }
            # Invoke-Install throws when an exception or an error record comes out of the function.
            $lines = @(Invoke-Install)
            ($lines -join "`n") | Should -BeExactly (@(
                    'Downloading isshoni 0.2.0',
                    $GoodLine,
                    'Installing isshoni 0.2.0',
                    ('isshoni: Windows blocked the installer (This command cannot be run due to the error: An Application ' +
                    'Control policy has blocked this file.)'),
                    "More: $Site/download#windows-smart-app-control",
                    $Nothing) -join "`n")
        }

        It 'when something unexpected fails' {
            Mock Get-FileHash { throw 'The process cannot access the file because it is being used by another process.' }
            $lines = @(Invoke-Install)
            $lines[-2] | Should -BeExactly 'isshoni: The process cannot access the file because it is being used by another process.'
            $lines[-1] | Should -BeExactly $Nothing
            Should -Invoke Start-Process -Times 0 -Exactly
        }
    }

    Context 'the installer fails' {
        It 'exit code <Code>' -TestCases @(
            @{ Code = 3; Want = 'isshoni: Quit isshoni from its tray icon and run the command again.' },
            @{ Code = 4; Want = 'isshoni: A newer isshoni is installed (set ISSHONI_ALLOW_DOWNGRADE=1 to replace it).' },
            @{ Code = 5; Want = 'isshoni: This Windows version is not supported.' },
            @{ Code = 7; Want = 'isshoni: WebView2 could not be installed: https://moonwx.github.io/isshoni/troubleshooting#desktop-webview2' },
            @{ Code = 8; Want = 'isshoni: Another isshoni setup is running; run the command again when it has finished.' }
        ) {
            $FakeExitCode = $Code
            $lines = @(Invoke-Install)
            $lines[-1] | Should -BeExactly $Want
            $lines | Should -Not -Contain 'isshoni 0.2.0 is installed.'
            $lines | Should -Not -Contain $Nothing
        }
    }
}

Describe 'the whole script, as irm | iex runs it' {
    BeforeEach {
        $SavedEnv = Save-TestEnvironment
        $FakeTemp = New-TestTemp
    }

    AfterEach {
        Restore-TestEnvironment -Saved $SavedEnv
    }

    It 'outside Windows: one isshoni: line, no error record' {
        if ($OnWindows) {
            Set-ItResult -Skipped -Because 'this case is for pwsh outside Windows'
            return
        }
        Mock Invoke-WebRequest { throw 'no download expected' }
        $lines = @(Invoke-WholeScript)
        ($lines -join "`n") | Should -BeExactly (('isshoni: This script installs the isshoni app on Windows; run it in ' +
                "PowerShell on a Windows 10 or 11 PC.`n") + $Nothing)
        Should -Invoke Invoke-WebRequest -Times 0 -Exactly
    }

    It 'on Windows with Smart App Control on: its line and no download' {
        if (-not $OnWindows) {
            Set-ItResult -Skipped -Because 'this case needs Windows'
            return
        }
        Mock Get-ItemProperty { New-Object -TypeName PSObject -Property @{ VerifiedAndReputablePolicyState = 1 } }
        Mock Invoke-WebRequest { throw 'no download expected' }
        $lines = @(Invoke-WholeScript)
        $lines[-2] | Should -BeLike 'isshoni: Smart App Control is on, *'
        $lines[-1] | Should -BeExactly $Nothing
        Should -Invoke Invoke-WebRequest -Times 0 -Exactly
        (Get-LeftoverFolder -Temp $FakeTemp).Count | Should -Be 0
    }
}
