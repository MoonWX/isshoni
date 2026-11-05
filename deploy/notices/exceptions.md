# License exceptions

Go modules that the isshoni binary may link although their license is not on the shipped allowlist
(`docs/m1/06-deploy-and-ci.md` §8.3). `tools/notices` (`task notices`) fails for such a module unless it has a row
here; go-licenses (`task licenses`) already lets MPL-2.0 through as "reciprocal", so this table is where the reason
is written down.

Only file-level copyleft licenses such as MPL-2.0 qualify, for a module used unmodified. GPL, AGPL, LGPL, SSPL and
BUSL never do: the tool refuses such a row.

One row per module and license: the module path in backquotes, the license id that licensecheck reports for the
module (the tool's error message names it), and the reason: what the module does for isshoni, that it is used
unmodified, and where its source is. `task notices` warns about a row that matches no linked module; remove it then.

| Module | License | Reason |
|---|---|---|

## Native components of the desktop app

Native components (`native-windows.json`, `docs/m2/06-windows-release.md` §9.2–§9.3) whose license is not an SPDX id
but a `LicenseRef-*`: `tools/notices -native` fails for such an entry unless it has a row here. Only `LicenseRef-*`
licenses can be listed; every other license is decided by the allowlist (and FFmpeg's LGPL-2.1-or-later by the gate's
own rule). One row per component: its `name` from `native-windows.json` in backquotes, the license, and the reason:
what it is, how it ships and whose terms allow that.

| Native component | License | Reason |
|---|---|---|
| `Microsoft Edge WebView2 Runtime bootstrapper` | LicenseRef-Microsoft-WebView2-Distribution | Microsoft's signed `MicrosoftEdgeWebview2Setup.exe`, unmodified, carried inside the installer and run only when the WebView2 Runtime the app needs is missing or too old; never installed. Microsoft's WebView2 distribution terms allow shipping it with an application (`licenses/webview2-bootstrapper.txt`). |
