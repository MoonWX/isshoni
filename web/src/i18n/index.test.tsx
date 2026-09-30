import { render, screen } from '@testing-library/react';
import { useTranslation } from 'react-i18next';
import { describe, expect, it } from 'vitest';

import en from './en.json';
import { DEFAULT_LANGUAGE, i18n, initI18n } from './index';

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

describe('en.json', () => {
  it('has only non-empty strings as leaves', () => {
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
    walk(en, '');
    expect(bad).toEqual([]);
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
