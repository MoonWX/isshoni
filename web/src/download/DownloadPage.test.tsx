// /download (docs/m2/06-windows-release.md §7.5, §7.6, §16 "Unit"), through the app's own routes: the three numbered
// Windows steps, the warning lines and the requirements with their project-site anchors, the PowerShell command,
// "Open isshoni" (after the download, with ?room=, hidden without an https link), a development build's note, the
// order of the platforms, and links that leave the SPA in a new tab or through the platform.
import { fireEvent, screen, waitFor, within } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';

import { createAppRoutes, defaultSources } from '../app/router';
import type { Platform } from '../platform/types';
import type { ClientOS } from '../protocol/types.gen';
import { infoFixture } from '../test/msw';
import { createTestPlatform } from '../test/platform';
import { createTestServices, renderRoute } from '../test/render';
import * as folder from './index';

const HEADING = 'Desktop app';
const ORIGIN = 'https://watch.example.com';
const ROOM = 'k3j9x2m4p7q8';
const INSTALLER =
  'https://github.com/MoonWX/isshoni/releases/download/v0.2.0/isshoni-desktop_0.2.0_windows_amd64_setup.exe';
const LATEST = 'https://github.com/MoonWX/isshoni/releases/latest';
const SITE = 'https://moonwx.github.io/isshoni/';

interface Options {
  version?: string;
  publicUrl?: string;
  search?: string;
  os?: ClientOS;
  platform?: Partial<Platform>;
}

/** /download for a server of `version` at `publicUrl`, in a browser that reports `os`. */
async function renderPage({
  version = '0.2.0',
  publicUrl = ORIGIN,
  search = '',
  os = 'windows',
  platform = {},
}: Options = {}) {
  const base = createTestPlatform(platform);
  const services = createTestServices({
    platform: { ...base, client: { ...base.client, os } },
    info: infoFixture({ server: { name: 'Test server', version, publicUrl } }),
  });
  const view = renderRoute({ path: `/download${search}`, services });
  await screen.findByRole('heading', { level: 1, name: HEADING });
  return view;
}

/** The Windows section, by its heading. */
function windows(): HTMLElement {
  return screen.getByRole('region', { name: 'Windows' });
}

/** The numbered steps, as their list items. */
function steps(): HTMLElement[] {
  return within(windows())
    .getAllByRole('listitem')
    .filter((li) => li.parentElement?.tagName === 'OL');
}

/** Numbered step `n` (1–3). */
function step(n: 1 | 2 | 3): HTMLElement {
  const li = steps()[n - 1];
  if (li === undefined) throw new Error(`no step ${String(n)}`);
  return li;
}

/** A list item of `list`, by its index. */
function item(list: HTMLElement[], i: number): HTMLElement {
  const li = list[i];
  if (li === undefined) throw new Error(`no item ${String(i)}`);
  return li;
}

/** Puts a clipboard on the navigator, as the browser has it (or not). */
function stubClipboard(value: unknown): void {
  Object.defineProperty(navigator, 'clipboard', { configurable: true, value });
}

/** Stops a link's navigation at the document (jsdom can't follow one) and says whether the page stopped it first. */
function click(link: HTMLElement): boolean {
  let preventedByPage = false;
  const lastWord = (e: Event): void => {
    preventedByPage = e.defaultPrevented;
    e.preventDefault();
  };
  document.addEventListener('click', lastWord);
  fireEvent(link, new MouseEvent('click', { bubbles: true, cancelable: true }));
  document.removeEventListener('click', lastWord);
  return preventedByPage;
}

/** A link that leaves the SPA: a new tab, without opener or referrer (05 §20). */
function expectExternal(link: HTMLElement, href: string): void {
  expect(link).toHaveAttribute('href', href);
  expect(link).toHaveAttribute('target', '_blank');
  expect(link).toHaveAttribute('rel', 'noopener noreferrer');
}

afterEach(() => {
  Reflect.deleteProperty(navigator, 'clipboard');
});

describe('DownloadPage', () => {
  it('is the download folder of the build, exported by name from its index', async () => {
    expect(Object.keys(folder)).toEqual(['DownloadPage']);
    const load = defaultSources().lazy.download;
    expect(load).toBeTypeOf('function');
    expect(await load?.()).toHaveProperty('DownloadPage', folder.DownloadPage);
    // Without the folder the same route is the router's stand-in: the page comes from this folder.
    renderRoute({ path: '/download', routes: createAppRoutes({ lazy: {}, eager: {} }) });
    expect(await screen.findByRole('heading', { name: 'Not in this build yet' })).toBeInTheDocument();
    expect(screen.queryByRole('heading', { name: HEADING })).not.toBeInTheDocument();
  });

  it('shows the three numbered Windows steps, which stay the whole time', async () => {
    await renderPage();
    expect(steps()).toHaveLength(3);
    const [download, run, open] = [step(1), step(2), step(3)];
    expect(within(download).getByRole('heading', { level: 3 })).toHaveTextContent('Step 1 of 3: Download');
    expect(within(run).getByRole('heading', { level: 3 })).toHaveTextContent('Step 2 of 3: Run it');
    expect(within(open).getByRole('heading', { level: 3 })).toHaveTextContent('Step 3 of 3: Open isshoni');

    // Step 1: the exact installer of the server's own release, and its release page.
    const installer = within(download).getByRole('link', { name: 'Download for Windows' });
    expectExternal(installer, INSTALLER);
    expect(within(download).getByText(/Version 0\.2\.0/)).toBeInTheDocument();
    expectExternal(
      within(download).getByRole('link', { name: 'Release notes' }),
      'https://github.com/MoonWX/isshoni/releases/tag/v0.2.0',
    );
    expect(screen.queryByText(/development build/)).not.toBeInTheDocument();

    // Step 3 waits for the download: a disabled button that says why.
    const waiting = within(open).getByRole('button', { name: 'Open isshoni' });
    expect(waiting).toBeDisabled();
    expect(waiting).toHaveAccessibleDescription('after the install');
    expect(within(open).queryByRole('link', { name: 'Open isshoni' })).not.toBeInTheDocument();

    click(installer);
    const openApp = await within(open).findByRole('link', { name: 'Open isshoni' });
    expect(openApp).toHaveAttribute('href', 'isshoni://watch.example.com/');
    // The app's protocol handler, not a tab.
    expect(openApp).not.toHaveAttribute('target');
    expect(within(open).queryByRole('button', { name: 'Open isshoni' })).not.toBeInTheDocument();

    // The steps are still there, as they were.
    expect(steps()).toHaveLength(3);
    expect(within(step(1)).getByRole('link', { name: 'Download for Windows' })).toBe(installer);
  });

  it('opens step 3 after a middle click on the installer, which downloads it without a click', async () => {
    await renderPage();
    const installer = within(step(1)).getByRole('link', { name: 'Download for Windows' });
    // A right-button auxclick isn't a download.
    fireEvent(installer, new MouseEvent('auxclick', { button: 2, bubbles: true }));
    expect(within(step(3)).getByRole('button', { name: 'Open isshoni' })).toBeDisabled();
    fireEvent(installer, new MouseEvent('auxclick', { button: 1, bubbles: true }));
    expect(await within(step(3)).findByRole('link', { name: 'Open isshoni' })).toHaveAttribute(
      'href',
      'isshoni://watch.example.com/',
    );
  });

  it('opens step 3 after the context menu on the installer ("Save link as…")', async () => {
    await renderPage();
    expect(within(step(3)).getByRole('button', { name: 'Open isshoni' })).toBeDisabled();
    fireEvent.contextMenu(within(step(1)).getByRole('link', { name: 'Download for Windows' }));
    expect(await within(step(3)).findByRole('link', { name: 'Open isshoni' })).toBeInTheDocument();
  });

  it('shows the three warning lines in the order the friend meets them, with their project-site anchors', async () => {
    await renderPage();
    const lines = within(step(2)).getAllByRole('listitem');
    expect(lines).toHaveLength(3);
    const [browser, smartScreen, smartAppControl] = [item(lines, 0), item(lines, 1), item(lines, 2)];
    expect(browser).toHaveTextContent(
      "Your browser may say the file isn't commonly downloaded: choose Keep (Edge: … → Keep → Show more → Keep anyway).",
    );
    expectExternal(within(browser).getByRole('link'), `${SITE}download#windows-smartscreen`);
    expect(smartScreen).toHaveTextContent(
      'Windows may warn that the app is unrecognized: choose More info → Run anyway.',
    );
    expectExternal(within(smartScreen).getByRole('link'), `${SITE}download#windows-smartscreen`);
    expect(smartAppControl).toHaveTextContent(
      "If Windows says Smart App Control blocked it, this version can't run on your PC yet: share from Chrome or Edge instead.",
    );
    expectExternal(within(smartAppControl).getByRole('link'), `${SITE}download#windows-smart-app-control`);
  });

  it('lists the requirements, linking the troubleshooting anchors', async () => {
    await renderPage();
    expect(within(windows()).getByText('Windows 10 22H2 or later, 64-bit')).toBeInTheDocument();
    expectExternal(
      within(windows()).getByRole('link', {
        name: /A current GPU driver \(NVIDIA 531\.61 or newer for NVIDIA encoding\)/,
      }),
      `${SITE}troubleshooting#desktop-gpu-driver`,
    );
    expectExternal(
      within(windows()).getByRole('link', { name: /Windows N editions: install the Media Feature Pack/ }),
      `${SITE}troubleshooting#desktop-media-feature-pack`,
    );
  });

  it('shows the PowerShell command for this server and copies it on a click', async () => {
    const writeText = vi.fn<(text: string) => Promise<void>>(() => Promise.resolve());
    stubClipboard({ writeText });
    await renderPage();
    const block = screen.getByRole('region', { name: 'Or install with PowerShell' });
    expect(
      within(block).getByText(
        'No warning, and the app opens set up for this server; you approve it once in this browser.',
      ),
    ).toBeInTheDocument();
    const command = within(block).getByText('irm https://watch.example.com/install-desktop.ps1 | iex');
    expect(command.tagName).toBe('CODE');
    expectExternal(
      within(block).getByRole('link', { name: /What the command does/ }),
      `${SITE}download#windows-powershell`,
    );

    fireEvent.click(within(block).getByRole('button', { name: 'Copy' }));
    expect(await within(block).findByRole('button', { name: 'Copied' })).toBeInTheDocument();
    expect(writeText.mock.calls).toEqual([['irm https://watch.example.com/install-desktop.ps1 | iex']]);
  });

  it('selects the command when the browser refuses to copy', async () => {
    stubClipboard({ writeText: () => Promise.reject(new DOMException('denied', 'NotAllowedError')) });
    await renderPage();
    const block = screen.getByRole('region', { name: 'Or install with PowerShell' });
    fireEvent.click(within(block).getByRole('button', { name: 'Copy' }));
    expect(await within(block).findByRole('status')).toHaveTextContent(/didn't allow copying/);
    await waitFor(() => {
      expect(globalThis.getSelection()?.toString()).toBe('irm https://watch.example.com/install-desktop.ps1 | iex');
    });
  });

  it('opens the app with a valid ?room=, and with the plain link for an invalid one', async () => {
    const openHref = async (search: string): Promise<string | null> => {
      const view = await renderPage({ search });
      click(within(step(1)).getByRole('link', { name: 'Download for Windows' }));
      const href = (await within(step(3)).findByRole('link', { name: 'Open isshoni' })).getAttribute('href');
      view.unmount();
      return href;
    };
    expect(await openHref(`?room=${ROOM}`)).toBe(`isshoni://watch.example.com/r/${ROOM}`);
    expect(await openHref('?room=no.such.room')).toBe('isshoni://watch.example.com/');
    expect(await openHref(`?room=${'a'.repeat(65)}`)).toBe('isshoni://watch.example.com/');
    expect(await openHref(`?room=${ROOM}&room=${ROOM}`)).toBe('isshoni://watch.example.com/');
  });

  it('has no "Open isshoni" for a server on http://localhost', async () => {
    await renderPage({ version: '0.0.0-dev', publicUrl: 'http://localhost', search: `?room=${ROOM}` });
    const open = step(3);
    expect(within(open).queryByRole('button', { name: 'Open isshoni' })).not.toBeInTheDocument();
    click(within(step(1)).getByRole('link', { name: 'Download for Windows' }));
    expect(within(open).queryByRole('link', { name: 'Open isshoni' })).not.toBeInTheDocument();
    expect(within(open).getByText('Start isshoni from the Start menu and add this server.')).toBeInTheDocument();
    expect(screen.getByText('irm http://localhost/install-desktop.ps1 | iex')).toBeInTheDocument();
  });

  it('links a development build to the latest release', async () => {
    await renderPage({ version: '0.0.0-e2e.local' });
    expect(
      within(windows()).getByText(
        'This server is a development build (0.0.0-e2e.local); the app comes from the latest release.',
      ),
    ).toBeInTheDocument();
    expectExternal(within(windows()).getByRole('link', { name: /Latest release/ }), LATEST);
    expectExternal(within(step(1)).getByRole('link', { name: 'Download for Windows' }), LATEST);
    expect(within(step(1)).queryByRole('link', { name: 'Release notes' })).not.toBeInTheDocument();
    // The command and the app link are this server's all the same.
    expect(screen.getByText('irm https://watch.example.com/install-desktop.ps1 | iex')).toBeInTheDocument();
  });

  it.each([
    ['windows', ['Windows', 'macOS', 'Linux']],
    ['macos', ['macOS', 'Windows', 'Linux']],
    ['linux', ['Linux', 'Windows', 'macOS']],
    ['android', ['Windows', 'macOS', 'Linux']],
    ['other', ['Windows', 'macOS', 'Linux']],
  ] as const)('orders the platforms for a browser on %s', async (os, order) => {
    await renderPage({ os });
    expect(screen.getAllByRole('heading', { level: 2 }).map((h) => h.textContent)).toEqual(order);
    expect(screen.getByRole('region', { name: 'macOS' })).toHaveTextContent('Coming (M3).');
    expect(screen.getByRole('region', { name: 'Linux' })).toHaveTextContent('Coming (M4).');
  });

  it("links the project site's download page and back to the app", async () => {
    await renderPage();
    const site = screen.getByRole('link', { name: /All downloads on the project site/ });
    expectExternal(site, `${SITE}download`);
    expect(screen.getByRole('main')).toContainElement(site);
    expect(screen.getByRole('link', { name: 'Back to isshoni' })).toHaveAttribute('href', '/');
  });

  it('leaves the links to the browser in a tab, and to the platform anywhere else', async () => {
    const openExternal = vi.fn<(url: string) => void>();
    const browser = await renderPage({ platform: { openExternal } });
    expect(click(screen.getByRole('link', { name: /All downloads on the project site/ }))).toBe(false);
    expect(openExternal).not.toHaveBeenCalled();
    browser.unmount();

    // Outside a tab (RequireBrowser keeps the desktop app from this page, 03 §7.5) the system browser opens them.
    await renderPage({ platform: { kind: 'desktop', openExternal } });
    expect(click(screen.getByRole('link', { name: /All downloads on the project site/ }))).toBe(true);
    expect(click(within(step(1)).getByRole('link', { name: 'Download for Windows' }))).toBe(true);
    expect(openExternal.mock.calls).toEqual([[`${SITE}download`], [INSTALLER]]);
  });
});
