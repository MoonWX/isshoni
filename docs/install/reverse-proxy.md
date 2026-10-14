# Behind a reverse proxy

If a web server or proxy (Caddy, nginx, Traefik, …) already owns ports 80 and 443 on your server, run isshoni behind
it: the proxy keeps the certificate and forwards one domain to isshoni over plain HTTP. isshoni then runs with
`tls.mode = "off"`.

**Media does not go through the proxy.** Video and sound use port 7882 directly, so **UDP 7882 and TCP 7882 must
still be reachable** from the internet: open them in your provider's firewall and on the server
([Ports](/install/vps#ports)). Without a port 443 of its own, isshoni carries media over TCP on 7882 only.

## Install {#install}

```sh
curl -fsSL https://moonwx.github.io/isshoni/install.sh | ISSHONI_PUBLIC_URL=https://share.example.com sh -s -- --tls-mode off
```

`ISSHONI_PUBLIC_URL` is the address friends open; off mode needs it. isshoni then serves plain HTTP on
`127.0.0.1:8080`, which only programs on the same machine can reach. The configuration it writes,
`/etc/isshoni/isshoni.toml`, has these values among its comments:

```toml
public_url = "https://share.example.com"

[tls]
mode = "off"

[network]
trusted_proxies = ["127.0.0.0/8", "::1/128"]
```

**Client addresses.** isshoni limits sign-in attempts and connections per client address, so it must see your
friends' addresses, not the proxy's. It reads `X-Forwarded-For` and `X-Forwarded-Proto` only from the addresses in
`network.trusted_proxies`. A proxy on the same machine needs no setting: with the loopback address above, `127.0.0.0/8`
and `::1/128` are trusted. A proxy on another machine needs `listen.http` on an address it can reach and its own
address in `network.trusted_proxies`, for example:

```toml
[listen]
http = "10.0.0.5:8080"

[network]
trusted_proxies = ["10.0.0.2/32"]
```

Restart isshoni after a change: `sudo systemctl restart isshoni`.

The proxy must:

- forward `https://share.example.com` to `http://127.0.0.1:8080`;
- pass WebSocket upgrades: the signaling connection is a WebSocket at `/ws`, which stays open while the app is open
  (it sends a ping every 15 seconds, so the usual idle timeouts are fine);
- set `X-Forwarded-For` and `X-Forwarded-Proto`.

## Caddy {#caddy}

Caddy gets the certificate, passes WebSockets and sets the forwarded headers by itself:

```
share.example.com {
	reverse_proxy 127.0.0.1:8080
}
```

## nginx {#nginx}

```nginx
map $http_upgrade $connection_upgrade {
    default upgrade;
    ''      close;
}

server {
    listen 443 ssl;
    listen [::]:443 ssl;
    server_name share.example.com;

    ssl_certificate     /etc/letsencrypt/live/share.example.com/fullchain.pem;
    ssl_certificate_key /etc/letsencrypt/live/share.example.com/privkey.pem;

    location / {
        proxy_pass http://127.0.0.1:8080;
        proxy_http_version 1.1;
        proxy_set_header Host $host;
        proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
        proxy_set_header X-Forwarded-Proto $scheme;
        proxy_set_header Upgrade $http_upgrade;
        proxy_set_header Connection $connection_upgrade;
    }
}
```

Keep your usual `server` block on port 80 that redirects to HTTPS and answers your certificate tool's challenges.

## Traefik {#traefik}

For Traefik on the host (not in a container), as dynamic configuration of its file provider. Traefik passes
WebSockets and sets the forwarded headers by itself:

```yaml
http:
  routers:
    isshoni:
      rule: Host(`share.example.com`)
      entryPoints: [websecure]
      service: isshoni
      tls:
        certResolver: letsencrypt   # the name of your certificate resolver
  services:
    isshoni:
      loadBalancer:
        servers:
          - url: http://127.0.0.1:8080
```

Traefik in a container reaches isshoni the way the Docker section below describes.

## Docker behind the host's proxy {#docker}

With Docker, use this complete file **instead of** `compose.yaml` (an override file can't remove its ports 80 and
443). Your proxy on the host forwards `https://share.example.com` to `127.0.0.1:8080`:

```yaml
# isshoni behind your own reverse proxy on this host. Your proxy forwards https://share.example.com to 127.0.0.1:8080.
# Put ISSHONI_PUBLIC_URL=https://share.example.com in a .env file next to this file.
# Docs: https://moonwx.github.io/isshoni/install/reverse-proxy#docker
name: isshoni
services:
  isshoni:
    image: ghcr.io/moonwx/isshoni:latest
    restart: unless-stopped
    ports:
      - "127.0.0.1:8080:8080/tcp" # plain HTTP for the proxy on this host only; never publish it on all addresses
      - "7882:7882/udp"           # media, direct (not through the proxy)
      - "7882:7882/tcp"           # ICE-TCP, direct (443 belongs to the proxy)
    environment:
      ISSHONI_TLS_MODE: "off"
      ISSHONI_PUBLIC_URL: ${ISSHONI_PUBLIC_URL:?set ISSHONI_PUBLIC_URL=https://your.domain in .env}
      ISSHONI_LISTEN_HTTP: 0.0.0.0:8080                  # inside the container; the mapping above keeps it on loopback
      ISSHONI_NETWORK_TRUSTED_PROXIES: 172.30.89.0/24    # this file's network: the proxy's connections arrive from its gateway
      ISSHONI_PUBLIC_IP: ${ISSHONI_PUBLIC_IP:-}          # empty: detected with STUN
    networks: [isshoni]
    volumes:
      - data:/var/lib/isshoni
    read_only: true
    tmpfs:
      - /tmp
      - /run/isshoni:uid=65532,gid=65532,mode=0750
    cap_drop: [ALL]
    security_opt:
      - no-new-privileges:true
    stop_grace_period: 20s
    logging:
      driver: json-file
      options: { max-size: "10m", max-file: "3" }
networks:
  isshoni:
    ipam:
      config:
        - subnet: 172.30.89.0/24   # fixed, so ISSHONI_NETWORK_TRUSTED_PROXIES can name it; any free private /24 works
volumes:
  data:
```

- **`0.0.0.0:8080` inside, `127.0.0.1` outside.** Off mode listens on the loopback address by default, which a
  published port can't reach inside a container. Publishing on `127.0.0.1` keeps plain HTTP off the internet.
- **Why the trusted range is the compose network.** The proxy's connections reach the container through Docker's port
  forwarding, so they come from the network's gateway (`172.30.89.1`), not from `127.0.0.1`. Without that range in
  `ISSHONI_NETWORK_TRUSTED_PROXIES` every friend would have the gateway's address. Only isshoni, and a proxy container
  you attach, are on this network, so trusting it trusts nobody else.
- **A proxy that runs in Docker too** joins this network (in its own compose file:
  `networks: { isshoni: { external: true, name: isshoni_isshoni } }`) and forwards to `http://isshoni:8080`; the
  `8080` mapping can then go.
- The proxy's configuration is the one above (WebSocket upgrade for `/ws`, upstream `127.0.0.1:8080`). Port 7882 must
  be open in your provider's firewall; [published ports bypass ufw](/install/docker#host).
- The setup link, diagnostics, backup and restore are the commands of the [Docker page](/install/docker#operations).
