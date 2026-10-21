# deploy/notices

Inputs of `THIRD_PARTY_NOTICES`, the license notices shipped in every release archive, package and image
(`docs/m1/06-deploy-and-ci.md` §8.3–8.4). `task notices` runs `tools/notices`, which writes the file (it is
gitignored, never committed) from:

- the Go modules linked into `bin/isshoni` for linux, darwin and windows, and the Go runtime;
- `web/dist/licenses.txt`, the npm packages bundled into the web client (`npm run build` writes it with
  `web/scripts/licenses.mjs notices`);
- `extra.txt` here.

| File | What | Who edits it |
|---|---|---|
| `extra.txt` | Code and data adapted from other projects, reproduced verbatim as the "Adapted source code and data" section. Each entry has a `License:` line, which must name an allowlisted license. | Whoever adapts code or data (code also gets an entry in the top-level `NOTICE`) |
| `exceptions.md` | Go modules allowed under a license that is not on the allowlist (MPL-2.0), each with a reason | Whoever adds such a dependency, in the same pull request |
| `native-windows.json` | The native components of the Windows app: the static libraries inside `isshoni-media.dll`, the FFmpeg DLLs and what is built into them. Input of the desktop file's "Native components" section (`docs/m2/06-windows-release.md` §9.2; the gate of §9.3 comes later) | Whoever adds a native dependency, in the same pull request (append-only) |

The license gate has two halves. `task licenses` runs go-licenses over `./cmd/...` and the npm check of
`web/scripts/licenses.mjs`; `task notices` then fails for any linked module whose license files licensecheck cannot
identify, whose license is neither allowlisted nor excepted here, or any of whose other license files
(`LICENSE-GPL`, `COPYING.LESSER`, ...) names GPL, AGPL, LGPL, SSPL or BUSL. Tools we only run (golangci-lint,
ShellCheck) are not dependencies: they live in `tools/go.mod` or come from a package manager, and nothing we ship
links them.
