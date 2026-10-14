---
layout: home
title: isshoni
titleTemplate: Screen sharing for friends, on your own server

hero:
  name: isshoni
  text: Share screens together, on your own server
  tagline: Everyone shares and watches at the same time, with sound, up to 1080p60 by default. Friends watch in the browser, phones included.
  image:
    src: /icon.svg
    alt: ''
  actions:
    - theme: brand
      text: Install
      link: /install/
    - theme: alt
      text: Guide for friends
      link: /guide/
    - theme: alt
      text: Source on GitHub
      link: https://github.com/MoonWX/isshoni
---

## What isshoni is

isshoni (一緒に, "together") is a self-hosted server for sharing screens and system audio with a group of friends,
while the group keeps talking in its usual voice app (Discord, TeamSpeak, …). It is one program that holds the web
app, the accounts and a WebRTC SFU, so each share goes up to your server once, however many friends watch. You run it
on a small VPS; friends open an invite link and watch in their browser, on a computer or a phone. It is open source
under the Apache License 2.0, with no telemetry.

![A room in isshoni: sora's game window on the stage, mika's chess window next to it, both shared at the same time](/screenshot.webp)

<ReleaseNotice />

## Install in 2 minutes

On a Linux server with systemd and a public IP address:

```sh
curl -fsSL https://moonwx.github.io/isshoni/install.sh | sh
```

The script asks one question (your domain name, or Enter for the server's IP address), offers to open the ports in
ufw or firewalld, gets a certificate from Let's Encrypt and prints the link that creates your admin account. Open TCP
80, 443, 7882 and UDP 7882 in your provider's firewall too: [Choosing a VPS and its firewall](/install/vps).
Everything the script does: [Install script](/install/).

Or with Docker Compose:

```sh
curl -fsSLO https://github.com/MoonWX/isshoni/releases/latest/download/compose.yaml
echo ISSHONI_DOMAIN=share.example.com > .env   # leave this out for a certificate for the server's IP address
docker compose up -d
docker compose exec isshoni isshoni setup-url
```

More: [Docker](/install/docker). isshoni gets its certificate from Let's Encrypt. Using it means you accept the Let's
Encrypt Subscriber Agreement: <https://letsencrypt.org/repository/>

## Voice apps are kept out

When you share a game or a film, your friends should hear it, not their own voices coming back from your voice app.
isshoni's planned desktop apps share all of your computer's sound except the voice apps (Discord, TeamSpeak and the
like), and they find those apps by themselves.

**The desktop apps are not here yet**: Windows comes in a later milestone (M2), then macOS (M3) and a Linux share agent
(M4); see [Desktop apps](/download). Until then you share from Chrome or Edge on a computer. The best choice there is
a window together with its audio, which leaves the voice app out (Windows 11, or macOS 14.2 and later, with a current
Chrome or Edge). A whole-screen share with sound includes the voice apps, and isshoni warns you before you start one.

## Links

- [Guide for friends](/guide/): joining, watching and sharing.
- [Troubleshooting](/troubleshooting): every `isshoni doctor` check and every connection test result.
- [Security](/security): reporting a vulnerability, the release keys, verifying a download.
- [Privacy](/privacy): what the server stores, and every connection it makes.
- [The project plan](https://github.com/MoonWX/isshoni/blob/main/docs/PLAN.md) and the
  [M1 design](https://github.com/MoonWX/isshoni/blob/main/docs/m1/README.md), on GitHub.
