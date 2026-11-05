# Privacy

isshoni is software you run yourself: whoever runs an isshoni server decides who uses it and holds its data. The
isshoni project runs no server of its own and receives nothing from yours. This page says what an isshoni server
stores, every connection it makes to others, what the desktop app keeps and sends, and what the installer and this
site do.

## The trust model {#trust}

- **The server is trusted.** Video and sound are encrypted on their way to and from the server, but the server, and
  whoever runs it, could see the streams. There is no end-to-end encryption yet.
- **No recording, no hidden viewers.** isshoni has no code that records a share, and nobody watches unseen: admins
  show up in "Watching now" like everyone else.
- **What the server stores**: usernames and their password hashes (argon2id); sessions and devices (hashes of their
  tokens, a name, when and from which IP address they were last seen); invites, rooms and push subscriptions; an
  audit log of sign-ins and admin actions with IP addresses, deleted after 30 days. There are no HTTP access logs,
  and nothing that contains a token or a connection's session description is logged.
- **What never leaves your device**: window titles, the list of your apps, the list of apps left out of your sound,
  and "What friends hear". A share is called "Screen" or "Window" unless you name it
  ([The desktop app](#desktop)).
- **No telemetry.** The server never contacts the project; its only call home is the optional daily release check
  below. The desktop app checks GitHub Releases for updates, and that can be turned off ([below](#desktop)).

## Linked apps {#linked-apps}

This section says what a server keeps for the desktop apps (the first, for Windows, comes with isshoni 0.2:
[Desktop apps](/download)). What the app keeps on your PC, and whom it contacts: [The desktop app](#desktop).

A desktop app signs in to a server once, when you link it to your account: you approve it in your browser, or sign
in with your username and password in the app ([how that sign-in is protected](/security#linked-apps)). For that,
the server keeps:

| Data | Table | Why | Kept until |
|---|---|---|---|
| Per linked app: the name (the computer's name unless you changed it), app kind, operating system, app version, when it was linked and last seen, its **last IP address**, and hashes of its tokens | `devices`, `device_tokens` | Signing in the apps; the Devices page | Removed or signed out; 90 days without a refresh (plus up to 1 hour); each token until it expires |
| Pending app links (a code waiting for approval): the same, plus the IP address the request came from | `device_codes` | The approval page | At most 70 minutes |

Only you see the list of your apps with their last-seen times and last IP addresses, on your account's Devices page.
Admins see how many apps each user has linked, and the audit log (kept 30 days), whose entries for linking, renaming
and removing an app carry its name and the IP address of the request.

## The desktop app {#desktop}

What the Windows app sends, to whom, and what it keeps on your PC. Like the server, it sends nothing to the isshoni
project.

**Its connections:**

| Destination | When | What | Off switch |
|---|---|---|---|
| The isshoni servers you link it to | While you use them | Your account's traffic, as in a browser: the rooms, the shares you watch and send, and what the server needs to know about the app (below) | Remove the server in the app |
| GitHub (`github.com` and its download hosts) | A few minutes after the app starts, then once a day, and when you click Check now | Requests for the latest release's update info and its signature, with `User-Agent: isshoni-desktop/<version>` and no cookies; with the beta channel also one request to GitHub's API for the list of releases; the installer of a newer version | "Check for updates automatically" in the installer or in Settings → Updates, or the policy `DisableUpdateChecks` ([Updates](/download#windows-updates)) |
| Cisco (`ciscobinary.openh264.org`) | Only when you turn on Cisco's OpenH264 encoder in Settings → Sharing | One HTTPS download of OpenH264 2.7.0 (`openh264-2.7.0-win64.dll.bz2`), checked against its known size and SHA-256 before it is used | Leave OpenH264 off (the default) |
| Microsoft | Only when the WebView2 Runtime is missing at install | The installer runs Microsoft's WebView2 bootstrapper, which downloads the runtime from Microsoft | None needed: current Windows has the runtime |

Windows itself checks a browser-downloaded installer with SmartScreen (Microsoft), as it does every download; the
PowerShell install and the app's own updates don't trigger that check. The app opens your server's pages and this
site in your browser when you click a link to them.

**Sent to a server when you link the app:** the device name, which is your computer's name unless you change it in
the app's setup (or later on the server's Devices page), the app kind, operating system and app version. The server
keeps them with the app's sign-in ([Linked apps](#linked-apps)).

**Never sent anywhere:** window titles, the names of the apps and windows you share (a share is called "Screen" or
"Window" unless you name it yourself), the list of your apps, the apps left out of your sound and your rules for
them, "What friends hear", sound levels, and the app's logs.

**Kept on your PC**, all in `%LOCALAPPDATA%\isshoni\`, for your Windows user on this PC only (nothing roams to other
PCs):

| What | Where | Why |
|---|---|---|
| Your settings | `settings.json` | The app's settings |
| The server list: each server's address and name, your username there, the app's device ID, the last room | `servers.json` | Opening your servers |
| The WebView2 profile (the pages' storage) | `WebView2\` | Showing isshoni's pages |
| Logs and crash reports | `logs\` | Troubleshooting; they stay on the PC unless you send them to someone. They never contain window titles, app names, passwords or tokens |
| OpenH264, when you turned it on | `codecs\` | The optional encoder |
| A downloaded update, until it is installed | `updates\` | Updates |

The app's sign-in to each server, its **refresh token**, is kept in **Windows Credential Manager** (entry
`isshoni:v1:<server address>`), stored for this PC only, with the device ID and your username. The access tokens
that the app gets with it are kept only in memory. Your password is never written to disk.
([How the sign-in is protected](/security#linked-apps).) The installer records its own settings in the registry under
`HKEY_CURRENT_USER\Software\isshoni`.

Uninstalling the app signs it out of its servers and removes its sign-ins from Credential Manager; with "Also remove
my settings and logs" it also removes `%LOCALAPPDATA%\isshoni\` ([Uninstall](/download#windows-uninstall)).

**No telemetry**: the app reports no usage, crashes or errors to anyone.

## The server's connections to others {#outbound}

Everything an isshoni server sends to a third party, and how to turn it off:

| Destination | When | What | Off switch |
|---|---|---|---|
| STUN (Cloudflare, Google) | Startup and every 10 min; every 30 s while a server that started without its public address waits for it, only while `public_ip` is `auto` | One UDP Binding request (reveals the server IP) | `network.stun_servers = []` stops STUN entirely (set `public_ip` to a literal then); a literal `public_ip` alone leaves one NAT check at startup |
| Let's Encrypt | Issuance, renewal; one HTTPS GET of the ACME directory for the clock check (doctor, also install.sh's pre-check; `auto`/`ip` modes) | ACME protocol; optional email | `tls.mode=manual` or `off` |
| GitHub API | Daily | One GET with `User-Agent: isshoni/<ver>` | `updates.release_check=false` |
| Browser push services (Google, Apple, Mozilla, Microsoft) | On notifications | Encrypted payload (RFC 8291); the service sees timing and size (padded) only | `push.enabled=false`, or users don't subscribe |

The server makes no OCSP requests: OCSP stapling is turned off, and certificates you provide yourself cause none
either. Nothing else leaves the server. Its logs contain no tokens, session descriptions, push endpoints or usernames.

The web app in your friends' browsers connects to your server only (it uses no STUN or TURN server), and
notifications reach a browser through its own push service once its user turns them on.

## The installer {#installer}

`install.sh` comes from this site (or from GitHub) and downloads the release from GitHub. Before the first start it
runs `isshoni doctor`, which asks the STUN servers above for the server's public address and, with a Let's Encrypt
certificate, reads Let's Encrypt's clock, as the server does. If `ssh-keygen` is missing it installs it from your
distribution's package mirrors. If `cosign` or a logged-in `gh` are installed, they contact Sigstore's and GitHub's
services to check the download. Nothing is sent to the project.

## This site {#site}

This site is static pages on GitHub Pages. It sets no cookies, has no analytics, no embedded content, and loads
nothing from any other site: its fonts and its search come with the pages, and the search runs in your browser. It
stores nothing about you. **GitHub hosts it and may log visitors' IP addresses**; see
[GitHub's privacy statement](https://docs.github.com/en/site-policy/privacy-policies/github-general-privacy-statement).
