<script setup lang="ts">
// The latest release's Windows installer and its SHA-256 (docs/m2/06-windows-release.md §13, "06" below), from the
// facts that .github/workflows/site.yml verified and wrote into .vitepress/release.json: `desktop` is
// {windowsInstaller, sha256} when the latest release has the app, null when it has none. Without the file (every
// local build, and the site before the first release) the glob below finds nothing and the page says so; a file of
// the wrong shape fails the build, as release.data.ts does.
const releases = import.meta.glob('./.vitepress/release.json', { eager: true, import: 'default' });

interface DesktopRelease {
  version: string;
  installer: string;
  sha256: string;
}

function desktopRelease(): DesktopRelease | null {
  const raw = Object.values(releases)[0] as Record<string, unknown> | undefined;
  if (raw === undefined) return null;
  const fail = (what: string): never => {
    throw new Error(`.vitepress/release.json: ${what} (written by .github/workflows/site.yml, 06 §13)`);
  };
  const version = raw.version;
  if (typeof version !== 'string' || !/^(0|[1-9]\d*)\.(0|[1-9]\d*)\.(0|[1-9]\d*)(-rc\.[1-9]\d*)?$/.test(version)) {
    fail('version is not X.Y.Z or X.Y.Z-rc.N');
  }
  if (raw.desktop === null) return null;
  const d = raw.desktop as Record<string, unknown> | undefined;
  if (typeof d !== 'object' || d === null) fail('desktop is neither null nor an object');
  if (d!.windowsInstaller !== `isshoni-desktop_${version as string}_windows_amd64_setup.exe`) {
    fail('desktop.windowsInstaller is not the release\'s installer name (06 §2.1)');
  }
  if (typeof d!.sha256 !== 'string' || !/^[0-9a-f]{64}$/.test(d!.sha256)) fail('desktop.sha256 is not a SHA-256');
  return { version: version as string, installer: d!.windowsInstaller as string, sha256: d!.sha256 as string };
}

const desktop = desktopRelease();
const releaseUrl = desktop ? `https://github.com/MoonWX/isshoni/releases/tag/v${desktop.version}` : '';
const installerUrl = desktop
  ? `https://github.com/MoonWX/isshoni/releases/download/v${desktop.version}/${desktop.installer}`
  : '';
</script>

# Desktop apps

The desktop apps share your screen with all of your computer's sound except the voice apps (Discord, TeamSpeak and
the like), with hardware video encoding. They come one platform at a time:

| App | Milestone |
|---|---|
| [Windows 10 22H2 and later](#windows) | M2 (isshoni 0.2) |
| [macOS 14.4 and later](#macos) | M3, coming |
| [Linux share agent (Ubuntu 22.04 and later)](#linux) | M4, coming |

Native iPhone and Android apps are pending; phones watch in the browser, and can add isshoni to the Home Screen
([Guide for friends](/guide/#phone)). Without an app, you share from Chrome or Edge on a computer
([Share](/guide/#share)).

## Windows {#windows}

<div v-if="desktop" class="tip custom-block">
  <p class="custom-block-title">Latest release: {{ desktop.version }}</p>
  <p>
    The Windows installer of <a :href="releaseUrl">v{{ desktop.version }}</a> is
    <a :href="installerUrl"><code>{{ desktop.installer }}</code></a>. Its SHA-256 is
    <code class="sha">{{ desktop.sha256 }}</code>
  </p>
</div>
<div v-else class="warning custom-block">
  <p class="custom-block-title">Not released yet</p>
  <p>
    The Windows app comes with isshoni 0.2, and no release with it is out yet. Until then, share from Chrome or Edge.
  </p>
</div>

**What it needs:**

- **Windows 10 22H2 or later, 64-bit** (Windows 11 recommended). On Windows 10 the app can't keep voice apps out of
  your sound ([Desktop sound](/troubleshooting#desktop-sound)).
- **A current graphics driver**: NVIDIA 531.61 or newer for NVIDIA's encoder, and a current AMD or Intel driver
  ([Graphics drivers](/troubleshooting#desktop-gpu-driver)). Without a usable GPU encoder the app uses Windows' own
  encoder, at up to 1080p and 30 frames per second.
- **Windows N and KN editions** need Microsoft's Media Feature Pack
  ([Media Feature Pack](/troubleshooting#desktop-media-feature-pack)).
- **The Microsoft Edge WebView2 Runtime**, version 120 or newer, which current Windows already has; the installer adds
  it when it is missing ([WebView2](#windows-webview2)).
- Not on PCs with **Smart App Control** turned on, until the app is code-signed
  ([Smart App Control](#windows-smart-app-control)).

**Install it from your server.** On the PC, open your isshoni server's Download page in a browser:
`https://<your server>/download` (isshoni's share sheet links to it too). The page has three steps:

1. **Download for Windows**: the installer of the version your server runs, from the project's
   [releases on GitHub](https://github.com/MoonWX/isshoni/releases).
2. **Run it**. The browser and Windows may warn you about it first, because 0.x builds aren't code-signed yet
   ([what to click](#windows-smartscreen)).
3. **Open isshoni**: the button opens the app already set up for your server. You approve the app once in the
   browser, and it is linked to your account ([Linking the app](/troubleshooting#desktop-linking)).

The installer installs for your Windows user only, into `%LOCALAPPDATA%\Programs\isshoni`, with no administrator
prompt, and asks two things: a desktop shortcut, and whether to check for updates automatically
([Updates](#windows-updates)).

### Install with PowerShell {#windows-powershell}

The same Download page shows a one-line command that downloads, checks and installs the app, with no browser or
SmartScreen warning, and then starts it set up for your server. Copy it from the page; it has this form:

```powershell
irm https://<your server>/install-desktop.ps1 | iex
```

Paste it into Windows PowerShell (Start → type "PowerShell"). What the script does:

1. It checks that Windows is 10 22H2 or later and 64-bit, and that Smart App Control is off: with Smart App Control
   on, it says so and downloads nothing.
2. It downloads the installer of your server's version, `checksums.txt` and its signature, from the project's
   releases on GitHub.
3. It checks the signature of `checksums.txt` with the project's release keys, which it carries, and the installer's
   SHA-256 against that signed list ([How to verify a download](/security#verify-desktop)). The line "Good
   "isshoni-release" signature … SHA256:…" shows the key that signed it.
4. It runs the installer silently, and starts isshoni (unless the PowerShell runs as administrator: then it says to
   start isshoni from the Start menu, because the app never runs elevated).

It writes nothing but a temporary folder, which it removes, and it never closes your PowerShell window.

**It needs Windows' OpenSSH Client** for the signature check. Current Windows 10 and 11 have it; if yours doesn't,
add it in Settings → System → Optional features → OpenSSH Client ([OpenSSH Client](/troubleshooting#desktop-openssh)),
or download the installer from the Download page instead.

What its messages mean:

| The script says | What to do |
|---|---|
| Smart App Control is on, and this version of the app can't run on your PC yet | Share from Chrome or Edge ([Smart App Control](#windows-smart-app-control)) |
| Windows' OpenSSH Client is needed to check the download | Add the OpenSSH Client, as above, and run the command again |
| your ssh-keygen is older than OpenSSH 8.1 | Update Windows, or use the Download page |
| The signature of checksums.txt is not valid | Stop: the download is not a release the project signed. Try again later, and report it if it stays ([Security](/security#report)) |
| This copy of the script carries no release signing key | A development server; use its Download page |
| Windows blocked the installer | Smart App Control, an app-control policy or an antivirus blocked it ([Smart App Control](#windows-smart-app-control)) |
| Quit isshoni from its tray icon and run the command again | isshoni was running |
| A newer isshoni is installed | Nothing to do, or set `$env:ISSHONI_ALLOW_DOWNGRADE=1` to install the older one |
| This Windows version is not supported | Windows 10 22H2 or later, 64-bit, is needed |
| WebView2 could not be installed | [WebView2](/troubleshooting#desktop-webview2) |
| Another isshoni setup is running | Wait for it to finish, then run the command again |

Optional settings, set in the same window before the command: `$env:ISSHONI_NO_UPDATE_CHECKS=1` installs with
automatic update checks off, `$env:ISSHONI_NO_LAUNCH=1` doesn't start the app, `$env:ISSHONI_VERSION='0.2.0'`
installs another version.

### "Windows protected your PC" {#windows-smartscreen}

isshoni is applying for free code signing; until then, Windows doesn't know the publisher of 0.x builds, and every
new release starts without a reputation. So a browser download meets up to two warnings:

1. **The browser** may say the file isn't commonly downloaded.
   - Edge: hover over the download, click **…** → **Keep**, then **Show more** → **Keep anyway**.
   - Chrome: click **Keep**.
2. **SmartScreen**, when you run the installer, may say "Windows protected your PC". Click **More info**, check that
   the file name is `isshoni-desktop_<version>_windows_amd64_setup.exe`, then click **Run anyway**.

To check the file itself instead of trusting the warning, [verify it](#windows-verify). The PowerShell command above
checks it for you and meets neither warning (PowerShell's downloads carry no "downloaded from the internet" mark).
Updates that the app installs itself don't show these warnings either.

### Smart App Control {#windows-smart-app-control}

Smart App Control, which is turned on for some fresh installs of Windows 11, blocks apps that aren't code-signed
wherever they came from, and has no "Run anyway". The 0.x builds of the app aren't signed, so **they can't run on a
PC with Smart App Control on**: share from Chrome or Edge on that PC instead ([Share](/guide/#share)).

Check it in Windows Security → App & browser control → Smart App Control settings. Turning it off works, but Windows
can't turn it back on without a reinstall, so we don't ask you to. This changes once the app is code-signed
([Code signing policy](/code-signing)); [Smart App Control](/troubleshooting#desktop-smart-app-control) has the
details.

### Verify the installer {#windows-verify}

Every release's `checksums.txt` lists the SHA-256 of every file of the release, the Windows installer included, and is
signed with the project's release key ([Release signing keys](/security#keys)). In Windows PowerShell, with the
OpenSSH Client (here for 0.2.0):

```powershell
$V = '0.2.0'; $B = "https://github.com/MoonWX/isshoni/releases/download/v$V"; $F = "isshoni-desktop_${V}_windows_amd64_setup.exe"
foreach ($n in $F, 'checksums.txt', 'checksums.txt.sig') { Invoke-WebRequest -UseBasicParsing "$B/$n" -OutFile $n }
Invoke-WebRequest -UseBasicParsing https://moonwx.github.io/isshoni/keys/allowed_signers -OutFile allowed_signers
cmd /c "ssh-keygen -Y verify -f allowed_signers -I isshoni-release -n isshoni-checksums -s checksums.txt.sig < checksums.txt"
(Get-FileHash $F -Algorithm SHA256).Hash.ToLower(); Select-String -Path checksums.txt -SimpleMatch -Pattern $F
```

1. `ssh-keygen` must say `Good "isshoni-release" signature` and print a fingerprint that is one of
   [the release keys'](/security#keys).
2. The hash on the next line must be the one `checksums.txt` lists for the installer.

`cmd /c` runs the check because PowerShell has no `<` redirection, and a pipe would change the file's bytes. More on
verifying, the update keys included: [Verifying the Windows app](/security#verify-desktop).

### Updates {#windows-updates}

The app updates itself from the project's [releases on GitHub](https://github.com/MoonWX/isshoni/releases), never from
your isshoni server:

- **When**: a few minutes after it starts, then once a day, it asks GitHub for the latest release's update info, which
  is signed with the project's update key; the app checks that signature before it uses anything. Settings → Updates →
  **Check now** asks at once.
- **What happens**: a newer version downloads in the background and is checked against the signed info. Then
  **Restart and update** in Settings, the tray menu or the notification installs it; otherwise it installs at the next
  start of the app, before the window opens. An update never interrupts a share. The app never installs an older
  version than the one it runs, and never update info that is out of date.
- **Channels**: **stable** (the default) gets releases. Settings → Updates → "Get release candidates (beta channel)"
  also gets release candidates, which come before a release and may have more bugs.
- **Turning it off**: untick "Check for updates automatically" in the installer or in Settings → Updates; the app then
  asks GitHub nothing, and installs nothing at start. Get a new version from your server's Download page instead. An
  administrator can turn it off for every user of a PC with the policy value `DisableUpdateChecks` = 1 (DWORD) under
  `HKEY_LOCAL_MACHINE\SOFTWARE\Policies\isshoni`; the setting then says "Turned off by your administrator".

**When an update fails:** the app tries an update twice, and then says "The update didn't install" and leaves that
version alone until a newer one comes out. Install it by hand from your server's Download page (the installer
updates the app in place and keeps your settings and sign-ins), or with the PowerShell command. "Update info is out of
date" means that no release came for months, or that your PC's clock is wrong: check the clock, then use the Download
page. [The update didn't install](/troubleshooting#desktop-update-failed) has more.

### Uninstall {#windows-uninstall}

Uninstall isshoni in Settings → Apps → Installed apps (Windows 10: Apps & features) → isshoni → Uninstall.

- **Every uninstall unlinks the app from its servers**: it signs the app out of each server it can reach (the app
  disappears from your account's Devices page) and removes its sign-ins from Windows Credential Manager. A server it
  can't reach removes the app by itself after 90 days unused, or you remove it on that server's Devices page.
- **Keep or remove your settings**: by default your settings, server list and logs stay on the PC, so a reinstall
  finds them. Tick **Also remove my settings and logs on this PC** to remove all of isshoni's data on the PC, the
  folder `%LOCALAPPDATA%\isshoni\` (what's in it: [Privacy](/privacy#desktop)). From a command line:
  `"%LOCALAPPDATA%\Programs\isshoni\uninstall.exe" /S /PURGE`.

### The firewall prompt {#windows-firewall}

The first time you share, Windows Defender Firewall may ask whether isshoni may communicate on your networks.
**Either answer works**: isshoni connects out to your server, which the firewall allows anyway. Allowing it on private
networks is fine, and so is Cancel.

### WebView2 {#windows-webview2}

The app shows isshoni's pages in Microsoft Edge WebView2, which Windows 10 and 11 keep up to date with Edge. The
installer checks for version 120 or newer and installs Microsoft's runtime when it is missing (it downloads it from
Microsoft). If that fails, or the app says it needs WebView2, see
[WebView2](/troubleshooting#desktop-webview2).

### FFmpeg and its source code {#ffmpeg-source}

The app encodes video with FFmpeg's libavcodec and libavutil (FFmpeg is licensed under the LGPL 2.1 or later). They
are shipped unmodified, as the two DLLs `avcodec-<N>.dll` and `avutil-<N>.dll` in the install folder, and are not
signed by this project.

- **The source**: every release has the complete corresponding source, with the build script, as the asset
  `isshoni-desktop_<version>_ffmpeg-source.tar` next to the installer on its
  [GitHub release](https://github.com/MoonWX/isshoni/releases).
- **What was built and how**: `licenses\ffmpeg\README-LGPL.txt` in the install folder names the FFmpeg version and its
  configure line; the licenses are next to it.
- **Replacing the DLLs**: you may put an interface-compatible build in their place: the same file names (the same
  major versions), at least the shipped minor versions, in the install folder. isshoni checks the versions when it
  loads them. An update installs the shipped DLLs again.

The licenses of everything else the app contains are in `licenses\THIRD_PARTY_NOTICES.txt` (Settings → About →
Licenses).

## macOS {#macos}

**Coming** with M3: macOS 14.4 and later, sharing the Mac's sound without the voice apps. Until then, share from
Chrome or Edge on the Mac.

## Linux {#linux}

**Coming** with M4: a share agent for Ubuntu 22.04 and later. Until then, share from Chrome or Edge.

The roadmap is in the
[project plan](https://github.com/MoonWX/isshoni/blob/main/docs/PLAN.md#roadmap-solo-developer-the-estimates-are-rough).

<style scoped>
.sha {
  word-break: break-all;
}
</style>
