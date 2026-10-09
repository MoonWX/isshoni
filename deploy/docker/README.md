# deploy/docker

The server's container image and its test; `docs/m1/06-deploy-and-ci.md` §6 explains the design. The two compose
files that people download are next door: `deploy/compose.yaml` (bridge network, the default) and
`deploy/compose.host.yaml` (host network).

| File | What it is |
|---|---|
| `Dockerfile` | the image: distroless `static-debian13:nonroot` pinned by digest, plus the static binary. goreleaser builds it at a release |
| `rootfs/` | the two directories the image adds, each with a `.keep` file because Git has no empty directories |
| `test/docker-build.sh` | builds the image from the working tree without goreleaser, from the same kind of build context |
| `test/docker-smoke.sh` | the smoke test (06 §11.3) |
| `test/compose.smoke*.yaml` | the test's overrides for the two compose files: the local image and a throwaway certificate |

## The image

- **User** `65532:65532` (distroless `nonroot`). `compose.host.yaml` runs it as root with `NET_BIND_SERVICE` as its
  only capability, because only root keeps a capability in the host's network namespace.
- **`/var/lib/isshoni`** (0700) and **`/run/isshoni`** (0750) belong to 65532. Docker copies the owner and the mode
  into a new named volume, so the data volume is writable without a `chown`, and the server finds its data directory
  private, as it wants it. The Dockerfile copies each directory on its own: one `COPY --chown` of `rootfs/` onto `/`
  would also hand `/var`, `/var/lib` and `/run` to 65532.
- **No `VOLUME`.** An anonymous volume would hide a missing mount. With `ISSHONI_IN_CONTAINER=1` the server refuses
  to start (exit 78) when `/var/lib/isshoni` is not a mount, and says how to mount one.
- **No config file.** The config comes from `ISSHONI_*` variables; an empty one counts as unset.
- **`HEALTHCHECK`** is `isshoni healthcheck`: liveness, asked on the admin socket in `/run/isshoni`, where the compose
  files mount a tmpfs because the root filesystem is read-only.

## The smoke test

```sh
task docker:smoke              # builds the web app, the notices and the image, then runs every part
task docker:smoke -- -s host   # flags after -- go to test/docker-smoke.sh; -h lists them
```

It needs Docker with compose and buildx, curl, openssl and jq, and the host's ports 80, 443 and 7882. Its compose
project is `isshoni-smoke`, so it never touches an install named `isshoni`. CI runs it on linux/amd64 (job
`docker-smoke`).

| Part | What it asserts |
|---|---|
| `image` | the `HEALTHCHECK` command, the user, and that the binary has the version it was built with |
| `novolume` | `docker run IMAGE` exits 78 within 10 s and prints "Mount a volume at /var/lib/isshoni (see compose.yaml)" |
| `bridge` | `compose.yaml` with manual TLS: healthy within 90 s; `https://smoke.test/api/v1/info` answers with the version; `docker compose exec -T isshoni isshoni setup-url` prints the link; the admin account survives `down` and `up`; `ReadonlyRootfs` and `CapDrop: [ALL]` |
| `backup` | in `bridge` and `host`, the commands of 06 §6.4 as documented: the archive written under `umask 077` is `0600`, `restore --yes -` succeeds, the server is healthy again within 60 s and the admin is still there |
| `ipv6` | in `bridge`: a client on `[::1]` is seen as the compose network's gateway, not as `::1` (the warning of 06 §6.4). Skipped with a note when port 443 does not answer on `[::1]` |
| `host` | `compose.host.yaml`: what `bridge` asserts, plus the host network, user `0:0`, `NET_BIND_SERVICE` as the only capability, and `ss -ltnp` showing the host's port 443 owned by the container's process |
| `noenv` | `compose.yaml` without a `.env` file: `ISSHONI_DOMAIN` and `ISSHONI_PUBLIC_IP` arrive empty, the container keeps running, and `config print` shows both from the defaults and the TLS mode `ip`. The override points the ACME directory at a closed port, so the test never asks Let's Encrypt for anything |

**Docker Desktop.** Where the engine runs in a virtual machine (macOS, Windows), two things differ, and the test
says so in its output:
- A published port is forwarded from the machine's own address (192.168.65.1 by default on Docker Desktop for Mac),
  so that is what `ipv6` sees instead of the bridge gateway. The finding of 06 §6.4 is the same: not the client's
  address.
- The host network of `compose.host.yaml` is the virtual machine's. From outside, the test can only check that the
  container is healthy, how it is confined, `setup-url` through `exec`, and that the data directory survives `down`
  and `up`. The checks over HTTPS, the owner of port 443, the admin account and the backup are skipped there; CI
  runs them on Linux.
