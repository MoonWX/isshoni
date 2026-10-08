// The lazy namespaces at run time (05 §16.5): in a build (and in dev) the catalog starts with en.json alone, and a
// lazy namespace is there once a module that imports i18n/lazy/<ns>.ts has run, which each page folder's entry
// module does for the namespaces its pages use. The other tests never see this: Vitest's mode is `test`, where
// initI18n() loads every namespace up front. Here each case gets its own i18next instance and its own copy of the
// modules, and sets the mode itself.
import type * as I18next from 'i18next';
import { afterEach, describe, expect, it, vi } from 'vitest';

import type * as I18n from './index';

/** A fresh copy of ./index over a fresh i18next instance, as the first import of a page load finds it. */
async function freshI18n(mode: 'production' | 'development' | 'test'): Promise<typeof I18n> {
  vi.resetModules();
  vi.stubEnv('MODE', mode);
  vi.doMock('i18next', async (importOriginal) => {
    const actual = await importOriginal<typeof I18next>();
    return { ...actual, default: actual.default.createInstance() };
  });
  return import('./index');
}

afterEach(() => {
  vi.doUnmock('i18next');
  vi.unstubAllEnvs();
  vi.resetModules();
});

describe.each(['production', 'development'] as const)('the catalog in %s mode', (mode) => {
  it('starts with the main bundle’s namespaces only', async () => {
    const { initI18n } = await freshI18n(mode);
    const i18n = initI18n();
    expect(i18n.t('common.appName')).toBe('isshoni');
    expect(i18n.exists('errors.unknown')).toBe(true);
    for (const key of ['admin.title', 'account.back', 'setup.steps.label']) {
      expect(i18n.exists(key)).toBe(false);
      // What a page would show without its namespace: the bare key.
      expect(i18n.t(key)).toBe(key);
    }
  });

  it.each([
    ['./lazy/admin', 'admin.title', 'Admin'],
    ['./lazy/account', 'account.back', 'Back to isshoni'],
    ['./lazy/setup', 'setup.steps.label', 'Setup'],
  ])('%s adds its namespace when it runs', async (module, key, text) => {
    const { initI18n } = await freshI18n(mode);
    const i18n = initI18n();
    expect(i18n.exists(key)).toBe(false);
    // A variable specifier on purpose: these imports are the test's, not the page folders'.
    await import(/* @vite-ignore */ module);
    expect(i18n.t(key)).toBe(text);
    // The main bundle's namespaces are as they were.
    expect(i18n.t('common.appName')).toBe('isshoni');
  });
});

describe('a lazy page folder, in production mode', () => {
  // The folder's entry module, as the router loads it, and a key its pages show.
  it.each([
    ['admin', () => import('../admin/index'), ['admin.title', 'admin.nav.users']],
    ['account', () => import('../account/index'), ['account.back', 'account.title']],
    ['download', () => import('../download/index'), ['account.download.title', 'account.back']],
    ['setup', () => import('../setup/index'), ['setup.steps.label', 'setup.admin.title']],
  ])('%s/ has its namespace once its entry module has run', async (_folder, load, keys) => {
    const { initI18n } = await freshI18n('production');
    const i18n = initI18n();
    for (const key of keys) expect(i18n.exists(key)).toBe(false);
    await load();
    for (const key of keys) {
      expect(i18n.exists(key)).toBe(true);
      expect(i18n.t(key)).not.toBe(key);
    }
  });

  it('leaves the other lazy namespaces out', async () => {
    const { initI18n } = await freshI18n('production');
    const i18n = initI18n();
    await import('../download/index');
    expect(i18n.exists('account.back')).toBe(true);
    expect(i18n.exists('admin.title')).toBe(false);
    expect(i18n.exists('setup.steps.label')).toBe(false);
  });
});

describe('addMessages', () => {
  it('keeps what is added before initI18n() and puts it in the catalog then', async () => {
    const { addMessages, initI18n, i18n } = await freshI18n('production');
    expect(i18n.isInitialized).toBeFalsy();
    addMessages({ early: { hello: 'Hello', nested: { deep: 'Deep' } } });
    addMessages({ other: { one: 'One' } });
    initI18n();
    expect(i18n.t('early.hello')).toBe('Hello');
    expect(i18n.t('early.nested.deep')).toBe('Deep');
    expect(i18n.t('other.one')).toBe('One');
    expect(i18n.t('common.appName')).toBe('isshoni');
  });
});

describe('the catalog in test mode', () => {
  it('has every lazy namespace from the start, for tests that render a page by itself', async () => {
    const { initI18n } = await freshI18n('test');
    const i18n = initI18n();
    expect(i18n.t('admin.title')).toBe('Admin');
    expect(i18n.t('account.back')).toBe('Back to isshoni');
    expect(i18n.t('setup.steps.label')).toBe('Setup');
  });
});
