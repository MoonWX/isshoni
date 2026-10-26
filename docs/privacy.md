# Privacy

isshoni is software you run yourself: whoever runs an isshoni server decides who uses it and holds its data. The
isshoni project runs no server of its own and receives nothing from yours. This page says what an isshoni server
stores, every connection it makes to others, and what the installer and this site do.

## The trust model {#trust}

- **The server is trusted.** Video and sound are encrypted on their way to and from the server, but the server, and
  whoever runs it, could see the streams. There is no end-to-end encryption yet.
- **No recording, no hidden viewers.** isshoni has no code that records a share, and nobody watches unseen: admins
  show up in "Watching now" like everyone else.
- **What the server stores**: usernames and their password hashes (argon2id); sessions and devices (hashes of their
  tokens, a name, when and from which IP address they were last seen); invites, rooms and push subscriptions; an
  audit log of sign-ins and admin actions with IP addresses, deleted after 30 days. There are no HTTP access logs,
  and nothing that contains a token or a connection's session description is logged.
- **What never leaves your device** (with the desktop apps, which come later): window titles, the list of your apps,
  the list of apps left out of your sound, and "What friends hear". A share is called "Screen" or "Window" unless
  you name it.
- **No telemetry.** The server never contacts the project; its only call home is the optional daily release check
  below. The desktop apps will check GitHub Releases for updates, and that can be turned off.

## Linked apps {#linked-apps}

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
