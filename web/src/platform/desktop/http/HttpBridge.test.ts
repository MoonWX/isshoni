// HttpBridge (docs/m2/03 §7.4, §22.4 A): the wire that internal/desktop/e2ehost serves. Calls go to
// POST /__bridge/call as {service, method, args} with the bound Go names; errors come back as the bridge error JSON;
// events arrive on /__bridge/events as {name, data}.
import { afterEach, describe, expect, it, vi } from 'vitest';

import type { AuthState } from '../bridge';
import { BridgeError } from '../errors';
import { authState } from '../testing/fakeBridge';
import { BridgeCallPath, BridgeEventsPath, createHttpBridge } from './HttpBridge';

const BASE = 'http://wails.localhost/desktop/setup';

class FakeEventSource {
  static last: FakeEventSource | undefined;
  readonly url: string;
  onmessage: ((ev: MessageEvent) => void) | null = null;
  constructor(url: string) {
    this.url = url;
    FakeEventSource.last = this;
  }
  /** Delivers one SSE message. */
  send(data: string): void {
    this.onmessage?.(new MessageEvent('message', { data }));
  }
}

interface CallBody {
  service: string;
  method: string;
  args: unknown[];
}

/** The JSON body HttpBridge sent (always a string). */
const bodyOf = (init?: RequestInit): CallBody => JSON.parse(init?.body as string) as CallBody;

function setup(reply: (body: CallBody) => Response) {
  const fetchMock = vi.fn((_url: URL | RequestInfo, init?: RequestInit) => Promise.resolve(reply(bodyOf(init))));
  const bridge = createHttpBridge({
    fetch: fetchMock as unknown as typeof fetch,
    EventSource: FakeEventSource as unknown as typeof EventSource,
    base: BASE,
  });
  const es = FakeEventSource.last;
  if (!es) throw new Error('no EventSource');
  return { bridge, fetchMock, es };
}

const ok = (v: unknown): Response =>
  new Response(JSON.stringify(v), { status: 200, headers: { 'Content-Type': 'application/json' } });

afterEach(() => {
  FakeEventSource.last = undefined;
});

describe('HttpBridge', () => {
  it('posts {service, method, args} with the bound Go names to /__bridge/call', async () => {
    const { bridge, fetchMock } = setup(() => ok({ token: 'isa_x', expiresAt: 1 }));
    expect(await bridge.auth.accessToken('https://watch.example.com', true)).toEqual({ token: 'isa_x', expiresAt: 1 });
    await bridge.app.openOSSettings('sound-communications');
    await bridge.audio.setAppDecision('exe:kook.exe', 'auto');
    const bodies = fetchMock.mock.calls.map(([, init]) => bodyOf(init));
    expect(bodies).toEqual([
      { service: 'Auth', method: 'AccessToken', args: ['https://watch.example.com', true] },
      { service: 'App', method: 'OpenOSSettings', args: ['sound-communications'] },
      { service: 'Audio', method: 'SetAppDecision', args: ['exe:kook.exe', 'auto'] },
    ]);
    const [url, init] = fetchMock.mock.calls[0] ?? [];
    expect(url instanceof URL ? url.href : url).toBe(`http://wails.localhost${BridgeCallPath}`);
    expect(init).toMatchObject({ method: 'POST', credentials: 'same-origin' });
    expect(new Headers(init?.headers).get('Content-Type')).toBe('application/json');
  });

  it('leaves out trailing undefined arguments, and answers null and no body alike', async () => {
    const { bridge, fetchMock } = setup(({ method }) =>
      method === 'PendingLink' ? ok(null) : new Response(null, { status: 204 }),
    );
    bridge.app.log('info', 'hello', undefined);
    expect(await bridge.app.pendingLink()).toBeNull();
    await expect(bridge.app.linkHandled()).resolves.toBeNull();
    expect(bodyOf(fetchMock.mock.calls[0]?.[1])).toEqual({ service: 'App', method: 'Log', args: ['info', 'hello'] });
  });

  it('rejects with the bridge error JSON of a failed call', async () => {
    const { bridge } = setup(() => Response.json({ code: 'not_found', params: { origin: 'x' } }, { status: 400 }));
    const err = await bridge.auth.sessionEnded('https://watch.example.com', 'session_revoked').catch((e: unknown) => e);
    expect(err).toBeInstanceOf(BridgeError);
    expect(err).toMatchObject({ code: 'not_found', params: { origin: 'x' } });
  });

  it('rejects with internal for a reply that is not the error JSON, or no reply', async () => {
    const { bridge } = setup(() => new Response('<html>', { status: 502 }));
    await expect(bridge.auth.state()).rejects.toMatchObject({ code: 'internal' });
    const broken = createHttpBridge({
      fetch: () => Promise.reject(new TypeError('Failed to fetch')),
      EventSource: FakeEventSource as unknown as typeof EventSource,
      base: BASE,
    });
    await expect(broken.auth.state()).rejects.toMatchObject({ code: 'internal' });
  });

  it('stamps share.stats with the receipt time', async () => {
    const { bridge, es } = setup(() => ok({ layers: [], audioKbps: 64 }));
    const sample = await bridge.share.stats();
    expect(sample).toMatchObject({ layers: [], audioKbps: 64 });
    expect(typeof sample?.at).toBe('number');
    const got = vi.fn();
    bridge.on('share.stats', got);
    es.send(JSON.stringify({ name: 'isshoni:share.stats', data: { layers: [], audioKbps: 1 } }));
    expect(got).toHaveBeenCalledWith(expect.objectContaining({ audioKbps: 1, at: expect.any(Number) as number }));
  });

  it('opens /__bridge/events at once and delivers isshoni: events to their listeners', () => {
    const { bridge, es } = setup(() => ok(null));
    expect(es.url).toBe(`http://wails.localhost${BridgeEventsPath}`);
    const states: AuthState[] = [];
    const off = bridge.on('auth.state', (s) => states.push(s));
    const other = vi.fn();
    bridge.on('update.state', other);
    const state = authState('https://watch.example.com', 'linked');
    es.send(JSON.stringify({ name: 'isshoni:auth.state', data: state }));
    es.send(JSON.stringify({ name: 'auth.state', data: state })); // no prefix: not ours
    es.send('not json');
    es.send(JSON.stringify({ name: 'isshoni:auth.state', data: state }));
    off();
    es.send(JSON.stringify({ name: 'isshoni:auth.state', data: state }));
    expect(states).toEqual([state, state]);
    expect(other).not.toHaveBeenCalled();
  });

  it('keeps delivering when a listener throws', () => {
    const { bridge, es } = setup(() => ok(null));
    const after = vi.fn();
    bridge.on('window.state', () => {
      throw new Error('boom');
    });
    bridge.on('window.state', after);
    es.send(
      JSON.stringify({ name: 'isshoni:window.state', data: { visible: true, focused: true, fullscreen: false } }),
    );
    expect(after).toHaveBeenCalledTimes(1);
  });
});
