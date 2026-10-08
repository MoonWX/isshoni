import { render, screen } from '@testing-library/react';
import { useTranslation } from 'react-i18next';
import { describe, expect, it } from 'vitest';

import en from './en.json';
import { addMessages, DEFAULT_LANGUAGE, i18n, initI18n, type Messages } from './index';

/** The lazy namespaces' files (lazy/<ns>.en.json), by namespace. */
const lazyFiles = Object.fromEntries(
  Object.entries(import.meta.glob<Messages>('./lazy/*.en.json', { eager: true, import: 'default' })).map(
    ([file, messages]) => [file.replace(/^\.\/lazy\/(\w+)\.en\.json$/, '$1'), messages],
  ),
);

describe('initI18n', () => {
  it('is ready as soon as it returns (bundled catalog, no async init)', () => {
    // src/test/setup.ts already called it; a second call returns the same, initialized instance.
    const instance = initI18n();
    expect(instance).toBe(i18n);
    expect(instance.isInitialized).toBe(true);
    expect(instance.t('common.appName')).toBe('isshoni');
  });

  it('sets <html lang>', () => {
    document.documentElement.lang = '';
    initI18n();
    expect(document.documentElement.lang).toBe(DEFAULT_LANGUAGE);
  });

  it('resolves plural keys by suffix and does not HTML-escape (React does)', () => {
    i18n.addResourceBundle(DEFAULT_LANGUAGE, 'itest', {
      watching_one: '{{count}} watching',
      watching_other: '{{count}} watching',
      share: "<b>{{name}}</b>'s window",
    });
    const t = i18n.getFixedT(DEFAULT_LANGUAGE, 'itest');
    expect(t('watching', { count: 1 })).toBe('1 watching');
    expect(t('watching', { count: 3 })).toBe('3 watching');
    expect(t('share', { name: 'a&b' })).toBe("<b>a&b</b>'s window");
  });

  it('treats ":" as part of a key, not a namespace separator', () => {
    expect(i18n.t('common:appName')).toBe('common:appName');
  });
});

describe('initI18n in tests', () => {
  it('loads every lazy namespace up front: a test renders a page without its folder’s entry module', () => {
    expect(Object.keys(lazyFiles).sort()).toEqual(['account', 'admin', 'setup']);
    expect(i18n.t('admin.title')).toBe('Admin');
    expect(i18n.t('account.back')).toBe('Back to isshoni');
    expect(i18n.t('setup.steps.label')).toBe('Setup');
  });
});

describe('addMessages', () => {
  it('adds messages under their full keys and leaves the rest of the catalog alone', () => {
    expect(i18n.exists('itestAdd.hello')).toBe(false);
    addMessages({ itestAdd: { hello: 'Hello', nested: { one: 'One' } } });
    expect(i18n.t('itestAdd.hello')).toBe('Hello');
    expect(i18n.t('itestAdd.nested.one')).toBe('One');
    expect(i18n.t('common.appName')).toBe('isshoni');
  });

  it('merges into a namespace that is there, replacing the keys it brings', () => {
    addMessages({ itestMerge: { kept: 'Kept', nested: { one: 'One' }, text: 'Old' } });
    addMessages({ itestMerge: { nested: { two: 'Two' }, text: 'New' } });
    expect(i18n.t('itestMerge.kept')).toBe('Kept');
    expect(i18n.t('itestMerge.nested.one')).toBe('One');
    expect(i18n.t('itestMerge.nested.two')).toBe('Two');
    expect(i18n.t('itestMerge.text')).toBe('New');
  });

  it('adding the same messages again changes nothing', () => {
    const admin = lazyFiles['admin'];
    expect(admin).toBeDefined();
    const before = JSON.stringify(i18n.getResourceBundle(DEFAULT_LANGUAGE, 'translation'));
    if (admin) addMessages(admin);
    expect(JSON.stringify(i18n.getResourceBundle(DEFAULT_LANGUAGE, 'translation'))).toBe(before);
  });
});

describe('the catalog files', () => {
  const files: [string, unknown][] = [
    ['en.json', en],
    ...Object.entries(lazyFiles).map(([ns, m]) => [`lazy/${ns}.en.json`, m] as [string, unknown]),
  ];

  it.each(files)('%s has only non-empty strings as leaves', (_file, catalog) => {
    const bad: string[] = [];
    const walk = (node: unknown, path: string): void => {
      if (typeof node === 'string') {
        if (node.trim() === '') bad.push(path);
      } else if (node !== null && typeof node === 'object' && !Array.isArray(node)) {
        for (const [k, v] of Object.entries(node)) walk(v, path ? `${path}.${k}` : k);
      } else {
        bad.push(path);
      }
    };
    walk(catalog, '');
    expect(bad).toEqual([]);
  });

  it('a lazy file holds its own namespace, which en.json leaves to it', () => {
    for (const [ns, messages] of Object.entries(lazyFiles)) {
      expect(Object.keys(messages)).toEqual([ns]);
      expect(Object.keys(en)).not.toContain(ns);
    }
  });
});

describe('react-i18next with the Vitest setup file', () => {
  function Title() {
    const { t } = useTranslation();
    return <h1>{t('common.appName')}</h1>;
  }

  it('renders catalog strings and has the jest-dom matchers', () => {
    render(<Title />);
    expect(screen.getByRole('heading', { level: 1 })).toHaveTextContent('isshoni');
    expect(screen.getByText('isshoni')).toBeInTheDocument();
  });
});
