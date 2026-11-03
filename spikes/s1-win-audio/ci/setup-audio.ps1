<#
.SYNOPSIS
Gives a GitHub-hosted windows-2025 runner a working audio output for the S1 self-test (.github/workflows/s1-audio.yml).

.DESCRIPTION
The runner has no sound card, and Windows Server ships the audio services off. This script, for a throwaway CI machine
only (never a PC):

1. sets the Windows Audio services (AudioEndpointBuilder, Audiosrv) to Automatic and starts them;
2. allows desktop apps to use the microphone and sets communications ducking to "Do nothing", the README's manual
   steps, so the mic-user scenario can open VB-CABLE's capture side without lowering the other tones;
3. downloads VB-CABLE (VB-Audio Virtual Cable, www.vb-cable.com, donationware by Vincent Burel) from -Url, checks its
   SHA-256 against -Sha256 before unpacking (any mismatch stops here), and logs each catalog's signer;
4. creates the root-enumerated device node the cable driver binds to and installs the driver with SetupAPI, as
   `devcon install` does but without a devcon binary: the Windows 10 package first (signed through Microsoft's
   hardware portal), the 2014 cross-signed Windows 7 package if that one does not start;
5. waits for the cable's audio endpoints, and sets the default output's volume to 100% and unmutes it.

VB-CABLE's licence allows using and sharing the package as is, not building it into another program's installer: it
is downloaded at run time onto the runner, never committed and never shipped.

Exit status: 0 when an audio output works, otherwise PowerShell's error exit (the step fails).
#>
param(
    [Parameter(Mandatory)] [string] $Url,
    [Parameter(Mandatory)] [string] $Sha256
)

Set-StrictMode -Version 3.0
$ErrorActionPreference = 'Stop'
$ProgressPreference = 'SilentlyContinue'
$HardwareId = 'VBAudioVACWDM' # %HardwareId% of both INFs

# 1. Audio services.
foreach ($svc in 'AudioEndpointBuilder', 'Audiosrv') {
    Set-Service -Name $svc -StartupType Automatic
    Start-Service -Name $svc
}
Get-Service AudioEndpointBuilder, Audiosrv | Format-Table -AutoSize Name, Status, StartType | Out-String -Width 200

# 2. Microphone access for desktop apps; communications ducking off (3 = "Do nothing").
$consent = 'SOFTWARE\Microsoft\Windows\CurrentVersion\CapabilityAccessManager\ConsentStore\microphone'
foreach ($key in "HKLM:\$consent", "HKCU:\$consent", "HKCU:\$consent\NonPackaged") {
    New-Item -Path $key -Force | Out-Null
    Set-ItemProperty -Path $key -Name Value -Value Allow -Type String
}
New-Item -Path 'HKCU:\Software\Microsoft\Multimedia\Audio' -Force | Out-Null
Set-ItemProperty -Path 'HKCU:\Software\Microsoft\Multimedia\Audio' -Name UserDuckingPreference -Value 3 -Type DWord

# 3. Download, verify, unpack.
$zip = Join-Path $env:RUNNER_TEMP 'vbcable.zip'
$dir = Join-Path $env:RUNNER_TEMP 'vbcable'
for ($try = 1; ; $try++) {
    try {
        Invoke-WebRequest -UseBasicParsing -Uri $Url -OutFile $zip
        break
    } catch {
        if ($try -ge 3) { throw }
        "download failed ($($_.Exception.Message)); trying again"
        Start-Sleep -Seconds (5 * $try)
    }
}
$got = (Get-FileHash -Algorithm SHA256 -LiteralPath $zip).Hash.ToLowerInvariant()
if ($got -cne $Sha256.ToLowerInvariant()) {
    throw "VB-CABLE: SHA-256 $got is not the pinned $Sha256 (the vendor replaced the file? check it and update the pin)"
}
"VB-CABLE: $Url, SHA-256 $got (pinned)"
Expand-Archive -LiteralPath $zip -DestinationPath $dir -Force

# 4. Device node and driver.
Add-Type -TypeDefinition @'
using System;
using System.ComponentModel;
using System.Runtime.InteropServices;
using System.Text;

public static class S1DevInstall {
    [StructLayout(LayoutKind.Sequential)]
    struct SP_DEVINFO_DATA { public int cbSize; public Guid ClassGuid; public int DevInst; public IntPtr Reserved; }

    [DllImport("setupapi.dll", SetLastError = true, CharSet = CharSet.Unicode)]
    static extern bool SetupDiGetINFClassW(string inf, out Guid classGuid, StringBuilder className, int size, out int required);
    [DllImport("setupapi.dll", SetLastError = true)]
    static extern IntPtr SetupDiCreateDeviceInfoList(ref Guid classGuid, IntPtr hwnd);
    [DllImport("setupapi.dll", SetLastError = true, CharSet = CharSet.Unicode)]
    static extern bool SetupDiCreateDeviceInfoW(IntPtr set, string name, ref Guid classGuid, string desc, IntPtr hwnd, int flags, ref SP_DEVINFO_DATA data);
    [DllImport("setupapi.dll", SetLastError = true, CharSet = CharSet.Unicode)]
    static extern bool SetupDiSetDeviceRegistryPropertyW(IntPtr set, ref SP_DEVINFO_DATA data, int property, byte[] buffer, int size);
    [DllImport("setupapi.dll", SetLastError = true)]
    static extern bool SetupDiCallClassInstaller(int function, IntPtr set, ref SP_DEVINFO_DATA data);
    [DllImport("setupapi.dll", SetLastError = true)]
    static extern bool SetupDiDestroyDeviceInfoList(IntPtr set);
    [DllImport("newdev.dll", SetLastError = true, CharSet = CharSet.Unicode)]
    static extern bool UpdateDriverForPlugAndPlayDevicesW(IntPtr hwnd, string hardwareId, string inf, int flags, out bool reboot);

    const int DICD_GENERATE_ID = 1, SPDRP_HARDWAREID = 1, DIF_REGISTERDEVICE = 0x19, INSTALLFLAG_FORCE = 1;

    // CreateRootDevice adds ROOT\<class>\NNNN with the hardware ID (devcon install's first half).
    public static void CreateRootDevice(string inf, string hardwareId) {
        Guid cls; int req; var name = new StringBuilder(256);
        if (!SetupDiGetINFClassW(inf, out cls, name, name.Capacity, out req)) throw new Win32Exception(Marshal.GetLastWin32Error(), "SetupDiGetINFClass");
        IntPtr set = SetupDiCreateDeviceInfoList(ref cls, IntPtr.Zero);
        if (set == new IntPtr(-1)) throw new Win32Exception(Marshal.GetLastWin32Error(), "SetupDiCreateDeviceInfoList");
        try {
            var data = new SP_DEVINFO_DATA(); data.cbSize = Marshal.SizeOf(typeof(SP_DEVINFO_DATA));
            if (!SetupDiCreateDeviceInfoW(set, name.ToString(), ref cls, null, IntPtr.Zero, DICD_GENERATE_ID, ref data))
                throw new Win32Exception(Marshal.GetLastWin32Error(), "SetupDiCreateDeviceInfo");
            byte[] ids = Encoding.Unicode.GetBytes(hardwareId + "\0\0"); // REG_MULTI_SZ
            if (!SetupDiSetDeviceRegistryPropertyW(set, ref data, SPDRP_HARDWAREID, ids, ids.Length))
                throw new Win32Exception(Marshal.GetLastWin32Error(), "SetupDiSetDeviceRegistryProperty");
            if (!SetupDiCallClassInstaller(DIF_REGISTERDEVICE, set, ref data))
                throw new Win32Exception(Marshal.GetLastWin32Error(), "DIF_REGISTERDEVICE");
        } finally { SetupDiDestroyDeviceInfoList(set); }
    }

    // InstallDriver installs inf on every device with the hardware ID (devcon install's second half);
    // returns whether Windows asks for a reboot.
    public static bool InstallDriver(string inf, string hardwareId) {
        bool reboot;
        if (!UpdateDriverForPlugAndPlayDevicesW(IntPtr.Zero, hardwareId, inf, INSTALLFLAG_FORCE, out reboot))
            throw new Win32Exception(Marshal.GetLastWin32Error(), "UpdateDriverForPlugAndPlayDevices");
        return reboot;
    }
}
'@

function Get-CableDevice {
    @(Get-PnpDevice -PresentOnly -ErrorAction SilentlyContinue |
        Where-Object { $_.HardwareID -contains $HardwareId })
}

# Waits until the cable device runs (Status OK); $false after the timeout.
function Wait-Cable([int] $Seconds) {
    $deadline = (Get-Date).AddSeconds($Seconds)
    while ((Get-Date) -lt $deadline) {
        if (@(Get-CableDevice | Where-Object Status -eq 'OK').Count -gt 0) { return $true }
        Start-Sleep -Milliseconds 500
    }
    return $false
}

function Install-Package([string] $Inf, [string] $Cat) {
    $sig = Get-AuthenticodeSignature -LiteralPath $Cat
    $signer = $sig.SignerCertificate # $null for an unsigned or unreadable catalog
    $who = if ($signer) { "signer '$($signer.Subject)', issuer '$($signer.Issuer)'" } else { 'no signer' }
    "${Cat}: signature $($sig.Status), $who"
    if ($signer) {
        # Trusting the signer up front rules out a "trust this publisher?" prompt nobody can answer. X509Store, not
        # the PKI module's Import-Certificate, which PowerShell 7 may load through the Windows PowerShell compat layer.
        $store = [Security.Cryptography.X509Certificates.X509Store]::new('TrustedPublisher', 'LocalMachine')
        $store.Open('ReadWrite')
        try { $store.Add($signer) } finally { $store.Close() }
    }
    try {
        $reboot = [S1DevInstall]::InstallDriver($Inf, $HardwareId)
        "installed $Inf$(if ($reboot) { ' (Windows asks for a reboot)' })"
    } catch {
        "installing $Inf failed: $($_.Exception.Message)"
    }
}

$win10 = Join-Path $dir 'vbMmeCable64_win10.inf'
$win7 = Join-Path $dir 'vbMmeCable64_win7.inf'
if (@(Get-CableDevice).Count -eq 0) { # @(): a function's empty result is $null, and strict mode throws on $null.Count
    [S1DevInstall]::CreateRootDevice($win10, $HardwareId)
    'created the root device node'
}
Install-Package $win10 (Join-Path $dir 'vbaudio_cable64_win10.cat')
if (-not (Wait-Cable 30)) {
    'the Windows 10 package did not start the device; trying the Windows 7 package'
    Get-CableDevice | Format-List Status, Problem, InstanceId, FriendlyName | Out-String -Width 200
    Install-Package $win7 (Join-Path $dir 'vbaudio_cable64_win7.cat')
    if (-not (Wait-Cable 30)) {
        Get-CableDevice | Format-List Status, Problem, InstanceId, FriendlyName | Out-String -Width 200
        throw 'VB-CABLE: neither driver package started the device'
    }
}
Get-CableDevice | Format-Table -AutoSize Status, FriendlyName, InstanceId | Out-String -Width 200

# 5. Endpoints, then the default output's volume.
$deadline = (Get-Date).AddSeconds(60)
$restarted = $false
while (@(Get-PnpDevice -Class AudioEndpoint -PresentOnly -Status OK -ErrorAction SilentlyContinue).Count -eq 0) {
    if (-not $restarted -and (Get-Date) -gt $deadline.AddSeconds(-30)) {
        'no audio endpoint after 30 s; restarting Audiosrv'
        Restart-Service -Name Audiosrv -Force
        $restarted = $true
    }
    if ((Get-Date) -gt $deadline) { throw 'no audio endpoint appeared within 60 s of the driver install' }
    Start-Sleep -Milliseconds 500
}
Get-PnpDevice -Class AudioEndpoint -PresentOnly | Format-Table -AutoSize Status, FriendlyName | Out-String -Width 200

Add-Type -TypeDefinition @'
using System;
using System.Runtime.InteropServices;

// [PreserveSig]: the methods return their HRESULT (checked below) instead of throwing.
[Guid("5CDF2C82-841E-4546-9722-0CF74078229A"), InterfaceType(ComInterfaceType.InterfaceIsIUnknown), ComImport]
interface IS1AudioEndpointVolume {
    [PreserveSig] int RegisterControlChangeNotify(IntPtr notify);
    [PreserveSig] int UnregisterControlChangeNotify(IntPtr notify);
    [PreserveSig] int GetChannelCount(out uint count);
    [PreserveSig] int SetMasterVolumeLevel(float levelDB, ref Guid context);
    [PreserveSig] int SetMasterVolumeLevelScalar(float level, ref Guid context);
    [PreserveSig] int GetMasterVolumeLevel(out float levelDB);
    [PreserveSig] int GetMasterVolumeLevelScalar(out float level);
    [PreserveSig] int SetChannelVolumeLevel(uint channel, float levelDB, ref Guid context);
    [PreserveSig] int SetChannelVolumeLevelScalar(uint channel, float level, ref Guid context);
    [PreserveSig] int GetChannelVolumeLevel(uint channel, out float levelDB);
    [PreserveSig] int GetChannelVolumeLevelScalar(uint channel, out float level);
    [PreserveSig] int SetMute([MarshalAs(UnmanagedType.Bool)] bool mute, ref Guid context);
    [PreserveSig] int GetMute([MarshalAs(UnmanagedType.Bool)] out bool mute);
}

[Guid("D666063F-1587-4E43-81F1-B948E807363F"), InterfaceType(ComInterfaceType.InterfaceIsIUnknown), ComImport]
interface IS1MMDevice {
    [PreserveSig] int Activate(ref Guid iid, int clsCtx, IntPtr activationParams, [MarshalAs(UnmanagedType.IUnknown)] out object iface);
}

[Guid("A95664D2-9614-4F35-A746-DE8DB63617E6"), InterfaceType(ComInterfaceType.InterfaceIsIUnknown), ComImport]
interface IS1MMDeviceEnumerator {
    [PreserveSig] int EnumAudioEndpoints(int dataFlow, int stateMask, out IntPtr devices);
    [PreserveSig] int GetDefaultAudioEndpoint(int dataFlow, int role, out IS1MMDevice device);
}

[ComImport, Guid("BCDE0395-E52F-467C-8E3D-C4579291692E")]
class S1MMDeviceEnumerator { }

public static class S1Volume {
    // SetDefaultOutput sets the default render endpoint (eRender, eMultimedia) to level and unmutes it;
    // returns the level read back.
    public static float SetDefaultOutput(float level) {
        var enumerator = (IS1MMDeviceEnumerator)new S1MMDeviceEnumerator();
        IS1MMDevice device;
        Marshal.ThrowExceptionForHR(enumerator.GetDefaultAudioEndpoint(0, 1, out device));
        Guid iid = typeof(IS1AudioEndpointVolume).GUID;
        object o;
        Marshal.ThrowExceptionForHR(device.Activate(ref iid, 1 /* CLSCTX_INPROC_SERVER */, IntPtr.Zero, out o));
        var volume = (IS1AudioEndpointVolume)o;
        Guid context = Guid.Empty;
        Marshal.ThrowExceptionForHR(volume.SetMute(false, ref context));
        Marshal.ThrowExceptionForHR(volume.SetMasterVolumeLevelScalar(level, ref context));
        float got;
        Marshal.ThrowExceptionForHR(volume.GetMasterVolumeLevelScalar(out got));
        return got;
    }
}
'@
$deadline = (Get-Date).AddSeconds(30)
for (;;) {
    try {
        $level = [S1Volume]::SetDefaultOutput(1.0)
        "default output volume: $([math]::Round($level * 100))%, unmuted"
        break
    } catch {
        # 0x80070490 (E_NOTFOUND): no default output yet.
        if ((Get-Date) -gt $deadline) { throw "setting the default output's volume failed: $($_.Exception.Message)" }
        Start-Sleep -Milliseconds 500
    }
}
