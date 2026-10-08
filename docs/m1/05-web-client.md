# M1 design 05: Web client (`web/`)

The React SPA that every isshoni user sees in M1: sign-up and login, the room with its viewer and the Chrome/Edge
sharer, the setup wizard with the connection test, account and admin pages, and the installable web app (PWA) with Web
Push. The server binary embeds it with `go:embed`.

Inputs: `docs/PLAN.md` (owner-approved) and the M0 spikes, mainly `spikes/s4-sfu` (its `web/app.js` is the throwaway
ancestor of this client). The sibling M1 docs are the source of truth for their parts, and this doc uses their names:
- `01-protocol.md`: everything on the WebSocket, and the TypeScript `SignalClient`, `codecs.ts` and `errors.ts` in
  `web/src/protocol/`;
- `02-sfu.md`: codec policy, `ShareParams` numbers, the connection-test probe PCs;
- `03-accounts-and-store.md`: REST paths, DTOs, errors, CSRF, sessions, invites, rooms;
- `04-server-platform.md`: SPA serving and headers, the connection-test endpoint, Web Push, doctor, dashboard;
- `06-deploy-and-ci.md`: tool pins, `.gitignore`, the license script, CI and the e2e runner.

The integrator reconciled the sibling docs (`README.md`, "Integration decisions"); §22 records how each conflict that
touched the SPA was settled.

**Tags.** Everything here is **M1** unless marked *later (Mx)*. A "later" item gets only the room it needs now (a type,
a reserved name, a route), never code.

---

## 1. Goals and non-goals

M1 goals:
- A **viewer on every current browser**, including iPhone/iPad Safari (tab and Home Screen app) and Android Chrome: tile
  grid, click to focus, fullscreen, keyboard navigation, audio follows focus, one "tap to unmute".
- A **Chrome/Edge desktop sharer**: "window + its audio" recommended, 2 simulcast layers, presets Auto/Game/Movie/Text,
  and the sharer always sees who watches.
- **Setup wizard** with a real connection test; invite, login and approval flows; account and admin pages.
- **PWA** with an app-shell service worker and Web Push ("Alex started sharing").
- **Seams for later clients**: a Platform adapter, media controllers that don't import React, and no cookie or
  `location` assumptions outside `BrowserPlatform`. The desktop app (M2/M3) bundles this same SPA.

Non-goals in M1: native code of any kind; the "What friends hear" and exclusions UI (desktop, M2); the device-flow
approval page `/link` (M2); "Copy diagnostics" (M5; M1 only collects the data); the 720p middle layer (M5); several
shares per user in the UI (later); languages other than English (later); a data-saver mode (M5).

---

## 2. Toolchain and dependencies

**Package manager: npm** (06 D12; Node 26 no longer ships corepack/pnpm). `web/package.json` has
`"engines": {"node": ">=26"}` and `"packageManager": "npm@<exact 11.x>"` (06 §7.1); `web/.npmrc` sets
`engine-strict=true` and `fund=false`. `package-lock.json` is committed and CI uses `npm ci`.

Versions are the current releases as of 2026-09-29 (`npm view`). The lockfile pins exact versions; `package.json` uses
`^` on these majors.

| Package | Version | License | Notes |
|---|---|---|---|
| react, react-dom | 19.3 | MIT | |
| react-router | 8.4 | MIT | Data router (`createBrowserRouter`, `RouterProvider`, lazy routes). v8 is recent: confirm the API names in slice W1 |
| zustand | 5.0 | MIT | Client and realtime state |
| @tanstack/react-query | 5.x | MIT | REST state |
| i18next / react-i18next | 26 / 17 | MIT | |
| uqr | 0.1 | MIT | QR matrix only, rendered as our own SVG (no `innerHTML`) |
| lucide-react | 1.x | ISC | Icons, tree-shaken |
| *dev:* vite | 8.3 | MIT | |
| *dev:* @vitejs/plugin-react | 6.1 | MIT | No React Compiler in M1 (fewer moving parts) |
| *dev:* typescript | **6.0.x** | Apache-2.0 | **Not 7.x**: typescript-eslint 8.71 requires TypeScript `<6.1` |
| *dev:* vitest, jsdom | 5.0, 30 | MIT | |
| *dev:* @testing-library/react, jest-dom, user-event | 16, current | MIT | |
| *dev:* msw | 3.0 | MIT | REST mocks in component tests |
| *dev:* eslint, @eslint/js | **9.39** | MIT | **Not 10**: eslint-plugin-jsx-a11y 6.10 declares `eslint ^9` at most |
| *dev:* typescript-eslint | 8.71 | MIT | Type-aware rules |
| *dev:* eslint-plugin-react-hooks | 7.1 | MIT | |
| *dev:* eslint-plugin-jsx-a11y | 6.10 | MIT | |
| *dev:* eslint-plugin-i18next | 6.1 | ISC | `no-literal-string` |
| *dev:* prettier | 3.9 | MIT | printWidth 120 |
| *dev:* @playwright/test | 1.63 | Apache-2.0 | channel `chrome` |
| *dev:* @axe-core/playwright | 4.13 | MPL-2.0 | Dev only, never shipped; 06's gate allows MPL for dev packages |
| *dev:* spdx-expression-parse, spdx-satisfies | current | MIT | Used by 06's `scripts/licenses.mjs` |

No UI component library, no CSS framework, no Workbox (§16.2 explains the hand-written service worker), no web fonts
(system font stack). Rationale: a small, auditable bundle that embeds cleanly and keeps the license list short.

**Styling**: CSS modules (`*.module.css` next to each component, built into Vite, no dependency), with `ui/tokens.css`
(custom properties) and `ui/global.css` as the only global sheets. Vite emits them as files under `/assets/`, which
04's `style-src 'self'` allows.

**Scripts** (`web/package.json`). Names match 06's Taskfile (§7.2) and CI job:

| Script | Command | Used by |
|---|---|---|
| `dev` | `vite` | `task dev:web` |
| `build` | `tsc -b && vite build && node scripts/licenses.mjs notices` | `task build:web`, CI, release |
| `typecheck` | `tsc -b --pretty` (projects set `noEmit`) | `task lint:web` |
| `lint` | `eslint . --max-warnings=0 && prettier --check .` | `task lint:web` |
| `test` | `vitest` (06 calls `npm test -- --run`) | `task test:web` |
| `e2e` | `playwright test` | `task e2e` (needs `ISSHONI_BIN`) |
| `check:i18n` | `node scripts/check-i18n.mjs` | CI `web` job (06 adds the step) |
| `check:size` | `node scripts/check-size.mjs` | CI `web` job (06 adds the step) |

`scripts/licenses.mjs` (`check` and `notices` modes, the allowlist, `licenses.overrides.json`) is specified by 06
§8.3; this doc only relies on its output `dist/licenses.txt`.

**TypeScript**: `strict`, `noUncheckedIndexedAccess`, `verbatimModuleSyntax`, `moduleResolution: "bundler"`, target
ES2022. Three projects: `tsconfig.app.json` (DOM), `tsconfig.sw.json` (`lib: ["ES2022","WebWorker"]` for `src/sw/`),
`tsconfig.node.json` (Vite config, `build/`, `scripts/`, `e2e/`). Browser APIs newer than `lib.dom` are declared in
`src/types/dom-extras.d.ts` (display-media options, `restrictOwnAudio`, `suppressLocalAudioPlayback`, …).

**ESLint** (`eslint.config.js`, flat): a first entry that ignores generated and build-output files, then `@eslint/js`
recommended, `typescript-eslint` strict-type-checked, react-hooks recommended, jsx-a11y strict, and:

```js
{ ignores: ['dist/', 'src/protocol/*.gen.ts', 'playwright-report/', 'test-results/'] },   // first entry
// … recommended configs …
{
  rules: {
    'i18next/no-literal-string': ['error', {
      mode: 'jsx-only',
      'jsx-attributes': { include: ['aria-label', 'aria-description', 'title', 'alt', 'placeholder', 'label'] },
    }],
    'no-restricted-syntax': ['error',
      { selector: "JSXAttribute[name.name='dangerouslySetInnerHTML']", message: 'No raw HTML (CSP + XSS).' }],
    'no-restricted-globals': ['error', 'localStorage', 'sessionStorage'], // use platform.storage (try/catch wrapper)
  },
},
{ files: ['**/*.test.*', 'e2e/**', 'scripts/**', 'build/**', 'src/sw/**', 'public/boot-check.js'],
  rules: { 'i18next/no-literal-string': 'off' } },
{ files: ['src/platform/browser/storage.ts', '**/*.test.*', 'e2e/**'],
  rules: { 'no-restricted-globals': 'off' } },
```

**Prettier** skips the same files: `web/.prettierignore` lists `dist/`, `src/protocol/*.gen.ts`, `build-report.json`,
`playwright-report/` and `test-results/`. Generated TypeScript is never reformatted, so `task gen:check` compares the
generator's raw output; `typecheck` still covers it.

---

## 3. Directory structure

The plan lists `src/{platform,viewer,share,rooms,auth,admin,protocol,i18n}`. This doc adds `app` (shell and routing),
`setup`, `conntest`, `account`, `download`, `lib`, `ui`, `sw`, `test` and `types`, so each page group and the service
worker has its own folder. Files marked (01), (03) or (06) are specified by that doc.

```
web/
├─ embed.go                  package web: //go:embed all:dist → Dist()   (04 §9.5)
├─ package.json · package-lock.json · .npmrc · .prettierrc.json · .prettierignore · licenses.overrides.json (06)
├─ index.html                meta tags, <div id="root">, boot-check.js, main.tsx
├─ vite.config.ts            build targets, dev proxy, plugins, Vitest config
├─ tsconfig.json · tsconfig.app.json · tsconfig.sw.json · tsconfig.node.json
├─ eslint.config.js · playwright.config.ts
├─ build/
│  ├─ sw-plugin.ts           after the main build: builds src/sw/sw.ts (IIFE) with the shell list
│  ├─ version-plugin.ts      writes dist/version.json
│  ├─ compress-plugin.ts     writes .br and .gz siblings (node:zlib, no dependency)
│  └─ report-plugin.ts       gzip sizes of initial and lazy chunks → web/build-report.json (gitignored)
├─ scripts/                  licenses.mjs (06) · check-i18n.mjs · check-size.mjs
├─ public/                   copied as-is into dist/
│  ├─ manifest.webmanifest · robots.txt (Disallow: /) · boot-check.js
│  └─ icons/                 icon.svg · icon-192.png · icon-512.png · maskable-512.png · apple-touch-icon.png · badge-72.png
├─ dist/.gitkeep             committed (06 §7.2); everything else in dist/ is build output
├─ e2e/                      Playwright (§19.3): global-setup.ts · fixtures.ts · stats.ts · tone.html · *.spec.ts
└─ src/
   ├─ main.tsx               entry: calls startApp() of app/boot.tsx (§4)
   ├─ app/                   boot.tsx (boot sequence, §4) · router.tsx · layouts/ · guards.tsx · me.ts (the ['me']
   │                         query) · session.ts (logout BroadcastChannel) · info.ts (the ['info'] query) ·
   │                         queryClient.ts · context.ts · uiStore.ts · prefs.ts · App.tsx · NotFound.tsx · screens/
   │                         (Offline, Unsupported, NeedsHttps, NotSetUp, VersionMismatch, Fatal) · ErrorBoundary.tsx ·
   │                         Announcer.tsx · Toasts.tsx · UpdatePill.tsx
   ├─ platform/              types.ts · detect.ts · browser/ (BrowserPlatform.ts, device.ts, storage.ts, pwa.ts,
   │                         push.ts, wakeLock.ts, displayMedia.ts, classify.ts, fakeDisplay.ts)
   ├─ protocol/              types.gen.ts · registry.gen.ts · api.gen.ts (tygo: 01's protocol; 03's and 04's REST DTOs) · signal-client.ts ·
   │                         codecs.ts · errors.ts · index.ts (01) · rest.ts · queryKeys.ts · invalidate.ts (05)
   ├─ rooms/                 connection.ts · RoomSession.ts · roomStore.ts · RoomPage.tsx · RoomHeader.tsx ·
   │                         PeoplePanel.tsx · RoomSwitcher.tsx · InRoomBar.tsx
   ├─ viewer/                SubscriberPC.ts · mediaRegistry.ts · viewerStore.ts · layerPolicy.ts · autoFocus.ts ·
   │                         audioOut.ts · ViewerLayout.tsx · Stage.tsx · Tile.tsx · TapToStart.tsx ·
   │                         WatchersPopover.tsx · fullscreen.ts · keyboard.ts · useVisibility.ts
   ├─ share/                 BrowserSharing.ts (SharingProvider) · PublisherPC.ts · presets.ts · encodings.ts ·
   │                         codecPrefs.ts · shareStore.ts · ShareButton.tsx · ShareSheet.tsx · ScreenAudioWarning.tsx ·
   │                         SharePanel.tsx · LevelMeter.tsx · hints.ts
   ├─ auth/                  LoginPage · InvitePage · SignupPage · PendingPage · ResetPage · AboutPage ·
   │                         fragmentToken.ts · useMe.ts
   ├─ setup/                 SetupPage (step 1) · WelcomePage (steps 2–3) · InviteLinkCard · QrCode
   ├─ conntest/              probe.ts · runConnTest.ts · verdict.ts · fixText.ts · ConnTestPanel.tsx
   ├─ account/               AccountPage · DevicesPage · NotificationsPage · PushCard · InstallCard
   ├─ admin/                 AdminLayout · DashboardPage · UsersPage · ApprovalsPage · InvitesPage · RoomsPage ·
   │                         SettingsPage · AuditPage · DoctorPage
   ├─ download/              DownloadPage.tsx (M1 placeholder)
   ├─ lib/                   sdp.ts · stats/ (collector.ts, summarize.ts) · log.ts · emitter.ts · time.ts ·
   │                         clipboard.ts · ua.ts
   ├─ ui/                    Button · Dialog (native <dialog>) · Sheet · Popover · Field · Stepper · Spinner ·
   │                         VisuallyHidden · tokens.css · global.css
   ├─ i18n/                  index.ts · en.json
   ├─ sw/                    sw.ts · routes.ts (pure fetch strategy) · push.ts (payload → notification)
   ├─ test/                  setup.ts · FakeWebSocket · FakeRTCPeerConnection · fake media element · MSW handlers
   └─ types/                 dom-extras.d.ts · globals.d.ts
```

Components have a sibling `*.module.css` file (`Tile.tsx` + `Tile.module.css`); the tree above leaves them out. It
also leaves out the entry module of each page folder: `rooms`, `auth`, `setup`, `account`, `admin` and `download`
each have an `index.ts` that exports the folder's pages by name, which is how `router.tsx` finds them (§5).

**Layering rule.** React components never touch `WebSocket` or `RTCPeerConnection`. Controllers (`RoomSession`,
`SubscriberPC`, `PublisherPC`, `runConnTest`) and 01's `SignalClient` never import React. They talk through Zustand
stores (created with `createStore` from `zustand/vanilla`, read in React with `useStore`) and small emitters. This
keeps the media code unit-testable and reusable by the desktop SPA.

```
 pages/components (React) ──read──► Zustand stores ◄──write── controllers (RoomSession, SubscriberPC, PublisherPC)
        │                                                        │            │
        └── TanStack Query ─► protocol/rest.ts ─► Platform.apiFetch            │
                                                  SignalClient (01) ─► Platform.signaling()
                                                  WebRTC ─► Platform.createPeerConnection(), Platform.sharing
```

---

## 4. Boot sequence and app-level screens

`index.html` loads `/boot-check.js` (a classic script, allowed by `script-src 'self'`) before the module entry. It
checks the minimum the SPA needs: `window.RTCPeerConnection`, `RTCRtpTransceiver`, `Array.prototype.at`,
`structuredClone`. If one is missing, it writes a static English "This browser is too old for isshoni" message into
`#root` and sets `window.__ISSHONI_UNSUPPORTED__ = true`; `main.tsx` then does nothing. This catches old iOS versions
that would otherwise show a blank page.

`main.tsx` only finds `#root` and calls `startApp(container)`. The sequence itself is `startApp` in `app/boot.tsx`
(S27), so the tests run it with a fake platform, location, history and router (`BootOptions`):
0. If the path is `/setup`, `/invite` or `/reset` and `location.hash` is non-empty, `stashFragmentToken()` (in
   `app/boot.tsx`) stores the fragment, percent-decoded, in `sessionStorage['isshoni.<setup|invite|reset>']` (the key
   is `fragmentTokenKey(kind)`) and calls `history.replaceState` to drop the fragment. This runs before any request,
   including `/info`. It writes through `platform.storage.session` (§8), so in the code it runs right after step 2:
   the store's memory fallback must be the one the pages read later, and `detectPlatform()` makes no request. The
   pages take the token from there through `auth/fragmentToken.ts`, which reads and clears it (§14.1, §15.1).
1. If `!window.isSecureContext` → **NeedsHttps** screen ("isshoni needs HTTPS. Ask your admin to check the TLS
   setup"). getDisplayMedia, the service worker, push and wake lock all need a secure context. Without
   `RTCPeerConnection` → **Unsupported** (`boot-check.js` normally catches that first). Both screens need the
   platform and i18n, so the code runs steps 2, 0 and 3 before this check; none of them makes a request.
2. `platform = detectPlatform()` (§8).
3. Initialize i18next with `en.json` (bundled, synchronous) and set `<html lang>`.
4. `GET /api/v1/info` (03 §12.4.1; no auth; retried 3× after 1 s, 2 s, 4 s):
   - no response, a 5xx, `server_busy` or `rate_limited` every time → **Offline** screen, auto-retry on the `online`
     event and every 10 s;
   - any other failure (a 4xx, or a body that isn't `Info`) → **Fatal**;
   - `setupRequired: true` and the path is not `/setup` → **NotSetUp** screen ("This server isn't set up yet. On the
     server run `sudo isshoni setup-url` (Docker: `docker compose exec isshoni isshoni setup-url`) and open the link
     it prints").
5. Create the `QueryClient` (seeded with `info`: `seedInfo` in `app/info.ts`; pages read it with `useInfo()`) and the
   router; render. Boot also starts the cross-tab logout listener (`listenForLogout`, §6.2).
6. After first render and only in production builds: register the service worker (§16.2) when the browser is idle.

App-level screens (`src/app/screens/`): Offline, Unsupported (no WebRTC), NeedsHttps, NotSetUp, VersionMismatch
(§16.4), Fatal (unrecoverable error with "Reload"). A route-level `ErrorBoundary` shows "Something went wrong" with
"Reload" and logs the error to the in-memory log (§10.8). A browser without any H.264 decoder isn't blocked (audio
still plays); the room shows a banner instead (§10.6). The in-app browser banner (§16.3) is not a screen either: the
invite, login and room pages show it above their content.

---

## 5. Routes

React Router data router; every page except the room is a lazy chunk. `RequireAuth` loads the `['me']` query and
redirects to `/login?next=<path>` on a 401 `unauthenticated` (§6.2). `RequireAdmin` also needs `me.user.role === "admin"` (else
NotFound, so admin pages aren't advertised). `RequireInviter` allows admins and users with
`me.permissions.createInvites`, else NotFound; only `/admin/invites` uses it. `next` is accepted only if it starts with `/` and not `//` (open-redirect guard). No route segment
contains a dot (04 §9.5 serves dotted paths as files).

| Path | Page | Access | Chunk | Notes |
|---|---|---|---|---|
| `/` | RootRedirect | user | main | → `/r/<lastRoomId>` if that room still exists, else `/r/<defaultRoomId>` (from `GET /api/v1/rooms`) |
| `/r/:roomId` | RoomPage | user | main (eager) | Lounge by default (§11–§13). `?focus=<shareId>` focuses that share (push links, 04 §14.3) |
| `/login` | LoginPage | public | auth | `?next=`; trust-model lines (§6.3); "Forgot your password? Ask an admin for a reset link." (03 §7.10) |
| `/invite` | InvitePage | public | auth | Token in the fragment: `/invite#<token>` |
| `/signup` | SignupPage | public | auth | Only when `info.registration === "approval"`; otherwise redirects to `/login` |
| `/pending` | PendingPage | public | auth | Static: "An admin will review your request. Try logging in later." (pending users get no session, 03 §7.9) |
| `/reset` | ResetPage | public | auth | `/reset#<token>` (admin-issued reset links, 03 §7.10) |
| `/setup` | SetupPage | public + token | setup | `/setup#<token>`, step 1 only. The server answers 404 once an admin exists (§14) |
| `/admin/welcome` | WelcomePage | admin | setup | Wizard steps 2–3, re-enterable (§14) |
| `/download` | DownloadPage | public | download | M1 placeholder; per-OS installers are *later (M2)* |
| `/about` | AboutPage | public | auth | Versions, trust model, links to the project site's privacy and code-signing pages, `/licenses.txt`. It loads from `auth/` (W3 writes it with the login page's trust-model lines); there is no `misc` chunk |
| `/account` | AccountPage | user | account | Username, change password, delete account, sign out |
| `/account/devices` | DevicesPage | user | account | Browser sessions and (from M2) linked devices, revoke |
| `/account/notifications` | NotificationsPage | user | account | Web Push on this device, preferences, test |
| `/admin` | DashboardPage | admin | admin | |
| `/admin/users` | UsersPage | admin | admin | |
| `/admin/approvals` | ApprovalsPage | admin | admin | Nav badge from `me.badges.pendingApprovals` |
| `/admin/invites` | InvitesPage | admin, or user with `me.permissions.createInvites` | admin | `RequireInviter`. Members see only their own invites (the server filters them) |
| `/admin/rooms` | RoomsPage | admin | admin | |
| `/admin/settings` | SettingsPage | admin | admin | |
| `/admin/audit` | AuditPage | admin | admin | 03's audit log (`GET /api/v1/admin/audit`) |
| `/admin/doctor` | DoctorPage | admin | admin | Doctor report, connection test and bandwidth calculator |
| `/link` | DeviceLinkPage | user | — | *later (M2)*: RFC 8628 approval page (`/link?code=…`). Reserved; renders NotFound in M1 |
| `*` | NotFound | public | main | |

The server serves `index.html` for every path without a dot (04 §9.5), so it needs no route list; `/setup` is its only
special case. Room IDs are opaque strings from the API.

**Page folders (S27's contract with the page slices).** `app/router.tsx` declares every route above once, and no
page slice edits it. Each route names one page component and the folder it comes from (`route.handle` is
`{folder, page}`):

| Folder | Named exports of `<folder>/index.ts` | Chunk |
|---|---|---|
| `rooms/` | `RootRedirect`, `RoomPage` | main (eager) |
| `auth/` | `LoginPage`, `InvitePage`, `SignupPage`, `PendingPage`, `ResetPage`, `AboutPage` | `auth` |
| `setup/` | `SetupPage`, `WelcomePage` | `setup` |
| `account/` | `AccountPage`, `DevicesPage`, `NotificationsPage` | `account` |
| `admin/` | `AdminLayout` (the layout route around the admin pages), `DashboardPage`, `UsersPage`, `ApprovalsPage`, `InvitesPage`, `RoomsPage`, `SettingsPage`, `AuditPage`, `DoctorPage` | `admin` |
| `download/` | `DownloadPage` | `download` |

- A page folder's entry module is `<folder>/index.ts` (or `index.tsx`), and it exports the folder's pages as **named**
  exports with exactly these names. The router picks a page by its export name and reads no default export.
- The router finds the entry modules with `import.meta.glob` (`../auth/index.{ts,tsx}` and so on): the lazy folders
  as loaders, so each is one chunk fetched on the first visit to one of its routes, and `rooms/` with
  `{eager: true}`, so the room is in the main chunk.
- A folder that doesn't exist yet, or an export that is missing, renders `PageUnavailable` (a missing `AdminLayout`
  renders just its child page). A page slice adds its folder's `index.ts` and exports, and its routes start working
  with no change to `router.tsx`.
- The guards are layout routes in `app/guards.tsx` (`RequireAuth`, `RequireAdmin`, `RequireInviter`, with `safeNext`
  for the `next` rule and `loginPath`), placed around the routes in `router.tsx`, so a page doesn't guard itself. They
  read the `['me']` query of `app/me.ts` (§6.2). The public routes sit outside every guard: the router asks for
  `/api/v1/me` only on guarded routes, and a public page that needs to know reads `['me']` itself.

---

## 6. State, REST and errors

### 6.1 Stores (Zustand, `zustand/vanilla`)

| Store | File | Holds | Written by |
|---|---|---|---|
| `connectionStore` | `rooms/connection.ts` | 01's `SignalState`, `welcome` (server version, limits, features, user, default room), `resumed`, `downSince`, `stopReason` | `SignalClient.onState` |
| `roomStore` | `rooms/roomStore.ts` | `roomId`, `joinState`, the last `room.state` (participants, shares, `rev`), own `connectionId` and `userId` | RoomSession |
| `viewerStore` | `viewer/viewerStore.ts` | `focusedShareId`, `focusMode` (auto, manual), `audibleShareId`, `audio` (locked, playing, blocked, muted), `volume`, `fullscreen`, `pipShareId`, `visible` per share, `pageHiddenSince`, `status` per share (from `subscribe.status`) | viewer components, RoomSession, SubscriberPC |
| `shareStore` | `share/shareStore.ts` | local share state machine (§13.1), `preset`, `withAudio`, `picked` classification, latest `ShareParams`, `hints` | BrowserSharing, SharePanel |
| `uiStore` | `app/` | toasts, announcer queue, install availability, `updateReady` | many |
| `prefsStore` | `app/prefs.ts` | persisted per device: `volume`, `lastRoomId`, `preset`, dismissed hints, `debug` | settings UI |

`prefsStore` persists through `platform.storage.local` (a try/catch wrapper; the page works when storage is
blocked). `lastRoomId` uses the key `isshoni.lastRoomId`, which 01 §10.5 reads for rejoining. Per-tab values (stashed
setup/invite/reset token, the reload guard, debug flags) use `platform.storage.session`. Media tracks are not kept in
stores: `viewer/mediaRegistry.ts` maps `shareId → {video?, audio?: MediaStreamTrack}` with a subscribe API, and tiles
read it through `useShareMedia(shareId)`.

### 6.2 REST (TanStack Query)

- `protocol/rest.ts` exports `api<T>(method, path, body?) → Promise<T>` built on `platform.apiFetch`. Unsafe methods
  always send `Content-Type: application/json` and a JSON body (`{}` when empty). 03 §7.5's CSRF defence is Go's
  cross-origin protection plus that mandatory content type, so no token or custom header is needed. A 2xx with status
  204, or with no `application/json` body (for example 202 from `/push/test`), resolves to `undefined`; otherwise the
  body is parsed as JSON.
- Non-2xx responses become `ApiError {status, code, fields?, params?, retryAfterSec?, requestId?}` from the single
  REST envelope `{"error": {code, fields?, params?, retryAfter?, requestId?}}` (03 §12.2; 04 uses it too). A
  non-JSON body (the Host check's 421, a proxy error page) becomes `code: 'unknown'` with the status.
- Query defaults: `staleTime: 30_000`; retry network errors, 5xx, `server_busy` and `rate_limited` (after
  `retryAfter`) up to 3 times, nothing else; `refetchOnWindowFocus` only for admin lists.
- A 401 whose code is `unauthenticated` (M2: also `invalid_token`) clears `['me']`. On a route under RequireAuth it also
  sends the user to `/login?next=…`. On public routes (`/login`, `/invite`, `/signup`, `/pending`, `/reset`, `/setup`,
  `/download`, `/about`) a 401 from `GET /api/v1/me` means signed out, and nothing redirects (`useMe()`'s no-redirect
  mode, which public pages use). Other 401 codes
  (`invalid_credentials`) are ordinary form errors. A `BroadcastChannel('isshoni')` message `{type: 'logout'}` clears
  `['me']` in the user's other tabs, with the same redirect rule.
- The guards, the `['me']` query and the logout channel live in `app/`, not in `auth/` (S27; the page slices use
  them):
  - `app/me.ts`: `fetchMe`, `meQueryOptions`, `useMeQuery` and `clearMe`. The query **resolves to `null`** when
    `GET /api/v1/me` answers 401 `unauthenticated` (M2: `invalid_token`), so "signed out" is data, not an error. The
    guards (`app/guards.tsx`, §5) redirect on `null`; a public page reads the same query and shows its form.
    `auth/useMe.ts` builds on `meQueryOptions`. Any other failure of `/me` on a guarded route shows Offline (with "Try
    now") when it is retryable, else Fatal.
  - `clearMe(queryClient)` sets `['me']` to `null`. The query client's caches call it for a 401 `unauthenticated`
    from any query or mutation (`app/queryClient.ts`), and boot's logout listener calls it too.
  - `app/session.ts`: `broadcastLogout()` posts the message, and the logout flow calls it after
    `POST /api/v1/auth/logout` succeeded (§15.1); `listenForLogout(fn)` is what boot registers. Both do nothing in a
    browser without `BroadcastChannel`.
- **01's `invalidate` message** refetches REST data (`protocol/invalidate.ts`):

  | Topic (01 §8.12) | Query keys invalidated |
  |---|---|
  | `rooms` | `['rooms']` |
  | `me` | `['me']` |
  | `devices` | `['me','sessions']`, `['me','devices']` |
  | `admin.users` | `['admin','users']` |
  | `admin.invites` | `['invites']` |
  | `admin.approvals` | `['admin','approvals']`, `['me']` (badge) |
  | `admin.settings` | `['admin','settings']`, `['info']` |

REST calls the SPA makes (03 §12.3 unless marked 04):

| Call | Used by |
|---|---|
| `GET /api/v1/info` | boot |
| `POST /api/v1/auth/login` · `/auth/logout` · `/auth/logout-everywhere` | login, menu, devices |
| `POST /api/v1/auth/register` · `/auth/invite/check` | invite, signup |
| `POST /api/v1/auth/setup/check` · `/auth/setup/complete` | setup |
| `POST /api/v1/auth/reset/check` · `/auth/reset/complete` | reset |
| `GET /api/v1/me` · `POST /api/v1/me/password` · `POST /api/v1/me/delete` | guards, account |
| `GET /api/v1/me/sessions` · `DELETE …/{id}` · `POST /api/v1/me/sessions/revoke-others` · `GET/DELETE /api/v1/me/devices…` | devices |
| `GET /api/v1/rooms` | room switcher, root redirect (`defaultRoomId`, `showRoomList`) |
| `GET /api/v1/invites?state=all` | invites page (the list) |
| `POST /api/v1/invites` · `DELETE /api/v1/invites/{id}` | wizard step 3, "Invite friends" card, invites page |
| `POST /api/v1/push/subscriptions` · `POST /api/v1/push/unsubscribe` · `POST /api/v1/push/test` · `GET/PUT /api/v1/push/preferences` (all 03; the VAPID key is in `GET /api/v1/info`) | notifications (§16.3) |
| (04) `POST /api/v1/conntest` | connection test (§14.2) |
| `GET /api/v1/admin/users` · `PATCH/DELETE /api/v1/admin/users/{id}` · `POST …/{id}/password-reset` · `POST …/{id}/sign-out` | users |
| `GET /api/v1/admin/approvals` · `POST …/{id}/approve` · `POST …/{id}/reject` (and its `all` form: `POST …/all/reject` `{"all": true}` → 200 `{rejected}`, 03 §7.9) | approvals |
| `POST /api/v1/admin/rooms` · `PATCH/DELETE /api/v1/admin/rooms/{id}` | rooms |
| `GET/PATCH /api/v1/admin/settings` | settings, dashboard checklist, wizard Done |
| `GET /api/v1/admin/audit` | audit |
| (04) `GET /api/v1/admin/dashboard` · (04) `GET\|POST /api/v1/admin/doctor` · (04) `GET /api/v1/admin/bandwidth` | dashboard, doctor |

REST DTO types come from tygo: `api.gen.ts` from `internal/protocol/api`, which holds 03's DTOs and 04's (dashboard,
doctor, bandwidth, connection test, push payload). All JSON is camelCase.

### 6.3 Error handling

- **Codes, never English from the server.** `errors` i18n keys: `errors.<code>` for WebSocket (01 §12.1) and REST
  (03 §12.2, which includes 04's codes) codes; `fieldErrors.<field>.<code>` for 03's field codes; fallback `errors.unknown` with the code
  and request id. `rate_limited` shows the wait time in seconds (`ProtocolError.retryAfterMs / 1000` or
  `ApiError.retryAfterSec`). The `internal` reference shown is `params.ref` (WebSocket) or `requestId` (REST).
  `limit_reached` uses `errors.limit_reached.<params.limit>` (`rooms`, `invites`, `member_invites`,
  `pending_signups`), and `setting_locked` names `params.field`.
- **Client-local codes** live under `errors.local.*`: 01's `connection_lost`, `request_timeout`, `not_ready` (§12.4),
  plus 05's `capture_failed`, `h264_unavailable`, `webrtc_failed`, `offline`.
- **WebSocket errors** are handled as 01 §12.1–§12.3 prescribe (the "Client action" column), by scope:

  | Scope | 05 handling |
  |---|---|
  | `request` | Rejects the pending request (`SignalClient.request()` never retries; the caller applies 01 §12.1's action): `not_in_room` → RoomSession rejoins the desired room and retries once; `room_not_found` → navigate to `defaultRoomId` with a toast; `room_full` → "Room is full"; `rate_limited` → retry once after `retryAfterMs`; `internal` → retry once, then a toast with `params.ref`; `share_limit` → the ShareSheet message; `share_not_found` → ignore (`room.state` is authoritative); `feature_disabled` → hide the feature; others → a generic toast, only for user-initiated actions |
  | `subscription` | Log; the tile renders from `subscribe.status` |
  | `share` | Share panel shows the error; the local share goes to `failed` (§13.1) |
  | `pc` | An error whose `pc`/`gen`/`neg` match the outstanding offer clears it; `sdp_invalid` and `bad_request` → rebuild once (§9, 01 §9 rule 8); `stale_negotiation` → ignore; `rate_limited` → retry the rebuild after `retryAfterMs`; others → log |
  | `room` | Leave the room UI, go to `defaultRoomId`, toast (`room_closed`; `kicked`, reserved for a later admin kick: "You were removed from {room}") |
  | `connection` | `retryable` → backoff (01 client); else the Fatal or VersionMismatch screen |
  | `session` | `account_disabled` → Fatal screen (§7.1); other codes → `/login?next=…` with a notice (`session_revoked`: "You were signed out") |

- The login page shows the trust model in three lines (plan "Privacy and trust model"): the server is trusted and its
  admin could see streams; there's no recording and no hidden viewers; no telemetry. It links to `/about`.

---

## 7. Signaling (`src/rooms/connection.ts`)

01 specifies and owns the TypeScript protocol layer: `types.gen.ts` and `registry.gen.ts` (tygo plus a registry
generator), `SignalClient` (`signal-client.ts`, states `connecting | handshaking | ready | backoff | stopped`),
`ProtocolError` (`errors.ts`) and `detectCaps()`/`h264Key()` (`codecs.ts`). Its behaviour (ping every 15 s, 10 s pong
timeout, immediate ping on visible/`online` and through `probe()` on PC `disconnected` (§9), backoff 0.5 → 10 s ×2
±20 % with skip-wait, in-memory resume token, stale-build detection, close 1000 on `pagehide`, restart on bfcache
`pageshow`) is normative in 01 §3.4, §10 and §16. This doc doesn't repeat it.

The server also sends WebSocket ping frames (01 §3.4), which the browser answers itself. A hidden tab with throttled
timers therefore stays connected; 05 needs no keep-alive of its own.

05 owns the wiring:

```ts
// src/rooms/connection.ts
export function createConnection(platform: Platform, stores: Stores): SignalClient {
  const { url, auth } = platform.signaling();            // auth: later (M2), bearer in hello.auth (01 D2)
  const client = new SignalClient({
    url,
    client: platform.client,                             // 01 ClientInfo {kind:'web', version, os, browser}
    role: platform.role,                                 // 'full' if the platform can share, else 'viewer' (01 §6.3)
    caps: () => platform.capsNow(),                      // 01 detectCaps(), re-read on every (re)connect
    features: [],                                        // later: 'share.pause' (M2), 'layer.mid' (M5)
    onResync: (w) => stores.session?.resync(w),          // RoomSession, §11.1
  });
  client.onState((s, info) => stores.connection.setState(s, info));   // UI, §7.1
  return client;
}
```

- **One connection per tab**, created on the first RoomPage mount and kept until logout or tab close (§11.1).
- **Intentional leave**: 01's client closes with 1000 on `pagehide`; logout calls `stop()`. Both make the server skip
  the 30 s grace (01 §4.2).

### 7.1 Connection UI

| Signal state or event | UI |
|---|---|
| `backoff` for < 2 s | nothing (01 §10.2) |
| `backoff` 2–30 s | top banner "Reconnecting…"; media keeps playing if the PCs are healthy |
| `backoff` > 30 s | "Can't reach the server. Retrying… [Retry now] [Test my connection]" |
| `server.shutdown` (01's `onState` `info.shutdown`) or 503 `server_shutdown` | "Server restarting…" (no error styling) until `ready` |
| `ready` with `staleBuild` | §16.4 (reload once) |
| `stopped` (`unauthenticated`, `session_revoked`) | `/login?next=…` |
| `stopped` (`account_disabled`) | Fatal screen "Your account was disabled" |
| `stopped` (`protocol_unsupported`) | VersionMismatch (§16.4) |
| `stopped` (`too_many_connections`) | Fatal screen "Close other isshoni tabs" with Reload |
| `stopped` (`replaced`) | nothing (another socket took over) |
| `stopped` (`bad_message`) | Fatal screen with Reload |
| `stopped` (any other code, or a stop-type close code without an error: `hello_required`, `bad_request`, 1003, 4400, 4403) | Fatal screen "Something went wrong" with Reload (01 §12.3) |

"Retry now" calls 01's `SignalClient.retryNow()`. During a rate-limit wait (01 §10.2), `retryNow()` has no effect, and
the button is disabled with the remaining seconds. The banner is an `aria-live="polite"` region.

---

## 8. Platform adapter (`src/platform/`)

The plan's adapter, as exact interfaces. `BrowserPlatform` is the only M1 implementation. Wire types come from 01's
`types.gen.ts`.

```ts
// src/platform/types.ts
import type { ClientNotifications, ClientRequests, ServerEnvelope, ServerMessages } from '../protocol/registry.gen';
import type { Caps, ClientInfo, HelloAuth, MessageTypeHello, ShareKind, Preset, ShareParams }
  from '../protocol/types.gen';

export type PlatformKind = 'browser' | 'desktop' | 'mobile';
export type PushSupport = 'supported' | 'needs-install' | 'denied' | 'unsupported';

export interface PlatformCapabilities {
  caps: Caps;                                   // 01 detectCaps(): decode/encode CodecKeys, simulcast, displayCapture
  canShare: boolean;                            // sharing !== null and usable on this device
  fullscreen: 'element' | 'video-only' | 'none';
  pip: boolean;                                 // false on iOS in M1 (plan: PiP unreliable)
  wakeLock: boolean;
  push: PushSupport;
  install: 'prompt' | 'ios-manual' | 'installed' | 'none';
}

export interface KeyValueStore { get(k: string): string | null; set(k: string, v: string): void; remove(k: string): void }
// Both stores wrap Web Storage in try/catch and fall back to memory (private windows, blocked site data).
export interface WakeLockHandle { release(): Promise<void> }
export interface VersionActions { reload?: () => void; openInBrowser?: () => void; updateApp?: () => void }

export interface Platform {
  readonly kind: PlatformKind;
  readonly client: ClientInfo;                  // → hello.client
  readonly role: 'full' | 'viewer';             // browser: 'full' when sharing is possible; desktop SPA: 'viewer' (M2)
  readonly serverOrigin: string;                // browser: location.origin; desktop: the linked server (M2)
  capsNow(): Caps;                              // synchronous 01 detectCaps()
  capabilities(): Promise<PlatformCapabilities>;
  apiFetch(path: `/api/${string}`, init?: RequestInit): Promise<Response>;
  signaling(): { url: string; auth?: HelloAuth };   // url = new URL('/ws', serverOrigin) with ws(s):
  createPeerConnection(config: RTCConfiguration): RTCPeerConnection;
  readonly sharing: SharingProvider | null;     // null: can't share here (phones in M1)
  readonly notifications: NotificationsProvider;
  readonly pwa: PwaProvider | null;             // browser only
  requestWakeLock(): Promise<WakeLockHandle | null>;
  inAppBrowser(): string | null;                // a chat/social app's built-in browser (known UA token), else null
  openExternal(url: string): void;
  readonly storage: { local: KeyValueStore; session: KeyValueStore };
  versionActions(): VersionActions;
}

// ---- sharing ----
export interface PickedSource {
  kind: ShareKind;                              // 'screen' | 'window' | 'tab' (01); never a window title
  audioScope: 'window' | 'tab' | 'system' | 'none';
  warning: 'screen-with-system-audio' | 'no-audio' | null;
  preview: MediaStream;                         // owned by the provider
  release(): void;                              // stop tracks if the user backs out at the warning
}

// The part of 01's SignalClient (protocol/signal-client.ts) that sharing uses, as a structural type with
// SignalClient's own signatures (01 §16): request() never sends hello, and a listener gets its own type's envelope.
export interface SignalClientLike {
  request<K extends Exclude<keyof ClientRequests, typeof MessageTypeHello>>(
    type: K, data: ClientRequests[K]['data'], opts?: { timeoutMs?: number },
  ): Promise<ClientRequests[K]['result']>;
  notify<K extends keyof ClientNotifications>(type: K, data: ClientNotifications[K]): boolean;
  on<K extends keyof ServerMessages>(
    type: K, fn: (data: ServerMessages[K], env: ServerEnvelope<K>) => void,
  ): () => void;
  probe(): void;
}

export interface ShareContext {
  signal: SignalClientLike;
  roomId: string;
}

export interface ShareStatsSample {
  at: number;                                   // performance.now()
  layers: { rid: string; width?: number; height?: number; fps?: number; kbps: number;
            limit?: 'none' | 'bandwidth' | 'cpu' | 'other'; encoder?: string; hw?: boolean }[];
  audioKbps: number;
}

export type LocalShareState = 'starting' | 'live' | 'paused' /* later (M2) */ | 'reconnecting' | 'ended';
export type LocalEndReason = 'user' | 'browser-stopped' | 'server' | 'server-unreachable' | 'error';
export type ShareHint = { kind: 'upload-limited'; approxHeight: number } | { kind: 'cpu-limited' };

export interface ActiveShare {
  readonly shareId: string;                     // changes on re-publish (`replaces`, 01 §10.6); watch 'state'
  readonly kind: ShareKind;
  readonly preview: MediaStream | null;
  readonly state: LocalShareState;
  readonly params: ShareParams | null;          // latest from share.start / share.update / quality.hint
  on(ev: 'state', fn: (s: LocalShareState) => void): () => void;
  on(ev: 'ended', fn: (r: LocalEndReason) => void): () => void;
  on(ev: 'hint', fn: (h: ShareHint | null) => void): () => void;
  setPreset(p: Preset): Promise<void>;
  setPaused(paused: boolean): Promise<void>;   // later (M2, feature share.pause); M1 rejects with not_supported
  setAudioEnabled(on: boolean): Promise<void>;
  stop(): Promise<void>;
  stats(): Promise<ShareStatsSample | null>;
}

export interface SharingProvider {
  readonly mode: 'in-page' | 'native';          // browser: 'in-page'; desktop app and Linux agent: 'native' (M2/M4)
  /** Call synchronously from the click handler (transient user activation). null = cancelled. */
  pick(opts: { preset: Preset }): Promise<PickedSource | null>;
  start(src: PickedSource, opts: { preset: Preset; withAudio: boolean }, ctx: ShareContext): Promise<ActiveShare>;
}

// ---- notifications and PWA ----
export interface NotificationsProvider {
  support(): Promise<PushSupport>;
  status(): Promise<'on' | 'off'>;
  enable(): Promise<void>;                      // from a user gesture; throws ApiError or a local error
  disable(): Promise<void>;
  test(): Promise<void>;
}
export interface PwaProvider {
  installState(): 'prompt' | 'ios-manual' | 'installed' | 'none';
  promptInstall(): Promise<'accepted' | 'dismissed' | 'unavailable'>;
  onUpdateReady(fn: () => void): () => void;
  applyUpdate(): void;                          // postMessage SKIP_WAITING, reload on controllerchange
}
```

`detect.ts`:

```ts
export function detectPlatform(): Platform {
  // later (M2/M3): window.__ISSHONI_DESKTOP__ (the Wails bridge) → new DesktopPlatform(bridge)
  //   Two transports (plan): Wails bindings, and the same-user agent relay (01 §8.14; Linux agent M4,
  //   web → desktop handoff M2).
  // pending (mobile apps): window.Capacitor?.isNativePlatform?.() → new MobilePlatform()
  return new BrowserPlatform();
}
```

`ShareContext.signal` is the structural `SignalClientLike`, not the `SignalClient` class: `platform/` declares what
sharing needs from the client (`request`, `notify`, `on`, `probe`) and imports only generated types. `SignalClient`
satisfies it, which `platform/types.test.ts` checks at compile time, so callers pass the client as it is and a
`SharingProvider` test passes a small fake.

The global names `__ISSHONI_DESKTOP__` and `Capacitor` are reserved now. `DesktopPlatform` (M2) will use a bearer
token in `apiFetch` and `hello.auth`, `serverOrigin` from the linked server and `role: 'viewer'`. So nothing in the SPA
outside `platform/` may read cookies or `location.origin` or build `/api` URLs by hand.

**BrowserPlatform** specifics:
- `client.os`/`browser` come from `navigator.userAgentData` when present, else the UA string (`lib/ua.ts`). They are
  used for `hello.client` (diagnostics only, 01) and help text, never to gate features.
- `capsNow()` is 01's `detectCaps()` (H.264 packetization-mode 1 profiles as `CodecKey`s, `opus`, `simulcast`,
  `displayCapture`).
- `sharing` is non-null when `navigator.mediaDevices?.getDisplayMedia` exists, the sender can encode H.264, and the
  device is not a phone or tablet (`userAgentData.mobile`, else `(pointer: coarse)` without `getDisplayMedia`). This is
  feature detection, not a browser allowlist; §13.8 covers Firefox and Safari. `role` follows it.
- `fullscreen`: `document.fullscreenEnabled` → `element`; else `HTMLVideoElement.prototype.webkitEnterFullscreen` →
  `video-only` (iPhone); else `none` (CSS pseudo-fullscreen).
- `createPeerConnection` is `new RTCPeerConnection(config)`. Keeping it behind the adapter lets tests inject fakes.
- `inAppBrowser()` (`lib/ua.ts`) matches only known UA tokens: `FBAN`, `FBAV` (Facebook, Messenger), `Instagram`,
  `Line/` and `MicroMessenger` (WeChat). It returns the matched app name, else `null`. There are no heuristics: many
  chat apps (often Discord and Telegram, depending on version and settings) open links in a view whose UA is plain
  Safari or Chrome, and those cases get the one-line tip in §16.3 instead. Like `client.os`, the result only chooses
  help text (the §16.3 banner), never gates a feature. `DesktopPlatform` (M2) returns `null`.

---

## 9. WebRTC rules

01 §9 (negotiation) and §10.4 (PC recovery) are normative for the web client. How 05 implements them:

- **Configuration** for every PC: `{iceServers: welcome.iceServers, bundlePolicy: 'max-bundle', rtcpMuxPolicy:
  'require'}`. `iceServers` is `[]` in M1: the server has a public address and puts all its candidates (UDP 7882,
  TCP 443, TCP 7882) in its SDP. ICE-TCP needs nothing from the client; the browser pairs its active TCP candidates with
  the server's passive ones when UDP fails.
- **Fixed offerers, `gen` and `neg`**: the client offers on `pub`, the server on `sub`. Each PC class keeps its `gen`
  and `neg`, has at most one outstanding offer, folds changes made meanwhile into one follow-up offer, drops messages
  of an older `gen`, ignores answers whose `neg` isn't the outstanding one, and (as answerer) re-sends its stored last
  answer for a repeated `neg`.
- **Track mapping** by the offer's `tracks` (`mid → shareId, kind`), re-read on every offer, for both PCs: the pub
  offer lists this page's shares, and every sub offer from the server lists the mapping (01 §9 rule 4, 02 §5.2). The
  sub msid stream id equals the shareId (02) and is a debugging aid only.
- **Trickle**: the client sends `pc.ice {pc, gen, candidate}` and buffers remote candidates that arrive before the
  remote description (at most 64 per PC, oldest dropped).
- **Promise queue per PC**: `setRemoteDescription`, `createAnswer`/`createOffer`, `setLocalDescription` never
  interleave.
- **Recovery** (01 §10.4), per PC. The client drives recovery of both PCs. The server starts only three actions on
  its own: `pc.restart {pc:'pub', gen, mode:'rebuild', reason:'failed'}` when its side of the current pub PC reaches
  `failed` or a new pub PC misses its 10 s handshake; an ICE-restart offer for a sub PC that isn't connected, inside
  `Resync()` after a resumed `welcome`; and its own sub rebuilds for codec recovery (02 §8.5, §10.6 here). It never
  sends `pc.restart {mode:'ice'}` and never rebuilds a sub PC because of its own ICE state, so outside a resync a
  broken sub PC is fixed only by the client's requests in the sub column below.

  When any PC's `connectionState` or `iceConnectionState` becomes `disconnected`, call `signal.probe()` at once, then
  start the 3 s timer.

  | Condition | pub (client offers) | sub (server offers) |
  |---|---|---|
  | ICE `disconnected` for 3 s (timer cancelled if it recovers) | `restartIce()` and a new offer, same `gen` | `pc.restart {pc:'sub', gen, mode:'ice', reason:'disconnected'}` |
  | ICE restart not `connected` within 15 s | rebuild: new PC, `gen + 1`, same tracks and shareIds | 15 s from the sub offer with a new `ice-ufrag` (below): `pc.restart {pc:'sub', gen, mode:'rebuild', reason:'disconnected'}`; the server offers `gen + 1` |
  | PC `failed` | rebuild at once | `pc.restart {pc:'sub', gen, mode:'rebuild', reason:'failed'}` |
  | after a resumed `welcome`, the PC isn't `connected` | ICE restart | nothing: the server's `Resync()` sends an ICE-restart offer, which counts as the ICE restart (below); the timers above keep running |
  | server sends `pc.restart {pc:'pub', mode:'rebuild'}` | rebuild | — |
  | `sdp_invalid` or `bad_request` error (scope `pc`) | rebuild once; a second within 60 s → "Can't connect media" with Reload | same, through `pc.restart {mode:'rebuild'}` |

  **A sub offer with a new `ice-ufrag` is the sub ICE restart**, whoever asked for it: the client's
  `pc.restart {pc:'sub', mode:'ice'}` or the server's `Resync()` (01 §10.4, 02 §5.3). `SubscriberPC.handleOffer`
  compares the offer's `a=ice-ufrag` with the current remote description's. On a change it cancels the 3 s timer, so
  no `pc.restart {mode:'ice'}` of its own follows, and starts the 15 s rebuild timer from that offer, not from its
  request. This is the usual case after a network switch (Wi-Fi to LTE): the sub PC has been `disconnected` for 3 s
  when the socket resumes, so `Resync()` and the client's recorded rule both ask for a restart. The SFU runs only one
  (it skips a request while one is queued or was offered less than 5 s ago), and the client's timer follows the
  restart that actually happens.

  Restart reasons are `disconnected` and `failed` only. Limits (they bind the client only): one ICE restart per PC per
  5 s, one rebuild per PC per 10 s. A `pc.restart` from the server whose `gen` is older than the current pub `gen` is
  ignored. After 5 rebuilds without reaching `connected` (about 1 min), the room shows "Can't reach the server's media
  port. [Test my connection]" and keeps retrying every 30 s. While signaling isn't `ready`, PC state changes are only
  recorded; the rules run on `ready`.
- **Closing**: `pc.close {pc:'pub', gen}` when the pub PC is closed on purpose (last share stopped, leaving the room).
  The sub PC is closed locally.

---

## 10. Subscribing (viewer PC)

### 10.1 `SubscriberPC`

```ts
export class SubscriberPC {
  constructor(deps: { platform: Platform; signal: SignalClient; registry: MediaRegistry; log: Logger });
  readonly gen: number;
  handleOffer(o: PCOffer): Promise<void>;       // queued; higher gen → replace the PC; repeated neg → resend answer; new ice-ufrag = ICE restart (§9)
  handleIce(i: PCICE): Promise<void>;
  requestRestart(mode: RestartMode, reason: RestartReason): void;   // pc.restart {pc:'sub', gen, mode, reason}; reason 'disconnected' | 'failed'
  getStats(): Promise<RTCStatsReport | null>;
  close(): void;                                // closes the local PC; no message (room.leave closes the server side)
}
```

- The PC is created on the first `pc.offer {pc:'sub'}` (the server offers once the first subscription exists).
- `handleOffer`: map `tracks` → `setRemoteDescription(offer)` → flush buffered ICE → `createAnswer()` → **Opus stereo
  munge** (`lib/sdp.ts forceOpusStereo`: add `stereo=1;sprop-stereo=1` to the Opus fmtp if missing; 01 §9 rule 6,
  spike `forceOpusStereo`) → `setLocalDescription` → `notify('pc.answer', {pc:'sub', gen, neg, sdp})`. If the munged
  answer is rejected, retry once unmunged and log.
- `ontrack`: `shareId` from the track map by `transceiver.mid` (a missing mapping is a bug: log it and ignore the track) →
  `registry.set(shareId, kind, track)`. Transceivers are reused (02), so `ontrack` fires again for a new share on an
  old mid; the registry replaces the entry.
- Shares that end disappear from `room.state`; their tiles unmount and the registry entry is dropped. The client never
  removes transceivers itself.

### 10.2 Tile media and playback

- Each tile has one `<video autoplay playsinline muted disableRemotePlayback>` whose `srcObject` holds only that share's
  video track. Videos are **always muted**, so they autoplay everywhere.
- **All audio plays through one `<audio>` element** (`viewer/audioOut.ts`), created once per page load and appended to
  `document.body`, so the unlock survives room switches. Its `srcObject` is `new MediaStream([audioTrack])` of the
  audible share. Audio-follows-focus is a `srcObject` swap. Lip sync still works: the SFU gives a share's audio and
  video DownTracks one msid stream and forwards SRs (02), and the browser syncs them in the receiver, not per element.
- The spike's approach (per-tile `video.muted` toggling) is dropped because iOS needs a gesture for every element that
  starts playing with sound.

### 10.3 Audio unlock ("tap to unmute")

```
locked ──assign track, play() ok──► playing ◄──tap (user gesture): audio.play() + video.play() for blocked tiles── blocked
   │                                   │  ▲                                                                         ▲
   └──── play() NotAllowedError ───────┼──┼──────────────────────────────────────────────────────────────────────────┘
                                       │  └── unmute ── muted ◄── user mutes (M key / button)
                                       └── later play() rejects (iOS interruption) → blocked
```

- `TapToStart` is a large centred button over the stage ("Tap to unmute") plus a header pill. One tap handler calls
  `audio.play()` synchronously and retries `play()` on every tile video that was rejected. iOS Low Power Mode blocks
  even muted autoplay; the same button covers it ("Tap to start video").
- Chrome usually allows sound after the invite or login form (sticky activation in the same document) and in installed
  PWAs; iOS Safari normally needs the tap (plan journey 2: "needs one tap to unmute").
- `navigator.audioSession.type` is **not** changed: `'playback'` could interrupt a voice call running on the same phone.
  Manual test M-IOS-3 checks a Discord call and isshoni together (§19.4).
- Volume: a slider on the stage (desktop). iOS ignores `volume` (read back after setting; hide the slider when it
  doesn't change). Stored in `prefsStore.volume`.

### 10.4 Subscription status

`subscribe.status` (01 §8.9) is stored per share in `viewerStore.status`:

| `reason` | Tile |
|---|---|
| absent | normal |
| `waiting` | spinner, "Connecting…" |
| `bandwidth` | badge "Lower quality (your connection)" |
| `unavailable` | badge "The sharer isn't sending this quality right now" (e.g. a 4K60 share without a preview layer, S2-VT finding 5) |
| `codec` | §10.6 |
| unknown | shown without a specific reason (01 §8.13) |

### 10.5 Rebuild and resume

The PC recovery rules are in §9. After a signaling `welcome`, `RoomSession.resync` (§11.1) re-sends the desired
subscriptions; with `resumed: false` it first discards both PCs and resets `gen`.

### 10.6 Firefox and the OpenH264 wait

S4 found that a fresh Firefox profile negotiates H.264 only after it downloads Cisco's OpenH264 plugin in the
background (about a minute), and that Firefox decodes only Constrained Baseline and Baseline (finding 3). The flow is
01 §11.7 with 02 §8.5:
- The browser subscribes normally. The server sends audio only and `subscribe.status {video: off, reason: codec}`,
  which is the only signal (no subscription-scoped error; `codec_not_supported` is used for scope `share` only).
- On that status the tile keeps playing audio and shows: "Your browser is still getting its video decoder (Firefox
  does this once, about a minute). Retrying…". A toast, if any, is shown on the first status with `reason: codec`.
- While any subscription has `reason: codec`, the client re-reads `detectCaps()` every 5 s. When the H.264 list
  changes, it sends `caps.update {caps}`; the server rebuilds the sub PC (`gen + 1`), and video starts.
- The client sends no `pc.restart` for codecs. For a Firefox that lists H.264 before it can use it, the server
  rebuilds the sub PC itself every 20 s, at most 9 times (02 §8.5); the client just answers the new offers.
- After 3 minutes: "Firefox hasn't got its video decoder yet. Reload the page. If that doesn't help, open Settings →
  Extensions & Themes → Plugins and check that 'OpenH264 Video Codec provided by Cisco Systems, Inc.' is enabled, or
  use Chrome or Edge."
- A browser with no H.264 decoder at all (and not Firefox) shows a room banner: "This browser can't play isshoni video.
  Use Chrome, Edge, Safari or Firefox." Audio still plays.

### 10.7 Stats collection (for "Copy diagnostics", M5)

`lib/stats/collector.ts` runs while a RoomSession exists:
- Samples `getStats()` of the sub and pub PCs every 2 s (every 1 s while the debug overlay is open) and summarizes:
  - per subscribed share, video: `frameWidth/Height`, `framesPerSecond`, kbps, `framesDecoded`, `framesDropped`,
    `keyFramesDecoded`, `freezeCount`, `totalFreezesDuration`, `packetsLost`, jitter-buffer ms, `pliCount`,
    `nackCount`, codec, and `decoderImplementation`/`powerEfficientDecoder` (Chrome exposes these two only to pages
    with an active capture);
  - audio: kbps, `audioLevel`, `totalAudioEnergy`, `concealedSamples`, `totalSamplesReceived` (and their ratio);
  - per outbound rid: size, fps, kbps, `qualityLimitationReason`, `encoderImplementation`, `powerEfficientEncoder`;
  - per PC: state, the selected pair's local candidate type and transport (`udp`/`tcp`), `currentRoundTripTime`,
    `availableOutgoingBitrate`. **No IP addresses are stored.**
- Keeps a ring buffer of 150 samples (5 minutes).
- Consumers in M1:
  - the `stats` notification every 10 s, in 01's `ClientStats` shape (`pcs`, `inbound`, `outbound`, §8.11), mapped
    as below;
  - tile freeze detection (§12.6) and the sharer hints (§13.7);
  - the debug overlay (`Shift+D` or `?debug=1`), which also turns on 01's `stats.watch` for the server's view;
  - e2e assertions.
- **getStats → `ClientStats`** (`lib/stats/summarize.ts`; rates are computed from the previous sample):

  | 01 type | Field ← source |
  |---|---|
  | `PCStats` | `state` ← `connectionState`; `rttMs` ← selected pair `currentRoundTripTime` × 1000; `outgoingBitrate` ← `availableOutgoingBitrate`; `candidateType`, `transport` ← the selected pair's local candidate |
  | `InboundStats` | `bitrate`, `packetsLost`, `jitterBufferMs`, `fps`, `width`, `height`, `freezeCount`; `decoder` ← `decoderImplementation`; `hwDecoder` ← `powerEfficientDecoder`; `codec` ← `h264Key(sdpFmtpLine)` of the inbound codec; `framesDecoded`, `framesDropped`; `freezeDurationMs` ← `totalFreezesDuration` × 1000; `concealedSamples`; `totalSamples` ← `totalSamplesReceived` |
  | `OutboundStats` | `rid`, `bitrate`, `fps`, `width`, `height`; `encoder` ← `encoderImplementation`; `hwEncoder` ← `powerEfficientEncoder`; `qualityLimitation` ← `qualityLimitationReason` |

- `lib/log.ts` keeps the last 500 log lines in memory, with no SDP bodies and no tokens.
- *later (M5)*: "Copy diagnostics" serializes build and protocol versions, UA, caps, the last 60 samples, the log and
  the last connection-test result.

`window.__isshoni` (read-only `stats()` and `state()`, plus `dropSocket()` for the reconnect test, which closes the
socket with code 3000 so the server treats it as a network drop) exists only when
`sessionStorage['isshoni.debug'] === '1'`, which e2e sets with `addInitScript`. It exposes nothing the user can't see
in the debug overlay.

---

## 11. Room session and presence (`src/rooms/`)

### 11.1 `RoomSession`

```ts
export class RoomSession {
  constructor(deps: { platform: Platform; signal: SignalClient; stores: Stores; log: Logger });
  join(roomId: string): Promise<void>;        // records the desired room; room.join when ready (ok is followed by room.state, 01 §7)
  leave(): Promise<void>;                     // stops local shares, pc.close {pub}, room.leave, closes the sub PC locally
  resync(w: Welcome): Promise<void>;          // 01 §10.5, called by the SignalClient's onResync
  startShare(p: PickedSource, o: { preset: Preset; withAudio: boolean }): Promise<void>;
  stopShare(): Promise<void>;
  dispose(): void;
}
```

**Desired room.** `join(roomId)` first stores `roomId` as the desired room (the "memory" of 01 §10.5), before
anything else. If signaling is `ready`, it sends `room.join`; otherwise it resolves when the next resync has joined,
and never surfaces `connection_lost`. `leave()` clears the desired room.

`resync` implements 01 §10.5. 01's `SignalClient` calls it for every `welcome` (the first after `start()` included),
with `state` already `ready`, and doesn't await it; `onState` listeners hear `ready` right after it returns. It joins
the desired room first. Only with no desired room, and only when the route has no room id, does it fall back to
`isshoni.lastRoomId`, then `welcome.defaultRoomId` (not after a `leave()`); with no room at all it only resets PCs.
- **Resumed**: if `w.roomId` is absent or differs from the desired room, send `room.join` for the desired room first;
  re-send a pending pub offer (same `neg`); apply §9's after-resume row (ICE-restart a non-connected pub PC; the
  server's `Resync()` handles the sub PC); send the full desired subscription set with `subscribe.update` when it is
  non-empty, in chunks of at most 64 items; retry requests that failed with `connection_lost` (`share.start` with the
  same `ref`); reconcile shares against `room.state` (a local live share missing on the server is re-published with
  `replaces`; a server share of this `connectionId` that the page no longer has gets `share.stop`). This re-publish
  rule applies only here, during resync; a share the server ends at other times follows §13.1.
- **Not resumed**: discard both PCs and reset `gen`; `room.join` the desired room (fallbacks above); re-publish every
  local share whose capture is still alive (`share.start {replaces, ref: new}`, new pub PC `gen 1`); send the desired
  subscriptions mapped to the new share ids (when non-empty, in chunks of at most 64).

Other rules:
- The session is **app-level**: it survives navigation to `/account` or `/admin`. Leaving the room happens only on
  "Leave", switching rooms, logout or closing the tab. Away from the room page, tiles unmount, so their video drops to
  `off` (§12.4) while the audible share keeps playing; an `InRoomBar` shows "In Lounge · sharing · [Back] [Leave]".
  Rationale: changing a setting shouldn't end a share or a movie night.
- Switching rooms while sharing asks "Stop sharing and switch to <room>?" (01: joining another room ends the shares).
- `room.state` snapshots replace the stored one when `rev` is newer; `rev` resets on every `welcome` (01 §8.5).
- **`room.event`** drives toasts and `aria-live` only (01 §8.6): `participant.joined`/`left` and `share.started`/
  `stopped`, with the user's name. Events of the user's own id and `share.started` with `replaces` are not announced.
  `room.event share.stopped` of the own user is not announced as a toast, but when its `shareId` is a local share it
  drives the share state machine (§13.1).
- **Participant status** `reconnecting` dims the person in the people panel.

### 11.2 Room page layout

- Header: room name, the switcher when `GET /api/v1/rooms` says `showRoomList` (03: the list stays hidden until a
  second room exists), people count ("5 here"), Share button (when `canShare`), account menu. Admins see "Create room"
  (a link to `/admin/rooms`; 03 has no member endpoint) only when `showRoomList` is true, as the plan requires. Until
  then, admins create the second room under Admin → Rooms.
- People panel (drawer): each participant with a "sharing" badge and an admin badge. Clicking a sharing person focuses
  their share. OS and version of others are not shown (01 §8.5 privacy).
- Members with `me.permissions.createInvites` and admins get "Invite friends" in the account menu, which creates an
  invite and shows the `InviteLinkCard` (§14.1). Under the same `createInvites` condition, members also get "My
  invites", a link to `/admin/invites` (§5, `RequireInviter`).
- Empty state: "Nobody is sharing yet." plus [Share your screen] (if `canShare`) or "Sharing from phones isn't
  available yet", and the notifications card (§16.3).
- The document title shows live state: `● 2 live · Lounge · isshoni`, and `● Sharing · …` while you share.

---

## 12. Viewer (`src/viewer/`)

### 12.1 Layout

`ViewerLayout` has a **stage** (the focused share, high layer) and **others** (every other remote `live` or `stalled`
share plus the own share's local preview, low layer), ordered newest first. Shares in `starting` get no tile (01 §4.4).

| Viewport | Stage | Others |
|---|---|---|
| ≥ 1024 px wide | fills the main area | right column 280 px wide, vertical scroll, 16:9 tiles |
| < 1024 px, portrait | full width, 16:9, top | horizontal scroll strip below (tile width `min(44vw, 220px)`) |
| phone landscape | fills the viewport | hidden; a "Shares (N)" button opens a bottom sheet |
| fullscreen | only the stage container | not rendered |

Tiles are `object-fit: contain`. Safe areas use `env(safe-area-inset-*)` with `viewport-fit=cover`. Touch targets are
at least 44×44 px. With no remote share, the stage shows the empty state (§11.2), or "You're live · 3 watching" while
you share.

### 12.2 Focus

- **Auto-focus** (plan: "the most recent share is focused automatically"): while `focusMode === 'auto'`, the stage shows
  the newest remote share by `startedAt`.
- A click, tap, Enter/Space or number key on a tile sets `focusMode = 'manual'` and focuses that tile. Manual focus
  holds until that share ends; then auto-focus resumes. While manual, a new share shows a toast "bo started sharing
  [Watch]" instead of taking the stage. Rationale: the plan's rule, without jumping away from a movie the user picked
  (owner question, §24). The unmute tap doesn't count as a manual choice.
- `?focus=<shareId>` (push notification links, 04 §14.3) acts as a manual focus once the share is `live`; it waits up to
  5 s for the share to appear, then is dropped from the URL.
- **Re-published shares** (01 §10.6): when a new share's `replaces` names the focused or audible share and both have
  the same `userId`, focus, tile position and audio move to the new share.
- The own share is never auto-focused and is never subscribed. Clicking it enlarges the local preview.
- `autoFocus.ts` is a pure reducer `(state, event) → state` with events `shareLive`, `shareEnded`, `shareReplaced`,
  `userFocus`, `focusParam`, unit-tested.

### 12.3 Audio follows focus

- `audibleShareId` defaults to the focused share. Focusing moves the audio.
- Every other tile has a **speaker button** ("Listen to bo"). It moves the audio to that tile without moving video
  focus; the stage then shows a muted-speaker indicator. Exactly one share is audible at a time.
- Each change is **one** `subscribe.update` with all affected shares, so the server stops the old audio and starts the
  new one in the same step (01 §8.9).

### 12.4 Layer policy

`layerPolicy.ts` is a pure function, unit-tested, and the only place that decides what to subscribe to. 01 §8.9 calls
this "05 policy".

```ts
import type { VideoLayer, AudioState, SubscriptionWant } from '../protocol/types.gen';
export interface LayerInputs {
  remoteShares: string[];                  // live or stalled, excluding own shares
  focused: string | null;
  audible: string | null;
  visible: Record<string, boolean>;        // IntersectionObserver, ≥ 10% visible
  pageHiddenForMs: number;                 // 0 when visible
  fullscreen: boolean;
  pip: string | null;
}
export function desiredSubscriptions(i: LayerInputs): SubscriptionWant[];   // {shareId, video, audio}
```

Rules, in order:
1. Audio: `on` only for `audible`, whatever the page visibility (background tabs keep playing sound on desktop and
   Android).
2. The PiP share → `high`, even when the page is hidden.
3. Page hidden for ≥ 10 s → every other share's video `off` (01 §11.4's example).
4. The focused share → `high`.
5. In fullscreen, every non-focused share → `off` (IntersectionObserver can't see that they're covered).
6. Other shares: visible → `low`, not visible (off-screen, scrolled away, unmounted) → `off`.

`SubscriptionSync` (in RoomSession) diffs the result against what it last sent and sends one `subscribe.update`
request with every changed share (at most 64 per message, 01 §13). Raising a layer is sent at once (debounced 150 ms to
merge focus changes). Dropping to `off` because a tile left the viewport waits 1 s (no flapping while scrolling). The
full desired set is re-sent after every `welcome` when it is non-empty, in chunks of at most 64 items (01 §8.9
requires 1–64) (§11.1). `ok.ignored` ids are dropped from local state.

### 12.5 Fullscreen, PiP, wake lock

- Fullscreen: `requestFullscreen()` on the stage container, so overlays stay visible. iPhone:
  `video.webkitEnterFullscreen()` (audio keeps playing from the `<audio>` element). Otherwise a CSS pseudo-fullscreen
  overlay. Toggle with `F`, double-click or double-tap on the stage, or the button.
- PiP (`requestPictureInPicture` on the stage video): desktop only. It's hidden on iOS in M1 (plan: PiP unreliable).
- Wake lock: `navigator.wakeLock.request('screen')` while a share is being watched and the page is visible. It's
  released when hidden and re-requested on `visibilitychange → visible`.

### 12.6 Tile states

| State | Condition | Overlay |
|---|---|---|
| connecting | subscribed, no track yet, or `reason: waiting` | spinner, "Connecting…" |
| playing | frames decoding | — |
| frozen | `framesDecoded` unchanged for 3 s while video ≠ off and the share is `live` | "Waiting for video…" |
| stalled | share `status: stalled` (the sharer's connection dropped, 01 §4.4) | last frame, "Connection unstable" |
| lower-quality | `reason: bandwidth` or `unavailable` | badge (§10.4) |
| decoder-pending | `reason: codec` | §10.6 message; audio keeps playing |
| blocked | autoplay rejected | TapToStart |

Each tile shows the owner's name, the localized kind (`share.label.<kind>`: "Screen", "Window", "Tab"; a user-typed
`label` if present) and an eye with the **viewer count** (`share.watchers.length`, 01 §8.5). The eye opens
`WatchersPopover` with the names ("Watching: bo, cy, you"). The plan's "no hidden viewers" applies to everyone, admins
included.

### 12.7 Keyboard

The tile list is a roving-tabindex group (one tab stop). Shortcuts are active when focus isn't in a text field:

| Key | Action |
|---|---|
| Arrow keys | move between tiles |
| Enter / Space | focus the tile's share |
| 1–9 | focus the Nth share (newest first) |
| F | fullscreen on/off |
| M | mute/unmute |
| L | listen to the keyboard-focused tile (moves the audio) |
| Esc | leave fullscreen, close dialogs (native `<dialog>`) |
| ? | shortcuts dialog |
| Shift+D | debug overlay |

### 12.8 Mobile and iOS limits

- iOS: foreground only. WebRTC is suspended in the background and when the screen locks. On return
  (`visibilitychange → visible`): 01's client pings at once and resumes or reconnects; the viewer re-`play()`s every
  video and shows TapToStart if the audio element was interrupted.
- A one-time hint on iOS: "Keep isshoni open while watching. Video pauses when you switch apps." (dismissal stored).
- No PiP on iOS in M1; fullscreen is the native video player on iPhone.
- Android Chrome: background audio keeps playing (rule 1 in §12.4). `navigator.mediaSession.metadata` is set to
  `{title: "bo's window", artist: "<room>"}` so the notification shade shows what's playing.

---

## 13. Web sharer (`src/share/`)

### 13.1 Flow and state machine

```
idle ─Share click─► picking ──cancelled──► idle
                      │ picked
                      ▼
                  confirming (only when warning = screen-with-system-audio) ──pick again / cancel──► idle
                      │ continue (with or without sound)
                      ▼
                  starting: share.start → ShareParams; transceivers; codec prefs; pc.offer; answer; setParameters
                      │ first keyframe (share live in room.state)       │ error
                      ▼                                                 ▼
   ┌──────────────► live                                             failed ──(dismiss)──► idle
   │                  │
   │   welcome resumed:false, or pub rebuild → reconnecting → live (re-published with `replaces` if the id changed)
   │                  │
   └──────────────────┤ Stop button, track ended (browser's "Stop sharing"), leave, logout,
                      │ or 60 s without the server (01 §10.6: "Sharing stopped: the server was unreachable")
                      ▼
                  stopping: share.stop, stop transceivers and re-offer (or pc.close), stop tracks ──► idle

   starting / live ──server ended the share──► stop tracks, remove transceivers, re-offer (or pc.close) ──► idle or failed
```

- **Server ended the share** (a transition out of `starting` and `live`): `room.event share.stopped` for our
  `shareId` while we aren't stopping it ourselves, or, as a fallback, a newer `room.state` without it while signaling
  is `ready` and no resync is running. The page stops the capture tracks, removes the transceivers and re-offers the
  pub PC (or sends `pc.close` when none are left), then goes by the event's `reason`:
  - `stopped` or `left` → `idle` with the toast "Sharing was stopped from another tab or device"
    (`share.ended.elsewhere`);
  - `media_timeout` → `failed` with "Sharing stopped: no video reached the server" (`share.ended.mediaTimeout`) and
    [Test my connection];
  - `room_closed` → the room-scope handling (§6.3);
  - any other or unknown reason, and the `room.state` fallback → `failed` with "Sharing stopped by the server"
    (`share.ended.generic`).

  It is never re-published here: re-publishing with `replaces` happens only in resync after a resumed `welcome`
  (§11.1).

- `ShareSheet` opens from the Share button with a preset picker (Auto, Game, Movie, Text, one line each), the tip
  "Recommended: pick a **Window** and keep **Share audio** on. Friends hear only that window.", and "Get the desktop
  app (keeps Discord out of your audio)" linking to `/download`.
- Its **Share** button calls `platform.sharing.pick()` as the first statement of the click handler: no `await` before
  `getDisplayMedia` (transient activation).
- The M1 UI starts at most one share. If `room.state` has a live share of the same user from another connection, the
  sheet says "You're already sharing from another tab or device" with a **Stop it** button (`share.stop` on that
  share, allowed for the same user, 01 §4.1).

### 13.2 `getDisplayMedia` options (plan values, feature-detected)

```ts
const options: DisplayMediaStreamOptions = {
  video: { frameRate: { ideal: 60, max: 60 }, displaySurface: 'window' },
  audio: {
    echoCancellation: false, noiseSuppression: false, autoGainControl: false,
    suppressLocalAudioPlayback: false,       // only if in getSupportedConstraints()
    restrictOwnAudio: true,                  // only if in getSupportedConstraints(); keeps this tab's playback out
  },
  systemAudio: 'include',
  windowAudio: 'window',                     // per-app audio on Windows 11 and macOS 14.2+ with a current Chrome
  selfBrowserSurface: 'exclude',             // never share the isshoni tab itself (mirror + audio loop)
  surfaceSwitching: 'include',
  monitorTypeSurfaces: 'include',
  preferCurrentTab: false,
};
```

- `displaySurface: 'window'` makes the picker open on Windows (plan: "the picker defaults to a window").
- No width or height constraints: the capture keeps its native size, and the encodings' `maxPixels` budget (01 §8.7)
  scales it, so ultrawide screens keep their aspect ratio.
- Errors: `TypeError` (an option value this browser doesn't know) → retry once with only `video`, `audio` and
  `systemAudio`. `OverconstrainedError` → retry without `frameRate`. `NotAllowedError` → treated as cancel, no message
  (it can't be told apart from a denial). `NotReadableError`/`NotFoundError`/`AbortError` →
  `errors.local.capture_failed` ("Couldn't capture that. Another app may be blocking it.").
- The audio track gets `contentHint = 'music'` (plan). The video track's hint comes from the preset (§13.5).

### 13.3 Classifying the pick and the whole-screen warning

`classify.ts` (pure) uses `videoTrack.getSettings().displaySurface` and whether an audio track came back:

| displaySurface | audio | `kind` | `audioScope` | UX |
|---|---|---|---|---|
| `window` | yes | window | window | Recommended: straight to live |
| `window` | no | window | none | Live, with the note "This window's sound isn't shared (needs Windows 11 or macOS 14.2+ and a current Chrome/Edge)" |
| `browser` | yes | tab | tab | Live |
| `browser` | no | tab | none | Live, note "Tick 'Also share tab audio' to include sound" |
| `monitor` | yes | screen | system | **Warning dialog** (below) |
| `monitor` | no | screen | none | Live, note "No sound is shared" |

The warning (`ScreenAudioWarning`, a modal dialog): "Friends will hear everything on this computer, including your
voice app (Discord, TeamSpeak…), so they'll hear themselves." One more line explains that browser screen sharing can't
leave apps out: Discord Web's own screen share has the same problem (PLAN, "Discord Web's own screen share", checked in
its code on 2026-09-29), and isshoni's desktop app keeps voice apps out. Buttons:
- **Share without sound** (primary): stops the audio track and continues;
- **Share with sound anyway**;
- **Pick something else**: releases the stream and reopens the picker from this click;
- link: **Get the desktop app** → `/download`.

### 13.4 Starting: `share.start`, codec preferences and encodings

1. `request('share.start', {kind, preset, audio, ref})` (01 §8.7; `ref` is a fresh random id; `replaces` on
   re-publish). The reply is `ShareParams {shareId, codec, encodings[], audioBitrate}`, computed by the server from the
   preset, the room's codec safe set and admin limits (02 owns the numbers). `ShareParams.encodings` is in wire order,
   high first (`f`, then `q`); that is not the order the browser gets them in (step 2).
2. Video transceiver: set the track's `contentHint` from the preset (§13.5), then
   `addTransceiver(video, {direction:'sendonly', streams:[stream], sendEncodings})` with **complete** encodings, built
   by `encodings.ts` from `ShareParams.encodings` and `track.getSettings()`. Each entry carries `rid`, `active`,
   `maxBitrate`, `maxFramerate` and `scaleResolutionDownBy = max(1, sqrt(width × height / maxPixels))` (01 §8.7).
   - Order: ascending, `q` then `f`, as in the S4 spike. S4's Chrome and Safari simulcast results (hardware and
     software simulcast encoders) were measured with that setup.
   - Nothing is left to browser defaults. Without `scaleResolutionDownBy`, the WebRTC default is 2^(n−1−i): the first
     entry would be encoded at half size and the last at full size, uncapped, until the first `setParameters`.
   - Every later read or write finds an encoding by `rid`, never by index.
   - If simulcast is rejected, fall back to one encoding (`f`) and log.
3. **Codec preferences** (`codecPrefs.ts`, pure, unit-tested): from `RTCRtpSender.getCapabilities('video').codecs`,
   keep `video/H264` with `packetization-mode=1`, put the profile matching `ShareParams.codec` first (matched by
   `h264Key`, level ignored), then the other H.264 profiles in the order `6400, 640c, 42e0, 4200, 4d00`, then all
   `video/rtx` entries (01 §9 rule 7). Nothing else: H.264 everywhere in v1. S4 finding 4: Chrome on macOS
   hardware-encodes both simulcast layers only with High, so the server picks High whenever the room allows it.
4. Audio transceiver (if any): `sendonly` with `sendEncodings: [{maxBitrate: audioBitrate}]` as a cap (the answer's
   `maxaveragebitrate` is what the browser encodes at, 02 §8.4), and `setCodecPreferences` with only `audio/opus`.
5. `degradationPreference` is a parameters field, not an encoding field, so `sendEncodings` can't carry it. One
   `sender.setParameters()` call on the video sender sets the preset's value before the offer: start from
   `getParameters()`, leave the encodings as they are. try/catch; a browser that rejects it keeps its default.
6. `notify('pc.offer', {pc:'pub', gen, neg, sdp, tracks})`; apply the answer.
7. After that, `setParameters` is used only for later changes: `quality.hint` encodings (§13.6), source resizes, and
   preset changes (§13.5; the audio sender's `maxBitrate` too when `audioBitrate` changes). Each call starts from
   `getParameters()` and edits the encodings matched by `rid`. The source size is re-checked every 2 s (window
   resizes), and `scaleResolutionDownBy` is updated when it changes by more than 10 %.
8. No H.264 encoder at all → `errors.local.h264_unavailable` ("This browser can't send H.264 video. Use Chrome or
   Edge."). The server answers the same case with `codec_not_supported` (scope `share`).

### 13.5 Presets

The preset is sent in `share.start`/`share.update`; the server turns it into `ShareParams`. The client adds the parts
that only the browser can set:

| Preset | `contentHint` (video) | `degradationPreference` | Expected `ShareParams` (02 owns; plan values) |
|---|---|---|---|
| **Auto** (default) | `''` | `balanced` | `f` ≤ 1080p (maxPixels 2 073 600), 60 fps, 8 Mbps · `q` 360p (230 400), 15 fps, 0.3 Mbps · audio 128 kbps |
| **Game** | `motion` | `maintain-framerate` | as Auto |
| **Movie** | `motion` | `maintain-framerate` | as Auto, audio **256 kbps** |
| **Text** | `text` (fallback `detail`) | `maintain-resolution` | as Auto; 02 may lower the frame rate |

- Browsers run their own congestion control over TWCC (plan "Rate control"); `maxBitrate` values are caps.
- **Changing the preset while live**: `request('share.update', {shareId, preset})` returns new `ShareParams`; the
  client applies `encodings` and `contentHint` at once and, if `audioBitrate` changed, re-offers the pub PC so the
  answer carries the new Opus `maxaveragebitrate` (01 §8.7).

### 13.6 `PublisherPC` and server hints

```ts
export class PublisherPC {
  constructor(deps: { platform: Platform; signal: SignalClient; log: Logger });
  readonly gen: number;
  addShare(shareId: string, stream: MediaStream, params: ShareParams, preset: Preset): Promise<void>;
  removeShare(shareId: string): Promise<void>;   // stops its transceivers; re-offers, or pc.close when none are left
  applyParams(shareId: string, p: Partial<ShareParams>): Promise<void>;  // encodings → setParameters; codec or
                                                                         // audioBitrate change → re-offer
  // later (M2): setPaused(shareId: string, paused: boolean): Promise<void>;
  handleAnswer(a: PCAnswer): Promise<void>;
  handleIce(i: PCICE): Promise<void>;
  handleRestart(r: PCRestart): Promise<void>;    // server asks: 'ice' → restartIce(); 'rebuild' → rebuild()
  rebuild(): Promise<void>;                      // gen + 1, same tracks and shareIds, new offer
  close(): void;                                 // pc.close {pc:'pub', gen}
}
```

- One pub PC per connection, created on the first share. The protocol allows several shares per user (01: up to
  `limits.maxSharesPerUser`); the UI offers one, and the class already keeps a `Map<shareId, {transceivers, params}>`.
- Offers carry `tracks` for every m-section that carries a share (01 §9 rule 4).
- **`quality.hint`** (01 §8.10) for one of our shares: `encodings` → `setParameters` within 1 s; `codec` → re-order the
  codec preferences with that profile first and re-offer (same `gen`, `neg + 1`); Chrome switches encoders with a
  keyframe. This is also how the room drops to Constrained Baseline when a Firefox viewer joins (02 §8.3), and how 02's
  optional layer pausing turns `f` off while nobody focuses the share (02 §11).
- **No Pause in the M1 web sharer** (integration decision: the plan has Pause only for the desktop app, and a
  pause that viewers can't see is confusing). *Later (M2)*, with 01's `share.pause` feature: every video encoding
  `active: false`, `audioTrack.enabled = false`, `share.update {shareId, paused}`, and a hint's `active: true` never
  overrides the pause.
- **Server unreachable**: the capture tracks stay alive for 60 s without a `ready` connection (01 §10.6), then stop
  with "Sharing stopped: the server was unreachable".
- `beforeunload` shows the browser's leave prompt while sharing.

### 13.7 Sharer panel and hints

The `SharePanel` is a bottom bar on desktop: red dot, "You're live · Window · Movie · 3 watching", **Stop**,
and an expander with the preset picker, a **sound on/off** toggle, a level meter and the watcher list.
- **Watchers**: the names from `share.watchers` (plan: the sharer always sees who is watching). A polite announcement
  "3 watching" is debounced to 5 s.
- **Level meter** (`LevelMeter.tsx`): a WebAudio `AnalyserNode` on the captured audio track (the `AudioContext` is
  created in the Share click). "No sound captured" appears after 10 s of digital silence while sound is on.
- **Upload hint**: if the full layer reports `qualityLimitationReason === 'bandwidth'` in 3 consecutive samples (6 s)
  and its height is below the target size → "Your upload allows about 720p" (height rounded to 360, 480, 540, 720, 900
  or 1080). It clears after 10 s without a bandwidth limit.
- **CPU hint**: `cpu` for 6 s → "Your computer is struggling to encode. Try the Text preset or share a smaller window."

### 13.8 Which browsers can share

The Share button appears whenever the capability probe says sharing works (§8), not by browser name (plan: "detect
features at runtime"). Chrome and Edge on desktop are the tested and supported sharers. Firefox and Safari on desktop
pass the probe too (Safari 27 published simulcast Constrained High in S4) and are labeled "works best in Chrome or
Edge" in the ShareSheet; only Chrome and Edge are tested per release (decided at integration, §24). Phones and tablets have no
getDisplayMedia; they connect with role `viewer` and show "Sharing from phones needs the app (coming later)".

---

## 14. Setup wizard and connection test

### 14.1 Flow

Three steps across two routes, so a reload never loses progress:

1. **`/setup#<token>`, create the admin.**
   - On load: take the token that boot step 0 stashed (§4) in `sessionStorage['isshoni.setup']` and keep it in memory
     (`fragmentToken.ts`, 03 §7.8).
   - `POST /api/v1/auth/setup/check {token}`: `setup_token_invalid` → "This setup link was replaced or has expired. On
     the server run `sudo isshoni setup-url` (Docker: `docker compose exec isshoni isshoni setup-url`) for a new
     one."; `setup_unavailable` → "Already set up → Log in".
   - Form: username, password (with show/hide, no confirm field; rules from `info.accountRules`), optional server name.
     `POST /api/v1/auth/setup/complete` → 201 with the session cookie → clear the stored token → navigate to
     `/admin/welcome?step=2`.
   - This step is slice W3 (§23), so a fresh server can be set up from the browser early. Until W10 adds steps 2–3,
     `SetupPage` navigates to `/` instead.
2. **`/admin/welcome?step=2`, connection test** (`ConnTestPanel`, §14.2). "Continue" is always allowed; after a
   failure it is visually secondary to "Test again".
3. **`/admin/welcome?step=3`, invite friends.**
   - Entering the step creates an invite with the server defaults (`POST /api/v1/invites {}`: 7 days, 10 uses) and gets
     `{invite, url}` (03 §12.4.5).
   - `InviteLinkCard` shows the `url` with **Copy**, a **QR code** (`QrCode.tsx`: `uqr` matrix → one SVG `<path>`),
     **Share…** (`navigator.share` when available) and the expiry.
   - "Copy it now: for safety it can't be shown again. You can always make a new one under Admin → Invites." (03
     stores only a hash.)
   - **Done → Go to Lounge** sets `PATCH /api/v1/admin/settings {setupWizardDone: true}` (03 §7.8).

The stepper shows all three steps on both routes. The admin dashboard shows a checklist card until `setupWizardDone`.

### 14.2 Connection test

The server side is 04 §7.7 (endpoint) on top of 02 §7.6 (probe PCs): `POST /api/v1/conntest {transport, offer}` →
`{answer, expiresInS, server: {publicIp, provider, udpPort, tcpPorts, nat, container}}`, with transports `udp`,
`tcp443` and `tcp7882`, and an echo data channel. `publicIp` is `""` when the server doesn't know it. Any signed-in
user may run it. A user has at most one probe per transport, and a new probe of the same transport replaces the old
one (04).

`runConnTest(platform, opts)` runs the three probes in parallel:

```ts
export type ProbeTransport = 'udp' | 'tcp443' | 'tcp7882';
export interface ProbeVerdict {
  transport: ProbeTransport;
  result: 'ok' | 'failed' | 'disabled';       // disabled: 409 transport_disabled, any transport without a server listener
  rttMs?: number;                             // median of 5 data-channel echoes; fallback currentRoundTripTime
  error?: 'timeout' | 'server' | 'rate_limited';   // 'server' and 'rate_limited' mean "not tested", never ✗
}
export interface ConnTestResult {
  at: string; client: ClientInfo;
  probes: ProbeVerdict[];
  server?: {
    provider: string; nat: string; udpPort: number; tcpPorts: number[];
    container: string;                        // 04, as in the doctor's env.container: none | docker | podman | other
    publicIpKnown: boolean;                   // derived: server.publicIp non-empty
    publicIpPrivate: boolean;                 // derived: server.publicIp empty, private or loopback (the IP itself isn't kept)
  };
}
export function runConnTest(platform: Platform, opts?: { signal?: AbortSignal }): Promise<ConnTestResult>;
```

Per probe:
1. `pc = platform.createPeerConnection({iceServers: [], bundlePolicy: 'max-bundle'})`;
   `dc = pc.createDataChannel('probe')`.
2. `createOffer` → `setLocalDescription`. No need to wait for gathering: the server learns peer-reflexive candidates
   from the browser's checks (04, 02).
3. `POST /api/v1/conntest {transport, offer: pc.localDescription.sdp}` → `setRemoteDescription({type:'answer', sdp})`.
4. Success = `connectionState === 'connected'` and `dc` open within **8 s** (04's ICE failed timeout).
5. RTT: 5 messages `{"n":i,"t":<performance.now()>}` 200 ms apart; the server echoes them; take the median.
6. Close the PC.

Rows shown: "Media over UDP (port 7882)", "Media over TCP (port 443)", "Media over TCP (port 7882)" (ports from
`server`), and "Round trip". A disabled TCP row is hidden. A disabled UDP row is shown as ✗ with `conntest.udpDisabled`
("UDP media is turned off in the server config (`listen.ice_udp`)") and counts as UDP ✗ below, because UDP matters for
quality.

A probe whose `error` is `rate_limited` or `server` is **not tested**: it shows neither ✓ nor ✗, is left out of the
verdict table and produces no fix text. If any probe is not tested, the panel shows "Couldn't finish the test. Try
again in a minute." (respecting `Retry-After`). "Test again" is disabled while a run is in progress.

| UDP | any TCP | Status | Text |
|---|---|---|---|
| ✓ | any | green | "Friends get the best quality." |
| ✗ | ✓ | amber | "Works, but video may stutter on weak networks. Open UDP port 7882." + fix text |
| ✗ | ✗ | red | "Friends can't receive video yet." + fix text |

RTT labels: ≤ 80 ms good, ≤ 200 ms OK, above that "high latency". Every result has **Copy result** (JSON of
`ConnTestResult`, no IP addresses).

**Fix text** (`fixText.ts`, admins only; catalog keys owned here):
- **No public address** (checked first): when `server.publicIp` is empty (`publicIpKnown` false) and every tested
  probe failed, the probe answers carried no IPv4 candidates (04 §7.7, 02 §7.6), so the firewall is not the problem.
  This happens with a domain or off-mode server in a Docker bridge or behind a router when STUN is blocked, or with
  `network.stun_servers = []` and no `public_ip`. The admin sees one line, `conntest.noPublicIp`: "The server
  doesn't know its public address, so browsers can't reach its media ports. Set `public_ip` in
  /etc/isshoni/isshoni.toml (Docker: `ISSHONI_PUBLIC_IP` in .env) and restart." It is the only fix text for that
  result: the provider, host firewall, Docker, macOS and blocked-network lines below are left out. "Test again" after
  the restart shows them if something else is still wrong.
- **provider**: `fix.firewall.<provider>` for 04 §13.3's ids: `aws` (security group inbound rules), `gcp` (VPC firewall
  rule), `azure` (network security group), `oracle` (security list **and** the image's own iptables rules, which block
  by default), `hetzner`, `digitalocean`, `vultr`, `linode` (Cloud Firewall), `scaleway`, `ovh`, `alibaba`,
  `tencent` (security group), `unknown` (generic "your provider's firewall or security group"). Each names the ports:
  UDP 7882, TCP 443 and 7882, TCP 80 for certificates.
- **host firewall**: always both lines, labeled: `sudo ufw allow 7882/udp` and `sudo firewall-cmd --permanent
  --add-port=7882/udp && sudo firewall-cmd --reload`.
- **nat** (04 §7.4): `conntest.nat.<nat>`; `port_forward` → "Forward TCP 80, 443, 7882 and UDP 7882 on your router to
  this machine"; `cgnat_likely`/`symmetric` → "This server has no public address; isshoni needs one (a VPS or a
  router port forward)". When `server.nat` is `port_forward`, admins also see "You may be testing from the server's
  own network. Test again from your phone's mobile data" on every result, including green.
- **Docker** (when `server.container` is `docker` or `podman`): "Check that compose publishes `7882:7882/udp`;
  published ports bypass ufw."
- **macOS Local Network** (S4 finding 6): when `server.publicIp` is empty, private or loopback (`publicIpPrivate`) and
  the admin's OS is macOS: "On a Mac, allow your browser Local Network access (System Settings → Privacy & Security →
  Local Network)."
- always, under a red UDP row: "If this network blocks UDP (some offices and hotels do), test from home or from your
  phone's mobile data."

Non-admins run the test from "Test my connection" (account menu, the reconnect banner, and 01's "Can't reach the
server's media port" state). They see the ✓/✗ rows and "Send this to your admin: [Copy result]" instead of fix text.

**Troubleshooting links**: every non-green result links to the project site's `/troubleshooting#ct-<code>` (06
§10.4). The codes are exported as `web/src/conntest/codes.json` (06's anchor test reads it): `udp_blocked` (UDP ✗,
TCP ✓), `no_media` (all ✗), `no_public_ip` (all ✗ with `server.publicIp` empty; used instead of `no_media`),
`tcp443_blocked` (TCP 443 ✗ while UDP ✓), `high_rtt` (> 200 ms), `nat_port_forward`, `nat_cgnat`,
`mac_local_network`, `docker_ports`. Provider fix text also links to `/install/vps#<provider>` with 04
§13.3's provider ids.

---

## 15. Accounts, admin, download

### 15.1 Auth pages (03 §7, §12.4.2)

- **Invite** (`/invite#<token>`): take the token that boot step 0 stashed in `sessionStorage['isshoni.invite']` (§4) →
  `POST /api/v1/auth/invite/check` → "Alex invited you to <server name>" or the reason the link doesn't work
  (`invite_invalid`, `invite_expired`, `invite_used_up`, `invite_revoked`, `registration_closed`) → one form
  (username, password with show/hide) → `POST /api/v1/auth/register {inviteToken, username, password}` → 201 → `/`
  (the default room). A signed-in user sees "You're signed in as alex. [Go to Lounge] [Sign out and create another
  account]". The signed-in check uses `useMe()` in its no-redirect mode: a 401 means signed out, so the form shows
  (§6.2).
- **Signup** (approval mode): the same form without a token → 202 → `/pending`.
- **Login**: username, password, the trust-model lines, "Need an account? Ask a friend for an invite link." When
  `info.registration === 'approval'`, that line reads "Need an account? [Request one]" and links to `/signup`. 429 and
  `server_busy` show the wait; `account_pending` → `/pending`; `account_disabled` → notice.
- **Reset** (`/reset#<token>`): take the token that boot step 0 stashed in `sessionStorage['isshoni.reset']` (§4) →
  `POST /api/v1/auth/reset/check` shows the username → new password → `POST /api/v1/auth/reset/complete` → logged in →
  `/`.
- **Logout**: stop local shares; `SignalClient.stop()` (close 1000); `POST /api/v1/auth/logout` (the server deletes this
  session and its push subscriptions, 03); then `PushSubscription.unsubscribe()` locally; `broadcastLogout()` (`app/session.ts`, §6.2);
  `queryClient.clear()`.

### 15.2 Account pages

- `/account`: username; change password (`currentPassword`, `newPassword`): "Changing your password signs you out on
  your other devices" (03 §7.7); delete account (password; 409 `last_admin` explained); sign out.
- `/account/devices`: browser sessions from `GET /api/v1/me/sessions` (`name`, `lastSeenAt`, `lastIp`, `current`),
  revoke one (not the row with `current: true`, which shows "This browser" and a Sign out button that runs the §15.1
  logout flow), "Sign out other browsers" (`revoke-others`), "Log out everywhere" (`logout-everywhere`). Linked devices
  (`GET /api/v1/me/devices`) are listed in the same page and are empty until M2.
- `/account/notifications`: see §16.3.

### 15.3 Admin pages

| Page | Content | Calls |
|---|---|---|
| Dashboard | Live rooms; per share: owner, layers with size/fps/bitrate/loss, viewer counts, egress; total egress now; month-to-date transfer and projection; client versions; TLS and public IP; last doctor summary; alerts ("isshoni vX available (security fix)"); accounts and security events (`accounts`, 03); the setup checklist (§14.1) | `GET /api/v1/admin/dashboard` (04), every 2 s while visible; `GET /api/v1/admin/settings` (for `settings.setupWizardDone`, the checklist card) |
| Users | Username, role, status, created via, last seen, online; rename; make or remove admin (asks for the admin's password, 03 §7.11); disable/enable; reset link (shown once; when the target is an admin, first asks for your password and sends it as `currentPassword`, and `wrong_password` shows under the field, 03 §7.10); sign out everywhere; delete. On your own row, disable, delete and reset are hidden (03 §7.11: use `/account`) | `GET/PATCH/DELETE /api/v1/admin/users…`, `POST …/password-reset`, `POST …/sign-out` |
| Approvals | Pending sign-ups (username, time, IP) with Approve and Reject. **Reject all** (for a flood of fake sign-ups, 03 §7.9) asks for confirmation ("Reject all pending sign-ups? Anyone who is real can sign up again."), then shows "Rejected {rejected} sign-ups" from the response | `GET /api/v1/admin/approvals`, `POST …/approve`, `…/reject`; Reject all: `POST /api/v1/admin/approvals/all/reject` `{"all": true}` → 200 `{rejected}` |
| Invites | Create (expiry 1 h, 1 day, 7 days, 30 days; uses 1, 10, 50; note), list (creator, uses, expiry, state; a filter hides inactive invites by default, client-side), revoke. The link shows once, at creation | `GET /api/v1/invites?state=all`, `POST /api/v1/invites`, `DELETE /api/v1/invites/{id}` |
| Rooms | List, create, rename, delete (not the default room: 409 `room_is_default`) | `GET /api/v1/rooms`, `POST/PATCH/DELETE /api/v1/admin/rooms…` |
| Settings | Server name (`serverName`, empty = the server's host); registration mode; invite defaults and `membersCanInvite`; soft limits (participants and shares per room, share bitrate cap); transfer alert (GB per month); `minClientVersion`; release check; fields in `locked` are read-only with "Set in isshoni.toml"; `setupWizardDone` is never shown as a field. On save, the `admin.settings` invalidate topic already refreshes `['info']` (§6.2) | `GET/PATCH /api/v1/admin/settings` |
| Audit | Paged log with filters (action, actor, target); "Load more" by `nextBefore` | `GET /api/v1/admin/audit` |
| Doctor | "Run doctor" → checks rendered from `doctor.<code>` with `params` and `doctor.fix.<fix_code>` (04 §13.5); "local only" checks labeled; the connection test (§14.2); the bandwidth calculator | `GET\|POST /api/v1/admin/doctor` (429 `doctor_busy`), `GET /api/v1/admin/bandwidth` |

**Bandwidth calculator**: a small form (people, sharing, thumbnails, quality, preset, hours) that calls 04's
`GET /api/v1/admin/bandwidth` (one implementation for CLI and UI, 04 §13.4) and shows egress, per-viewer rate and GB
per session.

### 15.4 Download page (placeholder)

`/download` in M1: "The isshoni desktop app is coming (Windows first). It keeps Discord and other voice apps out of
what friends hear. Until then, share from Chrome or Edge: pick a Window and keep Share audio on." It links to the
project site (`https://moonwx.github.io/isshoni/`, 06 D11). *later (M2)*: per-OS installers, the pre-filled
`install-desktop.sh`/`.ps1` commands (04 reserves the paths), and the OS detected from `ClientInfo.os`.

---

## 16. PWA, Web Push, i18n, accessibility, versions

### 16.1 Manifest and HTML head

`public/manifest.webmanifest`:

```json
{
  "id": "/", "name": "isshoni", "short_name": "isshoni",
  "description": "Watch your friends' screens together",
  "start_url": "/", "scope": "/", "display": "standalone", "orientation": "any",
  "background_color": "#101014", "theme_color": "#101014",
  "icons": [
    {"src": "/icons/icon-192.png", "sizes": "192x192", "type": "image/png"},
    {"src": "/icons/icon-512.png", "sizes": "512x512", "type": "image/png"},
    {"src": "/icons/maskable-512.png", "sizes": "512x512", "type": "image/png", "purpose": "maskable"},
    {"src": "/icons/icon.svg", "sizes": "any", "type": "image/svg+xml"}
  ]
}
```

`index.html` head: `<meta name="viewport" content="width=device-width, initial-scale=1, viewport-fit=cover">`,
`<meta name="referrer" content="no-referrer">` (04 also sends the header), `theme-color` for light and dark,
`<link rel="manifest">`, `<link rel="apple-touch-icon" href="/icons/apple-touch-icon.png">`,
`<meta name="mobile-web-app-capable" content="yes">`, and a `<noscript>` line. *later*: a server-rendered manifest name
("isshoni · <server name>").

### 16.2 Service worker (app shell only, versioned)

Hand-written (`src/sw/`, about 150 lines) rather than Workbox: its only jobs are caching the shell for offline start
and handling push. `build/sw-plugin.ts` builds it after the main build as a separate **IIFE** `dist/sw.js` (module
service workers aren't universal), with build-time constants:
- `__SHELL__`: `/`, the entry chunk, its static imports and CSS, `/boot-check.js`, `/manifest.webmanifest`,
  `/icons/icon-192.png`. Lazy route chunks are not precached; they're cached on first use. The shell version hash
  covers `/boot-check.js`, so a new worker refreshes it (04 serves it `no-cache`).
- `__SHELL_VERSION__`: the first 12 hex digits of SHA-256 over the shell files' contents.
- `__PUSH_STRINGS__`: the `push` section of `en.json`.

Behaviour:
- `install`: `caches.open('isshoni-shell-' + version).addAll(__SHELL__)`. No automatic `skipWaiting`.
- `activate`: delete other `isshoni-shell-*` caches; `clients.claim()`.
- `fetch` uses the pure `route(url, mode, method, origin)` from `sw/routes.ts`:

  | Request | Strategy |
  |---|---|
  | not GET, other origin, `/api/*`, `/ws`, `/healthz`, `/readyz`, `/download*`, `/install*` | not handled (network) |
  | navigation | network first, 4 s timeout, then the cached `/` (the SPA shows the Offline screen) |
  | `/assets/*` (hashed), `/boot-check.js` | cache first, then network (and cache it) |
  | `/icons/*`, `/manifest.webmanifest` | stale-while-revalidate |

- `message {type: 'SKIP_WAITING'}` → `skipWaiting()`.
- Registration: `navigator.serviceWorker.register('/sw.js', {scope: '/', updateViaCache: 'none'})`, production only.
  04 serves `/sw.js` with `no-cache` at the root (§17.2).
- Updates: when `registration.waiting` appears, `UpdatePill` offers "Update ready · Reload" (not while sharing). A
  stale build seen at `welcome` triggers §16.4.
- Nothing user-specific is ever cached, so logout needs no cache cleanup.

### 16.3 Web Push

Support states (`platform.notifications.support()`):

| State | Condition | UI |
|---|---|---|
| `supported` | service worker, `PushManager`, `Notification`, and the server has push (`info.push` present) | toggle |
| `needs-install` | iOS/iPadOS in a Safari tab (not `display-mode: standalone`) | "Add isshoni to your Home Screen to get notifications (iOS 16.4 or later)": Share → Add to Home Screen, then open it from there. "You'll log in once more inside the app": iOS Home Screen apps have their own cookie jar (03 §7.4). One more line for chat apps whose built-in browser `inAppBrowser()` can't detect: "Opened this from a chat app? Use its ⋯ menu → Open in Safari first." |
| `denied` | `Notification.permission === 'denied'` | how to re-allow in browser settings |
| `unsupported` | anything else, including an installed iOS app without `PushManager`, or no `info.push` | "This browser can't show isshoni notifications" (iOS: "Update to iOS 16.4 or later"; server off: "Notifications are turned off on this server") |

iOS/iPadOS is recognised by the UA or `MacIntel` with `maxTouchPoints > 1`. This only chooses help text; whether push
works is decided by the features present once installed.

**In-app browsers**: friends often get the invite link in a chat app, which may open it in its own browser. That
browser has its own cookie jar and no Add to Home Screen, so a friend who signs up there is logged out in Safari, can't
follow the `needs-install` steps, and must log in a third time inside the Home Screen app; some also force native
fullscreen video on iPhone, which breaks the tile grid. When `platform.inAppBrowser()` (§8) matches, the invite,
login and room pages show a dismissible banner (`InAppBrowserBanner`, above the page content): "For notifications and
the Home Screen app, open this link in Safari" ("in Chrome" on Android, "in your browser" elsewhere) with **Copy link**.
- The copied link on the invite page is rebuilt as `<origin>/invite#<token>` from the token in memory, because boot
  step 0 removed the fragment from the address bar (§4); on the other pages it is `location.href`. If
  `navigator.clipboard.writeText` fails, the link appears in a selected read-only field.
- Dismissal lasts for the tab (`platform.storage.session`, `isshoni.inAppBannerDismissed`).
- While the banner shows, the `needs-install` sheet and card stay hidden; they appear once the banner is dismissed,
  or when the page is opened in a real browser.
- The same tip goes on the project site's `/guide/` (06 §10.2) and in the exit-test run book (06 §12.2).

**Enable** (from a click): `Notification.requestPermission()` → `pushManager.subscribe({userVisibleOnly: true,
applicationServerKey: <VAPID public key>})` → `POST /api/v1/push/subscriptions` with `subscription.toJSON()` (03;
the server derives the device label from the User-Agent). The VAPID key comes from `info.push.vapidPublicKey` (03).
**On every app start** with permission granted, the SPA re-posts its subscription and re-subscribes if the
subscription's `applicationServerKey` differs from the key in `/info` (after `rotate-secrets --include-vapid`), but
only once `GET /api/v1/me` has succeeded (a signed-in session); logged-out pages never call the push endpoints.
**Disable**: `POST /api/v1/push/unsubscribe {endpoint}` (03), then `unsubscribe()`. **Test**:
`POST /api/v1/push/test` (this browser; at most 1 per 10 s). **Preferences** (`GET/PUT /api/v1/push/preferences`,
03): "Tell me when someone starts sharing" (`shareStarted`: `all` or `off`) and, for admins, "Admin alerts"
(`adminAlerts`).

**Where it's offered**: Account → Notifications; a one-time card in the room after 3 minutes of watching ("Get a
notification when friends start sharing? [Turn on] [Not now]", dismissal kept 30 days); the first launch of an iOS
Home Screen app. Never an automatic permission prompt.

**Service worker handlers** (`sw/push.ts`, pure mapping, unit-tested). 04 §14.3's payload carries data, not text:

```json
{"v": 1, "type": "share.started", "ts": 1790712000000, "tag": "share:lounge:k3m9p2qxw7ht",
 "url": "/r/lounge?focus=s_q7m2x9c4v8b1n5k3",
 "room": {"id": "lounge", "name": "Lounge"}, "user": {"id": "k3m9p2qxw7ht", "name": "Alex"},
 "shareId": "s_q7m2x9c4v8b1n5k3"}
```

- `push`: title and body from `__PUSH_STRINGS__` by `type` with the payload's names: `share.started` → "Alex started
  sharing" / "in Lounge · Tap to watch"; `admin.alert` → `push.adminAlert.<kind>` for 03's kinds (`signup_pending`,
  `admin_granted`, `admin_revoked`, `admin_password_reset`, `registration_mode_changed`, `secrets_rotated`) and 04's
  `transfer_threshold`; `push.test` has its own key. `showNotification(title, {body, tag, icon:
  '/icons/icon-192.png', badge: '/icons/badge-72.png', timestamp: ts, data: {url}})`. An unknown `type` shows a
  generic notification: every push **must** show one, or iOS may drop the subscription (04).
- `notificationclick`: close it; accept `data.url` only if it resolves to the same origin (else `/`); focus an existing
  window and `postMessage({type: 'open', url})` (the router navigates), else `clients.openWindow(url)`.
- `pushsubscriptionchange`: subscribe again with the same key, then `fetch('/api/v1/push/subscriptions',
  {method: 'POST', credentials: 'same-origin', headers: {'Content-Type': 'application/json'}, body:
  JSON.stringify(sub.toJSON())})`. The CSRF rule in 03 §7.5 applies to the service worker too. A 401 is ignored: the
  page re-posts after the next login.

**Install prompts**: Chrome/Edge/Android: keep the `beforeinstallprompt` event; show "Install app" in the account menu
and, on Android, in the room's one-time card. iOS: the Home Screen sheet above, at most every 14 days.

### 16.4 Version mismatch

| Trigger | Web behaviour |
|---|---|
| `ready` with `staleBuild` (01 §6.1: `welcome.serverVersion` ≠ build version; neither is a dev build, which 04 §15 defines as a SemVer prerelease that starts with `dev`) | The shell is stale (a cached PWA; the server always serves a matching SPA). Ask the service worker to update and reload once, guarded by `sessionStorage['isshoni.reloadedFor'] = serverVersion`. While sharing, show the UpdatePill instead of reloading |
| `stopped` with `protocol_unsupported` (`params {serverMin, serverMax, serverVersion}`) | VersionMismatch screen: "This page is out of date. [Reload]", or "Ask your admin to update the server" when the server is older. Desktop (M2): "Update the app" + "Open in browser" via `platform.versionActions()` |
| a reload didn't help | the screen stays, with both version numbers |

### 16.5 i18n

- `src/i18n/en.json`, one namespace, nested keys: `common`, `auth`, `setup`, `conntest`, `room`, `viewer`, `share`,
  `account`, `admin`, `doctor`, `fix`, `errors` (+ `errors.local`), `fieldErrors`, `push`, `a11y`. Plurals use
  i18next suffixes (`viewer.watching_one`, `viewer.watching_other`). Dates and numbers use `Intl` with
  `i18n.language`.
- `i18next/no-literal-string` fails the lint on JSX text and on user-visible attributes.
- `scripts/check-i18n.mjs` fails CI when:
  - a `t('…')` literal key is missing from `en.json`;
  - an `ErrorCode` from `types.gen.ts` (01) or a `Code…` constant from `api.gen.ts` (03) has no `errors.<code>`
    entry. An `errors.<code>` object with sub-keys counts: `limit_reached` has one per `params.limit` (`rooms`,
    `invites`, `member_invites`, `pending_signups`), and the check requires those;
  - a `CloudProvider` constant in `api.gen.ts` (04 §13.3) has no `fix.firewall.<provider>` entry, or a `NATKind`
    constant there has no `conntest.nat.<nat>` entry. The script reads both constant lists from `api.gen.ts`.

  Unused keys are warnings.

  CI runs `check:i18n` on every PR from the first web slice on, so each rule arrives with the slice that makes it
  checkable (§23): W1 writes the script with the `t('…')` rule; W2 adds the error-code rule and every `errors.<code>`
  key; W10 adds the `CloudProvider`/`NATKind` rule and its `fix.firewall.*` and `conntest.nat.*` texts; W13 adds the
  unused-key warnings and a test that a removed key fails the check.
- The catalog is the single source for Go-rendered strings later (plan): *later (M2)* the tray embeds it. 04's doctor
  CLI keeps its own English templates of the same codes (04 §13.2).

### 16.6 Accessibility

- The tile list is a `<ul aria-label="Shares">`. Each tile's main area is a `<button>` named "Watch alex's window,
  3 watching"; the speaker and watcher buttons are siblings, never nested. The stage is `<section aria-label="Now
  watching: alex's window">` with a `role="toolbar"` control bar.
- Two live regions in `Announcer`: polite (`room.event`s such as "alex started sharing", "Now listening to bo",
  "Reconnected", "3 watching"), assertive (errors, "You're live", "Sharing stopped").
- After a route change, focus moves to the page `<h1>`. Dialogs are native `<dialog>` with `showModal()` (focus trap
  and Esc for free).
- `prefers-reduced-motion`: no layout transitions, toasts appear without sliding, spinners become a static indicator.
  Colours meet WCAG AA in light and dark; `forced-colors` keeps focus rings.
- jsx-a11y (strict) in lint; axe in e2e (§19.3).

---

## 17. Build, embed and dev

### 17.1 Embed and `dist/`

`web/embed.go` is exactly 04 §9.5's file (`package web`, `//go:embed all:dist`, `func Dist() fs.FS`). `dist/.gitkeep`
is committed and the `.gitignore` rules (`web/dist/*`, `!web/dist/.gitkeep`) are 06's (§7.2), so `go build`, `go
test` and `go vet` work on a fresh clone; 04 serves a "web app is not built" page when `index.html` is missing.

**`dist/` layout** (what 04 serves):
- `index.html`, `assets/*` (hashed names), `sw.js`, `manifest.webmanifest`, `icons/*`, `boot-check.js`, `robots.txt`;
- `version.json` `{"version": "<ISSHONI_VERSION>", "protocol": 1, "shell": "<12 hex>"}` (04 compares `version`);
- `licenses.txt` (06's `licenses.mjs notices`; linked from `/about`);
- **precompressed siblings** (04 asks 05 to decide): `build/compress-plugin.ts` writes `<file>.br` (quality 11) and
  `<file>.gz` (level 9) for `.js`, `.css`, `.html`, `.svg`, `.json`, `.webmanifest` files of 1 KiB or more, using
  `node:zlib`. Rationale: phones on mobile data; it adds about the compressed size of the SPA (a few hundred KB) to the
  binary.

**Version**: the build reads `ISSHONI_VERSION` (passed by 06's Taskfile `VERSION` var; CI uses `0.0.0-ci.<run>`,
releases the tag without `v`) into `__ISSHONI_VERSION__`, default `0.0.0-dev`, which is the `BUILD_VERSION` that 01's
client compares. The Go binary gets the same string, so one build has one version (04 §15).

### 17.2 What the SPA relies on from the server

04 §9.5–§9.6 cover it: immutable caching for `/assets/*`, `no-cache` for `/sw.js`, `/manifest.webmanifest` and
`index.html`, 404 for missing dotted paths, `index.html` for every other GET (404 status for `/setup` after setup),
the CSP and headers. The SPA is built to satisfy that CSP: no inline scripts or styles, no `eval`, no web fonts, no
third-party origins; `boot-check.js` is an external file; React's `style={…}` uses the CSSOM, which `style-src 'self'`
doesn't block. No change to 04's CSP is needed. `screen-wake-lock` isn't in 04's Permissions-Policy, so its default
(`self`) applies, which is enough.

04 serves `/sw.js` with the HTML CSP (04 §9.6), because the worker's own `fetch` calls (shell cache, navigations,
`pushsubscriptionchange`) are governed by the CSP of its script response.

### 17.3 Vite config (essentials)

```ts
export default defineConfig({
  plugins: [react(), swPlugin(), versionPlugin(), compressPlugin(), reportPlugin()],
  define: { __ISSHONI_VERSION__: JSON.stringify(process.env.ISSHONI_VERSION ?? '0.0.0-dev') },
  build: {
    outDir: 'dist', emptyOutDir: false,        // keeps dist/.gitkeep; the plugins delete old build output
    sourcemap: false, assetsInlineLimit: 0,
    target: ['es2022', 'chrome111', 'edge111', 'firefox115', 'safari15.4'],
  },
  server: {
    port: 5173, strictPort: true,
    proxy: {
      '/api':     { target: DEV_SERVER },
      '/ws':      { target: DEV_SERVER, ws: true },
      '/healthz': { target: DEV_SERVER },
    },
  },
  test: { environment: 'jsdom', setupFiles: ['src/test/setup.ts'], include: ['src/**/*.test.{ts,tsx}'] },
});
// DEV_SERVER = process.env.ISSHONI_DEV_SERVER ?? 'http://127.0.0.1:8080'
```

- `safari15.4` keeps iOS 15 phones (e.g. iPhone 7, whose last iOS is 15.8) able to watch; push still needs 16.4
  (feature-detected).
- **Dev** (`task dev`, 06 §7.2–§7.3): the server runs with `tls.mode=off` on `127.0.0.1:8080` and its public URL set to
  the Vite origin `http://localhost:5173`. The proxy keeps `Host` and `Origin` as the browser sent them
  (`changeOrigin: false`), so 03's Origin checks see the public origin and 04's Host check accepts `localhost` in dev.
  The cookie is the non-`Secure` `isshoni_session` on plain http (03 §7.4). localhost is a secure context, so
  getDisplayMedia works (plan). Safari and phones need HTTPS: test them against a VPS prerelease (06).
- **Size budget** (`check:size`, from `build-report.json`): initial JS (entry + static imports) ≤ 200 KB gzip; each lazy
  chunk ≤ 120 KB gzip; CSS ≤ 30 KB gzip.

---

## 18. Limits and timeouts (05-owned; signaling values are 01 §13)

| What | Value |
|---|---|
| "Reconnecting…" shown after / "Can't reach the server" after | 2 s / 30 s |
| Subscription sync | up: 150 ms debounce; down to off (not visible): 1 s; page hidden → off: 10 s |
| Tile "frozen" | 3 s without decoded frames |
| Firefox: caps poll / give-up hint | 5 s / 3 min (the server's rebuild retries run 20 s × 9, 02 §8.5) |
| Upload/CPU hint | 3 samples (6 s) on, 10 s off |
| Stats sampling / ring buffer / `stats` notification | 2 s (1 s with overlay) / 150 samples / 10 s |
| Source size re-check (scaleResolutionDownBy) | 2 s, update on > 10 % change |
| Capture kept without the server | 60 s (01 §10.6) |
| Connection test: connect / pings | 8 s / 5 × 200 ms |
| SW navigation timeout | 4 s |
| Admin dashboard poll | 2 s while visible |
| `?focus=` wait | 5 s |
| One-time cards | push card after 3 min watching, 30 days after dismissal; iOS install sheet every 14 days at most |

---

## 19. Test plan

### 19.1 Unit (Vitest, node or jsdom)

| Module | Asserted |
|---|---|
| `lib/sdp.ts` | Opus stereo munge adds `stereo=1;sprop-stereo=1` once, idempotent, leaves other fmtp intact |
| `share/codecPrefs.ts` | `ShareParams.codec` first (Chrome `640034` matches `h264/6400`, Safari `640c1f`), then the fixed order, RTX kept, nothing else; no H.264 → error |
| `share/classify.ts` | the table in §13.3, all 6 rows |
| `share/encodings.ts` | `ShareParams.encodings` (wire order `f`, `q`) → complete `sendEncodings` in ascending order (`q`, `f`), every field set (`rid`, `active`, `maxBitrate`, `maxFramerate`, `scaleResolutionDownBy`); `scaleResolutionDownBy` from 1080p, 1440p, 2160p and 3440×1440 sources; later `setParameters` edits match by rid, never index (a reversed `getParameters()` order still updates the right layer); a hint's `active` flags applied per rid |
| `share/hints.ts` | upload hint after 3 limited samples, rounding, clears after 10 s |
| `viewer/layerPolicy.ts` | each rule in §12.4, including PiP while hidden, fullscreen, the 10 s hidden rule |
| `viewer/autoFocus.ts` | newest share auto-focused; manual holds; resumes after the manual share ends; `replaces` carries focus and audio only for the same user; `?focus=`; own share never focused |
| `viewer/audioOut.ts` (fake media element) | locked → blocked on `NotAllowedError` → playing after tap; swap keeps playing |
| `viewer/SubscriberPC.ts`, `share/PublisherPC.ts` (fake RTCPeerConnection) | `gen`/`neg` rules of §9: stale gen dropped, repeated neg resends the stored answer, one outstanding offer with folding, candidate buffering (64), `probe()` on `disconnected`, recovery timers (3 s, 15 s, spacing 5 s/10 s, 5 rebuilds → UI state), no sub `pc.restart` after a resumed welcome; a sub offer with a new `ice-ufrag` cancels the 3 s timer (no `pc.restart {mode:'ice'}` follows) and starts the 15 s rebuild timer from that offer, also when the client had already sent its own request |
| `rooms/RoomSession.ts` (fake SignalClient) | resync after resumed and not-resumed welcomes (01 §10.5): pending offer re-sent, full `subscribe.update` only when non-empty and in chunks of 64, re-publish with `replaces`, stray server share stopped; `join()` before `ready` joins on the next resync; a resumed welcome with a missing or other `roomId` re-joins the desired room; own `share.stopped` (each reason) and the `room.state` fallback drive §13.1 |
| `protocol/invalidate.ts` | each topic → its query keys |
| `protocol/rest.ts` | both error envelopes parse to the same `ApiError`; `Content-Type` always set on unsafe methods |
| `conntest/verdict.ts`, `fixText.ts` | status table; provider/nat/docker/macOS key selection (`server.container`, empty or private `publicIp`); empty `publicIp` with every tested probe ✗ → only `conntest.noPublicIp` (no provider, host firewall, Docker or macOS lines) and code `no_public_ip`; empty `publicIp` with a probe ✓ → the normal text; a disabled TCP row hidden, a disabled UDP row ✗ with `conntest.udpDisabled` and counted as UDP ✗; `rate_limited`/`server` probes "not tested" (no ✓/✗, out of the verdict, no fix text, the "Couldn't finish the test" line); the `port_forward` hint on a green result; non-admin text |
| `sw/routes.ts`, `sw/push.ts` | strategy per URL class; payload → notification options for every known type; same-origin guard on click URLs; generic fallback |
| `lib/ua.ts` | `inAppBrowser()` matches each known token (`FBAN`, `FBAV`, `Instagram`, `Line/`, `MicroMessenger`) and returns `null` for plain Safari, Chrome, iOS Chrome (`CriOS`), Firefox and Edge UAs |
| `app/boot.tsx` (`stashFragmentToken`), `auth/fragmentToken.ts` | boot step 0 stores the token for `/setup`, `/invite` and `/reset` and removes the fragment via `replaceState` before the first fetch (`/info` included); other paths and empty fragments are left alone; `fragmentToken.ts` reads the stored token and clears it |
| i18n | every generated error code maps to an existing `en.json` key (also enforced by `check:i18n`) |

01 owns the `SignalClient`, `codecs.ts` and registry tests (01 §19).

### 19.2 Component (Testing Library + MSW)

Login, invite and reset forms show field errors from codes; the setup wizard walks steps 1→2→3 (conntest mocked); the
tile shows the viewer count and opens the watchers popover; TapToStart appears when `play()` rejects and disappears
after a click; ScreenAudioWarning's three buttons call the right actions; the switcher and the admin's "Create room"
link follow `showRoomList`;
invites show the link once; locked settings are read-only; with an Instagram UA the invite page shows the in-app
banner and its Copy link includes `#<token>`, and in the room (iOS UA) the Home Screen sheet stays hidden until the
banner is dismissed.

### 19.3 End-to-end (Playwright, Google Chrome)

**Harness** (06 §7.2–§7.3, §8.2): `task e2e` builds the binary and runs `npm run e2e` with `ISSHONI_BIN` (06 passes an
absolute path). `e2e/global-setup.ts` only finds the binary: it derives the repo root from its own location
(`web/e2e/` → `../..`), resolves a relative `ISSHONI_BIN` against the repo root and checks that `isshoni version`
runs. Servers come from fixtures in `e2e/fixtures.ts`, so no spec depends on a server that another spec set up,
reconfigured or restarted:

- **`startServer({overrides?, setUp?})`** (test-scoped; `setUp` defaults to `true`) starts `ISSHONI_BIN serve` and
  returns `{url, admin, restart(), stop()}`. Each server gets:
  - a fresh temp data dir (`ISSHONI_DATA_DIR`) and `--config <root>/deploy/dev/isshoni.e2e.toml` (`tls.mode=off`,
    loopback candidates, no STUN, JSON logs);
  - free ports chosen in Node (bind port 0, read it, close), passed as `--listen.http 127.0.0.1:<p>`,
    `--listen.ice-udp :<u>`, `--listen.ice-tcp :<t>` and `--public-url http://127.0.0.1:<p>`. The TOML's ports are
    only defaults for a manual run; no port is fixed. If the server exits with an address-in-use error, the fixture
    picks new ports and retries once;
  - its own admin socket, `<os tmpdir>/isshoni-e2e-<pid>-<n>.sock` (short: macOS limits socket paths to 104 bytes),
    passed as `--listen.admin-socket` to `serve` and to `setup-url --json`;
  - `overrides` as config paths → values, turned into flags by 04 §4.2's naming rule: `{'listen.ice_udp': ''}` →
    `--listen.ice-udp=` (a flag can set an empty string; env can't);
  - its stdout and stderr in `web/test-results/server-<worker>-<n>.log` (06 §14).

  The fixture waits for `/readyz`. With `setUp: true` it runs `isshoni setup-url --json` and completes setup over
  REST (`POST /api/v1/auth/setup/complete`, 03 §7.8) with a generated admin, returned as `admin`. With
  `setUp: false`, `admin` is `null` and the server waits for the wizard. `restart()` stops the process (SIGTERM, wait
  for exit) and starts it again with the same data dir, ports and flags. `stop()` ends it and removes the data dir;
  every server is stopped when its test ends.
- **`server`** (worker-scoped, the default): one `startServer({})` per Playwright worker, shared by that worker's
  specs.

A spec that needs a server that isn't set up, a different config, a restart, or a change to server-wide settings
calls `startServer` for its own: `setup.spec`, `reconnect.spec` (c), `tcp-only.spec` and `admin.spec` (see the
table below). This is the rule for new specs too. The harness needs no TLS: the `tcp443` probe ("TCP 443 ✓") is
covered in Go (04 §17, a `servertest` case with TLS).

CI runs Chrome stable headful under `xvfb-run` with a PulseAudio null sink (06's `e2e` job). Chrome flags:
`--auto-select-tab-capture-source-by-title=isshoni-e2e-tone`, `--use-fake-ui-for-media-stream`,
`--allow-loopback-in-peer-connection`, and `--autoplay-policy=no-user-gesture-required` except in the unmute spec.

**Capture source**: `e2e/tone.html` (served through `context.route`, title `isshoni-e2e-tone`) draws an animated canvas
with a frame counter and plays a 1 kHz WebAudio tone. The sharer opens it in its own context and clicks the real
**Share** button; Chrome auto-selects that tab (plan CI item). S4 finding 7 (headless Chrome can't auto-accept a
picker) is why CI runs headful. For local headless runs, and for whole-screen cases, the fake-display seam is used:
when `sessionStorage['isshoni.e2e.fakeDisplay']` is set **and** the host is `localhost` or `127.0.0.1`,
`displayMedia.ts` skips `getDisplayMedia` and returns a canvas + WebAudio stream, reporting `displaySurface` from the
value (`browser`, `window`, `monitor`, `monitor+audio`; `1` means `browser` with audio) to `classify.ts`.

The tone tab has no flash/beep sync marker, and no browser spec measures the A/V offset. The plan's 45 ms target is
checked by S63's Go integration test (02 int. 5, flash-to-beep offset ≤ 5 ms through the SFU) and by the exit
session's survey ("nobody reports audio out of sync", 06 §12.3). M1 builds no browser flash/beep harness.

Stats are read through `window.__isshoni.stats()` (§10.7).

| Spec | Asserts |
|---|---|
| `setup.spec` | own server with `setUp: false`: wizard: admin created; connection test shows UDP ✓, TCP 7882 ✓, TCP 443 row hidden (`transport_disabled` in off mode); invite link and QR present; `setupWizardDone` set. A second own server with `{'listen.ice_udp': ''}`: the UDP row shows ✗ with `conntest.udpDisabled` |
| `invite.spec` | a second context opens the invite link, signs up, lands in Lounge; the fragment is gone from the URL; both appear in the people panel |
| `watch.spec` | sharer (tone tab) live; `room.state` lists layers `high` and `low`; the viewer's focused tile has `framesDecoded > 0` and `frameHeight ≥ 540`; audio `audioLevel > 0.05` and `totalAudioEnergy` rising; the sharer sees the viewer's name |
| `focus-audio.spec` | 2 sharers + viewer: audio bytes flow only for the focused share; clicking the other tile flips it within 2 s; the speaker button moves audio without moving focus; the newest share is auto-focused |
| `layers.spec` | thumbnail `frameHeight ≤ 360`; a tile scrolled out of view stops receiving video bytes within 3 s; fullscreen turns thumbnails off |
| `unmute.spec` | without the autoplay flag, "Tap to unmute" appears; after a click, the audio element plays and `audioLevel > 0.05` |
| `reconnect.spec` | (a) `__isshoni.dropSocket()`: resumed with the same `connectionId`, `framesDecoded` keeps rising, no new sub offer; (b) `context.setOffline(true)` for 5 s: the banner appears after 2 s, then clears; (c) own server, `restart()` (same data dir and ports): the viewer decodes the re-published share within 15 s, focused, with audio |
| `warning.spec` | fake display `monitor+audio` shows the warning; "Share without sound" publishes without an audio track; "Pick something else" reopens the picker |
| `a11y.spec` | axe (`wcag2a`, `wcag2aa`, `wcag21aa`) on login, invite, reset, setup, the room with a share, admin users: no serious or critical violations; the tile list is keyboard-reachable; `F` toggles fullscreen |
| `pwa.spec` | the manifest parses; the service worker activates; offline navigation shows the Offline screen from the cached shell |
| `version.spec` | `page.routeWebSocket` rewrites `serverVersion` in `welcome`: the page reloads once, then shows VersionMismatch; a routed `protocol_unsupported` error shows it at once |
| `admin.spec` | own server (approval mode is server-wide): create and revoke an invite; approve a pending sign-up in approval mode; doctor page renders results |
| `tcp-only.spec` (04 S14, README S90) | own server with `{'listen.ice_udp': ''}` (no UDP media stands in for a network that blocks UDP): the viewer's focused tile has `framesDecoded` rising, and its selected candidate pair is TCP |

01 §19 lists the same watch, reconnect, restart and focus scenarios; they are these specs, not a second copy.

`playwright.config.ts` holds only these per-PR specs. *later (M6)*: an automated soak project (plan: load and soak
tests). The M1 exit check is the human session of 06 §12.

### 19.4 Manual device matrix (every minor release; M1 exit)

| ID | Device | Checks |
|---|---|---|
| M-IOS-1 | iPhone, Safari tab | invite → sign up → Lounge; tap to unmute; fullscreen; rotate; lock and unlock (reconnect) |
| M-IOS-2 | iPhone, Home Screen app | Add to Home Screen; log in once; enable notifications; "Alex started sharing" arrives; tapping it opens the room with that share focused |
| M-IOS-3 | iPhone with a Discord call on the same phone | both audible; isshoni doesn't interrupt the call |
| M-IOS-4 (optional) | iPhone, invite link opened from a chat app | the in-app banner (or, where undetectable, the chat-app line of `needs-install`) appears; Copy link → Safari → sign up there and log in only once more inside the Home Screen app |
| M-IPAD | iPad Safari | element fullscreen; keyboard shortcuts with a hardware keyboard |
| M-AND-1 | Android Chrome (emulator + a friend's phone) | invite → sign up → Lounge; install prompt; push; background audio; media-session metadata |
| M-FF-1 | Firefox, fresh profile | decoder-pending message with audio playing, then video without a reload; the Chrome sharer switches to Constrained Baseline |
| M-SAF | Safari macOS viewer | plays High; PiP |
| M-EDGE | Edge on Windows 11 sharer | window + its audio: only that app heard; whole screen + system audio shows the warning |
| M-CHR-MAC | Chrome on macOS sharer | High profile; hardware encoders for both layers (stats) |
| M-LAN | Firefox on macOS against a LAN server | the Local Network fix text appears (S4 finding 6) |
| M-WIFI | phone viewer switching Wi-Fi → LTE | playback recovers without a reload (01 §11.5 C) |
| M-BW | Chrome viewer on a throttled downlink (macOS Network Link Conditioner, or `tc` on Linux), watching a share with motion | the focused tile drops to `low` with reason `bandwidth` ("Lower quality (your connection)"); after the throttle is lifted it returns to `high` within about 2 minutes, with no reload (02 §10.2, trial upgrades) |

---

## 20. Security and privacy rules for the SPA

- Setup, invite and reset tokens come only from the URL fragment, are removed with `replaceState` before any other
  request, and are posted in JSON bodies. Nothing sends them in a URL.
- No token of any kind in `localStorage`. 01's resume token stays in memory; setup, invite and reset tokens sit in
  `sessionStorage` only until the form succeeds.
- No raw HTML (`dangerouslySetInnerHTML` is lint-banned); the QR code is React SVG. External links use
  `rel="noopener noreferrer"`.
- The service worker never caches `/api` or `/ws` responses, and `notificationclick` opens only same-origin URLs.
- Share metadata sent to the server is `kind` (`screen`/`window`/`tab`), preset and flags: no window titles (01 §8.5).
- Stats, logs and "Copy result" contain no IP addresses.
- The debug handle (`window.__isshoni`) exists only when the debug flag is set, and the fake-display seam only on a
  loopback host.
- The clipboard is written only on a click.

---

## 21. Interfaces other docs rely on

| Export | Where | Consumers |
|---|---|---|
| `web/embed.go` as 04 §9.5 specifies (`Dist()`); `dist/` layout incl. `version.json`, `licenses.txt`, `.br`/`.gz` siblings | §17.1 | 04, 06 |
| SPA routes; no dots in route segments; `/r/{roomId}?focus={shareId}` for push links | §5 | 03 §12.6, 04 §9.5, §14.3 |
| Unsafe REST requests always send `Content-Type: application/json` | §6.2 | 03 §7.5 |
| `hello` values: `client` from `ClientInfo` (`kind: web`), `role` `full` or `viewer` (§8 rule), `caps` from 01's `detectCaps()` | §7, §8 | 01, 02 |
| Subscription policy ("05 policy" in 01 §8.9): the rules of §12.4, batched updates, hidden tab → video off after 10 s | §12.3–§12.4 | 01, 02 |
| `invalidate` topic → query-key map | §6.2 | 01 §8.12, 03 |
| `localStorage` key `isshoni.lastRoomId` | §6.1 | 01 §10.5 |
| Share start: `kind` from `displaySurface`; complete ascending `sendEncodings` (`q`, `f`) at `addTransceiver`, built from `ShareParams` (wire order high first) with `scaleResolutionDownBy = max(1, sqrt(w·h/maxPixels))`, addressed by rid; `degradationPreference` in one `setParameters` before the offer; codec order; `quality.hint` handling | §13.4–§13.6 | 01, 02 |
| Connection-test client: three probes, `ProbeVerdict`, `ConnTestResult`, fix-text keys | §14.2 | 04 §7.7, 02 §7.6 |
| i18n catalog `web/src/i18n/en.json`; key families `errors.<code>`, `errors.local.*`, `fieldErrors.<field>.<code>`, `doctor.<code>`, `doctor.fix.<fix_code>`, `fix.firewall.<provider>`, `conntest.nat.<nat>`, `push.<type>.*`, `push.adminAlert.<kind>`, `share.label.<kind>` | §16.5 | 01, 03, 04, M2 tray |
| Service-worker push contract: every payload shows a notification; handled `type`s; `url` same-origin; `tag` used as given | §16.3 | 04 §14.3 |
| Platform adapter types (`Platform`, `SharingProvider`, `ActiveShare`, `NotificationsProvider`, `PwaProvider`, and `SignalClientLike` for `ShareContext.signal`), reserved globals `__ISSHONI_DESKTOP__`, `Capacitor` | §8 | M2/M3 desktop, M4 agent |
| e2e harness: `ISSHONI_BIN`, `ISSHONI_DATA_DIR`, `deploy/dev/isshoni.e2e.toml` (base config; ports come from the fixture, none fixed), `fixtures.ts` (`server` per worker, `startServer({overrides, setUp})` → `{url, admin, restart(), stop()}`), per-server logs `web/test-results/server-*.log`, spec list, `window.__isshoni` | §19.3 | 01 §19, 04 §17, 06 |
| npm scripts `lint`, `typecheck`, `test`, `build`, `e2e`, `check:i18n`, `check:size`; env `ISSHONI_VERSION`, `ISSHONI_DEV_SERVER` | §2, §17 | 06 |

---

## 22. Depends on

**01-protocol.md**: the TS layer in §16 (`SignalClient` with `onResync`, `staleBuild`, `retryNow()`, `probe()` and
`info.shutdown`, `ProtocolError`, `detectCaps`, `h264Key`, generated `ClientRequests`/`ClientNotifications`/
`ServerMessages`); message payloads of §8 (incl. the `InboundStats` counters this SPA reports); negotiation and
recovery rules of §9–§10; error codes and client actions of §12.

**02-sfu.md**: `ShareParams` numbers per preset (02 §8.6, matching §13.5 here); codec policy and
`quality.hint{codec}` (§8.3); caps-driven Firefox handling and the server's retries (§8.5); probe PCs (§7.6); layer
pausing through `quality.hint.encodings` (§11); one sub ICE restart at a time (§5.3), so the client counts a sub offer
with a new `ice-ufrag` as its restart (§9 here).

**03-accounts-and-store.md**: the REST endpoints and DTOs of §12 (paths in §6.2 here, including push subscriptions
and preferences), the error envelope (§12.2), CSRF by content type (§7.5), cookies (§7.4), setup/invite/reset flows
(§7.8–§7.10), the approval queue and its Reject all (§7.9), `setupWizardDone` (§7.8), `Me.permissions` and
`Me.badges`, `GET /api/v1/rooms` with `showRoomList` and `defaultRoomId`, admin-only room creation.

**04-server-platform.md**: SPA serving and headers (§9.5–§9.6); `POST /api/v1/conntest` (§7.7); doctor JSON and
provider ids (§13); `GET /api/v1/admin/bandwidth` (§13.4); dashboard (§11.4); the push payload (§14.3); dev Host check
(§9.3).

**06-deploy-and-ci.md**: tool pins and `packageManager` (§7.1); Taskfile names (§7.2); dev and e2e configs (§7.3); the
license script and `licenses.txt` (§8.3); the `web` and `e2e` CI jobs (add `check:i18n` and `check:size` to the `web`
job); the project site URL (D11).

**Conflicts between sibling docs that touched the SPA, as the integrator settled them:**

| # | Topic | Decision |
|---|---|---|
| C1 | Wire shapes (01 vs 02 §6.4) | 01 owns the wire. 02's internal API maps to it through 01's `sfuplane` (01 §15.4). The SPA uses `tracks` for both PCs |
| C2 | Firefox retries | Server only: 02 rebuilds the sub PC every 20 s, at most 9 times; the client polls caps and sends `caps.update`, never `pc.restart` for codecs |
| C3 | REST error body | One envelope, 03 §12.2: `{"error": {code, fields?, params?, retryAfter?, requestId?}}`; 04 uses it |
| C4 | Push REST | 03 owns it: `POST /api/v1/push/unsubscribe {endpoint}`, `GET/PUT /api/v1/push/preferences`; the VAPID key is only in `/info` |
| C5 | JSON casing | camelCase everywhere, including 04's dashboard, doctor, conntest and push payload |
| C6 | Dashboard shape | 04 §11.4 (one endpoint), with 03's `accounts` object inside |
| C7 | Connection-test endpoint | 04's `POST /api/v1/conntest` with `udp`/`tcp443`/`tcp7882`, `offer`/`answer` |
| C8 | Keep file | `web/dist/.gitkeep` |
| C9 | Who may run the connection test | Any signed-in user; fix text for admins only |

## 23. Implementation slices

Ordered; each is testable on its own. Sizes: S ≈ 1–2 days, M ≈ 3–4 days, L ≈ 5+ days. A first "watch together"
demo is possible after W7.

| # | Slice | Size | Depends on | Acceptance |
|---|---|---|---|---|
| W1 | **Scaffold, build and embed**: npm project, Vite/React/TS 6 strict, ESLint (all plugins), Prettier, Vitest, i18n init with `en.json`, tokens and global CSS, `boot-check.js`, `embed.go`, build plugins (version, compress, report), `check:size`, `scripts/check-i18n.mjs` with its first rule (a `t('…')` literal key missing from `en.json`, §16.5); 06's license script wired into `build` | M | 06 S1 | `npm ci && npm run lint && npm run typecheck && npm test -- --run && npm run build && npm run check:size && npm run check:i18n` pass; a JSX literal string fails lint; a `t('…')` literal key missing from `en.json` fails `check:i18n`; `go build ./...` works on a fresh clone and embeds a real build after `task build:web`; `.br`/`.gz` siblings present; React Router 8 API names confirmed |
| W2 | **Platform + REST + boot**: `types.ts`, `BrowserPlatform` (client info, caps via 01's `detectCaps`, role, storage, apiFetch), `rest.ts`/`ApiError`, query client, `invalidate.ts`, boot sequence (`app/boot.tsx`, with step 0's fragment stash), `router.tsx` with every §5 route and the page-folder contract, the guards with the `next` guard, the `['me']` query and the logout `BroadcastChannel` (`app/guards.tsx`, `me.ts`, `session.ts`), Offline/NeedsHttps/NotSetUp/Unsupported/Fatal screens; the `check:i18n` error-code rule (§16.5) and an `errors.<code>` key in `en.json` for every `ErrorCode` and api `Code…` constant | M | W1, 01 P2, 03 DTOs | Unit tests for `ApiError` (both envelopes) and the topic map; MSW tests for each boot branch; `role` is `viewer` with a mobile UA; `check:i18n` fails when an error code has no `errors.<code>` key; the i18n unit test of §19.1 passes |
| W3 | **Auth and setup step 1**: login, invite, signup, pending, reset, logout (calling W2's `broadcastLogout()`), `useMe.ts` and `fragmentToken.ts` on W2's `['me']` query and stashed token, About/trust model (`AboutPage` in `auth/`), the pages exported from `auth/index.ts`; `/setup` step 1 (§14.1, `SetupPage`, exported from `setup/index.ts`: fragment token, `setup/check`, create-admin form, `setup/complete`), navigating to `/` until W10 | M | W2, 03 auth | Component tests for each error code on each form; the fragment is removed before the first request (spy on fetch); a logout in tab A redirects tab B; `SetupPage` (MSW) reads the `/setup` fragment token, calls `setup/check`, and its form calls `setup/complete`, then navigates to `/` |
| W4 | **Signaling wiring**: `connection.ts` with 01's `SignalClient`, `connectionStore`, the §7.1 UI states, stale-build reload | S | W2, 01 P10 | Against a `task dev` server the page reaches `ready`; killing the server shows "Reconnecting…" after 2 s and recovers; a fake `staleBuild` reloads once |
| W5 | **Room session and shell**: `RoomSession` join/leave/resync, `roomStore`, `room.event` announcements, RoomPage layout, header, people panel, switcher (`showRoomList`), `InRoomBar`, root redirect | M | W4, 01 P4–P5, 03 rooms | Two browsers in Lounge see each other within 1 s; closing a tab removes its presence at once (close 1000) while cutting its network keeps it for the grace period; the switcher appears only when `showRoomList` |
| W6 | **Web sharer core**: ShareSheet, `pick()` with the §13.2 options and fallbacks, fake-display seam, `classify`, `ScreenAudioWarning`, `share.start`, `PublisherPC` (gen/neg, tracks), codec prefs, encodings from `ShareParams`, presets, stop and browser-stop | L | W5, 01 P7, 02 publish | `watch.spec`'s sharer half: `room.state` shows the share live with layers `high` and `low`; `warning.spec` passes; the tone-tab capture works in CI (xvfb) and the choice is recorded; unit tests for classify/codecPrefs/encodings |
| W7 | **Viewer core**: `SubscriberPC`, media registry, Stage/Tile/ViewerLayout, auto-focus, audio-follows-focus via `audioOut`, TapToStart, watchers popover, stats collector core and the `window.__isshoni` debug handle | L | W5, W6, 02 subscribe | `watch.spec`, `focus-audio.spec` and `unmute.spec` pass |
| W8 | **Layers, fullscreen, keyboard, mobile**: `layerPolicy` + `SubscriptionSync`, IntersectionObserver and visibility, fullscreen/PiP/wake lock, shortcuts, phone layouts, iOS handling, media session, `?focus=` | M | W7 | `layers.spec` passes; policy unit tests; manual M-IOS-1 and M-AND-1 viewing checks pass |
| W9 | **Resilience**: §9 recovery table, `resync` (resumed and not), re-publish with `replaces` and focus carry-over, 60 s capture hold, Firefox codec wait (caps polling and `caps.update` only), `quality.hint` (codec switch and layer `active` flags) | L | W7, W6, 01 P9 | `reconnect.spec` (a)(b)(c) pass; manual M-FF-1 and M-WIFI pass; a `quality.hint{codec}` makes the sharer re-offer and viewers keep decoding |
| W10 | **Setup wizard steps 2–3 + connection test**: `/admin/welcome` (`WelcomePage` steps 2–3, `setupWizardDone`), W3's `SetupPage` now navigates to `/admin/welcome?step=2`; `runConnTest`, verdicts, fix text, `InviteLinkCard` with QR; the `check:i18n` `CloudProvider`/`NATKind` rule (§16.5) with every `fix.firewall.<provider>` and `conntest.nat.<nat>` text | L | W3, 04 conntest, 02 probe | `setup.spec` passes; `check:i18n` fails when a `CloudProvider` or `NATKind` constant has no text; on a test VPS with UDP 7882 blocked by the provider firewall the page shows ✗ UDP / ✓ TCP with that provider's text; a non-admin sees no fix text |
| W11 | **Account + PWA + Web Push**: account and devices pages, manifest, SW (`sw-plugin`, routes, push), update pill, install prompts, notifications flow incl. iOS Home Screen guidance, the in-app browser banner and preferences | L | W3, 03 push REST, 04 push wiring | `pwa.spec` passes; SW unit tests pass; manual M-IOS-2 and M-AND-1 push checks pass |
| W12 | **Admin pages**: dashboard, users, approvals, invites, rooms, settings, audit, doctor + bandwidth | L | W3, W10, 03/04 admin | `admin.spec` passes; the dashboard updates every 2 s while visible and stops when hidden |
| W13 | **Hardening**: debug overlay with `stats.watch`, `stats` notifications, sharer hints and level meter, announcer, reduced motion, `check:i18n` unused-key warnings (its other rules came in W1, W2 and W10), VersionMismatch, `a11y.spec`, `version.spec` | M | W7, W9 | Those specs pass; a test shows `check:i18n` fails when a key is removed, and the script warns about unused keys; the overlay shows per-tile stats with no IPs; the upload hint appears under Chrome DevTools network throttling (manual) |
| W14 | **CI and exit checks**: full e2e in CI (06), manual matrix §19.4 | M | all | CI green on a PR; the matrix filled in; the 06 §12 exit session passes (5 friends, 2 hours, no manual fixes; iPhone and Android watch) |

---

## 24. Owner decisions

1. **Auto-focus after a manual pick: toast** (owner, 2026-09-30). The newest share is focused automatically only
   while the friend hasn't picked a tile. Once they click a tile, a newly started share shows a "bo started sharing
   [Watch]" toast instead of taking over the stage and the sound, until the picked share ends; then the newest live
   share is focused again.

Decided at integration:
- **Web sharing in desktop Firefox and Safari**: the Share button appears wherever the feature probe passes (plan:
  "detect features at runtime"), labeled "works best in Chrome or Edge"; only Chrome and Edge are tested per release.
- **Pause in the web sharer**: not in M1 (the plan has Pause for the desktop app only); it arrives with 01's
  `share.pause` feature in M2.
