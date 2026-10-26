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

<!-- The desktop app links to #desktop-linking from its "Can't reach the server" texts (docs/m2/03-desktop-app.md
§8.1, §9): never rename or remove that anchor. The section's first paragraph goes once the Windows app is released
(06 R14, which turns /download into the Windows page). -->

## Linking the app {#desktop-linking}

The desktop apps aren't released yet: the first, for Windows, comes with isshoni 0.2 and links only to a server that
runs 0.2 or later ([Desktop apps](/download)). This section describes linking as it will work.

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
