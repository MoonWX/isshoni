# Docker

isshoni runs from the image `ghcr.io/moonwx/isshoni` (amd64 and arm64) with Docker Compose. The image holds only the
static program; it runs as an unprivileged user with a read-only file system. Its data lives in a volume.

<ReleaseNotice />

## Start {#start}

```sh
curl -fsSLO https://github.com/MoonWX/isshoni/releases/latest/download/compose.yaml
echo ISSHONI_DOMAIN=share.example.com > .env   # leave this out for a certificate for the server's IP address
docker compose up -d
docker compose exec isshoni isshoni setup-url
```

Open the printed link to create the admin account. Open TCP 80, 443, 7882 and UDP 7882 in your provider's firewall
([Choosing a VPS and its firewall](/install/vps)); Docker publishes the ports on the server itself.

isshoni gets its certificate from Let's Encrypt. Using it means you accept the Let's Encrypt Subscriber Agreement:
https://letsencrypt.org/repository/

If ports 80 and 443 already belong to a web server or proxy on this host, `docker compose up` fails with "port is
already allocated": run isshoni [behind that proxy](/install/reverse-proxy#docker) instead.

## The `.env` file {#env}

`compose.yaml` reads two values from a file `.env` next to it:

| Variable | Meaning |
|---|---|
| `ISSHONI_DOMAIN` | The domain friends open. Set: a certificate for that name. Empty or missing: a certificate for the server's public IP address |
| `ISSHONI_PUBLIC_IP` | The server's public IP address. Empty or missing: detected with STUN |

An empty value counts as unset, so a missing `.env` file gives an IP certificate and STUN detection. Inside Docker's
network the container only sees a private address, so isshoni must learn its public one: from STUN, or from
`ISSHONI_PUBLIC_IP` when STUN is blocked. Every other setting of the [configuration](/reference/config) is an
`ISSHONI_*` variable too; add it under `environment:` in `compose.yaml`, or mount a configuration file read-only at
`/etc/isshoni/isshoni.toml`.

Changed `.env`? Run `docker compose up -d`, which recreates the container with the new values; `docker compose
restart` would keep the old ones.

## Operations {#operations}

| Task | Command |
|---|---|
| Setup link | `docker compose exec isshoni isshoni setup-url` |
| Diagnostics | `docker compose exec isshoni isshoni doctor` |
| Logs (JSON lines) | `docker compose logs -f` |
| Upgrade | `docker compose pull && docker compose up -d` |
| Backup to this host | `(umask 077; docker compose exec -T isshoni isshoni admin backup --out - > isshoni-backup.tar.gz)` |
| Restore that backup | `docker compose exec -T isshoni isshoni admin restore --yes - < isshoni-backup.tar.gz` |

- **The backup holds the server's keys** (`secrets.json`) and its TLS private keys. The `umask 077` in a subshell
  makes the file readable by you only; a plain redirect would create it with your usual mode, often readable by every
  user of the host.
- **The restore needs `--yes`**: with `-T` there is no terminal to ask on, and the archive comes in on standard input.
- **Upgrades**: before it changes its database, the server writes a backup into the volume (`backups/`).
- **Pinning a version**: `compose.yaml` uses `:latest`. To stay on one minor version during 0.x, change the image to
  `ghcr.io/moonwx/isshoni:0.1` (or an exact `:0.1.0`); the release notes say when a new minor version needs care.

### When isshoni refuses to start {#refuses-to-start}

Some problems can't be fixed by a restart, for example a database from a newer isshoni after a downgrade. Docker then
restarts the container in a loop (`restart: unless-stopped`). `docker compose logs --tail 50 isshoni` shows the
reason and the exact command to fix it. A restore from a backup in the volume goes like this, in this order:

```sh
docker compose stop
docker compose run --rm isshoni admin restore --offline /var/lib/isshoni/backups/<file>
docker compose up -d
```

`docker compose stop` comes first: the restore refuses to run while any container still uses the data.

## Host settings {#host}

**UDP buffers.** Video needs larger UDP buffers than Linux allows by default, and Docker can't set them for a
container. Set them once on the host:

```sh
printf 'net.core.rmem_max=8388608\nnet.core.wmem_max=8388608\n' | sudo tee /etc/sysctl.d/60-isshoni.conf && sudo sysctl --system
```

`isshoni doctor` in the container reports the buffer size in use.

**Published ports bypass ufw.** Docker's own firewall rules come before ufw's, so `ufw deny` doesn't protect a
published port, and ufw needs no rule for isshoni's ports. Your provider's cloud firewall still applies. To limit a
port, publish it on one address (`"203.0.113.7:443:443/tcp"`) or use Docker's `DOCKER-USER` chain.

**IPv6.** Docker's default network has no IPv6. IPv6 visitors then reach isshoni through Docker's proxy, which hides
their addresses: they all share one address's limits for sign-in attempts and connections, and one stranger can lock
them all out. If your domain has an AAAA record, use the host network instead (below).

## Host network instead of Docker's network {#host-network}

`compose.host.yaml` runs isshoni on the host's network: for IPv6, or to avoid Docker's address translation. Use it
instead of `compose.yaml`, not next to it:

```sh
curl -fsSLO https://github.com/MoonWX/isshoni/releases/latest/download/compose.host.yaml
docker compose -f compose.host.yaml up -d
docker compose -f compose.host.yaml exec isshoni isshoni setup-url
```

- The container runs as root with only the capability to bind ports below 1024.
- The data is in `/var/lib/isshoni` on the host, not in a volume.
- **The host's firewall applies**: Docker adds no rules here, so open TCP 80, 443, 7882 and UDP 7882 in ufw or
  firewalld, as for the [install script](/install/#steps).

**Switching between the two** needs a one-time copy of the data, with the old containers stopped. From the volume of
`compose.yaml` to the host directory:

```sh
docker run --rm -v isshoni_data:/from -v /var/lib/isshoni:/to alpine sh -c 'cp -a /from/. /to/ && chown -R 0:0 /to'
```

The `chown` is needed: on the host network isshoni runs as root without the right to write files of another user. For
the other direction, swap the two mounts and end with `chown -R 65532:65532 /to`.

## A directory instead of a volume {#bind-mount}

To keep the data in a directory of your choice, give it to the image's user first and mount it at
`/var/lib/isshoni`:

```sh
mkdir data && sudo chown 65532:65532 data
```

```yaml
    volumes:
      - ./data:/var/lib/isshoni
```

The data must be mounted: without a volume or a directory at `/var/lib/isshoni`, isshoni refuses to start, so that a
recreated container never loses your users and rooms.

## Rootless Docker and Podman {#rootless}

Rootless Docker and Podman need `net.ipv4.ip_unprivileged_port_start=80` on the host to publish ports 80 and 443, and
their user-mode networking (slirp4netns, pasta) makes UDP media slow. Rootful Docker or the
[install script](/install/) is recommended.
