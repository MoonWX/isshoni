# Certificates (TLS modes)

Browsers only share screens, play sound, install apps and send notifications on a page with a trusted HTTPS
certificate. isshoni has four ways to get one, the key `tls.mode` in `/etc/isshoni/isshoni.toml`
(`ISSHONI_TLS_MODE` in Docker). The install script's question chooses between the first two.

| Mode | Certificate | When |
|---|---|---|
| `auto` | Let's Encrypt, for your domain | You have a domain whose A record points to the server. The default when `domain` is set |
| `ip` | Let's Encrypt, for the server's public IP address | No domain. The default when `domain` is empty |
| `manual` | Your own files | You get certificates another way |
| `off` | None: your reverse proxy has it | Another web server owns ports 80 and 443 |

To change the mode later, edit the file and restart isshoni (`sudo systemctl restart isshoni`); `sudo isshoni config
check` tells you first whether the new settings are valid.

## auto: a domain {#auto}

```toml
domain = "share.example.com"
```

isshoni gets a certificate for the domain from Let's Encrypt and renews it by itself. Let's Encrypt checks that the
domain is yours through port 80 or 443, so at least one of them must be open in every firewall, and the domain's A
record (and its AAAA record, if it has one) must point to this server. The install script runs `isshoni doctor`
before the first start to catch a wrong record, because Let's Encrypt limits how often a name may fail its check.

isshoni gets its certificate from Let's Encrypt. Using it means you accept the Let's Encrypt Subscriber Agreement:
https://letsencrypt.org/repository/

## ip: no domain {#ip}

isshoni gets a certificate for the server's public IP address. Let's Encrypt has issued these generally since January
2026, as its short-lived "six-day" certificates (160 hours), and isshoni renews them by itself. Friends then open
`https://203.0.113.7/` (your address).

- The address must be public. A server at home behind a router, or behind CGNAT, can't get one: use a domain there.
- **The address must not change.** A new address means a new link for everyone, and installed apps and notifications
  stop working. On AWS and Google Cloud attach a static address before you install ([Choosing a VPS](/install/vps#aws)).
- A domain is easier to remember and to move to another server. You can switch to one later.

isshoni gets its certificate from Let's Encrypt. Using it means you accept the Let's Encrypt Subscriber Agreement:
https://letsencrypt.org/repository/

## manual: your own certificate {#manual}

```toml
domain = "share.example.com"

[tls]
mode = "manual"
cert_file = "/etc/isshoni/fullchain.pem"
key_file = "/etc/isshoni/privkey.pem"
```

- `cert_file` holds the whole chain and `key_file` the matching private key, both as PEM.
- isshoni runs as the user `isshoni`, which must be able to read both files, for example:
  `sudo chgrp isshoni /etc/isshoni/privkey.pem && sudo chmod 640 /etc/isshoni/privkey.pem`. Keep them readable when
  your tool renews them.
- isshoni notices new files within a minute and loads them without a restart. A broken new pair is refused, and the
  old one keeps serving.
- `isshoni doctor` warns two weeks before the certificate expires.

With the install script: `--tls-mode manual` with `ISSHONI_DOMAIN`, `ISSHONI_TLS_CERT_FILE` and
`ISSHONI_TLS_KEY_FILE`.

## off: behind your reverse proxy {#off}

Your proxy terminates HTTPS and forwards to isshoni over plain HTTP. Off mode needs the address friends open,
`public_url` (`ISSHONI_PUBLIC_URL` for the install script and Docker). [Behind a reverse proxy](/install/reverse-proxy)
has the setup for Caddy, nginx, Traefik and Docker.

## Why there is no self-signed certificate {#self-signed}

A self-signed certificate is no option for a group of friends: installed web apps, service workers (and with them
notifications), iPhone Safari and the coming desktop apps refuse it or warn about it every time, and there is no
one-click way around that for the people you invite. So every mode ends with a certificate that browsers trust, or a
proxy that has one.
