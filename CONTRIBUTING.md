# Contributing to isshoni

Thanks for helping. This page gets you from a fresh clone to a running dev server, and lists the few rules every
change follows. The design documents are in [`docs/m1/`](docs/m1/README.md); its section 4 has the Go conventions
(errors, context, logging, testing, naming).

## Start in 5 minutes

```sh
git clone https://github.com/MoonWX/isshoni && cd isshoni
mise install              # Go, Node, Task and the linters from .tool-versions
task setup                # npm ci, go mod download, the pinned Go tools in .bin/
task dev                  # server on 127.0.0.1:8080 + Vite on :5173: open http://localhost:5173
task dev:setup-url        # in a second terminal: open the printed link, create the admin
```

**Without mise** you need Go 1.26 or newer (`go.mod` pins the toolchain, and `go` downloads it on first use) and
Node 26. Build the pinned [Task](https://taskfile.dev) into `.bin/` with Go, then use it (`.bin\task` on Windows):

```sh
go -C tools build -o ../.bin/ github.com/go-task/task/v3/cmd/task
.bin/task setup
```

`task setup` builds the other pinned Go tools into `.bin/` too (golangci-lint, tygo, go-licenses, govulncheck,
shfmt), and the tasks run them from there. ShellCheck, actionlint and goreleaser then come from your package manager,
at the versions in `.tool-versions`; only `task lint:sh`, `task lint:actions` and `task release:snapshot` need them.

Dev notes:
- The dev server runs in `tls.mode = "off"` with `deploy/dev/isshoni.dev.toml`; its data lives in `.dev/`
  (`task clean` removes it). localhost is a secure context, so screen sharing, service workers and Secure cookies
  work in Chrome, Edge and Firefox.
- Safari and phones need HTTPS: test them against a VPS install of a prerelease rather than adding TLS to the dev
  setup.
- On macOS, Local Network privacy can block a browser's LAN ICE candidates; dev uses loopback, which is not affected.

## Tasks

`task --list` shows every task. The everyday ones:

| Task | Does |
|---|---|
| `task dev` | server and Vite together (`dev:server`, `dev:web`) |
| `task gen` | regenerate `web/src/protocol/*.gen.ts` after a change in `internal/protocol` |
| `task lint` | every linter (Go, web, shell, workflows, pins, the systemd unit) |
| `task test` | every test suite (Go with the race detector, Vitest, install.sh) |
| `task build` | `bin/isshoni` with the web app embedded; `task build:go` alone needs `web/dist/index.html` |
| `task e2e` | Playwright end-to-end tests against the built binary (run `task setup:e2e` once first) |
| `task clean` | remove build output, `.dev/` and the pinned tools in `.bin/` except `task` (`task tools` builds them again) |

Tasks run through Task's built-in POSIX shell, so they work on macOS, Linux and Windows. `lint:unit`, `test:sh`,
`deploy:test` and `docker:smoke` need Linux (or containers). A task whose inputs do not exist yet prints a note and
succeeds.

## Before you push

Run `task lint test`, plus `task e2e` when you changed the server or the web app. CI runs the same tasks; its only
required check is `ci-ok`. Open pull requests against `main`.

## Rules

**The protocol.** The Go types in `internal/protocol` and `internal/protocol/api` are the source of truth for
everything on the wire. Change them, run `task gen`, and commit the generated `web/src/protocol/*.gen.ts` with your
change. Never edit a `.gen.ts` file by hand: CI fails when the generated files drift (`task gen:check`). Within a
protocol version, changes are additive only (`docs/m1/01-protocol.md` §14).

**Licenses.** isshoni is Apache-2.0 (`LICENSE`, `NOTICE`), and `task licenses` is the gate CI runs.
- Everything that ships, the Go modules linked into `isshoni` and the npm packages bundled into the web app, must be
  under one of: MIT, ISC, BSD-2-Clause, BSD-3-Clause, Apache-2.0, 0BSD, Zlib, CC0-1.0, Unlicense, BlueOak-1.0.0.
  No GPL, AGPL or LGPL code, copied or linked. An MPL-2.0 module needs an entry with a reason in
  `deploy/notices/exceptions.md`.
- Dev-only npm packages must not be GPL-, AGPL-, SSPL- or BUSL-only.
- Programs we only run are not dependencies: ShellCheck and golangci-lint (both GPL-3.0) are never linked into
  anything we ship. Our own programs under `tools/` follow the shipping rule all the same.
- Code adapted from another project keeps its copyright header, says what was changed, and gets an entry in
  `NOTICE`.

**Tool versions.** `.tool-versions` (read by mise and asdf) pins Go, Node, Task and the linters. `go.mod` has the
language version (`go 1.26.0`) and the Go toolchain (`toolchain go<the .tool-versions pin>`); CI reads the Go version
from `go.mod`. `tools/go.mod` pins the Go tools that `task tools` builds into `.bin/` (Task, golangci-lint and shfmt at the
`.tool-versions` versions), and holds our own tool programs in `tools/<name>/`. `task lint:pins` fails when these pins
disagree, so bump them together, in one pull request.

**Spikes.** `spikes/` holds throwaway experiments from the prototype phase, each with its own `go.mod` and a README
of results. They are not part of the main module, CI does not build or test them, and nothing ships from them.
