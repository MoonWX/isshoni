// @vitest-environment node
import { describe, expect, it } from 'vitest';

import en from '../i18n/en.json';
import {
  clickUrl,
  DEFAULT_CLICK_URL,
  genericNotification,
  NOTIFICATION_BADGE,
  NOTIFICATION_ICON,
  notificationFor,
  pushString,
} from './push';

const ORIGIN = 'https://isshoni.test';
/** What build/sw-plugin.ts injects as __PUSH_STRINGS__. */
const strings: PushStrings = en.push;

describe('pushString', () => {
  it('reads a string at a dotted path', () => {
    expect(pushString({ a: { b: 'text' } }, 'a.b')).toBe('text');
    expect(pushString(strings, 'generic.title')).toBe('isshoni');
  });

  it.each([
    ['a missing key', { a: { b: 'text' } }, 'a.c'],
    ['a group, not a string', { a: { b: 'text' } }, 'a'],
    ['a path through a string', { a: 'text' }, 'a.b'],
    ['an empty string', { a: '' }, 'a'],
    ['an inherited property', {}, 'toString'],
    ['an inherited property of a string', { a: 'text' }, 'a.length'],
  ])('is undefined for %s', (_name, catalog, path) => {
    expect(pushString(catalog, path)).toBeUndefined();
  });

  it('never throws on a catalog that is not what the build promised', () => {
    expect(pushString(null as unknown as PushStrings, 'generic.title')).toBeUndefined();
    expect(pushString('text' as unknown as PushStrings, 'generic.title')).toBeUndefined();
    expect(pushString({ generic: null } as unknown as PushStrings, 'generic.title')).toBeUndefined();
  });
});

describe('the generic notification', () => {
  it('has a title and a body in the catalog', () => {
    expect(en.push.generic.title).not.toBe('');
    expect(en.push.generic.body).not.toBe('');
  });

  it('keeps the tag, the time and the URL of the payload', () => {
    const payload = {
      v: 1,
      type: 'something.new',
      ts: 1790712000000,
      tag: 'share:lounge:k3m9p2qxw7ht',
      url: '/r/lounge',
    };
    expect(genericNotification(payload, strings)).toEqual({
      title: en.push.generic.title,
      options: {
        body: en.push.generic.body,
        icon: NOTIFICATION_ICON,
        badge: NOTIFICATION_BADGE,
        tag: 'share:lounge:k3m9p2qxw7ht',
        timestamp: 1790712000000,
        data: { url: '/r/lounge' },
      },
    });
  });

  it.each([
    ['no payload', null],
    ['undefined', undefined],
    ['a string', 'hello'],
    ['a number', 7],
    ['an array', []],
    ['an empty object', {}],
    ['fields of the wrong type', { tag: 5, ts: 'now', url: { href: '/' } }],
    ['empty fields', { tag: '', ts: 0, url: '' }],
    ['a time that is not a number', { ts: Number.NaN }],
  ])('is complete for %s: a title, the icon and / to open', (_name, payload) => {
    const n = genericNotification(payload, strings);
    expect(n.title).toBe(en.push.generic.title);
    expect(n.options).toEqual({
      body: en.push.generic.body,
      icon: '/icons/icon-192.png',
      badge: '/icons/badge-72.png',
      data: { url: DEFAULT_CLICK_URL },
    });
  });

  it('still has a title when the catalog has no push section', () => {
    const n = genericNotification({ tag: 'test' }, {});
    expect(n.title).toBe('isshoni');
    expect(n.options.body).toBe('');
    expect(n.options.tag).toBe('test');
  });
});

describe('notificationFor', () => {
  // S77 adds the texts per type; until then, and afterwards for types the worker doesn't know, it is the generic one.
  it.each([
    [{ v: 1, type: 'share.started', ts: 1790712000000, tag: 'share:lounge:u1', url: '/r/lounge?focus=s_1' }],
    [{ v: 1, type: 'admin.alert', ts: 1790712000000, tag: 'admin:signup_pending', url: '/admin/approvals' }],
    [{ v: 1, type: 'push.test', ts: 1790712000000, tag: 'test', url: '/account/notifications' }],
    [{ v: 2, type: 'from.the.future', ts: 1790712000000, tag: 'x', url: '/somewhere' }],
    [null],
    ['not an object'],
  ])('never stays silent: %j gets a notification with a title', (payload) => {
    const n = notificationFor(payload, strings);
    expect(n.title).not.toBe('');
    expect(n.options.icon).toBe(NOTIFICATION_ICON);
    expect(n.options.badge).toBe(NOTIFICATION_BADGE);
    expect(typeof n.options.data.url).toBe('string');
  });

  it('carries the payload’s tag and URL', () => {
    const n = notificationFor({ type: 'share.started', tag: 'share:lounge:u1', url: '/r/lounge?focus=s_1' }, strings);
    expect(n.options.tag).toBe('share:lounge:u1');
    expect(n.options.data).toEqual({ url: '/r/lounge?focus=s_1' });
  });
});

describe('clickUrl: a notification only opens this origin (05 §20)', () => {
  it.each([
    ['/r/lounge?focus=s_q7m2x9c4v8b1n5k3', '/r/lounge?focus=s_q7m2x9c4v8b1n5k3'],
    ['/admin/approvals', '/admin/approvals'],
    ['/account/notifications#test', '/account/notifications#test'],
    ['/', '/'],
    ['r/lounge', '/r/lounge'],
    ['https://isshoni.test/r/lounge?focus=s_1', '/r/lounge?focus=s_1'],
  ])('%s → %s', (url, want) => {
    expect(clickUrl(url, ORIGIN)).toBe(want);
  });

  it.each([
    ['another site', 'https://evil.example/r/lounge'],
    ['another scheme of this host', 'http://isshoni.test/r/lounge'],
    ['another port', 'https://isshoni.test:8443/r/lounge'],
    ['a protocol-relative URL', '//evil.example/r/lounge'],
    ['backslashes that browsers read as slashes', '/\\evil.example/r/lounge'],
    ['a same-origin URL whose path would resolve to another site', 'https://isshoni.test//evil.example/'],
    ['a dot segment that leaves two slashes', '/.//evil.example/'],
    ['javascript:', 'javascript:alert(1)'],
    ['data:', 'data:text/html,<script>alert(1)</script>'],
    ['a URL with credentials for another host', 'https://isshoni.test@evil.example/'],
    ['something that is not a URL', 'http://[bad'],
    ['an empty string', ''],
    ['no URL', undefined],
    ['null', null],
    ['a number', 42],
    ['an object', { href: '/r/lounge' }],
  ])('%s → /', (_name, url) => {
    expect(clickUrl(url, ORIGIN)).toBe('/');
  });

  it('gives a URL that resolves to this origin again', () => {
    for (const url of ['/r/lounge', 'https://isshoni.test/a/b?c#d', '/%2F%2Fevil.example', '/a//b']) {
      expect(new URL(clickUrl(url, ORIGIN), ORIGIN).origin).toBe(ORIGIN);
    }
  });
});
