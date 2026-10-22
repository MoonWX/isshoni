// DesktopPlatform with a fake NativeBridge (docs/m2/03 §7.3, §22.3; 02 §11.8): every accessToken, sessionEnded and
// helloRefused call passes bridge.js's server.origin; apiFetch's bearer, its omitted credentials, no Authorization
// without a token and its one retry with a forced token; signaling().auth; C1's stop codes; storage per server; link
// interception; no isa_ string in storage; capabilities(); version actions; the transport loaded on demand.
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { SignalClient } from '../../protocol/signal-client';
import { FakeSignalServer, makeError } from '../../protocol/testing';
import type { SharingProvider } from '../types';
import type { DesktopConfig } from './config';
import { DesktopPlatform, type DesktopPlatformOptions } from './DesktopPlatform';
import { BridgeError } from './errors';
import { authState, createFakeBridge, shareCaps, updateStatus, type FakeBridge } from './testing/fakeBridge';
import { LocalIdVectors } from './testing/localIdVectors';

const ORIGIN = 'https://watch.example.com';
const OTHER = 'https://b.example.com';

/** The derived local id of an origin of the vectors (localId.node.test.ts checks them against SHA-256). */
function localId(origin: string): string {
  const v = LocalIdVectors.find(([o]) => o === origin);
  if (!v) throw new Error(`no id vector for ${origin}`);
  return v[1];
}

const ID = localId(ORIGIN);

function config(over: Partial<DesktopConfig> = {}): DesktopConfig {
  return {
    v: 1,
    transport: 'wails',
    app: { version: '0.2.0', os: 'windows', osBuild: 26100, arch: 'amd64' },
    server: { id: ID, origin: ORIGIN, name: 'Friends' },
    serverIds: [ID],
    dev: false,
    ...over,
  };
}

const made: DesktopPlatform[] = [];

function make(
  opts: { cfg?: DesktopConfig; active?: string; linked?: boolean } & Omit<DesktopPlatformOptions, 'bridge'> = {},
): { p: DesktopPlatform; fake: FakeBridge } {
  const fake = createFakeBridge({ active: opts.active ?? ORIGIN, linked: opts.linked ?? true });
  const p = new DesktopPlatform(opts.cfg ?? config(), {
    bridge: fake.bridge,
    ...(opts.sharing ? { sharing: opts.sharing } : {}),
    ...(opts.interceptLinks !== undefined ? { interceptLinks: opts.interceptLinks } : {}),
  });
  made.push(p);
  return { p, fake };
}

const fetchMock = vi.fn<typeof fetch>();

function json(status: number, body: unknown): Response {
  return new Response(JSON.stringify(body), { status, headers: { 'Content-Type': 'application/json' } });
}

const refused = (code: string): Response => json(401, { error: { code } });

const urlOf = (input: string | URL | Request): string => (input instanceof Request ? input.url : input.toString());

/** The request fetch got at call i: its URL and init. */
function sent(i: number): { url: string; init: RequestInit; auth: string | null } {
  const call = fetchMock.mock.calls[i];
  if (!call) throw new Error(`no fetch call ${String(i)}`);
  const [url, init = {}] = call;
  return { url: urlOf(url), init, auth: new Headers(init.headers).get('Authorization') };
}

beforeEach(() => {
  fetchMock.mockReset();
  vi.stubGlobal('fetch', fetchMock);
});

afterEach(() => {
  for (const p of made.splice(0)) p.dispose();
  vi.unstubAllGlobals();
  vi.useRealTimers();
  localStorage.clear();
  sessionStorage.clear();
  history.replaceState(null, '', '/');
});

describe('DesktopPlatform basics (docs/m2/03 §7.3)', () => {
  it('reports the app as a desktop viewer client of the active server', () => {
    const { p } = make();
    expect(p.kind).toBe('desktop');
    expect(p.client).toEqual({ kind: 'desktop', version: '0.2.0', os: 'windows', browser: 'edge' });
    expect(p.role).toBe('viewer');
    expect(p.serverOrigin).toBe(ORIGIN);
    expect(p.pwa).toBeNull();
    expect(p.sharing).toBeNull();
    expect(p.inAppBrowser()).toBeNull();
    expect(p.desktop.app).toEqual({ version: '0.2.0', os: 'windows', osBuild: 26100 });
    expect(p.desktop.server).toEqual({ id: ID, origin: ORIGIN, name: 'Friends' });
    expect(p.desktop.wizardSteps()).toEqual(['link', 'sound', 'voice']);
  });

  it('has no Web Push', async () => {
    const { p } = make();
    expect(await p.notifications.support()).toBe('unsupported');
    expect(await p.notifications.status()).toBe('off');
    await expect(p.notifications.enable()).rejects.toThrow();
  });

  it('without an active server: serverOrigin and the signaling URL are empty, and apiFetch makes no request', async () => {
    const { p, fake } = make({ cfg: config({ server: null, serverIds: [] }), active: '' });
    expect(p.serverOrigin).toBe('');
    expect(p.desktop.server).toBeNull();
    expect(p.signaling().url).toBe('');
    await expect(p.apiFetch('/api/v1/info')).rejects.toThrow('no active server');
    expect(fetchMock).not.toHaveBeenCalled();
    expect(fake.callsOf('auth.accessToken')).toEqual([]);
  });
});

describe('apiFetch (docs/m2/02 §11.8)', () => {
  it('adds the bearer, omits credentials and resolves the path against serverOrigin', async () => {
    const { p, fake } = make();
    fetchMock.mockResolvedValueOnce(json(200, { ok: true }));
    const res = await p.apiFetch('/api/v1/rooms', {
      method: 'POST',
      headers: { Accept: 'application/json', 'Content-Type': 'application/json' },
      body: '{}',
    });
    expect(res.status).toBe(200);
    const { url, init, auth } = sent(0);
    expect(url).toBe(`${ORIGIN}/api/v1/rooms`);
    expect(auth).toBe(`Bearer ${fake.issued[0] ?? ''}`);
    expect(init).toMatchObject({ method: 'POST', body: '{}', credentials: 'omit', mode: 'cors' });
    expect(init.referrerPolicy).toBe('no-referrer');
    expect(new Headers(init.headers).get('Accept')).toBe('application/json');
    expect(fake.callsOf('auth.accessToken')).toEqual([[ORIGIN, false]]);
  });

  it('sends no Authorization when accessToken is null: the /info call at boot succeeds', async () => {
    const { p, fake } = make({ linked: false });
    fetchMock.mockResolvedValueOnce(json(200, { server: { name: 'Friends' } }));
    const res = await p.apiFetch('/api/v1/info', { headers: { Authorization: 'Bearer stale' } });
    expect(res.status).toBe(200);
    expect(sent(0).auth).toBeNull();
    expect(sent(0).init.credentials).toBe('omit');
    expect(fake.callsOf('auth.accessToken')).toEqual([[ORIGIN, false]]);
  });

  it('sends no Authorization once the fake switched its active server: no token of another server', async () => {
    const { p, fake } = make();
    fetchMock.mockResolvedValue(json(200, {}));
    // An added server became active before the window reloads (docs/m2/03 §8.2); its auth.state is ignored.
    fake.active = OTHER;
    fake.emit('auth.state', authState(OTHER, 'linked'));
    expect((await p.apiFetch('/api/v1/rooms')).status).toBe(200);
    expect(sent(0).auth).toBeNull();
    // A token this page already held is its own server's; when that server refuses it, the forced ask gets null.
    const { p: q, fake: f } = make();
    fetchMock.mockResolvedValueOnce(json(200, {})).mockResolvedValueOnce(refused('invalid_token'));
    await q.apiFetch('/api/v1/me');
    f.active = OTHER;
    expect((await q.apiFetch('/api/v1/me')).status).toBe(401);
    expect(fetchMock).toHaveBeenCalledTimes(3);
    for (const c of [fake, f]) expect(c.callsOf('auth.accessToken').every(([o]) => o === ORIGIN)).toBe(true);
    expect(f.issued).toHaveLength(1);
  });

  it.each(['invalid_token', 'unauthenticated'])(
    'on 401 %s with a token: accessToken(origin, true) and one retry with the new token',
    async (code) => {
      const { p, fake } = make();
      fetchMock.mockResolvedValueOnce(refused(code)).mockResolvedValueOnce(json(200, { ok: true }));
      const res = await p.apiFetch('/api/v1/me');
      expect(res.status).toBe(200);
      expect(fetchMock).toHaveBeenCalledTimes(2);
      expect(fake.callsOf('auth.accessToken')).toEqual([
        [ORIGIN, false],
        [ORIGIN, true],
      ]);
      expect(sent(0).auth).toBe('Bearer isa_fake0001');
      expect(sent(1).auth).toBe('Bearer isa_fake0002');
    },
  );

  it('hands a second 401 to the caller, without another retry', async () => {
    const { p, fake } = make();
    fetchMock.mockResolvedValueOnce(refused('invalid_token')).mockResolvedValueOnce(refused('unauthenticated'));
    const res = await p.apiFetch('/api/v1/me');
    expect(res.status).toBe(401);
    expect(await res.json()).toEqual({ error: { code: 'unauthenticated' } });
    expect(fetchMock).toHaveBeenCalledTimes(2);
    expect(fake.callsOf('auth.accessToken')).toHaveLength(2);
  });

  it('does not retry a 401 to a request that carried no token', async () => {
    const { p, fake } = make({ linked: false });
    fetchMock.mockResolvedValueOnce(refused('unauthenticated'));
    expect((await p.apiFetch('/api/v1/me')).status).toBe(401);
    expect(fetchMock).toHaveBeenCalledTimes(1);
    expect(fake.callsOf('auth.accessToken')).toEqual([[ORIGIN, false]]);
  });

  it('does not retry another 401 or another status', async () => {
    const { p, fake } = make();
    fetchMock
      .mockResolvedValueOnce(json(401, { error: { code: 'invalid_credentials' } }))
      .mockResolvedValueOnce(new Response('nope', { status: 401 }))
      .mockResolvedValueOnce(json(403, { error: { code: 'forbidden' } }));
    expect((await p.apiFetch('/api/v1/me')).status).toBe(401);
    expect((await p.apiFetch('/api/v1/me')).status).toBe(401);
    expect((await p.apiFetch('/api/v1/me')).status).toBe(403);
    expect(fetchMock).toHaveBeenCalledTimes(3);
    expect(fake.callsOf('auth.accessToken')).toEqual([[ORIGIN, false]]); // the cached token, never forced
  });

  it('returns the first 401, body intact, when the forced call gives no token', async () => {
    const { p, fake } = make();
    fetchMock.mockImplementationOnce(() => {
      fake.linked = false; // signed out meanwhile
      return Promise.resolve(refused('invalid_token'));
    });
    const res = await p.apiFetch('/api/v1/me');
    expect(res.status).toBe(401);
    expect(await res.json()).toEqual({ error: { code: 'invalid_token' } });
    expect(fetchMock).toHaveBeenCalledTimes(1);
  });

  it('returns the first 401, body intact, when the forced call fails (offline): the SPA 401 rule runs', async () => {
    const { p, fake } = make();
    fetchMock.mockImplementationOnce(() => {
      fake.answers['auth.accessToken'] = () => Promise.reject(new BridgeError('server_unreachable'));
      return Promise.resolve(refused('invalid_token'));
    });
    const res = await p.apiFetch('/api/v1/me');
    expect(res.status).toBe(401);
    expect(await res.json()).toEqual({ error: { code: 'invalid_token' } });
    expect(fetchMock).toHaveBeenCalledTimes(1);
    expect(fake.callsOf('auth.accessToken')).toEqual([
      [ORIGIN, false],
      [ORIGIN, true],
    ]);
  });

  it('rejects with the bridge error when Go cannot give a token (offline)', async () => {
    const { p, fake } = make();
    fake.answers['auth.accessToken'] = () => Promise.reject(new BridgeError('server_unreachable'));
    await expect(p.apiFetch('/api/v1/me')).rejects.toMatchObject({ code: 'server_unreachable' });
    expect(fetchMock).not.toHaveBeenCalled();
  });
});

describe('signaling() (docs/m2/02 §11.8)', () => {
  it('builds wss:// from the https server origin, ws:// from a dev loopback origin', () => {
    expect(make().p.signaling().url).toBe('wss://watch.example.com/ws');
    const dev = config({ dev: true, server: { id: 'tc55ev03', origin: 'http://127.0.0.1:8080', name: 'Dev' } });
    expect(make({ cfg: dev, active: 'http://127.0.0.1:8080' }).p.signaling().url).toBe('ws://127.0.0.1:8080/ws');
  });

  it('auth gives a fresh bearer, forces with forceRefresh, and gives nothing without a token', async () => {
    const { p, fake } = make();
    const { auth } = p.signaling();
    expect(await auth({ forceRefresh: false })).toEqual({ scheme: 'bearer', token: 'isa_fake0001' });
    expect(await auth({ forceRefresh: true })).toEqual({ scheme: 'bearer', token: 'isa_fake0002' });
    fake.active = OTHER;
    fake.emit('auth.state', authState(ORIGIN, 'unlinked'));
    expect(await auth({ forceRefresh: false })).toBeUndefined();
    expect(fake.callsOf('auth.accessToken')).toEqual([
      [ORIGIN, false],
      [ORIGIN, true],
      [ORIGIN, false],
    ]);
  });

  it("puts the bearer in every hello, and none once this page's server isn't active", async () => {
    vi.useFakeTimers();
    const server = FakeSignalServer.install();
    try {
      const { p, fake } = make();
      const { url, auth } = p.signaling();
      const client = new SignalClient({
        url,
        auth,
        client: p.client,
        role: p.role,
        caps: () => ({ decode: ['opus'] }),
        buildVersion: '',
      });
      client.start();
      await vi.advanceTimersByTimeAsync(0);
      expect(client.state).toBe('ready');
      expect(server.hellos[0]?.auth).toEqual({ scheme: 'bearer', token: 'isa_fake0001' });
      // The server refuses the token: one forced refresh, then a new connection with the new token.
      server.error(makeError('unauthenticated', 'session'));
      await vi.advanceTimersByTimeAsync(10_000);
      expect(server.hellos[1]?.auth).toEqual({ scheme: 'bearer', token: 'isa_fake0002' });
      // Another server became active: the next hello carries no token (02 §11.8).
      fake.active = OTHER;
      fake.emit('auth.state', authState(ORIGIN, 'unlinked'));
      server.drop();
      await vi.advanceTimersByTimeAsync(10_000);
      expect(server.hellos[2]).toBeDefined();
      expect(server.hellos[2]?.auth).toBeUndefined();
      expect(fake.callsOf('auth.accessToken').every(([origin]) => origin === ORIGIN)).toBe(true);
      client.stop();
    } finally {
      server.uninstall();
    }
  });
});

describe('C1 stops (docs/m2/03 §7.3)', () => {
  it.each([
    ['session_revoked', 'linked', 'redial'],
    ['account_disabled', 'linked', 'redial'],
    ['session_revoked', 'unlinked', 'relink'],
    ['account_disabled', 'unlinked', 'relink'],
    ['unauthenticated', 'unlinked', 'relink'],
    ['unauthenticated', 'linked', 'redial'],
  ] as const)('%s with the state %s after sessionEnded: %s', async (code, status, action) => {
    const { p, fake } = make();
    fake.sessionEndStatus = status;
    expect(await p.desktop.signalStopped({ code })).toBe(action);
    expect(fake.callsOf('auth.sessionEnded')).toEqual([[ORIGIN, code]]);
  });

  it.each(['client_outdated', 'protocol_unsupported'] as const)(
    '%s: helloRefused(origin, error) and the version screen',
    async (code) => {
      const { p, fake } = make();
      expect(await p.desktop.signalStopped({ code, params: { serverVersion: '0.3.0' } })).toBe('version');
      expect(await p.desktop.signalStopped({ code })).toBe('version');
      expect(fake.callsOf('auth.helloRefused')).toEqual([
        [ORIGIN, { code, params: { serverVersion: '0.3.0' } }],
        [ORIGIN, { code }],
      ]);
    },
  );

  it('not_found from either (this page is not the active server any more) stops C1 without a redial', async () => {
    const { p, fake } = make();
    fake.active = OTHER;
    expect(await p.desktop.signalStopped({ code: 'session_revoked' })).toBe('wait');
    expect(await p.desktop.signalStopped({ code: 'client_outdated' })).toBe('wait');
    expect(fake.callsOf('auth.sessionEnded')).toEqual([[ORIGIN, 'session_revoked']]);
    expect(fake.callsOf('auth.helloRefused')).toEqual([[ORIGIN, { code: 'client_outdated' }]]);
  });

  it('another bridge error: relink after a session end, the version screen after a refused hello', async () => {
    const { p, fake } = make();
    fake.answers['auth.sessionEnded'] = () => Promise.reject(new BridgeError('internal'));
    fake.answers['auth.helloRefused'] = () => Promise.reject(new BridgeError('internal'));
    expect(await p.desktop.signalStopped({ code: 'session_revoked' })).toBe('relink');
    expect(await p.desktop.signalStopped({ code: 'protocol_unsupported' })).toBe('version');
  });

  it.each(['room_full', 'forbidden', 'replaced', 'internal'])('%s is not a desktop case', async (code) => {
    const { p, fake } = make();
    expect(await p.desktop.signalStopped({ code })).toBe('none');
    expect(fake.calls).toEqual([]);
  });

  it('every accessToken, sessionEnded and helloRefused call of a session names server.origin', async () => {
    const { p, fake } = make();
    fetchMock.mockResolvedValueOnce(refused('invalid_token')).mockResolvedValue(json(200, {}));
    await p.apiFetch('/api/v1/me');
    await p.apiFetch('/api/v1/rooms');
    await p.signaling().auth({ forceRefresh: true });
    await p.desktop.sessionEnded('unauthenticated');
    await p.desktop.helloRefused({ code: 'client_outdated' });
    await p.desktop.signalStopped({ code: 'account_disabled' });
    fake.active = OTHER;
    await p.signaling().auth({ forceRefresh: true });
    await p.desktop.signalStopped({ code: 'protocol_unsupported' });
    const tokenCalls = fake.calls.filter((c) =>
      ['auth.accessToken', 'auth.sessionEnded', 'auth.helloRefused'].includes(c.method),
    );
    expect(tokenCalls.length).toBeGreaterThanOrEqual(8);
    expect(tokenCalls.every((c) => c.args[0] === ORIGIN)).toBe(true);
  });
});

describe('storage per server (docs/m2/03 §14.3)', () => {
  it.each(LocalIdVectors)('prefixes storage.local with isshoni:<id>: for %s', (origin, id) => {
    const { p } = make({ cfg: config({ dev: true, server: { id, origin, name: 'S' }, serverIds: [id] }) });
    p.storage.local.set('isshoni.lastRoomId', 'lounge');
    expect(localStorage.getItem(`isshoni:${id}:isshoni.lastRoomId`)).toBe('lounge');
    expect(p.storage.local.get('isshoni.lastRoomId')).toBe('lounge');
    p.storage.local.remove('isshoni.lastRoomId');
    expect(localStorage.length).toBe(0);
  });

  it('uses isshoni:_: without an active server, and leaves storage.session unprefixed', () => {
    const { p } = make({ cfg: config({ server: null, serverIds: [] }), active: '' });
    p.storage.local.set('isshoni.volume', '0.5');
    p.storage.session.set('isshoni.tab', '1');
    expect(localStorage.getItem('isshoni:_:isshoni.volume')).toBe('0.5');
    expect(sessionStorage.getItem('isshoni.tab')).toBe('1');
  });

  it('removes the keys of servers that servers.json no longer lists, at boot', () => {
    const known = localId('https://a.example.com');
    const removed = localId('https://b.example.com');
    localStorage.setItem(`isshoni:${ID}:isshoni.lastRoomId`, 'lounge');
    localStorage.setItem(`isshoni:${known}:isshoni.lastRoomId`, 'den');
    localStorage.setItem(`isshoni:${removed}:isshoni.lastRoomId`, 'gone');
    localStorage.setItem(`isshoni:${removed}:isshoni.volume`, '1');
    localStorage.setItem('isshoni:_:isshoni.volume', '0.3');
    localStorage.setItem('someone.else', 'x');
    const { p } = make({ cfg: config({ serverIds: [ID, known] }) });
    expect(localStorage.getItem(`isshoni:${removed}:isshoni.lastRoomId`)).toBeNull();
    expect(localStorage.getItem(`isshoni:${removed}:isshoni.volume`)).toBeNull();
    expect(localStorage.getItem(`isshoni:${known}:isshoni.lastRoomId`)).toBe('den');
    expect(localStorage.getItem('isshoni:_:isshoni.volume')).toBe('0.3');
    expect(localStorage.getItem('someone.else')).toBe('x');
    expect(p.storage.local.get('isshoni.lastRoomId')).toBe('lounge');
  });

  it('keeps no isa_ string in Web Storage after a session', async () => {
    const { p } = make();
    fetchMock.mockResolvedValueOnce(refused('invalid_token')).mockResolvedValue(json(200, {}));
    await p.apiFetch('/api/v1/me');
    await p.signaling().auth({ forceRefresh: false });
    await p.signaling().auth({ forceRefresh: true });
    p.storage.local.set('isshoni.lastRoomId', 'lounge');
    p.storage.session.set('isshoni.reloadedFor', '0.2.0');
    const dump = (s: Storage): string =>
      Array.from({ length: s.length }, (_, i) => {
        const k = s.key(i) ?? '';
        return `${k}=${s.getItem(k) ?? ''}`;
      }).join('\n');
    expect(dump(localStorage)).not.toBe('');
    expect(dump(localStorage)).not.toContain('isa_');
    expect(dump(sessionStorage)).not.toContain('isa_');
  });
});

describe('link interception (docs/m2/03 §5.5)', () => {
  function link(href: string, attrs: Record<string, string> = {}): HTMLAnchorElement {
    const a = document.createElement('a');
    a.href = href;
    for (const [k, v] of Object.entries(attrs)) a.setAttribute(k, v);
    a.append(document.createElement('span'));
    document.body.append(a);
    return a;
  }

  /** Dispatches a mouse event; reports whether the page cancelled it (then cancels it, so jsdom never navigates). */
  function click(el: Element, init: MouseEventInit = {}, type = 'click'): { defaultPrevented: boolean } {
    const ev = new MouseEvent(type, { bubbles: true, cancelable: true, composed: true, ...init });
    let prevented = false;
    const last = (e: Event): void => {
      prevented = e.defaultPrevented;
      e.preventDefault();
    };
    window.addEventListener(type, last, { once: true });
    el.dispatchEvent(ev);
    window.removeEventListener(type, last);
    return { defaultPrevented: prevented };
  }

  afterEach(() => {
    document.body.replaceChildren();
  });

  it('hands a click on a link off the asset origin to openExternal and cancels it', () => {
    const { fake } = make();
    const a = link('https://example.org/help');
    expect(click(a.firstElementChild ?? a).defaultPrevented).toBe(true);
    expect(fake.callsOf('app.openExternal')).toEqual([['https://example.org/help']]);
  });

  it('leaves a plain click on an asset-origin link to the router', () => {
    const { fake } = make();
    expect(click(link('/r/lounge')).defaultPrevented).toBe(false);
    expect(fake.callsOf('app.openExternal')).toEqual([]);
  });

  it('cancels what would open a second window on an asset-origin link', () => {
    const { fake } = make();
    expect(click(link('/r/lounge'), { ctrlKey: true }).defaultPrevented).toBe(true);
    expect(click(link('/r/lounge'), { metaKey: true }).defaultPrevented).toBe(true);
    expect(click(link('/r/lounge', { target: '_blank' })).defaultPrevented).toBe(true);
    expect(click(link('/r/lounge'), { button: 1 }, 'auxclick').defaultPrevented).toBe(true);
    expect(fake.callsOf('app.openExternal')).toEqual([]);
  });

  it('handles a middle click (auxclick) on an external link, not a right click', () => {
    const { fake } = make();
    const a = link('https://example.org/x');
    expect(click(a, { button: 1 }, 'auxclick').defaultPrevented).toBe(true);
    expect(click(a, { button: 2 }, 'auxclick').defaultPrevented).toBe(false);
    expect(fake.callsOf('app.openExternal')).toEqual([['https://example.org/x']]);
  });

  it("opens the browser once when the link's own onClick opens it too, and again on a later click", async () => {
    const { p, fake } = make();
    const a = link('https://example.org/privacy', { target: '_blank', rel: 'noopener noreferrer' });
    // What React's root listener does for AboutPage's ExternalLink outside the browser: bubble phase, after ours.
    const root = (e: Event): void => {
      e.preventDefault();
      p.openExternal(a.href);
    };
    document.body.addEventListener('click', root);
    try {
      expect(click(a).defaultPrevented).toBe(true);
      expect(fake.callsOf('app.openExternal')).toEqual([['https://example.org/privacy']]);
      await new Promise((r) => setTimeout(r, 0)); // a later task: a separate click opens again
      click(a);
      expect(fake.callsOf('app.openExternal')).toEqual([
        ['https://example.org/privacy'],
        ['https://example.org/privacy'],
      ]);
    } finally {
      document.body.removeEventListener('click', root);
    }
  });

  it('turns window.open into openExternal and returns null', async () => {
    const { fake } = make();
    expect(window.open('https://example.org/a', '_blank')).toBeNull();
    expect(window.open('/relative')).toBeNull();
    expect(window.open()).toBeNull();
    await Promise.resolve();
    expect(fake.callsOf('app.openExternal')).toEqual([
      ['https://example.org/a'],
      [`${location.origin}/relative`], // refused by Go (not https): logged and dropped
    ]);
  });

  it('cancels dragover and drop, so a drop never navigates', () => {
    make();
    const over = new Event('dragover', { bubbles: true, cancelable: true });
    const drop = new Event('drop', { bubbles: true, cancelable: true });
    document.body.dispatchEvent(over);
    document.body.dispatchEvent(drop);
    expect(over.defaultPrevented).toBe(true);
    expect(drop.defaultPrevented).toBe(true);
  });

  it('dispose removes the listeners and restores window.open; interceptLinks: false installs none', () => {
    const open = vi.fn(() => null);
    vi.stubGlobal('open', open);
    const { p, fake } = make();
    p.dispose();
    made.splice(made.indexOf(p), 1);
    expect(click(link('https://example.org/')).defaultPrevented).toBe(false);
    window.open('https://example.org/');
    expect(open).toHaveBeenCalledTimes(1);
    expect(fake.callsOf('app.openExternal')).toEqual([]);
    const { fake: other } = make({ interceptLinks: false });
    expect(click(link('https://example.org/')).defaultPrevented).toBe(false);
    expect(other.callsOf('app.openExternal')).toEqual([]);
  });
});

describe('capabilities() (docs/m2/03 §7.3)', () => {
  const sharing: SharingProvider = {
    mode: 'native',
    pick: () => Promise.resolve(null),
    start: () => Promise.reject(new Error('not in this test')),
  };

  it('maps the desktop: no push, no install, no Share without a sharing provider', async () => {
    const { p, fake } = make();
    const caps = await p.capabilities();
    expect(caps).toMatchObject({ canShare: false, push: 'unsupported', install: 'none', fullscreen: 'none' });
    expect(caps.caps).toEqual(p.capsNow());
    expect(fake.callsOf('share.capabilities')).toEqual([]);
  });

  it('takes canShare from Share.Capabilities, and false when the probe fails', async () => {
    const { p, fake } = make({ sharing: () => sharing });
    expect(p.sharing).toBe(sharing);
    fake.answers['share.capabilities'] = shareCaps(true);
    expect((await p.capabilities()).canShare).toBe(true);
    fake.answers['share.capabilities'] = shareCaps(false);
    expect((await p.capabilities()).canShare).toBe(false);
    fake.answers['share.capabilities'] = () => Promise.reject(new BridgeError('engine.internal'));
    expect((await p.capabilities()).canShare).toBe(false);
  });

  it('reports element fullscreen where the Fullscreen API works (W16)', async () => {
    Object.defineProperty(document, 'fullscreenEnabled', { configurable: true, value: true });
    try {
      expect((await make().p.capabilities()).fullscreen).toBe('element');
    } finally {
      Reflect.deleteProperty(document, 'fullscreenEnabled');
    }
  });
});

describe('openExternal and versionActions (docs/m2/03 §16.3)', () => {
  it('opens through App.OpenExternal; a refused URL is dropped without a rejection', async () => {
    const { p, fake } = make();
    p.openExternal('https://example.org/');
    p.openExternal('file:///etc/passwd');
    await Promise.resolve();
    expect(fake.callsOf('app.openExternal')).toEqual([['https://example.org/'], ['file:///etc/passwd']]);
  });

  it('has no reload (no stale-build reload): the SPA is the app own build', () => {
    const actions = make().p.versionActions();
    expect(actions.reload).toBeUndefined();
    expect(typeof actions.openInBrowser).toBe('function');
    expect(typeof actions.updateApp).toBe('function');
  });

  it('openInBrowser opens the current room on the server, or its home', () => {
    const { p, fake } = make();
    history.replaceState(null, '', '/r/lounge');
    p.versionActions().openInBrowser?.();
    history.replaceState(null, '', '/desktop/settings');
    p.versionActions().openInBrowser?.();
    expect(fake.callsOf('app.openExternal')).toEqual([[`${ORIGIN}/r/lounge`], [`${ORIGIN}/`]]);
  });

  it('updateApp installs an update that becomes ready', async () => {
    const { p, fake } = make();
    fake.answers['updates.check'] = updateStatus('checking');
    const outcome = p.versionActions().updateApp?.();
    await vi.waitFor(() => {
      expect(fake.callsOf('updates.check')).toHaveLength(1);
    });
    fake.emit('update.state', updateStatus('downloading', { available: '0.2.1', progress: 0.5 }));
    fake.emit('update.state', updateStatus('ready', { available: '0.2.1' }));
    expect(await outcome).toEqual({ kind: 'installing' });
    expect(fake.callsOf('updates.install')).toEqual([[]]);
    expect(fake.listeners('update.state')).toBe(0);
  });

  it('updateApp: up to date, not installed (opens the notes), another error, a refused check', async () => {
    const { p, fake } = make();
    const run = () => p.versionActions().updateApp?.();
    fake.answers['updates.check'] = updateStatus('up_to_date');
    expect(await run()).toEqual({ kind: 'up-to-date' });
    const notesUrl = 'https://github.com/MoonWX/isshoni/releases/tag/v0.2.1';
    fake.answers['updates.check'] = updateStatus('error', { errorCode: 'not_installed', notesUrl });
    expect(await run()).toEqual({ kind: 'not-installed', notesUrl });
    expect(fake.callsOf('app.openExternal')).toEqual([[notesUrl]]);
    fake.answers['updates.check'] = updateStatus('error', { errorCode: 'manifest_expired' });
    expect(await run()).toEqual({ kind: 'error', code: 'manifest_expired' });
    fake.answers['updates.check'] = updateStatus('off', { policyOff: true });
    expect(await run()).toEqual({ kind: 'error', code: 'off' });
    fake.answers['updates.check'] = () => Promise.reject(new BridgeError('busy'));
    expect(await run()).toEqual({ kind: 'error', code: 'busy' });
    expect(fake.callsOf('updates.install')).toEqual([]);
  });
});

describe('the transport (docs/m2/03 §7.2, §7.4)', () => {
  it('loads HttpBridge on demand for transport "http"', async () => {
    vi.stubGlobal(
      'EventSource',
      class {
        onmessage: unknown = null;
      },
    );
    fetchMock.mockImplementation((input) => {
      const url = urlOf(input);
      if (url.endsWith('/__bridge/call')) return Promise.resolve(json(200, { token: 'isa_http', expiresAt: 4e12 }));
      return Promise.resolve(json(200, {}));
    });
    const p = new DesktopPlatform(config({ transport: 'http' }));
    made.push(p);
    await p.apiFetch('/api/v1/me');
    const call = sent(0);
    expect(call.url).toBe(`${location.origin}/__bridge/call`);
    expect(JSON.parse(call.init.body as string)).toEqual({
      service: 'Auth',
      method: 'AccessToken',
      args: [ORIGIN, false],
    });
    expect(sent(1).auth).toBe('Bearer isa_http');
  });

  it('a Wails build without WailsBridge (WA48) rejects its bridge calls with not_implemented', async () => {
    const p = new DesktopPlatform(config());
    made.push(p);
    await expect(p.desktop.bridge.auth.state()).rejects.toMatchObject({ code: 'not_implemented' });
    await expect(p.apiFetch('/api/v1/info')).rejects.toMatchObject({ code: 'not_implemented' });
    expect(fetchMock).not.toHaveBeenCalled();
  });
});
