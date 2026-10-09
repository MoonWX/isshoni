# deploy/systemd

`isshoni.service` is the one unit of every systemd install; `docs/m1/06-deploy-and-ci.md` §4.5 explains each
setting. install.sh puts it in `/usr/local/lib/systemd/system/`, the deb and rpm packages in
`/usr/lib/systemd/system/`. `ExecStart=isshoni …` has no path on purpose: systemd finds the program in
`/usr/local/bin` (install.sh) or in `/usr/bin` (packages), so both share this file. Local changes belong in a
drop-in (`systemctl edit isshoni`), which an upgrade keeps.

## Checks

| Check | Where it runs | What it asserts |
|---|---|---|
| `task lint:unit` | the `lint-deploy` CI job, and any Linux with systemd | `systemd-analyze verify` on a copy whose `ExecStart` goes through `/usr/bin/env` (isshoni is not installed there); any message about the unit fails |
| Start as a service | install.sh scenarios C1 (containers) and V1 (VMs), 06 §11.2 | active, runs as `isshoni`, `CapEff` is `0000000000000400` (`CAP_NET_BIND_SERVICE` alone) |
| Exposure | V6, 06 §11.2 | `systemd-analyze security isshoni` is at most the recorded value + 0.2 |

## Recorded exposure: 1.7

`systemd-analyze security isshoni` reports an exposure of **1.7 (OK)**; 06 §4.5 asks for 2.5 or less. A change
that takes it above **1.9** fails V6. Measured for README S62 on 2026-10-09, with the unit started in a privileged
systemd container of each supported distro:

| Distro | systemd | Exposure |
|---|---|---|
| Ubuntu 22.04 | 249 | 1.7 |
| Debian 12 | 252 | 1.7 |
| Ubuntu 24.04 | 255 | 1.7 |
| Debian 13 | 257 | 1.7 |
| Ubuntu 26.04 | 259 | 1.7 |
| Fedora 44 | 259 | 1.7 |

From systemd 250 on (tried on 255), the same check needs neither a running systemd nor root, and answers with its
exit status (the threshold is the exposure times 10):

```sh
systemd-analyze security --offline=true --threshold=19 deploy/systemd/isshoni.service
```

What is left is the same 13 lines on every version above (the ones the report marks ✗), and each has a reason:

| Line in the report | Why it stays |
|---|---|
| `PrivateNetwork=`, `RestrictAddressFamilies=~AF_(INET\|INET6)`, `IPAddressDeny=` | isshoni is a network server, for clients at any address |
| `RestrictAddressFamilies=~AF_UNIX` | the admin socket `/run/isshoni/admin.sock` |
| `RestrictAddressFamilies=~AF_NETLINK` | Go's `net.Interfaces()`, which ICE gathering uses, reads the addresses over netlink |
| `AmbientCapabilities=`, `CapabilityBoundingSet=~CAP_NET_(BIND_SERVICE\|BROADCAST\|RAW)` | `CAP_NET_BIND_SERVICE`, the only capability, binds ports 80 and 443 |
| `PrivateUsers=` | in a user namespace that capability would not count for the host's network namespace |
| `ProcSubset=` | the server and doctor read `/proc/sys/net/core/{r,w}mem_max` |
| `SystemCallFilter=~@privileged`, `SystemCallFilter=~@resources` | parts of both sets belong to `@system-service`, the allow list the unit uses |
| `RootDirectory=/RootImage=` | the service sees the host's file system, read-only (`ProtectSystem=strict`) |
| `DeviceAllow=` | `ProtectClock=yes` adds this rule itself: read-only access to the real-time clock device |

When a setting of the unit changes, measure again on the oldest and the newest systemd of the table and update
this page and 06 §4.5.
