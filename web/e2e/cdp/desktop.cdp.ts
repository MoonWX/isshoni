// The real app over CDP (M2 03 §22.4 B), the skeleton of README WA48: it starts the e2e build of isshoni.exe (or
// attaches to a running one), connects Playwright to its WebView2 and checks the Wails checklist rows that only the
// real webview can answer (03 §3.2): W1 (the asset origin), W2 (a secure context), W3 (the CSP is enforced), W5
// (top-level navigation and new windows are cancelled) and W7 (closing the window hides it). Each test also writes
// its row as an annotation ("Wails checklist"), for the PR. README DK13 (WA90's run) completes the spec: W9, W13,
// W16, W21 in the webview, a device-flow link, a fake share watched in Chrome, close-to-tray while live, Quit.
//
// Environment (web/e2e/cdp/playwright.config.ts): ISSHONI_DESKTOP_EXE (the e2e build), or ISSHONI_CDP_ENDPOINT to
// attach to a running app. Windows only. The app is started with --relaunched (so it stays this process's child),
// ISSHONI_E2E_CDP_PORT (README WA72's e2e-only port) and ISSHONI_E2E_DATA_DIR: a temporary folder that stands in for
// %LOCALAPPDATA% (03 §22.4 B), whose isshoni\settings.json keeps the app in the tray when its window closes (03
// §11.6's closeToTray, so W7 needs no live share). Setting LOCALAPPDATA would not do: the app reads its data folder
// from the shell's known folder, never from the environment (cmd/isshoni-desktop/platform_windows.go), so the e2e
// build takes it from its own e2e-only variable (README WA72, flavor_e2e.go), as it takes the port. Once the endpoint
// answers, the fixture checks that the app wrote into that folder (its log file or its WebView2 profile) and fails
// before any test runs otherwise: an e2e build that ignored it would be using the owner's real servers, settings and
// WebView2 profile.
import { spawn, execFile, type ChildProcess } from 'node:child_process';
import fsp from 'node:fs/promises';
import net from 'node:net';
import os from 'node:os';
import path from 'node:path';
import { promisify } from 'node:util';

import { chromium, expect, test as base, type Browser, type Page } from '@playwright/test';

import { DesktopAssetOriginWindows } from '../../src/protocol/types.gen.ts';

const execFileAsync = promisify(execFile);

const EXE_ENV = 'ISSHONI_DESKTOP_EXE';
const ENDPOINT_ENV = 'ISSHONI_CDP_ENDPOINT';
/** The e2e build's own variable for its WebView2 debugging port (README WA72). */
const PORT_ENV = 'ISSHONI_E2E_CDP_PORT';
/** The e2e build's own variable for the folder that replaces %LOCALAPPDATA% (README WA72). */
const DATA_DIR_ENV = 'ISSHONI_E2E_DATA_DIR';
/** WebView2's first start (a new user data folder) takes a few seconds. */
const START_TIMEOUT_MS = 60_000;

interface RealApp {
  readonly browser: Browser;
  /** The main window's page at the asset origin. */
  readonly page: Page;
  /** The app's process, or null when the spec attached to a running app. */
  readonly process: ChildProcess | null;
}

/** Records a checklist row for the PR's "Wails checklist" (03 §3.2). */
function row(id: string, result: string): void {
  base.info().annotations.push({ type: 'Wails checklist', description: `${id}: ${result}` });
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
    await new Promise((r) => setTimeout(r, 250));
  }
}

const test = base.extend<{ app: RealApp }>({
  // eslint-disable-next-line no-empty-pattern -- Playwright reads the fixture names from this pattern
  app: async ({}, use) => {
    const attach = process.env[ENDPOINT_ENV];
    let child: ChildProcess | null = null;
    let dataDir: string | null = null;
    let endpoint: string;
    if (attach !== undefined && attach !== '') {
      endpoint = attach.replace(/\/$/, '');
    } else {
      const exe = process.env[EXE_ENV];
      if (exe === undefined || exe === '') throw new Error(`set ${EXE_ENV} or ${ENDPOINT_ENV}`);
      const port = await freePort();
      endpoint = `http://127.0.0.1:${String(port)}`;
      dataDir = await fsp.mkdtemp(path.join(os.tmpdir(), 'isshoni-cdp-'));
      await fsp.mkdir(path.join(dataDir, 'isshoni'), { recursive: true });
      await fsp.writeFile(
        path.join(dataDir, 'isshoni', 'settings.json'),
        JSON.stringify({ schema: 1, closeToTray: true, wizardDone: true }),
      );
      child = spawn(path.resolve(exe), ['--relaunched'], {
        env: { ...process.env, [DATA_DIR_ENV]: dataDir, [PORT_ENV]: String(port) },
        stdio: 'ignore',
      });
    }
    try {
      await waitForEndpoint(endpoint, child);
      if (dataDir !== null) await assertUsedDataDir(dataDir);
      const browser = await chromium.connectOverCDP(endpoint);
      const pages = (): Page[] => browser.contexts().flatMap((c) => c.pages());
      await expect
        .poll(() => pages().some((p) => p.url().startsWith(DesktopAssetOriginWindows)), { timeout: START_TIMEOUT_MS })
        .toBe(true);
      const page = pages().find((p) => p.url().startsWith(DesktopAssetOriginWindows));
      if (page === undefined) throw new Error('no page at the asset origin');
      await page.waitForLoadState('domcontentloaded');
      await use({ browser, page, process: child });
      await browser.close(); // disconnects; the app keeps running
    } finally {
      if (child?.pid !== undefined && child.exitCode === null) {
        await execFileAsync('taskkill', ['/PID', String(child.pid), '/T', '/F']).catch(() => undefined);
      }
      if (dataDir !== null) await fsp.rm(dataDir, { recursive: true, force: true, maxRetries: 5 });
    }
  },
});

test.skip(process.platform !== 'win32' && !process.env[ENDPOINT_ENV], 'the real app runs on Windows');

test('W1, W2: the page is at the asset origin, in a secure context', async ({ app }) => {
  const [origin, secure] = await app.page.evaluate(() => [location.origin, isSecureContext] as const);
  // W1's second half, "this value is in protocol.DesktopOrigins()", needs no webview: internal/protocol's
  // TestDesktopOrigins asserts DesktopOrigins()[0] == DesktopAssetOriginWindows. The skeleton has no server whose CORS
  // could show it end to end (README DK13's run links a server from this webview).
  row(
    'W1',
    `location.origin = ${origin}; DesktopOrigins membership: checked by internal/protocol's unit test ` +
      `(TestDesktopOrigins)`,
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
  const before = app.browser.contexts().flatMap((c) => c.pages()).length;
  // The SPA never does this (03 §5.5 layer 1); Go's NavigationStarting cancels it (layer 2).
  await page.evaluate(() => {
    location.href = 'https://example.com/';
  });
  await page.waitForTimeout(1_500);
  const origin = await page.evaluate(() => location.origin);
  // The native window.open (DesktopPlatform replaces window.open itself): NewWindowRequested cancels it.
  const opened = await page.evaluate(() => Window.prototype.open.call(window, 'https://example.com/w') === null);
  await page.waitForTimeout(1_000);
  const after = app.browser.contexts().flatMap((c) => c.pages()).length;
  row(
    'W5',
    `after location.href: ${origin}; native window.open returned null: ${String(opened)}; pages ${String(
      before,
    )} → ${String(after)}`,
  );
  expect(origin).toBe(DesktopAssetOriginWindows);
  expect(after).toBe(before);
});

test('W7: closing the window hides it; the app keeps running', async ({ app }) => {
  test.skip(app.process === null, 'needs the app this spec started (its pid)');
  const pid = String(app.process?.pid);
  // WM_CLOSE to the main window, as its close button sends it.
  await execFileAsync('powershell.exe', [
    '-NoProfile',
    '-NonInteractive',
    '-Command',
    `$null = (Get-Process -Id ${pid}).CloseMainWindow()`,
  ]);
  await expect.poll(() => app.page.evaluate(() => document.visibilityState), { timeout: 10_000 }).toBe('hidden');
  const alive = app.process?.exitCode === null;
  row('W7', `after WM_CLOSE: visibilityState hidden, process alive = ${String(alive)}`);
  expect(alive).toBe(true);
});
