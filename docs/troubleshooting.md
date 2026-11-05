# Troubleshooting

Two tools find most problems, and both link to this page:

- **`sudo isshoni doctor`** on the server (`docker compose exec isshoni isshoni doctor` in Docker) checks the
  configuration, the data, the address, the certificate and more, and prints a fix for each problem. It works whether
  the server runs or not. `--json` prints the report for a script, `--only dns,tls` runs some checks, and
  `--list-checks` lists them. The admin page has the same report.
- **The connection test** in the browser (the setup wizard's second step, and "Test my connection" in the account
  menu) checks from where you are whether video gets through, over UDP, over TCP and how fast.

<!-- The sections below with a {#doctor-…} or {#ct-…} anchor are linked from the server and the web app, and
TestDocsAnchors (internal/server/ops/doctor/docsanchors_test.go) requires one for every doctor check id and every
connection test code. Never rename or remove an anchor: older releases link to it. -->

## Doctor checks {#doctor}

Each line of doctor's output is one check: `ok`, `info` (something to know, never a problem), `warn`, `fail`, or
`skip` (doctor could not find out, for example a check that needs the running server). doctor exits with code 5 when a
check fails (with `--strict`, also on a warning).

### config: the configuration file {#doctor-config}

The configuration (`/etc/isshoni/isshoni.toml`, the `ISSHONI_*` variables and flags) loads and is valid. On a
problem doctor prints the first one with its fix; `sudo isshoni config check` lists all of them, each with the file
and line it comes from. A server with an invalid configuration doesn't start (exit code 78). The keys are in the
[configuration reference](/reference/config).

### data_dir: the data directory {#doctor-data_dir}

`/var/lib/isshoni` exists, belongs to the user `isshoni`, is private (`0700`) and has at least 1 GB free. In Docker
it must be a volume or a mounted directory: without one, isshoni refuses to start, so that a recreated container can't
lose your data ([Docker](/install/docker#bind-mount)). The usual fixes: `sudo chown -R isshoni:isshoni
/var/lib/isshoni` after files were copied in as root, `sudo chmod 700 /var/lib/isshoni`, or free disk space. Run
doctor with `sudo`: an ordinary user can't look inside the directory.

### secrets: the server's keys {#doctor-secrets}

`secrets.json` in the data directory holds the server's generated keys (for sessions, invites and notifications). It must be there,
private (`0600`) and readable. When it is damaged or missing while the database is there, restore it from a backup:
`sudo systemctl stop isshoni && sudo -u isshoni isshoni admin restore --offline <backup> && sudo systemctl start
isshoni`. Before the first start it doesn't exist yet, which is fine (`info`).

### schema: the database {#doctor-schema}

The database's version fits this isshoni. A database from a **newer** isshoni (after a downgrade) stops the server:
doctor prints the restore command for the backup that the upgrade made, for example
`sudo -u isshoni isshoni admin restore --offline /var/lib/isshoni/backups/pre-3-20261014T021500Z.db`, or tells you to
install the version that used the database last. A damaged database needs a backup. With an older database the next
start upgrades it, after making that backup.

### public_ip: the server's public address {#doctor-public_ip}

isshoni needs its public IP address for media, and in IP mode for the certificate. It looks on the network interfaces
first, then asks two STUN servers (Cloudflare and Google), and also finds out whether a router is in the way.

- **No public address found** (`fail`): set `public_ip = "<the server's public IPv4>"` in
  `/etc/isshoni/isshoni.toml` (Docker: `ISSHONI_PUBLIC_IP` in `.env`), or set a domain, then restart isshoni
  (`sudo systemctl restart isshoni`; Docker: `docker compose up -d`). If the network just wasn't up when isshoni
  started, a restart is enough. In IP mode such a server runs but isn't ready, and nobody can open it.
- **Behind a home router** (`warn`): forward TCP 80, 443, 7882 and UDP 7882 on the router to this machine
  ([A server at home](/install/vps#home)).
- **CGNAT or a symmetric NAT** (`fail`): the server has no public address of its own, and no port forward can reach
  it. Run isshoni on a VPS, or ask your internet provider for a public address.
- **IP mode on AWS or Google Cloud** (`info`): the default public address changes on stop and start. Attach an
  Elastic IP (AWS) or reserve a static IP (Google Cloud), or use a domain ([AWS](/install/vps#aws),
  [Google Cloud](/install/vps#gcp)).
- **No IPv6** (`info`) is fine: everything works over IPv4.
- **Not checked** on a development setup (loopback addresses and no STUN server) is expected there.

### dns: the domain's records {#doctor-dns}

With a domain, its A record (and AAAA record, if any) must point to this server, and a CAA record, if the domain has
one, must allow `letsencrypt.org`. doctor compares the records with the server's address and names the fix: add an A
record, change it to this server's address, remove a wrong AAAA record, or add the CAA record
`0 issue "letsencrypt.org"`. A record you just changed can take a while to show everywhere; run doctor again later.
Without a domain, or behind your own proxy, there is nothing to check.

### tls: the certificate {#doctor-tls}

The certificate is there and valid, and its renewal works. While isshoni gets its first certificate the check says
so (`info`); when an attempt fails, it gives the reason at once:

- **Let's Encrypt couldn't connect** on port 80 or 443: open TCP 80 and 443 in your provider's firewall
  ([Choosing a VPS](/install/vps)) and on the server.
- **The domain doesn't resolve, or points elsewhere**: fix the A record (see [dns](#doctor-dns)).
- **Rate limit**: Let's Encrypt limits how many certificates a name gets. The server's log says until when
  (`journalctl -u isshoni`). If you reinstalled, restore a backup: it carries the certificates.
- **IP refused** (IP mode): Let's Encrypt only certifies public addresses.
- **Manual mode**: the files can't be read by the user `isshoni`, are no certificate with its key, or are expired; see
  [Certificates](/install/tls#manual).

Also see [The certificate is not issued](#certificate-not-issued).

### clock: the system clock {#doctor-clock}

Certificates and sign-ins need the right time. doctor asks the kernel whether a time service keeps the clock in sync
and, in the Let's Encrypt modes, compares the clock with Let's Encrypt's. A clock that is off by more than 30 seconds
is a warning, by more than 5 minutes a failure. Fix: `sudo timedatectl set-ntp true` (in Docker, on the host).

### udp_buffers: UDP buffers {#doctor-udp_buffers}

Video arrives in bursts, and Linux's default UDP buffers are too small for them: a burst that doesn't fit is lost and
the picture stutters. isshoni asks for 8 MiB per media socket; the kernel limits must allow it:

```sh
printf 'net.core.rmem_max=8388608\nnet.core.wmem_max=8388608\n' | sudo tee /etc/sysctl.d/60-isshoni.conf && sudo sysctl --system
```

If isshoni's file `60-isshoni.conf` is already there, `sudo sysctl --system` is enough. In Docker run the command on
the host. When the limits were raised after isshoni started, restart it so its sockets get the larger buffers.

### ports: the ports on this machine {#doctor-ports}

isshoni listens on 443/tcp, 80/tcp, 7882/udp and 7882/tcp (offline: whether it can bind them). **This only checks
this machine**: whether the ports are open from outside, through your provider's firewall, only the browser's
connection test can tell. A port used by another program: stop that program, or for 80 and 443 run isshoni
[behind it](/install/reverse-proxy).

### firewall_hint: your provider's firewall {#doctor-firewall_hint}

Information only. doctor recognizes many cloud providers and says where their firewall is and which ports to open,
with a link to the provider's section of [Choosing a VPS and its firewall](/install/vps). In Docker it also notes
that published ports bypass ufw.

### container: Docker or Podman {#doctor-container}

Information only: whether isshoni runs in a container, which network mode it uses, and that the published ports
must be the same numbers inside and outside (`7882:7882`), because isshoni tells browsers its own port numbers.

### lan_privacy: Local Network access on a Mac {#doctor-lan_privacy}

Information only, on a server that runs on macOS: browsers on other Macs need Local Network access to reach a server
on their own network. See [Local Network access on a Mac](#ct-mac_local_network).

### transfer: this month's transfer {#doctor-transfer}

With a limit set (`limits.transfer_alert_gb`, also on the admin page), doctor warns at 80% of the month's outgoing
traffic and above the limit. It never stops anything. Compare with your provider's allowance; lower bitrate limits
(`limits.max_bitrate_kbps`) use less. [How much transfer](/install/vps#sizing).

### release: updates {#doctor-release}

The server checks GitHub once a day for a newer release (`updates.release_check`). A warning means one is out, or a
security fix: run the [install script](/install/#upgrade) again, or `docker compose pull && docker compose up -d`.

### nofile: the open-file limit {#doctor-nofile}

Every connection uses a few file descriptors, and below a limit of 8192 isshoni may run out with many friends.
isshoni raises its own limit as far as the system allows when it starts, so a warning means the system's limit is
low: `sudo systemctl edit isshoni`, add `LimitNOFILE=65536` under `[Service]`, and restart isshoni. In Docker: add
`ulimits: {nofile: 65536}` to the service in `compose.yaml`.

### bandwidth: the bandwidth estimate {#doctor-bandwidth}

Information: how much the server sends for a typical session (5 people, 2 sharing, 1080p60, 2 hours by default; see
`--people`, `--sharing`, `--quality` and `--hours`), and a warning when that comes close to the network card's speed.
[How much transfer](/install/vps#sizing).

## Connection test results {#connection-test}

The connection test opens three test connections from your browser to the server: media over UDP on port 7882, over
TCP on port 443, and over TCP on port 7882. Green means UDP works, which gives the best quality. Amber means only TCP
works: it works, but video may stutter on weak networks. Red means nothing gets through. "Copy result" copies the
result without any IP address, for your admin.

### UDP is blocked {#ct-udp_blocked}

Media over UDP failed, but TCP works. Friends can watch, with more delay and stutter on a weak network.

- Open **UDP 7882** in your provider's firewall ([your provider](/install/vps)) and on the server:
  `sudo ufw allow 7882/udp`, or `sudo firewall-cmd --permanent --add-port=7882/udp && sudo firewall-cmd --reload`.
- If the network you test from blocks UDP (some offices and hotels do), test from home or from your phone's mobile
  data. For friends on such networks the TCP path is the fallback, which is why it exists.

### No media connection at all {#ct-no_media}

Every test connection failed: nothing gets through to the media ports.

- Open TCP 80, 443, 7882 and UDP 7882 in your provider's firewall ([your provider](/install/vps)) and in the
  server's own firewall.
- In Docker, check that `compose.yaml` publishes `7882:7882/udp` and `7882:7882/tcp`.
- A server at home needs the ports forwarded on the router ([A server at home](/install/vps#home)).
- Test from another network, for example your phone's mobile data.

### The server doesn't know its public address {#ct-no_public_ip}

Every test connection failed, and the server has no public IPv4 address to offer: its answers carry no address the
browser could reach, **so the firewall is not the problem**. This happens with a domain or behind your own proxy, in
Docker's network or behind a router, when STUN is blocked, or with `network.stun_servers = []` and no `public_ip`.

Fix: set `public_ip = "<the server's public IPv4>"` in `/etc/isshoni/isshoni.toml`, then
`sudo systemctl restart isshoni`. In Docker set `ISSHONI_PUBLIC_IP` in `.env`, then run `docker compose up -d`, which
recreates the container with the new value (`docker compose restart` would keep the old one). Then run the test
again.

### TCP port 443 doesn't carry media {#ct-tcp443_blocked}

UDP works, so friends get the best quality. Only the fallback for networks that block everything but HTTPS is
missing: media over TCP on the server's own port 443. Check that TCP 443 is open from anywhere in your provider's
firewall, and that nothing in front of the server (a load balancer, a proxy) takes port 443: media over TCP must
reach isshoni itself. A network that inspects HTTPS traffic can block it too; test from another network.

### High latency {#ct-high_rtt}

The round trip between your browser and the server takes more than 200 ms. Video still works, with more delay. A
server closer to your group helps, and so do a wired network or a better Wi-Fi signal. Test again from another
network to see whether it is the server or your connection.

### Port forwarding on a home router {#ct-nat_port_forward}

The server is behind a router that forwards its ports. Forward TCP 80, 443, 7882 and UDP 7882 on the router to the
machine running isshoni. **You may be testing from the server's own network**, which says little about your friends:
test again from your phone's mobile data. [A server at home](/install/vps#home).

### A server without a public address {#ct-nat_cgnat}

The server has no public address of its own (carrier-grade NAT, or a symmetric NAT): friends can't reach it, and no
setting can change that. isshoni needs a public address: run it on a VPS ([Choosing a VPS](/install/vps)), or ask your
internet provider for a public IPv4 address and forward the ports to the server.

### Local Network access on a Mac {#ct-mac_local_network}

The server is on your own network (a private address), and on macOS a browser needs Local Network access to reach
it: System Settings → Privacy & Security → Local Network, then turn it on for your browser and reload the page.
Without it the connection test, and video, fail on that Mac only.

### Publishing ports from Docker {#ct-docker_ports}

isshoni runs in a container. Check that `compose.yaml` publishes the media ports with the same numbers inside and
outside, `7882:7882/udp` and `7882:7882/tcp`, and open them in your provider's firewall. Published ports bypass ufw,
so ufw rules neither help nor block them ([Docker](/install/docker#host)).

<!-- The desktop app, its installer, the PowerShell script and the server's Download page link to the
{#desktop-…} anchors below (docs/m2/03-desktop-app.md §8.1, §9, §23; docs/m2/06-windows-release.md §13, §17), and
TestDocsAnchors requires each of them: never rename or remove one. -->

## Linking the app {#desktop-linking}

The desktop app (for Windows, from isshoni 0.2) links only to a server that runs 0.2 or later
([Desktop apps](/download)).

The desktop app signs in to your server once, when you link it: it shows a code and opens your server's page in your
browser, where you approve it while signed in. "Sign in with username and password instead" in the app links it
without a browser. These are the usual problems.

### "Can't reach the server", but the browser can {#desktop-linking-proxy}

The server opens in your browser, and maybe in the app's own window, but the app says it can't reach the server, or
can't connect when you share. Most often the network needs a proxy: an office or school network, or security or
filtering software that sets one. Your browser and the app's window use the proxy set in Windows; the app's own
connection to the server, which links it and starts your shares, doesn't read that setting yet. Until it does, give
it the proxy yourself:

1. Find the proxy's address in **Settings → Network & internet → Proxy**, under "Manual proxy setup" (for example
   `proxy.example.com:8080`). If the proxy is set up automatically instead ("Automatically detect settings" or "Use
   setup script"), ask whoever runs the network for its address.
2. Set it as `HTTPS_PROXY` for your Windows user, in PowerShell:

   ```powershell
   [Environment]::SetEnvironmentVariable('HTTPS_PROXY', 'http://proxy.example.com:8080', 'User')
   ```

   or under **Edit environment variables for your account** (search for it in Start): New, name `HTTPS_PROXY`, value
   `http://proxy.example.com:8080`.
3. Quit isshoni from its tray icon and start it again: a program reads the setting when it starts.

If the proxy asks for a sign-in of its own (office proxies often use your Windows account), the app can't use it yet:
ask whoever runs the network to let `isshoni.exe`, or your server's address, through without one.

Other programs that read `HTTPS_PROXY`, such as some command-line tools, use it too; delete the variable once you
no longer need it. If the browser works without any proxy, look at your firewall or security software instead: one
that lets only known programs connect may block `isshoni.exe`, so allow it there. The proxy carries only the app's
connection to the server; video goes its own way to the server's media port. If the app links but no video gets
through, run the connection test in your browser ([Connection test results](#connection-test)).

### The code expired, or the browser doesn't take it {#desktop-linking-code}

- A code works for 10 minutes. When the app says "The code expired", click **Get a new code** and approve the new one.
- Approve in a browser where you're signed in to the same server. If it asks you to log in first, do: it comes back
  to the approval page. Check that the code on the page is the one in the app.
- "This code is wrong, was already used or has expired": start again in the app, which shows a new code. A code has
  letters only, no digits and no vowels (so no O or I), and the dash is optional.
- After too many wrong codes the page asks you to wait a few minutes.
- If you clicked **Don't link**, the app says linking was declined: click **Try again**.
- The page asks you to tick "I started this myself" when the app and the browser reach the server from different
  networks (for example, the PC uses a VPN or mobile data) or the server can't tell. If you started linking on your
  own PC, tick it. **If someone sent you the link, don't approve it**: they would get an app signed in as you.
- After you click **Link**, the app goes on by itself within a few seconds.

### The app was signed out {#desktop-linking-signed-out}

The app asks you to link it again when the server no longer accepts its sign-in:

- **"This PC was signed out"**: it was removed on your account's Devices page, or not used for 90 days; or you
  changed your password or used "Log out everywhere"; or an admin created a reset link for you or signed you out
  everywhere; or the server's admin restored an older backup or replaced the server's keys; or a 21st app was linked
  to your account, which removes the app seen least recently (an account has at most 20). Click **Link again**.
- **"Someone else used this PC's sign-in"**: the server saw the app's sign-in used twice, which happens when it was
  copied, so it removed the app. Rarely, the app causes it itself, when it was closed by a crash or a power cut right
  after the server renewed its sign-in. Link again, then check your Devices page for apps you don't recognize. If
  you suspect a copy, check this PC for unwanted software first: any program running as you can read the app's
  sign-in ([Linked apps](/security#linked-apps)).
- **"Your account is disabled"**: ask your admin.

### Wrong username or password {#desktop-linking-password}

"Sign in with username and password instead" takes the username and password of your server's login page, with the
same limits:

- "Wrong username or password": check both. The username is the one you log in with in the browser.
- After several wrong tries the app asks you to wait; try again after that time.
- "An admin hasn't approved your account yet": the server approves new accounts by hand; wait for an admin.
- Forgot your password? isshoni sends no email: ask your admin for a reset link, open it in your browser, choose a
  new password, then sign in to the app with it.

The app never saves your password: it is needed only to link, and the app then keeps the sign-in the server gives
it.

### Windows Credential Manager {#desktop-linking-credential-manager}

The app keeps its sign-in for each server in Windows Credential Manager: **Control Panel → Credential Manager →
Windows Credentials**, under "Generic Credentials", named `isshoni:v1:` and the server's address (for example
`isshoni:v1:https://watch.example.com`). It is stored for this PC only.

- **"Couldn't save the sign-in to Windows Credential Manager"**: the app works until you quit it, and then has to be
  linked again. Windows didn't let it store the entry, for example because of a policy on a PC that an organization
  manages. Signing out of Windows and back in, or a restart, helps when it was a passing problem.
- Deleting the entry signs the app out on this PC only: the server still lists the app on your Devices page until
  you remove it there, or until 90 days pass. **Sign out** in the app does both.
- Uninstalling isshoni signs it out of every server it can reach, and deletes its entries either way.
- A new PC, or a Windows profile copied to one, doesn't bring the sign-in along: link the app there again.

## The Windows app {#desktop}

Installing the app, its warnings and its updates are on [Desktop apps](/download#windows). These are the problems
the app itself points you to.

### Sound: what friends hear {#desktop-sound}

When you share with **Share sound** on, friends hear everything your PC plays **except**:

- **Voice apps**: Discord, TeamSpeak and the other apps on isshoni's built-in list are always kept out, so friends
  never hear themselves echoed back. The list can't be turned off.
- **Apps that use your microphone**: when an app uses the microphone, isshoni asks whether to keep it out of your
  stream, and remembers the answer (Settings → Audio). A voice chat in a browser tab (Discord in the browser) can only
  be kept out together with the whole browser; the Discord app works better.
- **isshoni itself**, and the shares you watch in it.

The share sheet's **What friends hear** lists what is kept out right now. Muting your PC or turning its volume down
doesn't change what friends hear: **Pause** or **Share sound** off does.

**"Voice apps can't be kept out on this PC"**: on Windows 10, and on a Windows 11 PC where the sound check found that
Windows' per-app capture doesn't work, isshoni captures everything your default speakers or headphones play (Settings
→ About shows "Everything on this PC"), voice apps included. Then:

- set your voice app's output to another device than Windows' default output (in the voice app's own voice
  settings), so friends don't hear it; or
- share without sound; or
- on Windows 10, update to Windows 11, where voice apps are kept out.

Other sound messages:

- **No speakers or headphones are connected**: Windows has no output device, so there is nothing to capture. Connect
  one.
- **Another app is using your speakers on its own (exclusive mode)**: close that app, or turn off "Allow applications
  to take exclusive control of this device" in the device's Sound properties (Sound settings → the device →
  Properties → Advanced), then run the sound check again (Settings → Audio).
- **isshoni is playing in a browser on this PC**: you are also watching your room in a browser on the same PC, whose
  sound would reach friends; pause it there.

### The update didn't install {#desktop-update-failed}

The app tried to install an update twice, and the version it runs is still the old one, so it stopped trying for that
version ([how updates work](/download#windows-updates)). Usually something blocked the installer: an antivirus, an
app-control policy, Smart App Control, or a full disk.

1. Quit isshoni from its tray icon.
2. Install the new version by hand from your server's Download page, or with its PowerShell command
   ([Install with PowerShell](/download#windows-powershell)): both update the app in place and keep your settings and
   sign-ins. The installer's and the script's messages say what went wrong.
3. If your antivirus quarantined a file of isshoni, restore it and allow it, or ask its vendor: 0.x builds aren't
   code-signed yet, which some scanners distrust.

Other update messages: **"Update info is out of date"** means that no release came for months, or that your PC's clock
is wrong: set the clock, and get the new version from your server's Download page. **"isshoni X is available"** with a
link instead of an update means that this copy runs from another folder than the installed app (a copy that was moved
or unpacked), which never updates itself: install the app.

### Smart App Control and other app-control policies {#desktop-smart-app-control}

Smart App Control (on some fresh installs of Windows 11) blocks apps and libraries that aren't code-signed, and the
0.x builds aren't ([Smart App Control](/download#windows-smart-app-control)). Organizations can block them the same
way (App Control for Business, AppLocker). What you see:

- the installer, or the PowerShell command, doesn't start ("Windows blocked the installer"), or the PowerShell command
  says "Smart App Control is on" and downloads nothing;
- in the app, sharing says that Windows blocked part of isshoni's video engine: the video encoder's libraries were
  blocked. If the app offers **Enable Cisco OpenH264**, that encoder is signed by Cisco and may be allowed.

Check Smart App Control in Windows Security → App & browser control → Smart App Control settings. While it is on,
share from Chrome or Edge ([Share](/guide/#share)); watching in the browser works too. On a PC your organization
manages, ask its administrators. The app will be code-signed once the project's application for free code signing is
approved ([Code signing policy](/code-signing)).

### WebView2 {#desktop-webview2}

isshoni shows its pages in Microsoft Edge WebView2 and needs its runtime, **version 120 or newer**. Windows 10 and 11
normally have it and keep it current with Edge. When it is missing, the installer installs it (it downloads it from
Microsoft); when that fails, the installer, the PowerShell command or the app says that WebView2 is needed.

1. Install the runtime from Microsoft's [WebView2 page](https://developer.microsoft.com/microsoft-edge/webview2/)
   (the Evergreen Bootstrapper).
2. If it is installed but too old, updates of Edge and WebView2 are probably blocked on this PC (an organization's
   policy, or a tool that turned updates off): let Microsoft Edge update (Edge → Settings → About Microsoft Edge).
3. Start isshoni again, or run the installer again.

### The OpenSSH Client, for the PowerShell install {#desktop-openssh}

The PowerShell command checks the signature of the download with `ssh-keygen` from Windows' **OpenSSH Client**
(OpenSSH 8.1 or newer), and installs nothing without it. Current Windows 10 and 11 have it. To add it:

- Windows 11: Settings → System → Optional features → View features, search "OpenSSH Client", Add.
- Windows 10: Settings → Apps → Optional features → Add a feature → OpenSSH Client.
- Or in a PowerShell run as administrator: `Add-WindowsCapability -Online -Name OpenSSH.Client~~~~0.0.1.0`

Then open a new PowerShell window and run the command again. If Git for Windows is installed, its `ssh-keygen` is
used when Windows' isn't there. "Your ssh-keygen is older than OpenSSH 8.1" means an old copy was found: update
Windows, or install from your server's Download page instead.

### Windows N and KN editions {#desktop-windows-n}

The N and KN editions of Windows (sold in Europe and Korea) come without Windows' media features. Without them, H.264
video, which isshoni uses, doesn't play: **watching fails in the app and in Microsoft Edge**, and the app can't use
Windows' own video encoders for sharing. The app says so when it finds an N edition.

Install the [Media Feature Pack](#desktop-media-feature-pack). Until then, Chrome and Firefox can watch, as they decode
H.264 themselves.

### Installing the Media Feature Pack {#desktop-media-feature-pack}

- Windows 11: Settings → System → Optional features → View features, search "Media Feature Pack", Add.
- Windows 10: Settings → Apps → Optional features → Add a feature → Media Feature Pack.

Restart the PC afterwards. It brings back H.264 decoding, for watching in the app and in Edge, and Windows' Media
Foundation encoders, which isshoni uses for sharing when no GPU encoder works.

### Graphics drivers {#desktop-gpu-driver}

isshoni encodes video on the graphics card (NVIDIA NVENC, Intel Quick Sync, AMD AMF) when it can, and that needs a
current driver:

- **NVIDIA**: driver **531.61 or newer**. With an older one the app says "Update your graphics driver (NVIDIA 531.61
  or newer)" and uses another encoder meanwhile.
- **AMD and Intel**: a current driver from AMD or Intel, or from Windows Update.

Get the driver from your graphics card's maker (NVIDIA, AMD or Intel) or your PC's maker, then restart isshoni.
Settings → Sharing shows the encoder isshoni found. Without a GPU encoder it uses Windows' own encoder (up to 1080p at
30 frames per second; not on N editions without the [Media Feature Pack](#desktop-media-feature-pack)), or Cisco's
OpenH264 when you turn it on there.

### Full-screen games show black {#desktop-fullscreen-games}

A game in **exclusive full-screen** mode can show friends a black picture or your desktop instead of the game, and on
Windows 10 a share of the whole screen can stop when the game changes the display mode. Set the game to **borderless**
(also called "windowed full-screen" or "full-screen windowed") in its display settings: it looks the same, and isshoni
captures it. Most current games use that mode already.

### A yellow border around a shared window {#desktop-windows10-border}

On Windows 10, Windows draws a yellow border around a window while it is being shared. That is Windows showing you
that the window is captured; it is expected, and friends don't see it. Windows 10 has no way to turn it off. Sharing a
whole screen shows no border there. On Windows 11 the border is left out when Windows allows it, and isshoni shows its
own **Sharing** indicator instead.

### When the app crashes {#desktop-crash-dumps}

The app writes its own crash reports to `%LOCALAPPDATA%\isshoni\logs\crash-<time>.txt`, next to its logs. For a crash
in the video engine, Windows can keep a crash dump: in a Command Prompt run as administrator,

```bat
reg add "HKLM\SOFTWARE\Microsoft\Windows\Windows Error Reporting\LocalDumps\isshoni.exe" /v DumpType /t REG_DWORD /d 1 /f
```

The next crash leaves `isshoni.exe.<number>.dmp` in `%LOCALAPPDATA%\CrashDumps`. Remove the setting afterwards with
`reg delete "HKLM\SOFTWARE\Microsoft\Windows\Windows Error Reporting\LocalDumps\isshoni.exe" /f`.

Every release publishes the engine's debug symbols as `isshoni-desktop_<version>_windows_amd64_symbols.zip`, which
make a dump readable. A dump holds part of the app's memory, which can include server addresses and what was on your
screen: open an [issue](https://github.com/MoonWX/isshoni/issues) that says you have one, and don't attach the dump
publicly. A "Copy diagnostics" button comes later ([Copying diagnostics](#copy-diagnostics)).

## Other problems {#other}

### The certificate is not issued {#certificate-not-issued}

The install script waits up to 3 minutes for the certificate. If it gives up, it prints doctor's report; the
[tls](#doctor-tls) and [dns](#doctor-dns) checks name the cause. The usual ones: port 80 and 443 closed in the cloud
firewall, a domain whose A record points elsewhere, or a domain that was just created. Fix it and run the install
script again: it waits again and then prints the setup link. Until the certificate is there, `http://` shows "isshoni
is getting its certificate".

### UDP is blocked for a friend {#udp-blocked}

Some networks (offices, hotels, schools) block UDP or everything but HTTPS. isshoni then carries media over TCP on
port 443, the same port as the web page, so it still works, with more delay on a weak network. Nothing to set up:
the browser picks the path by itself. Behind your own reverse proxy that fallback uses port 7882 instead.

### A server at home, or CGNAT {#home-server}

At home the router must forward TCP 80, 443, 7882 and UDP 7882 to the server, and a domain with dynamic DNS keeps the
link working when your address changes. If your provider shares one address between many customers (CGNAT),
`isshoni doctor` says so, and no forward can help: use a VPS. [A server at home](/install/vps#home).

### Firefox: the first join takes a minute {#firefox-first-join}

Firefox downloads its H.264 video decoder (OpenH264, from Cisco) in the background the first time it needs it, which
can take about a minute. Until then a Firefox viewer gets no video. If it still has none after a few minutes, reload
the page.

### Local Network permission on a Mac {#macos-local-network}

On macOS 15 and later, a browser needs Local Network access to reach a server on your own network: see
[Local Network access on a Mac](#ct-mac_local_network). A server on the internet doesn't need it.

### iPhone and iPad: tap to unmute {#ios-unmute}

iOS starts videos muted until you tap: tap once, and the sound follows. iOS also pauses video when you switch apps or
lock the screen; coming back resumes it, and sound may need one more tap.

### A shared video shows black {#drm-black}

Streaming services (Netflix and the like) protect their video against screen capture, so a share of it shows a black
picture. That is the protection working, not isshoni; it happens on every platform.

### Copying diagnostics {#copy-diagnostics}

A "Copy diagnostics" button that collects everything for a bug report comes in a later milestone (M5). Until then,
attach `sudo isshoni doctor --json` (it shows your server's addresses and domain) and the connection test's "Copy
result" (without IP addresses) to an issue.
