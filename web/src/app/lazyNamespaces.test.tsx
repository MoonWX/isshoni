// A lazy route and its namespace (05 §5, §16.5): the page's texts come with its folder's chunk, and the router has
// the page only once that chunk has run, so the page never shows a bare key.
//
// The app's real routes over the real folders, as a build runs them: the mode is `production`, where the catalog
// starts with en.json alone (in Vitest's own mode every namespace is there from the start, and this could not be
// seen). Each case has its own i18next instance and its own copy of the app's modules.
import { cleanup, screen } from '@testing-library/react';
import type * as I18next from 'i18next';
import { afterEach, describe, expect, it, vi } from 'vitest';

/** The app as a production page load has it: a fresh catalog, and the test helpers over the same modules. */
async function productionApp() {
  vi.resetModules();
  vi.stubEnv('MODE', 'production');
  vi.doMock('i18next', async (importOriginal) => {
    const actual = await importOriginal<typeof I18next>();
    return { ...actual, default: actual.default.createInstance() };
  });
  const { initI18n } = await import('../i18n');
  const i18n = initI18n();
  const { renderRoute } = await import('../test/render');
  const { setConsoleLevel } = await import('../lib/log');
  // Outside the `test` mode the log goes to the console.
  setConsoleLevel('off');
  return { i18n, renderRoute };
}

/** Everything the page shows from now on, one entry per change of the document. */
function watchText(): { seen: string[]; stop: () => void } {
  const seen: string[] = [];
  const observer = new MutationObserver(() => {
    seen.push(document.body.textContent);
  });
  observer.observe(document.body, { childList: true, subtree: true, characterData: true });
  return {
    seen,
    stop: () => {
      observer.disconnect();
    },
  };
}

afterEach(() => {
  cleanup();
  vi.doUnmock('i18next');
  vi.unstubAllEnvs();
  vi.resetModules();
});

describe('a lazy route in a production build', () => {
  it.each([
    ['/download', 'account', 'account.download.title', 'Desktop app'],
    ['/setup', 'setup', 'setup.done.title', 'This server is already set up'],
  ])('%s: the %s namespace arrives with the page, and no bare key is ever shown', async (path, ns, key, heading) => {
    const { i18n, renderRoute } = await productionApp();
    // Not in the main bundle's catalog: a page that rendered now would show the key itself.
    expect(i18n.exists(key)).toBe(false);
    expect(i18n.t(key)).toBe(key);

    const text = watchText();
    try {
      renderRoute({ path });
      expect(await screen.findByRole('heading', { level: 1, name: heading })).toBeInTheDocument();
    } finally {
      text.stop();
    }
    expect(i18n.exists(key)).toBe(true);
    // From the spinner to the page, nothing on the way had a key of that namespace in it.
    expect(text.seen.length).toBeGreaterThan(0);
    expect(text.seen.filter((shown) => shown.includes(`${ns}.`))).toEqual([]);
  });

  it('a page of the main bundle’s namespaces needs none of the lazy ones', async () => {
    const { i18n, renderRoute } = await productionApp();
    renderRoute({ path: '/pending' });
    expect(await screen.findByRole('heading', { level: 1 })).toBeInTheDocument();
    for (const key of ['admin.title', 'account.back', 'setup.steps.label']) expect(i18n.exists(key)).toBe(false);
  });
});
