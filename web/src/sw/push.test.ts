// @vitest-environment node
import { describe, expect, it } from 'vitest';

import en from '../i18n/en.json';
import * as api from '../protocol/api.gen';
import {
  clickUrl,
  DEFAULT_CLICK_URL,
  fill,
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

/** The values of the generated constants whose names start with prefix (api.gen.ts: PushType…, AdminAlertKind…). */
function constants(prefix: string): string[] {
  return Object.entries(api)
    .filter(([name, value]) => name.startsWith(prefix) && typeof value === 'string')
    .map(([, value]) => value as string)
    .sort();
}

const PUSH_TYPES = constants('PushType');
const ALERT_KINDS = constants('AdminAlertKind');

/** 04 §14.3's examples, one per type, with every field the server sends. */
const SHARE_STARTED: api.PushPayload = {
  v: 1,
  type: 'share.started',
  ts: 1790712000000,
  tag: 'share:lounge:k3m9p2qxw7ht',
  url: '/r/lounge?focus=s_q7m2x9c4v8b1n5k3',
  room: { id: 'lounge', name: 'Lounge' },
  user: { id: 'k3m9p2qxw7ht', name: 'Alex' },
  shareId: 's_q7m2x9c4v8b1n5k3',
};

function adminAlert(kind: string, extra: Partial<api.PushPayload> = {}): api.PushPayload {
  return {
    v: 1,
    type: 'admin.alert',
    ts: 1790712000000,
    tag: `admin:${kind}`,
    url: '/admin',
    kind: kind as api.AdminAlertKind,
    actor: 'sam',
    target: kind === 'transfer_threshold' ? '80' : 'kim',
    ...extra,
  };
}

const PUSH_TEST: api.PushPayload = {
  v: 1,
  type: 'push.test',
  ts: 1790712000000,
  tag: 'test',
  url: '/account/notifications',
};

/** The keys of en.json's push section that hold a notification's texts: {title, body} groups. */
function textGroups(node: PushStrings, path: string[] = []): string[] {
  const groups: string[] = [];
  if (typeof node['title'] === 'string' && typeof node['body'] === 'string') groups.push(path.join('.'));
  for (const [key, value] of Object.entries(node)) {
    if (typeof value !== 'string') groups.push(...textGroups(value, [...path, key]));
  }
  return groups;
}

describe('the catalog: every push type has its texts in en.json (05 §16.3)', () => {
  it('knows the types and kinds that api.gen.ts declares', () => {
    expect(PUSH_TYPES).toEqual(['admin.alert', 'push.test', 'share.started']);
    expect(ALERT_KINDS).toEqual(
      expect.arrayContaining([
        'admin_granted',
        'admin_password_reset',
        'admin_revoked',
        'registration_mode_changed',
        'secrets_rotated',
        'signup_pending',
        'transfer_threshold',
      ]),
    );
  });

  it.each(ALERT_KINDS)('has push.adminAlert.%s with a title and a body', (kind) => {
    expect(pushString(strings, `adminAlert.${kind}.title`)).toBeDefined();
    expect(pushString(strings, `adminAlert.${kind}.body`)).toBeDefined();
  });

  it('has no texts that no payload uses', () => {
    const used = ['generic', 'shareStarted', 'test', ...ALERT_KINDS.map((k) => `adminAlert.${k}`)].sort();
    expect(textGroups(strings).sort()).toEqual(used);
  });

  it('uses only the values a payload has', () => {
    const allowed: Record<string, string[]> = { shareStarted: ['room', 'user'], test: [], generic: [] };
    for (const group of textGroups(strings)) {
      const names = new Set<string>();
      for (const part of ['title', 'body']) {
        for (const m of (pushString(strings, `${group}.${part}`) ?? '').matchAll(/\{\{\s*(\w+)\s*\}\}/g)) {
          names.add(m[1] ?? '');
        }
      }
      expect(
        [...names].every((n) => (allowed[group] ?? ['actor', 'target']).includes(n)),
        group,
      ).toBe(true);
    }
  });
});

describe('notificationFor: the texts of each type', () => {
  it('share.started: "Alex started sharing" / "in Lounge · Tap to watch"', () => {
    expect(notificationFor(SHARE_STARTED, strings)).toEqual({
      title: 'Alex started sharing',
      options: {
        body: 'in Lounge · Tap to watch',
        icon: NOTIFICATION_ICON,
        badge: NOTIFICATION_BADGE,
        tag: 'share:lounge:k3m9p2qxw7ht',
        timestamp: 1790712000000,
        data: { url: '/r/lounge?focus=s_q7m2x9c4v8b1n5k3' },
      },
    });
  });

  it('push.test has its own texts', () => {
    const n = notificationFor(PUSH_TEST, strings);
    expect(n.title).toBe(en.push.test.title);
    expect(n.options).toMatchObject({ body: en.push.test.body, tag: 'test', data: { url: '/account/notifications' } });
  });

  it.each(ALERT_KINDS)('admin.alert %s renders push.adminAlert.<kind> with the names filled in', (kind) => {
    const n = notificationFor(adminAlert(kind), strings);
    const group = (en.push.adminAlert as Record<string, { title: string; body: string }>)[kind];
    expect(group).toBeDefined();
    if (!group) return;
    const values = { actor: 'sam', target: kind === 'transfer_threshold' ? '80' : 'kim' };
    expect(n.title).toBe(fill(group.title, values));
    expect(n.options.body).toBe(fill(group.body, values));
    expect(n.title).not.toBe(en.push.generic.title);
    expect(`${n.title} ${n.options.body}`).not.toMatch(/\{\{|\}\}/);
    expect(n.options).toMatchObject({ tag: `admin:${kind}`, timestamp: 1790712000000, data: { url: '/admin' } });
  });

  it('names the alerts as 04 §14.3 sends them', () => {
    const pending = notificationFor(adminAlert('signup_pending', { actor: 'system', target: 'sam_k' }), strings);
    expect(pending.title).toBe('New sign-up request');
    expect(pending.options.body).toBe('sam_k asked for an account. Tap to review it.');
    const granted = notificationFor(adminAlert('admin_granted', { actor: 'alex', target: 'bo' }), strings);
    expect([granted.title, granted.options.body]).toEqual(['bo is now an admin', 'Made an admin by alex.']);
    const transfer = notificationFor(adminAlert('transfer_threshold', { actor: 'system', target: '100' }), strings);
    expect(transfer.title).toBe("100% of this month's transfer");
  });

  it.each([
    ['cli', en.push.actor.cli],
    ['system', en.push.actor.system],
  ])('names the actor %s by its text, not as a username', (actor, name) => {
    const n = notificationFor(adminAlert('admin_granted', { actor, target: 'bo' }), strings);
    expect(n.options.body).toBe(`Made an admin by ${name}.`);
  });

  it('keeps a name as it is: a notification is plain text', () => {
    const n = notificationFor({ ...SHARE_STARTED, user: { id: 'u', name: '<b>&amp;{{room}}' } }, strings);
    expect(n.title).toBe('<b>&amp;{{room}} started sharing');
  });
});

describe('notificationFor never stays silent (05 §16.3, 04 §14.3)', () => {
  const generic = (n: ReturnType<typeof notificationFor>): boolean =>
    n.title === en.push.generic.title && n.options.body === en.push.generic.body;

  it.each([
    ['share.started without the sharer', { ...SHARE_STARTED, user: undefined }],
    ['share.started without the room', { ...SHARE_STARTED, room: undefined }],
    ['share.started with an empty name', { ...SHARE_STARTED, user: { id: 'u', name: '' } }],
    ['share.started with a name that is not a string', { ...SHARE_STARTED, room: { id: 'r', name: 7 } }],
    ['share.started with a blank name', { ...SHARE_STARTED, user: { id: 'u', name: '   ' } }],
    ['admin.alert without a kind', adminAlert('signup_pending', { kind: undefined })],
    ['admin.alert of a kind without texts', adminAlert('from_the_future')],
    ['admin.alert whose kind is a path', adminAlert('signup_pending.title')],
    ['admin.alert whose kind is an inherited name', adminAlert('constructor')],
    ['admin.alert without the target its text names', adminAlert('signup_pending', { target: undefined })],
    ['admin.alert without the actor its text names', adminAlert('admin_revoked', { actor: undefined })],
    ['a type from a newer server', { v: 2, type: 'from.the.future', ts: 1, tag: 'x', url: '/somewhere' }],
    ['no type', { v: 1, ts: 1, tag: 'x', url: '/' }],
    ['null', null],
    ['a string', 'not an object'],
  ])('%s gets the generic notification, with the payload’s tag and URL', (_name, payload) => {
    const n = notificationFor(payload, strings);
    expect(generic(n)).toBe(true);
    expect(n.options.icon).toBe(NOTIFICATION_ICON);
    expect(n.options.badge).toBe(NOTIFICATION_BADGE);
    expect(typeof n.options.data.url).toBe('string');
    const tag = (payload as { tag?: unknown } | null)?.tag;
    if (typeof tag === 'string') expect(n.options.tag).toBe(tag);
  });

  it('a payload that throws when it is read', () => {
    const payload = new Proxy(
      { type: 'share.started', tag: 't', url: '/' },
      {
        get(target, key) {
          if (key === 'user') throw new Error('boom');
          return (target as Record<string | symbol, unknown>)[key];
        },
      },
    );
    expect(generic(notificationFor(payload, strings))).toBe(true);
  });

  it('a catalog without the type’s texts', () => {
    const n = notificationFor(SHARE_STARTED, { generic: en.push.generic });
    expect(generic(n)).toBe(true);
    expect(n.options.tag).toBe(SHARE_STARTED.tag);
    expect(notificationFor(SHARE_STARTED, {}).title).toBe('isshoni');
  });

  it.each(PUSH_TYPES)('%s with nothing but its type still gets a title', (type) => {
    const n = notificationFor({ type }, strings);
    expect(n.title.trim()).not.toBe('');
    expect(n.options.data.url).toBe(DEFAULT_CLICK_URL);
  });
});

describe('fill', () => {
  it('fills placeholders, with or without spaces, as often as they come', () => {
    expect(fill('{{a}} and {{ b }} and {{a}}', { a: 'x', b: 'y' })).toBe('x and y and x');
    expect(fill('no placeholders', {})).toBe('no placeholders');
  });

  it.each([
    ['a missing value', '{{a}} b', {}],
    ['an empty value', '{{a}} b', { a: '' }],
    ['an inherited name', '{{constructor}}', {}],
    ['a blank result', '  ', {}],
  ])('is undefined for %s', (_name, template, values) => {
    expect(fill(template, values)).toBeUndefined();
  });

  it('leaves what is no placeholder alone', () => {
    expect(fill('{a} {{}} {{ 1x }} $t(x)', {})).toBe('{a} {{}} {{ 1x }} $t(x)');
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
