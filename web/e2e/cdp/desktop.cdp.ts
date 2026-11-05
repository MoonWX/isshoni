// The real app over CDP (M2 03 §22.4 B; README WA48's skeleton, completed by WA87 for DK13): it starts the e2e build
// of isshoni.exe (or attaches to a running one), connects Playwright to its WebView2 and checks what only the real
// webview can answer. The Wails checklist rows (03 §3.2): W1 (the asset origin; the server's CORS for it end to end
// in the link case), W2 (a secure context), W3 (the CSP is enforced), W5 (top-level navigation and new windows don't
// leave the asset origin; at beta.34 through the row's fallback), W7 (closing the window hides it), W9 (the WebView2
// settings that CDP can observe: zoom and the browser accelerator keys), W13 (audio plays without a click), W16
// (element fullscreen fills the screen and Esc returns), W21 (the runtime's own objects get 403 from the page and
// nothing opens) and W19 (a production build opens no debugging port or pipe, and its WEBVIEW2_* scrub holds). Then
// the app end to end against a real `isshoni serve`: a device-flow link approved in a second browser, a fake share
// watched in Chrome, the window closed to the tray while live (the share goes on), and Quit (the share ends, the
// process exits 0 within 3 s, 03 §20). Each check writes its row as an annotation ("Wails checklist"), which the PR
// copies; the budgets of 03 §20 that this runner can show are annotations too ("Budget (03 §20)": startup to the
// page with a new WebView2 profile, the private working sets while sharing the fake engine's 640x360). Those are CI
// numbers, not the budget's: the budget is the owner's PC sharing 1080p60 (M-DESK-12).
//
// Environment (web/e2e/cdp/playwright.config.ts): ISSHONI_DESKTOP_EXE (the e2e build), or ISSHONI_CDP_ENDPOINT to
// attach to a running app (then only the checks that need no process of their own run). Windows only. The app is
// started with --relaunched (so it stays this process's child), ISSHONI_E2E_CDP_PORT (README WA72's e2e-only port)
// and ISSHONI_E2E_DATA_DIR: a temporary folder that stands in for %LOCALAPPDATA% (03 §22.4 B), whose
// isshoni\settings.json the spec seeds (by default closeToTray, 03 §11.6, so W7 needs no live share, and wizardDone).
// Setting LOCALAPPDATA would not do: the app reads its data folder from the shell's known folder, never from the
// environment (cmd/isshoni-desktop/platform_windows.go), so the e2e build takes it from its own e2e-only variable
// (README WA72, flavor_e2e.go), as it takes the port. Once the endpoint answers, the fixture checks that the app wrote
// into that folder (its log file or its WebView2 profile) and fails before any test runs otherwise: an e2e build that
// ignored it would be using the owner's real servers, settings and WebView2 profile.
//
// The servers come from ../fixtures.ts (ISSHONI_BIN; the config defaults it to bin/isshoni.exe, which the CI job
// builds), and so do the browser contexts of the people (Google Chrome). The W19 case runs a production build too:
// ISSHONI_DESKTOP_PROD_EXE, or in CI the stage's dist-desktop/stage/isshoni.exe. A production build has no e2e data
// folder: it runs on the PC's real %LOCALAPPDATA%\isshoni, which is why it never runs unless asked for outside CI.
// It also runs under the real app id (winos.InstanceID): an installed isshoni that is running takes the
// single-instance forward and the build exits 0 at once (quit the installed app first), and Wails' notifications
// service rewrites HKCU\Software\Classes\CLSID\{GUID}\LocalServer32 to this build's path, so the installed app's toast
// activator points at it until the installed app next starts.
//
// What the runner can't show stays manual (03 §22.5, the owner's PC): W9's status bar, autofill and password save,
// swipe, external drop, the default context menu and DevTools of a production build, and the real keyboard's
// accelerators (CDP's key events enter at the renderer's widget; whether WebView2's own accelerator path sees them is
// what the manual step adds).
import { spawn, execFile, type ChildProcess } from 'node:child_process';
import { randomBytes } from 'node:crypto';
import fs from 'node:fs';
import fsp from 'node:fs/promises';
import net from 'node:net';
import os from 'node:os';
import path from 'node:path';
import { promisify } from 'node:util';

import { chromium, type Browser, type Page } from '@playwright/test';

import { DesktopAssetOriginWindows } from '../../src/protocol/types.gen.ts';
import { expect, test as e2e } from '../fixtures.ts';
import { REPO_ROOT } from '../global-setup.ts';
import { audioOf, readStats, waitForRoom, waitForState, waitForStats, videoOf } from '../stats.ts';
import {
  type ChildProcessInfo,
  descendants,
  environmentNames,
  MAIN_WINDOW_CLASS,
  mainWindow,
  memory,
  mib,
  PILL_WINDOW_CLASS,
  postClose,
  pressEscape,
  processWindows,
  ps,
  topWindows,
  webviewProcesses,
  windowState,
} from './windows.ts';

const execFileAsync = promisify(execFile);

const EXE_ENV = 'ISSHONI_DESKTOP_EXE';
const ENDPOINT_ENV = 'ISSHONI_CDP_ENDPOINT';
/** A production build for W19 (outside CI: only when set; it runs on the PC's real %LOCALAPPDATA%\isshoni). */
const PROD_EXE_ENV = 'ISSHONI_DESKTOP_PROD_EXE';
/** The e2e build's own variable for its WebView2 debugging port (README WA72). */
const PORT_ENV = 'ISSHONI_E2E_CDP_PORT';
/** The e2e build's own variable for the folder that replaces %LOCALAPPDATA% (README WA72). */
const DATA_DIR_ENV = 'ISSHONI_E2E_DATA_DIR';
/** WebView2's first start (a new user data folder) takes a few seconds. */
const START_TIMEOUT_MS = 60_000;
/** A killed app is gone within this; the next start needs its single-instance mutex free (03 §13.3). */
const EXIT_TIMEOUT_MS = 15_000;
/** 03 §20: Quit waits at most 3 s for the server. */
const QUIT_BUDGET_MS = 3_000;
/** C2 connects, publishes and goes live; a viewer's sub PC follows. */
const LIVE_TIMEOUT_MS = 30_000;
const MEDIA_TIMEOUT_MS = 20_000;
/** The device flow's poll interval is 5 s; an approval reaches the app within two of them. */
const LINKED_TIMEOUT_MS = 30_000;
/** The fake engine's picker names its window this way (internal/media/fake). */
const FAKE_LABEL = 'Fake window';

/** settings.json as the spec seeds it (03 §14.2, schema 1). */
type SeedSettings = Readonly<Record<string, unknown>>;

/** closeToTray, so W7's close hides without a live share; wizardDone, so a link goes straight to the room. */
const DEFAULT_SETTINGS: SeedSettings = { schema: 1, closeToTray: true, wizardDone: true };

interface LaunchOptions {
  /** isshoni\settings.json in the data folder; default DEFAULT_SETTINGS. */
  settings?: SeedSettings;
  /** The command line after --relaunched (e.g. --server <origin>, 03 §13.1). */
  args?: string[];
  /** More environment for the app (W19's control sets WEBVIEW2_* here). */
  env?: Readonly<Record<string, string>>;
}

/** How the app's process ended, and when (Date.now()). */
interface Exit {
  readonly code: number | null;
  readonly at: number;
}

interface RealApp {
  readonly browser: Browser;
  /** The main window's page at the asset origin. */
  readonly page: Page;
  /** The app's process, or null when the spec attached to a running app. */
  readonly process: ChildProcess | null;
  /** Settles when the process exits; never, for an attached app. */
  readonly exited: Promise<Exit>;
  /** The app's log folder (03 §17) under its e2e data folder, until stop(); null for an attached app. */
  readonly logDir: string | null;
  /** Kills the app (with its WebView2) unless it exited, waits for the exit and removes its data folder. */
  stop(): Promise<void>;
}

/** Records a checklist row for the PR's "Wails checklist" (03 §3.2). */
function row(id: string, result: string): void {
  e2e.info().annotations.push({ type: 'Wails checklist', description: `${id}: ${result}` });
}

/** Records a number for 03 §20's budgets, measured on this runner. */
function budget(what: string): void {
  e2e.info().annotations.push({ type: 'Budget (03 §20)', description: what });
}

function sleep(ms: number): Promise<void> {
  return new Promise((resolve) => setTimeout(resolve, ms));
}

function freePort(): Promise<number> {
  return new Promise((resolve, reject) => {
    const srv = net.createServer();
    srv.once('error', reject);
    srv.listen(0, '127.0.0.1', () => {
      const { port } = srv.address() as net.AddressInfo;
      srv.close(() => {
        resolve(port);
      });
    });
  });
}

/** Whether something accepts a TCP connection on 127.0.0.1:port within 2 s. */
function portOpen(port: number): Promise<boolean> {
  return new Promise((resolve) => {
    const socket = net.connect({ host: '127.0.0.1', port });
    const done = (open: boolean): void => {
      socket.destroy();
      resolve(open);
    };
    socket.setTimeout(2_000, () => {
      done(false);
    });
    socket.once('connect', () => {
      done(true);
    });
    socket.once('error', () => {
      done(false);
    });
  });
}

/**
 * The app's environment: this process's without any ISSHONI_* variable (the config's ISSHONI_BIN, CI's
 * ISSHONI_VERSION) and without WEBVIEW2_* (a developer's shell must not change the webview under test), plus extra.
 */
function appEnv(extra: Readonly<Record<string, string>>): NodeJS.ProcessEnv {
  const env: NodeJS.ProcessEnv = {};
  for (const [name, value] of Object.entries(process.env)) {
    const upper = name.toUpperCase();
    if (!upper.startsWith('ISSHONI_') && !upper.startsWith('WEBVIEW2_')) env[name] = value;
  }
  return { ...env, ...extra };
}

/** Spawns an exe with --relaunched and follows its exit. */
function spawnApp(
  exe: string,
  args: readonly string[],
  env: NodeJS.ProcessEnv,
): { child: ChildProcess; exited: Promise<Exit> } {
  const child = spawn(path.resolve(exe), ['--relaunched', ...args], { env, stdio: 'ignore' });
  const exited = new Promise<Exit>((resolve) => {
    child.once('exit', (code) => {
      resolve({ code, at: Date.now() });
    });
    child.once('error', () => {
      resolve({ code: null, at: Date.now() });
    });
  });
  return { child, exited };
}

/** Kills a process tree (the app and its WebView2) unless it is gone, and waits for its exit. */
async function kill(child: ChildProcess, exited: Promise<Exit>): Promise<void> {
  if (child.pid !== undefined && child.exitCode === null && child.signalCode === null) {
    await execFileAsync('taskkill', ['/PID', String(child.pid), '/T', '/F']).catch(() => undefined);
  }
  const gone = await Promise.race([exited.then(() => true), sleep(EXIT_TIMEOUT_MS).then(() => false)]);
  if (!gone) throw new Error(`isshoni.exe (${String(child.pid)}) did not exit within ${String(EXIT_TIMEOUT_MS)} ms`);
}

/**
 * Throws unless the app started under dataDir: its log file (03 §17) or its WebView2 user data folder exists in
 * <dataDir>\isshoni. Both are created before the window opens, so by the time the endpoint answers one is there.
 */
async function assertUsedDataDir(dataDir: string): Promise<void> {
  const root = path.join(dataDir, 'isshoni');
  for (const p of [path.join(root, 'logs', 'isshoni.log'), path.join(root, 'WebView2')]) {
    try {
      await fsp.access(p);
      return;
    } catch {
      // try the next one
    }
  }
  throw new Error(
    `the e2e build ignored its data folder: nothing under ${root} after its WebView2 opened. Does it read ` +
      `${DATA_DIR_ENV} (README WA72, flavor_e2e.go)? It is using the real %LOCALAPPDATA%\\isshoni instead.`,
  );
}

/** Waits until the debugging endpoint answers /json/version. */
async function waitForEndpoint(endpoint: string, child: ChildProcess | null): Promise<void> {
  const deadline = Date.now() + START_TIMEOUT_MS;
  for (;;) {
    if (child !== null && child.exitCode !== null) {
      throw new Error(`isshoni.exe exited (${String(child.exitCode)}) before its WebView2 opened ${endpoint}`);
    }
    try {
      const res = await fetch(`${endpoint}/json/version`, { signal: AbortSignal.timeout(2_000) });
      if (res.ok) return;
    } catch {
      // not yet
    }
    if (Date.now() > deadline) {
      throw new Error(
        `no CDP endpoint at ${endpoint} within ${String(START_TIMEOUT_MS / 1000)} s: is this an e2e build ` +
          `(task desktop:build:e2e) that reads ${PORT_ENV} (README WA72)?`,
      );
    }
    await sleep(250);
  }
}

/** Connects Playwright to the endpoint and returns the page at the asset origin, once it has loaded. */
async function attachPage(endpoint: string): Promise<{ browser: Browser; page: Page }> {
  // noDefaults: by default Playwright turns on focus emulation for every page it attaches to, which keeps
  // document.visibilityState 'visible' whatever the window does, so W7 could never see the hidden window (03 §4.4).
  // It also leaves acceptDownloads and media emulation at the webview's own settings; no check here relies on them.
  const browser = await chromium.connectOverCDP(endpoint, { noDefaults: true });
  const pages = (): Page[] => browser.contexts().flatMap((c) => c.pages());
  await expect
    .poll(() => pages().some((p) => p.url().startsWith(DesktopAssetOriginWindows)), { timeout: START_TIMEOUT_MS })
    .toBe(true);
  const page = pages().find((p) => p.url().startsWith(DesktopAssetOriginWindows));
  if (page === undefined) throw new Error('no page at the asset origin');
  await page.waitForLoadState('domcontentloaded');
  return { browser, page };
}

/** Starts the e2e build (or attaches to the running app) and connects to its webview. */
async function launch(options: LaunchOptions): Promise<RealApp> {
  const attach = process.env[ENDPOINT_ENV];
  if (attach !== undefined && attach !== '') {
    const { browser, page } = await attachPage(attach.replace(/\/$/, ''));
    return {
      browser,
      page,
      process: null,
      exited: new Promise<Exit>(() => undefined),
      logDir: null,
      stop: () => browser.close(),
    };
  }
  const exe = process.env[EXE_ENV];
  if (exe === undefined || exe === '') throw new Error(`set ${EXE_ENV} or ${ENDPOINT_ENV}`);
  const port = await freePort();
  const endpoint = `http://127.0.0.1:${String(port)}`;
  const dataDir = await fsp.mkdtemp(path.join(os.tmpdir(), 'isshoni-cdp-'));
  await fsp.mkdir(path.join(dataDir, 'isshoni'), { recursive: true });
  await fsp.writeFile(
    path.join(dataDir, 'isshoni', 'settings.json'),
    JSON.stringify(options.settings ?? DEFAULT_SETTINGS),
  );
  const started = Date.now();
  const { child, exited } = spawnApp(
    exe,
    options.args ?? [],
    appEnv({ ...options.env, [DATA_DIR_ENV]: dataDir, [PORT_ENV]: String(port) }),
  );
  let browser: Browser | null = null;
  const stop = async (): Promise<void> => {
    // A browser whose endpoint went away with the app can take its time to notice: never wait on it for long.
    if (browser !== null) await Promise.race([browser.close().catch(() => undefined), sleep(5_000)]);
    await kill(child, exited);
    // After a Quit, WebView2's processes outlive the app by a moment and hold its profile: a folder left in the
    // runner's temp is no failure.
    await fsp.rm(dataDir, { recursive: true, force: true, maxRetries: 10, retryDelay: 500 }).catch(() => undefined);
  };
  try {
    await waitForEndpoint(endpoint, child);
    await assertUsedDataDir(dataDir);
    const attached = await attachPage(endpoint);
    browser = attached.browser;
    const attachedAt = Date.now();
    // DOMContentLoaded as the page itself timed it (epoch ms on this machine's clock, as `started` is): the attach
    // time adds this spec's endpoint poll, the data-folder check and connectOverCDP, so it is recorded on its own.
    const dcl = await attached.page.evaluate(() => {
      const n = performance.getEntriesByType('navigation')[0] as PerformanceNavigationTiming | undefined;
      return n === undefined || n.domContentLoadedEventEnd === 0
        ? null
        : performance.timeOrigin + n.domContentLoadedEventEnd;
    });
    budget(
      `startup to the page (spawn of the e2e build, --relaunched, to DOMContentLoaded at the asset origin, a new ` +
        `WebView2 profile; CI runner): ${dcl === null ? 'unreadable (no navigation timing)' : `${String(Math.round(dcl - started))} ms`}; ` +
        `to Playwright attached: ${String(attachedAt - started)} ms`,
    );
    return {
      browser,
      page: attached.page,
      process: child,
      exited,
      logDir: path.join(dataDir, 'isshoni', 'logs'),
      stop,
    };
  } catch (err) {
    await stop();
    throw err;
  }
}

const test = e2e.extend<{ launchApp: (options?: LaunchOptions) => Promise<RealApp>; app: RealApp }>({
  // eslint-disable-next-line no-empty-pattern -- Playwright reads the fixture names from this pattern
  launchApp: async ({}, use) => {
    const own: RealApp[] = [];
    await use(async (options = {}) => {
      const app = await launch(options);
      own.push(app);
      return app;
    });
    for (const app of own) await app.stop();
  },
  app: async ({ launchApp }, use) => {
    await use(await launchApp());
  },
});

test.skip(process.platform !== 'win32' && !process.env[ENDPOINT_ENV], 'the real app runs on Windows');

/** The bound-method ids of the committed Wails bindings (03 §6.2; task gen:bindings writes them). */
const BINDINGS = path.join(
  REPO_ROOT,
  'web/src/platform/desktop/bindings/github.com/MoonWX/isshoni/internal/desktop/bridge',
);

/** The method id that the generated binding of service.method calls ($Call.ByID), read from its file. */
function methodId(service: 'App' | 'Auth' | 'Share', method: string): number {
  const source = fs.readFileSync(path.join(BINDINGS, `${service.toLowerCase()}.ts`), 'utf8');
  const m = new RegExp(`export function ${method}\\([^)]*\\)[^{]*\\{\\s*return \\$Call\\.ByID\\((\\d+)`).exec(source);
  if (m?.[1] === undefined) throw new Error(`no ${service}.${method} in the committed bindings`);
  return Number(m[1]);
}

/** A runtime request's answer, read in the page. */
interface RuntimeAnswer {
  readonly status: number;
  readonly body: string;
}

/**
 * Calls a bound method from the page with a raw /wails/runtime request, built as @wailsio/runtime builds a Call
 * (object 0, method 0, {call-id, methodID, args}): what the SPA's bindings send, through the Guard (03 §5.6).
 */
async function bound(
  page: Page,
  service: 'App' | 'Auth' | 'Share',
  method: string,
  args: unknown[] = [],
): Promise<RuntimeAnswer> {
  const id = methodId(service, method);
  return page.evaluate(
    async ({ methodID, callArgs }) => {
      const res = await fetch('/wails/runtime', {
        method: 'POST',
        headers: { 'x-wails-client-id': 'cdp', 'Content-Type': 'application/json' },
        body: JSON.stringify({
          object: 0,
          method: 0,
          args: { 'call-id': `cdp-${String(Date.now())}`, methodID, args: callArgs },
        }),
      });
      return { status: res.status, body: await res.text() };
    },
    { methodID: id, callArgs: args },
  );
}

/** A bound method's result, which must be 200 with JSON. */
async function boundJSON<T>(
  page: Page,
  service: 'App' | 'Auth' | 'Share',
  method: string,
  args: unknown[] = [],
): Promise<T> {
  const answer = await bound(page, service, method, args);
  expect(answer.status, `${service}.${method}: ${answer.body}`).toBe(200);
  return JSON.parse(answer.body) as T;
}

test('W1, W2: the page is at the asset origin, in a secure context', async ({ app }) => {
  const [origin, secure] = await app.page.evaluate(() => [location.origin, isSecureContext] as const);
  // W1's second half, "this value is in protocol.DesktopOrigins()", needs no webview: internal/protocol's
  // TestDesktopOrigins asserts DesktopOrigins()[0] == DesktopAssetOriginWindows. The server's CORS for it is shown
  // end to end by the link case below (the page reaches the server's REST API from this origin).
  row(
    'W1',
    `location.origin = ${origin}; DesktopOrigins membership: checked by internal/protocol's unit test ` +
      `(TestDesktopOrigins); the server's CORS for it: the link case`,
  );
  row('W2', `isSecureContext = ${String(secure)}`);
  expect(origin).toBe(DesktopAssetOriginWindows);
  expect(secure).toBe(true);
});

test("W3: the asset handler's CSP is enforced", async ({ app }) => {
  const result = await app.page.evaluate(
    () =>
      new Promise<{ ran: boolean; directive: string; policy: string }>((resolve) => {
        let directive = '';
        let policy = '';
        document.addEventListener('securitypolicyviolation', (e) => {
          directive = e.violatedDirective;
          policy = e.originalPolicy;
        });
        const s = document.createElement('script');
        s.textContent = 'window.__cdpInline = true';
        document.head.append(s);
        setTimeout(() => {
          resolve({ ran: (window as { __cdpInline?: boolean }).__cdpInline === true, directive, policy });
        }, 300);
      }),
  );
  row('W3', `inline script ran = ${String(result.ran)}, violated ${result.directive}`);
  expect(result.ran).toBe(false);
  expect(result.directive).toMatch(/^script-src/);
  expect(result.policy).toContain("script-src 'self'");
});

test('W5: navigation and new windows stay on the asset origin', async ({ app }) => {
  const { page } = app;
  const pages = (): number => app.browser.contexts().flatMap((c) => c.pages()).length;
  const before = pages();
  // The CSP sandbox (03 §5.3) must leave WebView2's own channel to Go in place: Wails' runtime and the navigation
  // guard's answer (navguard.go) post through it.
  const channel = await page.evaluate(
    () => typeof (window as { chrome?: { webview?: { postMessage?: unknown } } }).chrome?.webview?.postMessage,
  );
  // The SPA never does this (03 §5.5 layer 1). Beta.34 lets Go cancel nothing (W5 fails; 03 §3.2 records the
  // fallback): bridge.js's navigate listener cancels it (03 §5.2).
  await page.evaluate(() => {
    location.href = 'https://example.com/';
  });
  await page.waitForTimeout(1_500);
  const origin = await page.evaluate(() => location.origin);
  // The native window.open. DesktopPlatform replaced window's own (links.ts), and Window is a [Global] interface, so
  // open is an own property of window and Window.prototype has none. A src-less iframe's window still has the native
  // one (its about:blank needs no navigation, so the CSP's frame-src 'none' doesn't apply). Called on this window, it
  // opens from the main frame, as a script in the SPA would. Beta.34 has no NewWindowRequested handler: the HTML's
  // CSP sandbox, without allow-popups, refuses it and the call returns null (03 §5.3).
  const opened = await page.evaluate(() => {
    const frame = document.createElement('iframe');
    document.body.append(frame);
    try {
      const own = frame.contentWindow;
      if (own === null) throw new Error('the fresh frame has no window');
      return own.open.call(window, 'https://example.com/w') === null;
    } finally {
      frame.remove();
    }
  });
  await page.waitForTimeout(1_000);
  const after = pages();
  // A navigation no page script can cancel, started as the browser starts one (CDP's Page.navigate): Go's half of
  // the fallback (navguard.go) notices the foreign top level after WebViewNavigationCompleted and loads the start
  // page again. Both are asserted: that the window got there, so the check can't pass without leaving the asset
  // origin, and that it is back home.
  const started = Date.now();
  const left = await page
    .goto('https://example.com/g', { waitUntil: 'commit', timeout: 15_000 })
    .then(() => new URL(page.url()).origin)
    .catch((e: unknown) => `not reached (${e instanceof Error ? (e.message.split('\n')[0] ?? '') : String(e)})`);
  await expect
    .poll(() => page.evaluate(() => location.origin).catch(() => ''), { timeout: 20_000 })
    .toBe(DesktopAssetOriginWindows);
  const back = Date.now() - started;
  row(
    'W5',
    [
      `chrome.webview.postMessage: ${channel}`,
      `after location.href: ${origin}`,
      `native window.open returned null: ${String(opened)}`,
      `pages ${String(before)} → ${String(after)}`,
      `browser-started navigation reached ${left}, back on the asset origin after ${String(back)} ms`,
    ].join('; '),
  );
  expect(channel).toBe('function');
  expect(origin).toBe(DesktopAssetOriginWindows);
  expect(opened).toBe(true);
  expect(after).toBe(before);
  expect(left).toBe('https://example.com');
});

test('W7: closing the window hides it; the app keeps running', async ({ app }) => {
  test.skip(app.process?.pid === undefined, 'needs the app this spec started (its pid)');
  const pid = app.process?.pid ?? 0;
  // The main window's HWND (its Wails window class; windows.ts says why not .NET's MainWindowHandle).
  const hwnd = await mainWindow(pid);
  expect(hwnd).toMatch(/^[1-9]\d*$/);
  // WM_CLOSE to the main window, as its close button sends it. False would mean it wasn't posted.
  expect(await postClose(hwnd)).toBe(true);
  // The window itself, independent of Chromium: it still exists (not destroyed) and is hidden (Hide(), 03 §3.2 W7).
  // Read first, and both polls soft, so the row below always records the window's state and the webview's
  // separately: a WebView2 visibility quirk then reads apart from a broken close path.
  await expect.soft.poll(async () => (await windowState(hwnd)).visible, { timeout: 10_000 }).toBe(false);
  // What M1's layer policy reads: the webview reports hidden (03 §4.4).
  await expect.soft.poll(() => app.page.evaluate(() => document.visibilityState), { timeout: 10_000 }).toBe('hidden');
  const { exists, visible } = await windowState(hwnd);
  // A dead app has no page to read: the row still gets written, and the asserts below fail on it.
  const state = await app.page
    .evaluate(() => document.visibilityState)
    .catch((e: unknown) => `unreadable (${String(e)})`);
  const alive = app.process?.exitCode === null;
  row(
    'W7',
    `after WM_CLOSE: IsWindow = ${String(exists)}, IsWindowVisible = ${String(visible)}, visibilityState = ${state}, ` +
      `process alive = ${String(alive)}`,
  );
  expect(exists).toBe(true);
  expect(visible).toBe(false);
  expect(state).toBe('hidden');
  expect(alive).toBe(true);
});

test('W9: zoom control and the browser accelerator keys are off', async ({ app }) => {
  const { page, browser } = app;
  // A mark that a reload (F5, Ctrl+R) would wipe, and the print path's event (Ctrl+P).
  await page.evaluate(() => {
    const w = window as { __cdpMark?: number; __cdpPrinted?: boolean };
    w.__cdpMark = 1;
    w.__cdpPrinted = false;
    window.addEventListener('beforeprint', () => {
      w.__cdpPrinted = true;
    });
  });
  const zoom = (): Promise<{ dpr: number; scale: number; width: number }> =>
    page.evaluate(() => ({
      dpr: window.devicePixelRatio,
      scale: window.visualViewport?.scale ?? 1,
      width: window.innerWidth,
    }));
  const cdp = await browser.newBrowserCDPSession();
  const targets = async (): Promise<string[]> =>
    (await cdp.send('Target.getTargets')).targetInfos.map((t) => `${t.type} ${t.url}`);
  const targetsBefore = await targets();
  const zoomBefore = await zoom();

  // Zoom in only (Ctrl + wheel up, Ctrl+=, Ctrl+numpad +): a zoom out after it could hide one. Chromium hands a
  // wheel event the page didn't consume back to the browser side, where Ctrl + wheel zooms when zoom control is on.
  const viewport = await page.evaluate(() => [window.innerWidth, window.innerHeight] as const);
  await page.mouse.move(Math.floor(viewport[0] / 2), Math.floor(viewport[1] / 2));
  await page.keyboard.down('Control');
  for (let i = 0; i < 3; i++) await page.mouse.wheel(0, -240);
  await page.keyboard.press('Equal');
  await page.keyboard.press('NumpadAdd');
  await page.keyboard.up('Control');
  await page.waitForTimeout(1_000);
  const zoomAfter = await zoom();

  // The accelerators of 03 §5.4: F5 and Ctrl+R (reload), Ctrl+P (print), Ctrl+F (find), Ctrl+U (view source),
  // Ctrl+Shift+I and F12 (DevTools, which the e2e build has on, so a DevTools target would appear if they worked).
  const keys = ['F5', 'Control+KeyR', 'Control+KeyP', 'Control+KeyF', 'Control+KeyU', 'Control+Shift+KeyI', 'F12'];
  for (const key of keys) {
    await page.keyboard.press(key);
    await page.waitForTimeout(300);
  }
  await page.waitForTimeout(1_500);
  const after = await page.evaluate(() => {
    const w = window as { __cdpMark?: number; __cdpPrinted?: boolean };
    return { mark: w.__cdpMark ?? 0, printed: w.__cdpPrinted === true, origin: location.origin };
  });
  const newTargets = (await targets()).filter((t) => !targetsBefore.includes(t));
  await cdp.detach();
  row(
    'W9',
    [
      `zoom (devicePixelRatio, visualViewport.scale, innerWidth) ${JSON.stringify(zoomBefore)} → ` +
        `${JSON.stringify(zoomAfter)} after Ctrl + wheel up ×3, Ctrl+= and Ctrl+numpad + (CDP input)`,
      `after ${keys.join(', ')} (CDP input): page kept its state (no reload) = ${String(after.mark === 1)}, ` +
        `beforeprint fired = ${String(after.printed)}, new targets = ${newTargets.length === 0 ? 'none' : newTargets.join(' | ')}`,
      'status bar, autofill and password save, swipe, external drop, the context menu and DevTools of a ' +
        'production build and the real keyboard: manual (03 §22.5, owner’s PC)',
    ].join('; '),
  );
  expect(zoomAfter).toEqual(zoomBefore);
  expect(after).toEqual({ mark: 1, printed: false, origin: DesktopAssetOriginWindows });
  expect(newTargets).toEqual([]);
});

test('W13: audio plays without a click (the autoplay argument reaches WebView2)', async ({ app }) => {
  const { page } = app;
  // Playwright's page.evaluate runs as a user gesture, and activation sticks to a document: the check runs as an init
  // script of the next document, which nothing activates, and the reload is CDP's (no gesture either). It plays a
  // 0.2 s tone from a blob: URL (the CSP's media-src allows blob:) and keeps an AudioContext, whose state is read later.
  // Nothing evaluates in the page before the reload either: Chromium's autoplay policy also lets a document play
  // when the same origin's previous document had been activated.
  await page.addInitScript(() => {
    const w = window as {
      __cdpAutoplay?: { activated: boolean; play: string; ctx: AudioContext | null; ctxError: string };
    };
    const result = {
      activated: navigator.userActivation.hasBeenActive,
      play: 'pending',
      ctx: null as AudioContext | null,
      ctxError: '',
    };
    w.__cdpAutoplay = result;
    try {
      result.ctx = new AudioContext();
    } catch (e) {
      result.ctxError = String(e);
    }
    const rate = 8000;
    const samples = 1600;
    const buf = new ArrayBuffer(44 + samples * 2);
    const v = new DataView(buf);
    const ascii = (at: number, s: string): void => {
      for (let i = 0; i < s.length; i++) v.setUint8(at + i, s.charCodeAt(i));
    };
    ascii(0, 'RIFF');
    v.setUint32(4, 36 + samples * 2, true);
    ascii(8, 'WAVE');
    ascii(12, 'fmt ');
    v.setUint32(16, 16, true);
    v.setUint16(20, 1, true); // PCM
    v.setUint16(22, 1, true); // mono
    v.setUint32(24, rate, true);
    v.setUint32(28, rate * 2, true);
    v.setUint16(32, 2, true);
    v.setUint16(34, 16, true);
    ascii(36, 'data');
    v.setUint32(40, samples * 2, true);
    for (let i = 0; i < samples; i++)
      v.setInt16(44 + i * 2, Math.round(3000 * Math.sin((2 * Math.PI * 440 * i) / rate)), true);
    const audio = new Audio(URL.createObjectURL(new Blob([buf], { type: 'audio/wav' })));
    audio.play().then(
      () => {
        result.play = 'played';
      },
      (e: unknown) => {
        result.play = e instanceof DOMException ? e.name : String(e);
      },
    );
  });
  await page.reload();
  type Seen = { activated: boolean; play: string; ctx: string };
  const read = (): Promise<Seen> =>
    page.evaluate(() => {
      const r = (
        window as { __cdpAutoplay?: { activated: boolean; play: string; ctx: AudioContext | null; ctxError: string } }
      ).__cdpAutoplay;
      if (r === undefined) return { activated: true, play: 'no init script ran', ctx: '' };
      return { activated: r.activated, play: r.play, ctx: r.ctx?.state ?? `none (${r.ctxError})` };
    });
  await expect.poll(async () => (await read()).play, { timeout: 15_000 }).not.toBe('pending');
  await page.waitForTimeout(500);
  const seen = await read();
  row(
    'W13',
    `without a gesture (userActivation.hasBeenActive = ${String(seen.activated)}): <audio>.play() → ${seen.play}; ` +
      `a new AudioContext is ${seen.ctx}; the user data folder is the e2e data root's (the fixture's check)`,
  );
  expect(seen.activated).toBe(false);
  expect(seen.play).toBe('played');
});

test('W16: element fullscreen fills the screen and Esc brings the window back', async ({ app }) => {
  test.skip(app.process?.pid === undefined, 'needs the app this spec started (its pid)');
  const { page } = app;
  const hwnd = await mainWindow(app.process?.pid ?? 0);
  const size = (): Promise<{ w: number; h: number; sw: number; sh: number; fs: boolean }> =>
    page.evaluate(() => ({
      w: window.innerWidth,
      h: window.innerHeight,
      sw: window.screen.width,
      sh: window.screen.height,
      fs: document.fullscreenElement !== null,
    }));
  const before = await size();
  // page.evaluate counts as a user gesture, which requestFullscreen needs. Wails' ContainsFullScreenElementChanged
  // handler then makes the window itself fullscreen (Window.SetFullscreen's native half; beta.34's fullscreenChanged).
  const entered = await page.evaluate(async () => {
    try {
      await document.documentElement.requestFullscreen();
      return 'ok';
    } catch (e) {
      return String(e);
    }
  });
  expect(entered).toBe('ok');
  await expect
    .poll(
      async () => {
        const s = await size();
        return s.fs && s.w === s.sw && s.h === s.sh;
      },
      { timeout: 10_000, message: 'the fullscreen page fills the screen' },
    )
    .toBe(true);
  const full = await size();
  // Whether the page itself sees the Esc keydown is recorded, not asserted: WebView2 may handle the key before it. On
  // a failure it says which way the key went: never reaching the page, or reaching it with fullscreen left on (Wails
  // sets AreBrowserAcceleratorKeysEnabled(false), webview_window_windows.go, which could keep WebView2's own handling
  // from ending element fullscreen).
  const readKeys = (): Promise<string[]> => page.evaluate(() => (window as { __cdpKeys?: string[] }).__cdpKeys ?? []);
  await page.evaluate(() => {
    const w = window as { __cdpKeys?: string[] };
    w.__cdpKeys = [];
    window.addEventListener('keydown', (e) => w.__cdpKeys?.push(e.key), { capture: true });
  });
  // A real Esc (SendInput, the window in the foreground): WebView2's own key handling ends element fullscreen, and
  // CDP's key events never reach it (they enter at the renderer; the first windows-desktop run showed a CDP Escape
  // leaving fullscreenElement set). The window must be in front for the key to reach it.
  const esc = await pressEscape(hwnd);
  expect(esc.foreground, 'the main window is the foreground window for the real Esc').toBe(true);
  expect(esc.sent, "SendInput took the Esc key's down and up").toBe(2);
  // Polled by hand, so the failure's message can say whether the Esc keydown reached the page (read at the end).
  let stillFull = (await size()).fs;
  for (const until = Date.now() + 10_000; stillFull && Date.now() < until;) {
    await sleep(250);
    stillFull = (await size()).fs;
  }
  const seen = await readKeys();
  expect(
    stillFull,
    `Esc leaves fullscreen: ${
      seen.includes('Escape')
        ? 'the Esc keydown reached the page and fullscreenElement stayed set'
        : 'the Esc keydown never reached the page'
    } (the page's keydowns: ${seen.length === 0 ? 'none' : seen.join(', ')})`,
  ).toBe(false);
  await expect
    .poll(
      async () => {
        const s = await size();
        return [s.w, s.h];
      },
      { timeout: 10_000, message: 'the window is back at its size' },
    )
    .toEqual([before.w, before.h]);
  const win = await windowState(hwnd);
  const keys = await readKeys();
  row(
    'W16',
    `before ${String(before.w)}×${String(before.h)} on a ${String(before.sw)}×${String(before.sh)} screen; ` +
      `fullscreen ${String(full.w)}×${String(full.h)} (fullscreenElement set); after Esc (SendInput, the window ` +
      `in the foreground) fullscreenElement null and the window back at ${String(before.w)}×${String(before.h)}, ` +
      `visible = ${String(win.visible)}; the page's keydowns: ${keys.length === 0 ? 'none' : keys.join(', ')}`,
  );
  expect(win).toEqual({ exists: true, visible: true });
});

/** Process names of the windows that Browser.OpenURL('ms-settings:…') or ('search-ms:…') would show. */
const SETTINGS_OR_SEARCH = /^(SystemSettings|ApplicationFrameHost|SearchHost|SearchApp|SearchUI|explorer)$/i;

test("W21: the runtime's own objects get 403 from the page and nothing opens; a bound method answers", async ({
  app,
}) => {
  test.skip(app.process?.pid === undefined, 'needs the app this spec started (its pid)');
  const { page } = app;
  const hwnd = await mainWindow(app.process?.pid ?? 0);
  const before = await topWindows();
  // Built as the pinned runtime builds them (@wailsio/runtime's runtime.js: the x-wails-client-id header and
  // {object, method, args}; 03 §3.2 W21's ids: Browser 9 OpenURL 0, Clipboard 1 Text 1, Window 6 Hide 11).
  const statuses = await page.evaluate(async () => {
    const send = (object: number, method: number, args?: unknown): Promise<number> =>
      fetch('/wails/runtime', {
        method: 'POST',
        headers: { 'x-wails-client-id': 'cdp', 'Content-Type': 'application/json' },
        body: JSON.stringify(args === undefined ? { object, method } : { object, method, args }),
      }).then((r) => r.status);
    return {
      openSettings: await send(9, 0, { url: 'ms-settings:display' }),
      openSearch: await send(9, 0, { url: 'search-ms:query=x' }),
      clipboardText: await send(1, 1),
      windowHide: await send(6, 11),
      // The runtime's query form (an empty body) is refused the same way.
      openSettingsByQuery: await fetch(
        `/wails/runtime?object=9&method=0&args=${encodeURIComponent(JSON.stringify({ url: 'ms-settings:display' }))}`,
        { method: 'POST', headers: { 'x-wails-client-id': 'cdp' } },
      ).then((r) => r.status),
    };
  });
  const info = await bound(page, 'App', 'Info');
  // A Settings or search window takes a moment to appear.
  await page.waitForTimeout(3_000);
  const known = new Set(before.map((w) => w.hwnd));
  const appeared = (await topWindows()).filter((w) => !known.has(w.hwnd));
  const win = await windowState(hwnd);
  const visibility = await page.evaluate(() => document.visibilityState);
  row(
    'W21',
    `from the page: ${JSON.stringify(statuses)}; App.Info → ${String(info.status)}; new top-level windows after ` +
      `3 s: ${appeared.length === 0 ? 'none' : appeared.map((w) => w.process).join(', ')}; main window visible = ` +
      `${String(win.visible)}, visibilityState = ${visibility}`,
  );
  expect(statuses).toEqual({
    openSettings: 403,
    openSearch: 403,
    clipboardText: 403,
    windowHide: 403,
    openSettingsByQuery: 403,
  });
  expect(info.status).toBe(200);
  expect(typeof (JSON.parse(info.body) as { wizardDone?: unknown }).wizardDone).toBe('boolean');
  expect(appeared.filter((w) => SETTINGS_OR_SEARCH.test(w.process))).toEqual([]);
  expect(win).toEqual({ exists: true, visible: true });
  expect(visibility).toBe('visible');
});

/**
 * Whether the app's log (its e2e data folder's, 03 §17) shows the Sharing pill kept hidden because
 * SetWindowDisplayAffinity failed (winos' pill: "the Sharing pill isn't shown" with that call's error). False when
 * there is no log to read: the caller then asserts the pill.
 */
async function pillAffinityFailed(logDir: string | null): Promise<boolean> {
  if (logDir === null) return false;
  let names: string[];
  try {
    names = await fsp.readdir(logDir);
  } catch {
    return false;
  }
  for (const name of names) {
    const text = await fsp.readFile(path.join(logDir, name), 'utf8').catch(() => '');
    if (
      text
        .split(/\r?\n/)
        .some((l) => l.includes("the Sharing pill isn't shown") && l.includes('SetWindowDisplayAffinity'))
    )
      return true;
  }
  return false;
}

/** The friend's browser opens the approval page and approves the code. */
async function approve(page: Page, url: string): Promise<void> {
  await page.goto(url);
  await expect(page.getByRole('heading', { level: 1, name: 'Link the isshoni app?' })).toBeVisible();
  const tick = page.getByRole('checkbox', { name: 'I started this myself' });
  if ((await tick.count()) > 0) await tick.check();
  await page.getByRole('button', { name: /^Link( anyway)?$/ }).click();
  await expect(page.getByRole('heading', { level: 1, name: 'Linked' })).toBeVisible();
}

test('the app end to end: a device-flow link, a fake share watched in Chrome, close to tray while live, Quit', async ({
  launchApp,
  startServer,
  asUser,
}) => {
  test.skip(Boolean(process.env[ENDPOINT_ENV]), 'starts an app of its own (its arguments, its exit code)');
  test.setTimeout(300_000);
  const server = await startServer();
  // Two people: the one who links the app (and owns the share), and a friend who watches it in Chrome. A tab of the
  // sharer's own account would hand its playback over to the app (03 §12.2), so the viewer is someone else.
  const owner = await asUser(server);
  const friend = await asUser(server);
  // closeToTray off: the close below hides the window because the app is live (03 §11.6), not because of the setting.
  const app = await launchApp({
    settings: { schema: 1, closeToTray: false, wizardDone: true },
    args: ['--server', server.url],
  });
  const { page } = app;
  const pid = app.process?.pid ?? 0;
  const host = new URL(server.url).host;

  // --server (03 §13.1): "Add server {host}?"; Add (the same host, so no confirm step), the window reloads into the
  // link step with its code. In an e2e build an http loopback server is allowed (03 §8.1).
  await expect(page.getByRole('heading', { level: 1, name: `Add server ${host}?` })).toBeVisible({
    timeout: START_TIMEOUT_MS,
  });
  await page.getByRole('button', { name: 'Add', exact: true }).click();
  const codeEl = page.getByTestId('link-code');
  await expect(codeEl).toHaveText(/^[BCDFGHJKLMNPQRSTVWXZ]{4}-[BCDFGHJKLMNPQRSTVWXZ]{4}$/, {
    timeout: LINKED_TIMEOUT_MS,
  });
  const code = (await codeEl.textContent()) ?? '';
  // The app also opened the system browser at this URL (Shell.OpenURL); the owner approves in Chrome instead.
  await approve(owner.page, `${server.url}/link?code=${code}`);
  await expect(page).toHaveURL(/\/r\/[^/]+$/, { timeout: LINKED_TIMEOUT_MS });
  const roomId = new URL(page.url()).pathname.split('/').pop() ?? '';
  const auth = await boundJSON<{ status: string; origin: string; user: { username: string } | null }>(
    page,
    'Auth',
    'State',
  );
  expect(auth).toMatchObject({ status: 'linked', origin: server.url });
  expect(auth.user?.username).toBe(owner.account.username);
  row(
    'W1',
    `end to end: the page at ${DesktopAssetOriginWindows} linked by the device flow and opened room ${roomId} on ` +
      `${server.url} (the server's CORS answers the desktop origin)`,
  );

  // The friend watches the room in Chrome.
  await friend.page.goto(`${server.url}/r/${roomId}`);
  await waitForRoom(friend.page, roomId, { timeout: LIVE_TIMEOUT_MS });

  // Share → the desktop ShareSheet → the fake pick → the first-share sheet → Go live.
  await page.getByRole('banner').getByRole('button', { name: 'Share', exact: true }).click();
  await page
    .getByRole('dialog', { name: 'Share your screen' })
    .getByRole('button', { name: 'Share', exact: true })
    .click();
  const first = page.getByRole('dialog', { name: 'Ready to go live?' });
  await expect(first.getByText(`Friends will see: ${FAKE_LABEL}`)).toBeVisible();
  await first.getByRole('button', { name: 'Go live' }).click();
  const panel = page.getByRole('region', { name: 'Your share' });
  await expect(panel.getByText(`You're live · ${FAKE_LABEL} · Auto ·`)).toBeVisible({ timeout: LIVE_TIMEOUT_MS });

  const share = await waitForState(friend.page, (s) => s.viewer.shares.find((sh) => !sh.own && sh.status === 'live'), {
    timeout: LIVE_TIMEOUT_MS,
    message: "the app's share is live on the friend's page",
  });
  const watched = await waitForStats(friend.page, (s) => (videoOf(s, share.id)?.framesDecoded ?? 0) > 10 && s, {
    timeout: MEDIA_TIMEOUT_MS,
    message: 'the friend decodes frames of the share',
  });
  await expect(panel.getByText(`You're live · ${FAKE_LABEL} · Auto · 1 watching`)).toBeVisible({
    timeout: MEDIA_TIMEOUT_MS,
  });
  // The runner has no audio device; the tone's energy is recorded, the frames are what is asserted.
  const energy = audioOf(watched, share.id)?.totalAudioEnergy ?? 0;
  const mem = await memory(pid);
  budget(
    `while sharing the fake engine's 640×360 with 1 watching (CI runner, not the budget's 1080p60): isshoni.exe ` +
      `${mem.app === null ? 'unreadable' : mib(mem.app)} private working set; its ${String(mem.webviewProcesses)} ` +
      `WebView2 processes ${
        mem.webview === null
          ? `unreadable (${String(mem.webviewRead)} of ${String(mem.webviewProcesses)} processes read)`
          : `${mib(mem.webview)} together`
      } (the budget's own case, watching one focused share and 8 thumbnails, is M-DESK-12's)`,
  );

  // Close the window while live (03 §11.6): it hides to the tray, the process lives and the share goes on. The main
  // window by its class: while live the topmost Sharing pill is the process's first visible window, which .NET's
  // MainWindowHandle would pick, and the pill swallows WM_CLOSE (03 §11.4).
  const hwnd = await mainWindow(pid);
  expect(hwnd).toMatch(/^[1-9]\d*$/);
  expect(await postClose(hwnd)).toBe(true);
  await expect.poll(async () => (await windowState(hwnd)).visible, { timeout: 10_000 }).toBe(false);
  // The Sharing pill shows while the window is hidden and the share is live: the fake engine captures borderless
  // (NoOSIndicator), so the pill is required (03 §11.4). The one runner fact that keeps it hidden by design is a
  // failed SetWindowDisplayAffinity (03 §11.4: a pill that would be captured is not shown, with a WARN in the app's
  // log); only then is it recorded instead of asserted (M-DESK-2 and M-DESK-4 check it on the owner's PC).
  const pillVisible = async (): Promise<boolean> =>
    (await processWindows(pid)).some((w) => w.cls === PILL_WINDOW_CLASS && w.visible);
  let pillShown = await pillVisible();
  for (const until = Date.now() + 10_000; !pillShown && Date.now() < until;) {
    await sleep(250);
    pillShown = await pillVisible();
  }
  const noAffinity = pillShown ? false : await pillAffinityFailed(app.logDir);
  if (!noAffinity)
    expect(pillShown, 'the Sharing pill shows while live, borderless, with the window hidden').toBe(true);
  expect((await windowState(hwnd)).exists).toBe(true);
  expect(app.process?.exitCode).toBeNull();
  expect((await boundJSON<{ state: string } | null>(page, 'Share', 'Current'))?.state).toBe('live');
  const framesAtClose = videoOf(await readStats(friend.page), share.id)?.framesDecoded ?? 0;
  await waitForStats(friend.page, (s) => (videoOf(s, share.id)?.framesDecoded ?? 0) > framesAtClose + 30 && s, {
    timeout: MEDIA_TIMEOUT_MS,
    message: 'the friend still gets frames after the window closed',
  });

  // Quit (Settings → "Quit isshoni" → "Stop sharing and quit", App.Quit(true)): the share ends for the friend and the
  // process exits 0 within 3 s (03 §11.6, §20). The request isn't awaited: the app is gone before it could answer.
  const quitId = methodId('App', 'Quit');
  const sent = Date.now();
  await page.evaluate((methodID) => {
    void fetch('/wails/runtime', {
      method: 'POST',
      headers: { 'x-wails-client-id': 'cdp', 'Content-Type': 'application/json' },
      body: JSON.stringify({ object: 0, method: 0, args: { 'call-id': 'cdp-quit', methodID, args: [true] } }),
    }).catch(() => undefined);
  }, quitId);
  const exit = await Promise.race([app.exited, sleep(10_000).then(() => null)]);
  expect(exit, 'the app exited after Quit').not.toBeNull();
  const quitMs = (exit?.at ?? 0) - sent;
  await waitForState(friend.page, (s) => !s.viewer.shares.some((sh) => sh.id === share.id) && s, {
    timeout: 10_000,
    message: "the share is gone from the friend's page after Quit",
  });
  const goneMs = Date.now() - sent;
  row(
    'close-to-tray and Quit (03 §11.6, §22.4 B)',
    `closed while live (closeToTray off; WM_CLOSE to the ${MAIN_WINDOW_CLASS} window): hidden, process alive, ` +
      `Share.Current live, the friend got 30 more frames, Sharing pill visible = ${String(pillShown)}` +
      `${noAffinity ? " (not asserted: the app's log shows SetWindowDisplayAffinity failed on this runner, so the pill stays hidden by design)" : ''}; ` +
      `Quit: exit code ${String(exit?.code)} after ${String(quitMs)} ms, the share gone at the friend within ` +
      `${String(goneMs)} ms; the friend heard ${energy > 0 ? 'the tone' : 'no tone (no audio device on the runner)'}`,
  );
  budget(`Quit while live → process exit: ${String(quitMs)} ms (≤ ${String(QUIT_BUDGET_MS)} ms)`);
  expect(exit?.code).toBe(0);
  expect(quitMs).toBeLessThanOrEqual(QUIT_BUDGET_MS);
});

/** The five WEBVIEW2_* placeholders that the pinned Wails sets in every build (cmd/isshoni-desktop: wailsWebView2Values). */
const WAILS_PLACEHOLDERS = [
  'WEBVIEW2_ADDITIONAL_BROWSER_ARGUMENTS',
  'WEBVIEW2_PIPE_FOR_SCRIPT_DEBUGGER',
  'WEBVIEW2_BROWSER_EXECUTABLE_FOLDER',
  'WEBVIEW2_USER_DATA_FOLDER',
  'WEBVIEW2_RELEASE_CHANNEL_PREFERENCE',
];

/** The production build W19 runs, or null outside CI unless ISSHONI_DESKTOP_PROD_EXE names one. */
function productionExe(): string | null {
  const given = process.env[PROD_EXE_ENV];
  if (given !== undefined && given !== '') return path.resolve(REPO_ROOT, given);
  if (process.env['CI']) return path.join(REPO_ROOT, 'dist-desktop', 'stage', 'isshoni.exe');
  return null;
}

/** Whether \\.\pipe\<name> exists. */
async function pipeExists(name: string): Promise<boolean> {
  try {
    return fs.readdirSync('\\\\.\\pipe\\').some((p) => p.toLowerCase() === name.toLowerCase());
  } catch {
    return (await ps(`[IO.File]::Exists('\\\\.\\pipe\\${name}')`)) === 'True';
  }
}

test("W19: a production build opens no debugging port or pipe, and scrubs the parent's WEBVIEW2_*", async ({
  launchApp,
}) => {
  test.skip(Boolean(process.env[ENDPOINT_ENV]), 'starts the builds itself');
  const prod = productionExe();
  test.skip(
    prod === null,
    `set ${PROD_EXE_ENV} to a production isshoni.exe: it runs on this PC's real %LOCALAPPDATA%\\isshoni under ` +
      `the real app id, so quit any installed isshoni first (else the single-instance forward ends the build at once), ` +
      `and the installed app's toast activator (HKCU CLSID LocalServer32) points at this build until the installed ` +
      `app next starts`,
  );
  if (prod === null) return;
  if (!fs.existsSync(prod)) throw new Error(`no production build at ${prod} (task desktop:package:windows stages it)`);
  test.setTimeout(240_000);
  const port = await freePort();
  const pipe = `isshoni-test-${randomBytes(6).toString('hex')}`;
  // MARKER is no WEBVIEW2_* name, so no scrub touches it: where a WebView2 browser process has it, that process got
  // the app's environment, whatever became of WEBVIEW2_X there.
  const MARKER = 'X_ISSHONI_W19';
  const vars = {
    WEBVIEW2_ADDITIONAL_BROWSER_ARGUMENTS: `--remote-debugging-port=${String(port)}`,
    WEBVIEW2_PIPE_FOR_SCRIPT_DEBUGGER: pipe,
    WEBVIEW2_X: '1',
    [MARKER]: '1',
  };
  const upperNames = (names: readonly string[]): string[] => names.map((n) => n.toUpperCase());
  const has = (names: readonly string[], name: string): boolean => upperNames(names).includes(name);
  // A block counts as read only when it holds a variable every Windows process has: a read that returned some other
  // memory, or an empty block, is no evidence either way.
  const realBlock = (e: readonly string[] | null): e is string[] =>
    e !== null && (has(e, 'SYSTEMROOT') || has(e, 'PATH'));

  // The control: the e2e build, which keeps WEBVIEW2_*, started with the same variables, still has WEBVIEW2_X in its
  // environment block, so the reader below sees what a parent passed (a scrub-less production build would look so).
  // That block is the one that matters: the WebView2 loader runs inside isshoni.exe and reads WEBVIEW2_* from it
  // when it creates the environment. Its WebView2 browser process (no --type=) is read too, and what it shows decides
  // what the production build's browser process can prove: the first windows-desktop run found WEBVIEW2_X missing
  // from the control's browser block although isshoni.exe's had it. A child made by CreateProcess inherits its
  // parent's environment unless the caller passes a block of its own, and neither Wails nor the WebView2 loader's
  // public API documents one (the runtime that starts msedgewebview2.exe is closed source), so the control's browser
  // process was expected to hold MARKER; the second run showed that it doesn't (the runtime gives it an environment of
  // its own), which is recorded rather than read into. Where MARKER is there but WEBVIEW2_X is not, the runtime
  // drops WEBVIEW2_* from its browser process. In both cases "no WEBVIEW2_X in the production build's browser process"
  // proves nothing about the scrub: that half is the owner's (03 W19's manual column, §22.5 M-DESK-13).
  const browserOf = (tree: readonly ChildProcessInfo[]): ChildProcessInfo[] =>
    webviewProcesses(tree).filter((p) => !p.commandLine.includes('--type='));
  const control = await launchApp({ env: vars });
  const controlPid = control.process?.pid ?? 0;
  const controlNames = await environmentNames(controlPid);
  const controlBrowserEnvs = await Promise.all(
    browserOf(await descendants(controlPid)).map((p) => environmentNames(p.pid)),
  );
  await control.stop();
  expect(controlNames, "the e2e build's environment can be read").not.toBeNull();
  expect(upperNames(controlNames ?? [])).toContain('WEBVIEW2_X');
  expect(upperNames(controlNames ?? [])).toContain(MARKER);
  const controlBrowserRead = controlBrowserEnvs.filter(realBlock);
  expect(
    controlBrowserRead.length,
    "the e2e build's WebView2 browser process environment can be read (a block with SYSTEMROOT or PATH)",
  ).toBeGreaterThan(0);
  // What the runtime hands its browser process (names only; the annotation is public).
  const controlBrowserX = controlBrowserRead.some((e) => has(e, 'WEBVIEW2_X'));
  const controlBrowserMarker = controlBrowserRead.some((e) => has(e, MARKER));
  const controlBrowserPort = controlBrowserRead.some((e) => has(e, PORT_ENV));
  // The second windows-desktop run (PR #56) found a readable block without MARKER: the WebView2 runtime starts its
  // browser process with an environment of its own, so nothing of isshoni.exe's block, scrubbed or not, reaches it.
  // Either outcome is recorded; the production build's browser process must show the same inheritance as the
  // control's (the read is of the same kind of process), and never WEBVIEW2_X. The scrub that matters is asserted on
  // isshoni.exe's own block below, where the WebView2 loader reads WEBVIEW2_*.
  const inheritance = !controlBrowserMarker
    ? `the WebView2 runtime starts its browser process with its own environment (the e2e build's has no ${MARKER}), ` +
      "so the browser side shows nothing about the scrub (the owner's M-DESK-13)"
    : controlBrowserX
      ? "the e2e build's WebView2 browser process inherited the app's environment and kept WEBVIEW2_X"
      : `the WebView2 runtime drops WEBVIEW2_* from its browser process (the e2e build's has ${MARKER} but not ` +
        "WEBVIEW2_X), so the production browser process shows nothing about the scrub (the owner's M-DESK-13)";

  // The production build. It has no e2e data folder: remember whether %LOCALAPPDATA%\isshoni was there before.
  const localAppData = process.env['LOCALAPPDATA'] ?? '';
  const dataRoot = path.join(localAppData, 'isshoni');
  const hadData = localAppData === '' || fs.existsSync(dataRoot);
  const { child, exited } = spawnApp(prod, [], appEnv(vars));
  try {
    const pid = child.pid ?? 0;
    const deadline = Date.now() + START_TIMEOUT_MS;
    for (;;) {
      if (child.exitCode === 0) {
        throw new Error(
          'the production build exited 0 at once: another isshoni (same single-instance id) is probably running; ' +
            'quit it (tray → Quit) and run again',
        );
      }
      if (child.exitCode !== null) throw new Error(`the production build exited (${String(child.exitCode)})`);
      const shown = (await processWindows(pid)).some((w) => w.cls === MAIN_WINDOW_CLASS && w.visible);
      if (shown && webviewProcesses(await descendants(pid)).length > 0) break;
      if (Date.now() > deadline) throw new Error('the production build showed no window with a WebView2 within 60 s');
      await sleep(500);
    }
    // The browser process opens a port or a pipe as it starts; give it the time the e2e build's port takes.
    await sleep(3_000);
    const open = await portOpen(port);
    const piped = await pipeExists(pipe);
    const tree = await descendants(pid);
    const webviews = webviewProcesses(tree);
    const leaking = tree.filter(
      (p) =>
        p.commandLine.includes('--remote-debugging-port') ||
        p.commandLine.includes(pipe) ||
        /WEBVIEW2_/i.test(p.commandLine),
    );
    const names = await environmentNames(pid);
    const left = upperNames(names ?? []).filter((n) => n.startsWith('WEBVIEW2_') && !WAILS_PLACEHOLDERS.includes(n));
    // WebView2's browser process (no --type=): its environment too, where it can be read.
    const browserProcs = browserOf(tree);
    const browserEnvs = await Promise.all(browserProcs.map((p) => environmentNames(p.pid)));
    const browserRead = browserEnvs.filter(realBlock);
    const browserX = browserRead.filter((e) => has(e, 'WEBVIEW2_X')).length;
    const browserMarker = browserRead.some((e) => has(e, MARKER));
    row(
      'W19',
      [
        `production build started with WEBVIEW2_ADDITIONAL_BROWSER_ARGUMENTS=--remote-debugging-port=<p>, ` +
          `WEBVIEW2_PIPE_FOR_SCRIPT_DEBUGGER=isshoni-test-<random>, WEBVIEW2_X=1`,
        `port open = ${String(open)}, pipe exists = ${String(piped)}`,
        `${String(webviews.length)} WebView2 processes, command lines naming the port, the pipe or WEBVIEW2_: ${String(leaking.length)}`,
        `isshoni.exe's environment: WEBVIEW2_* beyond Wails' five placeholders: ${left.length === 0 ? 'none' : left.join(', ')}`,
        `WebView2 browser processes read: ${String(browserRead.length)}/${String(browserProcs.length)}, with WEBVIEW2_X: ` +
          `${String(browserX)}, with ${MARKER}: ${String(browserMarker)}`,
        `control: the e2e build kept WEBVIEW2_X in its own block; ${String(controlBrowserRead.length)} WebView2 ` +
          `browser process read, with WEBVIEW2_X = ${String(controlBrowserX)}, ${MARKER} = ` +
          `${String(controlBrowserMarker)}, ${PORT_ENV} = ${String(controlBrowserPort)}: ${inheritance}`,
      ].join('; '),
    );
    expect(open, 'the debugging port is open').toBe(false);
    expect(piped, 'the script-debugger pipe exists').toBe(false);
    expect(leaking.map((p) => p.name)).toEqual([]);
    expect(names, "the production build's environment can be read").not.toBeNull();
    expect(left).toEqual([]);
    // Not vacuous: at least one browser process's environment was read (it is the user's own, unsandboxed process).
    expect(
      browserRead.length,
      "a WebView2 browser process's environment can be read (a block with SYSTEMROOT or PATH)",
    ).toBeGreaterThan(0);
    // The browser side shows the scrub only where the control showed inheritance with WEBVIEW2_X kept; otherwise
    // that half is M-DESK-13's. Either way the production browser process inherits exactly as the control's did.
    expect(
      browserMarker,
      `the production build's WebView2 browser process has ${MARKER} exactly when the control's has (${String(controlBrowserMarker)})`,
    ).toBe(controlBrowserMarker);
    expect(browserX, "the production build's WebView2 browser process has WEBVIEW2_X").toBe(0);
  } finally {
    await kill(child, exited);
    if (!hadData) await fsp.rm(dataRoot, { recursive: true, force: true, maxRetries: 5 }).catch(() => undefined);
  }
});
