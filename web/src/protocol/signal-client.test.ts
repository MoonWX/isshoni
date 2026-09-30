// SignalClient and ProtocolError (01 §10.1–§10.3, §12, §16; the TypeScript cases of §19) against the fake server
// of ./testing, with Vitest's fake timers: handshake, backoff and its reset rule, skip-wait, ping and probe, resume,
// requests, the close-code and error → state mapping, unknown codes, stale builds, server.shutdown and page events.
import { afterEach, beforeEach, describe, expect, expectTypeOf, it, vi } from 'vitest';

import type * as api from './api.gen';
import {
  isProtocolError,
  LocalErrorCodeConnectionLost,
  LocalErrorCodeNotReady,
  LocalErrorCodeRequestTimeout,
  localErrorCodes,
  ProtocolError,
} from './errors';
import {
  backoffDelay,
  BackoffMaxMs,
  BackoffMinMs,
  isDevVersion,
  isStaleBuild,
  RateLimitMinWaitMs,
  ReconnectCloseCode,
  SignalClient,
  type SignalClientOptions,
  type SignalState,
  type SignalStateInfo,
} from './signal-client';
import { FakeSignalServer, FakeWebSocket, makeError, makeWelcome } from './testing';
import * as P from './types.gen';

const url = 'ws://localhost:5173/ws';
const clientInfo: P.ClientInfo = { kind: 'web', version: '0.1.0', os: 'macos', browser: 'chrome' };
const roomState: P.RoomState = { roomId: 'lounge', rev: 1, participants: [], shares: [] };

let server: FakeSignalServer;
let client: SignalClient | undefined;
let log: { state: SignalState; info: SignalStateInfo }[];

beforeEach(() => {
  vi.useFakeTimers();
  server = FakeSignalServer.install();
  client = undefined;
  log = [];
});

afterEach(() => {
  client?.stop();
  server.uninstall();
  vi.useRealTimers();
  vi.restoreAllMocks();
  Reflect.deleteProperty(document, 'visibilityState');
});

function makeClient(opts: Partial<SignalClientOptions> = {}): SignalClient {
  const c = new SignalClient({
    url,
    client: clientInfo,
    role: 'full',
    caps: () => ({ decode: ['h264/42e0', 'opus'] }),
    ...opts,
  });
  c.onState((state, info) => {
    log.push({ state, info });
  });
  client = c;
  return c;
}

/** Advances the fake clock; the fake server's automatic steps run at +0 ms. */
async function tick(ms = 0): Promise<void> {
  await vi.advanceTimersByTimeAsync(ms);
}

async function readyClient(opts?: Partial<SignalClientOptions>): Promise<SignalClient> {
  const c = makeClient(opts);
  c.start();
  await tick();
  expect(c.state).toBe('ready');
  return c;
}

const states = () => log.map((e) => e.state);
const lastInfo = (): SignalStateInfo => log.at(-1)?.info ?? {};

/** The socket at index i; fails the test when there is none. */
function socketAt(i: number): FakeWebSocket {
  const s = server.sockets[i];
  if (s === undefined) {
    throw new Error(`no socket ${String(i)}`);
  }
  return s;
}

/** Tracks a promise's outcome synchronously (and marks its rejection handled). */
function track<T>(p: Promise<T>): { done: boolean; value?: T; error?: unknown } {
  const r: { done: boolean; value?: T; error?: unknown } = { done: false };
  void p.then(
    (value) => {
      r.done = true;
      r.value = value;
    },
    (error: unknown) => {
      r.done = true;
      r.error = error;
    },
  );
  return r;
}

function setVisibility(state: DocumentVisibilityState): void {
  Object.defineProperty(document, 'visibilityState', { configurable: true, get: () => state });
  document.dispatchEvent(new Event('visibilitychange'));
}

function pageShow(persisted: boolean): Event {
  return typeof PageTransitionEvent === 'function'
    ? new PageTransitionEvent('pageshow', { persisted })
    : Object.assign(new Event('pageshow'), { persisted });
}

describe('handshake', () => {
  it('goes connecting → handshaking → ready and sends hello first', async () => {
    const c = makeClient({ role: 'viewer', features: ['agent.relay'] });
    expect(c.state).toBe('stopped');
    c.start();
    expect(c.state).toBe('connecting');
    expect(socketAt(0).url).toBe(url);
    await tick();
    expect(states()).toEqual(['connecting', 'handshaking', 'ready']);
    const [hello] = server.messages();
    expect(hello?.type).toBe('hello');
    expect(hello?.id).toMatch(/^[A-Za-z0-9_-]{1,32}$/);
    expect(hello?.data).toEqual({
      protocol: P.Version,
      minProtocol: P.MinVersion,
      features: ['agent.relay'],
      client: clientInfo,
      role: 'viewer',
      caps: { decode: ['h264/42e0', 'opus'] },
    });
    expect(c.welcome).toEqual(server.welcomes[0]);
    expect(lastInfo()).toEqual({ resumed: false, staleBuild: false });
  });

  it('sends features: [] when none are given', async () => {
    await readyClient();
    expect(server.hellos[0]?.features).toEqual([]);
  });

  it('re-reads caps on every (re)connect', async () => {
    let calls = 0;
    await readyClient({ caps: () => ({ decode: calls++ === 0 ? [] : ['opus'] }) });
    server.drop();
    await tick(BackoffMaxMs);
    expect(server.hellos.map((h) => h.caps.decode)).toEqual([[], ['opus']]);
  });

  it('calls onResync with the state already ready, before the ready listeners', async () => {
    const order: string[] = [];
    const c = makeClient({
      onResync: (w) => {
        order.push(`resync:${c.state}:${w.connectionId}`);
        expect(c.notify('caps.update', { caps: { decode: [] } })).toBe(true);
      },
    });
    c.onState((s) => order.push(s));
    c.start();
    await tick();
    const id = server.welcomes[0]?.connectionId ?? '';
    expect(order).toEqual(['connecting', 'handshaking', `resync:ready:${id}`, 'ready']);
    expect(server.messages('caps.update')).toHaveLength(1);
  });

  it('calls onResync after every welcome and logs its rejection', async () => {
    const error = vi.spyOn(console, 'error').mockImplementation(() => undefined);
    const onResync = vi.fn(() => Promise.reject(new Error('boom')));
    const c = await readyClient({ onResync });
    server.drop();
    await tick(BackoffMaxMs);
    expect(c.state).toBe('ready');
    expect(onResync).toHaveBeenCalledTimes(2);
    expect(error).toHaveBeenCalledWith('SignalClient: onResync failed', expect.any(Error));
  });

  it('reports no ready when onResync stops the client', async () => {
    const c = makeClient({
      onResync: () => {
        c.stop();
      },
    });
    c.start();
    await tick();
    expect(states()).toEqual(['connecting', 'handshaking', 'stopped']);
  });

  it('backs off when no welcome comes within 10 s', async () => {
    server.autoWelcome = false;
    const c = makeClient();
    c.start();
    await tick();
    expect(c.state).toBe('handshaking');
    await tick(9_999);
    expect(c.state).toBe('handshaking');
    await tick(1);
    expect(c.state).toBe('backoff');
    expect(lastInfo().error).toBeUndefined();
    expect(socketAt(0).clientClose?.code).toBe(ReconnectCloseCode);
  });

  it('backs off when the socket does not open within 10 s', async () => {
    server.autoOpen = false;
    const c = makeClient();
    c.start();
    await tick(9_999);
    expect(c.state).toBe('connecting');
    await tick(1);
    expect(c.state).toBe('backoff');
  });

  it('ignores a welcome that does not answer its hello', async () => {
    server.autoWelcome = false;
    const c = makeClient();
    c.start();
    await tick();
    socketAt(0).deliver({ type: 'welcome', re: 'nope', data: makeWelcome() });
    expect(c.state).toBe('handshaking');
    server.welcome();
    expect(c.state).toBe('ready');
  });

  it('keeps the token in memory only', async () => {
    await readyClient();
    expect(localStorage.length).toBe(0);
    expect(sessionStorage.length).toBe(0);
  });
});

describe('backoff', () => {
  it('computes min(10 s, 0.5 s × 2ⁿ) × U(0.8, 1.2), clamped to [0.5 s, 10 s]', () => {
    const nominal = [500, 1000, 2000, 4000, 8000, 10000, 10000, 10000];
    nominal.forEach((ms, n) => {
      expect(backoffDelay(n, () => 0.5)).toBeCloseTo(ms);
      expect(backoffDelay(n, () => 0)).toBeCloseTo(Math.max(BackoffMinMs, ms * 0.8));
      expect(backoffDelay(n, () => 0.999999)).toBeCloseTo(Math.min(BackoffMaxMs, ms * 1.2), 1);
    });
    for (let i = 0; i < 2000; i++) {
      const n = i % 12;
      const base = Math.min(BackoffMaxMs, BackoffMinMs * 2 ** n);
      const d = backoffDelay(n);
      expect(d).toBeGreaterThanOrEqual(Math.max(BackoffMinMs, base * 0.8));
      expect(d).toBeLessThanOrEqual(Math.min(BackoffMaxMs, base * 1.2));
    }
    expect(backoffDelay(1000)).toBeLessThanOrEqual(BackoffMaxMs);
  });

  it('waits about 0.5, 1, 2, 4, 8, 10, 10 s between failed attempts', async () => {
    vi.spyOn(Math, 'random').mockReturnValue(0.5);
    const c = await readyClient();
    server.autoOpen = false;
    for (const delay of [500, 1000, 2000, 4000, 8000, 10000, 10000]) {
      const count = server.sockets.length;
      server.drop();
      expect(c.state).toBe('backoff');
      expect(lastInfo()).toMatchObject({ delayMs: delay, rateLimited: false });
      await tick(delay - 1);
      expect(server.sockets).toHaveLength(count);
      await tick(1);
      expect(server.sockets).toHaveLength(count + 1);
      expect(c.state).toBe('connecting');
    }
  });

  it('applies the jitter to each wait', async () => {
    vi.spyOn(Math, 'random').mockReturnValue(0.999999);
    const c = await readyClient();
    server.autoOpen = false;
    server.drop();
    expect(lastInfo().delayMs).toBeCloseTo(600);
    await tick(600);
    server.drop();
    expect(c.state).toBe('backoff');
    expect(lastInfo().delayMs).toBeCloseTo(1200);
  });

  it('resets the attempt counter only after 10 s ready', async () => {
    vi.spyOn(Math, 'random').mockReturnValue(0.5);
    const c = await readyClient();
    server.drop();
    expect(lastInfo().delayMs).toBe(500);
    await tick(500);
    expect(c.state).toBe('ready');
    await tick(9_999); // a crash-looping server keeps the backoff high
    server.drop();
    expect(lastInfo().delayMs).toBe(1000);
    await tick(1000);
    expect(c.state).toBe('ready');
    await tick(10_000);
    server.drop();
    expect(lastInfo().delayMs).toBe(500);
  });

  it('skips the wait once on online', async () => {
    vi.spyOn(Math, 'random').mockReturnValue(0.5);
    const c = await readyClient();
    server.autoOpen = false;
    server.drop();
    await tick(500);
    server.drop();
    await tick(1000);
    server.drop();
    expect(lastInfo().delayMs).toBe(2000);
    window.dispatchEvent(new Event('online'));
    expect(c.state).toBe('connecting');
    expect(server.sockets).toHaveLength(4);
    server.drop();
    expect(lastInfo().delayMs).toBe(4000); // skipping doesn't reset the sequence
  });

  it('skips the wait when the page becomes visible', async () => {
    const c = await readyClient();
    server.autoOpen = false;
    server.drop();
    setVisibility('hidden');
    expect(c.state).toBe('backoff');
    setVisibility('visible');
    expect(c.state).toBe('connecting');
  });

  it('retryNow() skips the wait, and does nothing outside backoff', async () => {
    const c = makeClient();
    c.retryNow();
    expect(c.state).toBe('stopped');
    c.start();
    c.retryNow();
    expect(server.sockets).toHaveLength(1);
    await tick();
    c.retryNow();
    expect(c.state).toBe('ready');
    server.autoOpen = false;
    server.drop();
    expect(c.state).toBe('backoff');
    c.retryNow();
    expect(c.state).toBe('connecting');
    expect(server.sockets).toHaveLength(2);
  });
});

describe('heartbeat', () => {
  it('pings every limits.pingIntervalMs and stays ready while pongs come', async () => {
    const c = await readyClient();
    await tick(14_999);
    expect(server.messages('ping')).toHaveLength(0);
    await tick(1);
    const [ping] = server.messages('ping');
    expect(ping?.data).toEqual({ t: Date.now() });
    expect(ping?.id).toBeUndefined();
    await tick(45_000);
    expect(server.messages('ping')).toHaveLength(4);
    expect(c.state).toBe('ready');
  });

  it('uses the ping interval of the welcome', async () => {
    server.welcomeDefaults = { limits: { ...makeWelcome().limits, pingIntervalMs: 5000 } };
    await readyClient();
    await tick(5000);
    expect(server.messages('ping')).toHaveLength(1);
  });

  it('closes the socket and reconnects when a ping gets no pong within 10 s', async () => {
    const c = await readyClient();
    server.autoPong = false;
    await tick(15_000);
    expect(server.messages('ping')).toHaveLength(1);
    await tick(9_999);
    expect(c.state).toBe('ready');
    await tick(1);
    expect(c.state).toBe('backoff');
    expect(socketAt(0).clientClose?.code).toBe(ReconnectCloseCode);
    server.autoPong = true;
    await tick(BackoffMaxMs);
    expect(c.state).toBe('ready');
    expect(server.welcomes[1]?.resumed).toBe(true);
  });

  it('probe() pings at once with a 3 s timeout', async () => {
    const c = await readyClient();
    c.probe();
    expect(server.messages('ping')).toHaveLength(1);
    await tick(3_000);
    expect(c.state).toBe('ready'); // the pong came
    server.autoPong = false;
    c.probe();
    await tick(2_999);
    expect(c.state).toBe('ready');
    await tick(1);
    expect(c.state).toBe('backoff');
  });

  it('probe() shortens the deadline of a ping already out', async () => {
    const c = await readyClient();
    server.autoPong = false;
    await tick(15_000); // periodic ping: pong due at +10 s
    await tick(1_000);
    c.probe(); // pong due at +3 s now
    await tick(2_999);
    expect(c.state).toBe('ready');
    await tick(1);
    expect(c.state).toBe('backoff');
  });

  it('probe() never lengthens the deadline of a ping already out', async () => {
    const c = await readyClient();
    server.autoPong = false;
    await tick(15_000); // periodic ping: pong due at +10 s
    await tick(8_000);
    c.probe(); // +3 s would be later than the pong already due in 2 s
    await tick(1_999);
    expect(c.state).toBe('ready');
    await tick(1);
    expect(c.state).toBe('backoff');
  });

  it('a pong clears the deadline of every ping out', async () => {
    const c = await readyClient();
    server.autoPong = false;
    await tick(15_000);
    c.probe();
    server.send('pong', { t: 0, serverTimeMs: 0 });
    await tick(14_999);
    expect(c.state).toBe('ready');
  });

  it('probe() is a no-op unless ready', async () => {
    server.autoWelcome = false;
    const c = makeClient();
    c.probe();
    c.start();
    await tick();
    c.probe();
    expect(server.messages('ping')).toHaveLength(0);
  });

  it('pings at once when the page becomes visible or the browser goes online', async () => {
    const c = await readyClient();
    server.autoPong = false;
    setVisibility('visible');
    expect(server.messages('ping')).toHaveLength(1);
    await tick(3_000);
    expect(c.state).toBe('backoff');
    server.autoPong = true;
    await tick(BackoffMaxMs);
    expect(c.state).toBe('ready');
    server.autoPong = false;
    window.dispatchEvent(new Event('online'));
    expect(server.messages('ping', server.socket)).toHaveLength(1);
    await tick(3_000);
    expect(c.state).toBe('backoff');
  });
});

describe('resume', () => {
  it('sends the token of the last welcome and gets a new one each time', async () => {
    const c = await readyClient();
    const first = server.welcomes[0];
    expect(server.hellos[0]?.resumeToken).toBeUndefined();
    server.drop();
    await tick(BackoffMaxMs);
    expect(c.state).toBe('ready');
    expect(server.hellos[1]?.resumeToken).toBe(first?.resumeToken);
    const second = server.welcomes[1];
    expect(second).toMatchObject({ resumed: true, connectionId: first?.connectionId });
    expect(second?.resumeToken).not.toBe(first?.resumeToken);
    expect(lastInfo()).toMatchObject({ resumed: true });
    expect(c.welcome).toEqual(second);
    server.drop();
    await tick(BackoffMaxMs);
    expect(server.hellos[2]?.resumeToken).toBe(second?.resumeToken);
  });

  it('reports resumed: false when the server lost the connection', async () => {
    const c = await readyClient();
    server.restart();
    server.drop();
    await tick(BackoffMaxMs);
    expect(c.state).toBe('ready');
    expect(server.welcomes[1]?.resumed).toBe(false);
    expect(server.welcomes[1]?.connectionId).not.toBe(server.welcomes[0]?.connectionId);
    expect(lastInfo()).toMatchObject({ resumed: false });
  });

  it('starts a new connection after stop() and start()', async () => {
    const c = await readyClient();
    c.stop();
    expect(c.state).toBe('stopped');
    expect(socketAt(0).clientClose?.code).toBe(1000);
    c.start();
    await tick();
    expect(server.hellos[1]?.resumeToken).toBeUndefined();
    expect(lastInfo()).toMatchObject({ resumed: false });
  });
});

describe('requests', () => {
  it('resolves with the payload of the ok', async () => {
    const c = await readyClient();
    server.handle('room.join', ({ roomId }) => ({ ok: { room: { id: roomId, name: 'Lounge' } } }));
    const joined = await Promise.all([c.request('room.join', { roomId: 'lounge' }), tick()]);
    expect(joined[0]).toEqual({ room: { id: 'lounge', name: 'Lounge' } });
    const req = server.last('room.join');
    expect(req).toMatchObject({ data: { roomId: 'lounge' } });
    expect(req.id).toMatch(/^[A-Za-z0-9_-]{1,32}$/);
  });

  it('resolves {} for an ok without data, and gives each request its own id', async () => {
    const c = await readyClient();
    const a = track(c.request('room.leave', {}));
    const b = track(c.request('room.leave', {}));
    const [ra, rb] = server.messages('room.leave');
    expect(ra?.id).not.toBe(rb?.id);
    socketAt(0).deliver({ type: 'ok', re: rb?.id });
    await tick();
    expect(b.value).toEqual({});
    expect(a.done).toBe(false);
  });

  it('rejects with the server error', async () => {
    const c = await readyClient();
    const r = track(c.request('share.start', { kind: 'screen', preset: 'auto', audio: true, ref: 'k2j9' }));
    server.replyError(
      server.last('share.start'),
      makeError('share_limit', 'request', { params: { limit: 4, per: 'user' } }),
    );
    await tick();
    expect(r.error).toBeInstanceOf(ProtocolError);
    expect(r.error).toMatchObject({
      code: 'share_limit',
      scope: 'request',
      retryable: false,
      params: { limit: 4, per: 'user' },
      local: false,
    });
    expect(c.state).toBe('ready');
  });

  it('rejects with connection_lost when the socket goes away before the reply, and never retries', async () => {
    const c = await readyClient();
    const r = track(c.request('room.join', { roomId: 'lounge' }));
    server.drop();
    await tick();
    expect(r.error).toMatchObject({ code: LocalErrorCodeConnectionLost, local: true, retryable: true });
    await tick(BackoffMaxMs);
    expect(c.state).toBe('ready');
    expect(server.messages('room.join')).toHaveLength(1);
  });

  it('rejects at once with connection_lost while not ready and not_ready while stopped', async () => {
    const c = makeClient();
    const before = track(c.request('room.leave', {}));
    server.autoWelcome = false;
    c.start();
    const connecting = track(c.request('room.leave', {}));
    await tick();
    const handshaking = track(c.request('room.leave', {}));
    server.welcome();
    server.drop();
    const backoff = track(c.request('room.leave', {}));
    c.stop();
    const after = track(c.request('room.leave', {}));
    await tick();
    expect(before.error).toMatchObject({ code: LocalErrorCodeNotReady, retryable: false });
    expect(connecting.error).toMatchObject({ code: LocalErrorCodeConnectionLost });
    expect(handshaking.error).toMatchObject({ code: LocalErrorCodeConnectionLost });
    expect(backoff.error).toMatchObject({ code: LocalErrorCodeConnectionLost });
    expect(after.error).toMatchObject({ code: LocalErrorCodeNotReady });
    expect(server.messages('room.leave')).toHaveLength(0);
  });

  it('rejects with request_timeout after 10 s and ignores a late reply', async () => {
    const c = await readyClient();
    const r = track(c.request('stats.watch', { on: true }));
    await tick(9_999);
    expect(r.done).toBe(false);
    await tick(1);
    expect(r.error).toMatchObject({ code: LocalErrorCodeRequestTimeout, local: true, retryable: true });
    server.reply(server.last('stats.watch'));
    await tick();
    expect(c.state).toBe('ready');
  });

  it('takes a custom timeout', async () => {
    const c = await readyClient();
    const r = track(c.request('stats.watch', { on: true }, { timeoutMs: 500 }));
    await tick(500);
    expect(r.error).toMatchObject({ code: LocalErrorCodeRequestTimeout });
  });

  it('notify() sends only while ready', async () => {
    const c = makeClient();
    expect(c.notify('pc.close', { pc: 'pub', gen: 1 })).toBe(false);
    c.start();
    await tick();
    expect(c.notify('pc.close', { pc: 'pub', gen: 1 })).toBe(true);
    expect(server.last('pc.close')).toEqual({ type: 'pc.close', data: { pc: 'pub', gen: 1 } });
    server.autoOpen = false;
    server.drop();
    expect(c.notify('pc.close', { pc: 'pub', gen: 1 })).toBe(false);
    expect(server.messages('pc.close')).toHaveLength(1);
  });
});

describe('messages', () => {
  it('delivers notifications to on() listeners until unsubscribed', async () => {
    const c = await readyClient();
    const got: P.RoomState[] = [];
    const off = c.on('room.state', (data, env) => {
      expect(env).toEqual({ type: 'room.state', data });
      got.push(data);
    });
    server.send('room.state', roomState);
    off();
    server.send('room.state', roomState);
    expect(got).toEqual([roomState]);
  });

  it('fills in an absent data with {}', async () => {
    const c = await readyClient();
    const got: unknown[] = [];
    c.on('invalidate', (data) => got.push(data));
    socketAt(0).deliver({ type: 'invalidate' });
    expect(got).toEqual([{}]);
  });

  it('keeps delivering when a listener throws', async () => {
    const error = vi.spyOn(console, 'error').mockImplementation(() => undefined);
    const c = await readyClient();
    const second = vi.fn();
    c.on('room.state', () => {
      throw new Error('listener bug');
    });
    c.on('room.state', second);
    server.send('room.state', roomState);
    expect(second).toHaveBeenCalledTimes(1);
    expect(error).toHaveBeenCalledWith('SignalClient: a room.state listener failed', expect.any(Error));
    expect(c.state).toBe('ready');
  });

  it('drops unknown types and malformed frames', async () => {
    const error = vi.spyOn(console, 'error').mockImplementation(() => undefined);
    const c = await readyClient();
    const any = vi.fn();
    c.on('room.state', any);
    const s = socketAt(0);
    s.deliver({ type: 'future.thing', data: {} });
    s.deliver('not json');
    s.deliver('[1]');
    s.deliver({ no: 'type' });
    s.deliver({ type: 'room.state', data: 5 });
    expect(any).not.toHaveBeenCalled();
    expect(error).toHaveBeenCalledTimes(4);
    expect(c.state).toBe('ready');
  });

  it('gives replies to request() only', async () => {
    const c = await readyClient();
    const ok = vi.fn();
    const err = vi.fn();
    c.on('ok', ok);
    c.on('error', err);
    const a = track(c.request('room.leave', {}));
    const b = track(c.request('room.leave', {}));
    const [ra, rb] = server.messages('room.leave');
    server.reply(ra ?? '');
    server.replyError(rb ?? '', makeError('internal', 'request', { params: { ref: 'ab12cd34' } }));
    await tick();
    expect(a.value).toEqual({});
    expect(b.error).toMatchObject({ code: 'internal', retryable: true, params: { ref: 'ab12cd34' } });
    expect(ok).not.toHaveBeenCalled();
    expect(err).not.toHaveBeenCalled();
  });

  it('delivers error notifications of the other scopes to on("error")', async () => {
    const c = await readyClient();
    const got: P.Error[] = [];
    c.on('error', (data) => got.push(data));
    const pc = makeError('sdp_invalid', 'pc', { pc: 'sub', gen: 2, neg: 3 });
    const share = makeError('codec_not_supported', 'share', { shareId: 's_q7m2x9c4v8b1n5k3' });
    const room = makeError('room_closed', 'room', { roomId: 'lounge' });
    for (const e of [pc, share, room]) {
      server.error(e);
    }
    expect(got).toEqual([pc, share, room]);
    expect(c.state).toBe('ready');
  });
});

describe('close codes and errors → state (01 §12.2)', () => {
  it.each([
    [1000, 'backoff', undefined],
    [1001, 'backoff', undefined],
    [1006, 'backoff', undefined],
    [1009, 'backoff', undefined],
    [1011, 'backoff', undefined],
    [1012, 'backoff', undefined],
    [4408, 'backoff', undefined],
    [4503, 'backoff', undefined],
    [4999, 'backoff', undefined], // a code this build doesn't know
    [4429, 'backoff', 'rate_limited'],
    [1003, 'stopped', 'bad_message'],
    [4400, 'stopped', 'bad_message'],
    [4401, 'stopped', 'unauthenticated'],
    [4403, 'stopped', 'forbidden'],
    [4409, 'stopped', 'replaced'],
    [4426, 'stopped', 'protocol_unsupported'],
  ] as const)('close %i without an error → %s (%s)', async (code, state, errorCode) => {
    const c = await readyClient();
    server.close(code);
    expect(c.state).toBe(state);
    expect(lastInfo().error?.code).toBe(errorCode);
    if (errorCode !== undefined) {
      expect(lastInfo().error).toMatchObject({ closeCode: code, local: false });
    }
  });

  it.each([
    ['unauthenticated', 'session', 4401, 'stopped'],
    ['session_revoked', 'session', 4401, 'stopped'],
    ['account_disabled', 'session', 4403, 'stopped'],
    ['too_many_connections', 'connection', 4429, 'stopped'],
    ['protocol_unsupported', 'connection', 4426, 'stopped'],
    ['client_outdated', 'connection', 4426, 'stopped'],
    ['replaced', 'connection', 4409, 'stopped'],
    ['bad_message', 'connection', 4400, 'stopped'],
    ['hello_required', 'connection', 4400, 'stopped'],
    ['bad_request', 'connection', 4400, 'stopped'],
    ['hello_timeout', 'connection', 4408, 'backoff'],
    ['idle_timeout', 'connection', 4408, 'backoff'],
    ['slow_connection', 'connection', 4503, 'backoff'],
    ['internal', 'connection', 1011, 'backoff'],
    ['server_shutdown', 'connection', 1012, 'backoff'],
  ] as const)('error %s (%s) then close %i → %s', async (code, scope, closeCode, state) => {
    const c = await readyClient();
    server.error(makeError(code, scope));
    expect(c.state).toBe(state); // the error decides at once; the close that follows changes nothing
    server.close(closeCode);
    expect(c.state).toBe(state);
    expect(lastInfo().error).toMatchObject({ code, scope, closeCode: undefined });
    expect(socketAt(0).clientClose?.code).toBe(ReconnectCloseCode);
  });

  it('stops on errors that answer hello', async () => {
    server.autoWelcome = false;
    const c = makeClient();
    c.start();
    await tick();
    const params = { serverMin: 2, serverMax: 2, serverVersion: '0.9.0' };
    server.replyError(server.last('hello'), makeError('protocol_unsupported', 'connection', { params }));
    server.close(4426);
    expect(c.state).toBe('stopped');
    expect(lastInfo().error).toMatchObject({ code: 'protocol_unsupported', params });
  });

  it('stops listening to page events once stopped by an error', async () => {
    const c = await readyClient();
    server.error(makeError('session_revoked', 'session'));
    expect(c.state).toBe('stopped');
    window.dispatchEvent(new Event('online'));
    window.dispatchEvent(pageShow(true));
    await tick(BackoffMaxMs);
    expect(server.sockets).toHaveLength(1);
    c.start(); // only start() leaves stopped
    await tick();
    expect(c.state).toBe('ready');
    expect(server.hellos[1]?.resumeToken).toBeUndefined();
  });

  it('waits max(30 s, retryAfterMs) after a rate limit, and nothing shortens it', async () => {
    const c = await readyClient();
    server.error(makeError('rate_limited', 'connection', { retryAfterMs: 45_000 }));
    server.close(4429);
    expect(lastInfo()).toMatchObject({ delayMs: 45_000, rateLimited: true, error: { code: 'rate_limited' } });
    c.retryNow();
    window.dispatchEvent(new Event('online'));
    setVisibility('visible');
    await tick(44_999);
    expect(server.sockets).toHaveLength(1);
    await tick(1);
    expect(c.state).toBe('ready');
    server.error(makeError('rate_limited', 'connection', { retryAfterMs: 1000 }));
    expect(lastInfo()).toMatchObject({ delayMs: RateLimitMinWaitMs, rateLimited: true });
  });

  it('waits at least 30 s after close 4429 without an error', async () => {
    const c = await readyClient();
    server.close(4429);
    expect(lastInfo()).toMatchObject({ delayMs: RateLimitMinWaitMs, rateLimited: true });
    c.retryNow();
    expect(c.state).toBe('backoff');
  });

  it('does not treat a request-scope rate limit as a connection one', async () => {
    const c = await readyClient();
    const r = track(c.request('room.join', { roomId: 'lounge' }));
    server.replyError(server.last('room.join'), makeError('rate_limited', 'request', { retryAfterMs: 2000 }));
    await tick();
    expect(r.error).toMatchObject({ code: 'rate_limited', retryAfterMs: 2000 });
    expect(c.state).toBe('ready');
  });
});

describe('unknown error codes (01 §12.3)', () => {
  it.each([
    ['connection', true, 'backoff'],
    ['connection', false, 'stopped'],
    ['session', false, 'stopped'],
    ['session', true, 'stopped'],
  ] as const)('an unknown code in scope %s, retryable %s → %s', async (scope, retryable, state) => {
    const c = await readyClient();
    server.error(makeError('brand_new_code', scope, { retryable }));
    expect(c.state).toBe(state);
    expect(lastInfo().error).toMatchObject({ code: 'brand_new_code', scope, retryable });
  });

  it('rejects the request with an unknown code in scope request', async () => {
    const c = await readyClient();
    const r = track(c.request('room.leave', {}));
    server.replyError(server.last('room.leave'), makeError('brand_new_code', 'request', { retryable: true }));
    await tick();
    expect(isProtocolError(r.error)).toBe(true);
    expect(r.error).toMatchObject({ code: 'brand_new_code', scope: 'request', retryable: true });
    expect(c.state).toBe('ready');
  });

  it('hands unknown codes and scopes of notifications to on("error")', async () => {
    const c = await readyClient();
    const got: string[] = [];
    c.on('error', (e) => got.push(`${e.code}/${e.scope}`));
    server.error(makeError('brand_new_code', 'room'));
    server.error(makeError('brand_new_code', 'galaxy'));
    expect(got).toEqual(['brand_new_code/room', 'brand_new_code/galaxy']);
    expect(c.state).toBe('ready');
  });

  it('stops when hello gets a non-retryable error of any scope, and backs off on a retryable one', async () => {
    server.autoWelcome = false;
    const c = makeClient();
    c.start();
    await tick();
    server.replyError(server.last('hello'), makeError('brand_new_code', 'request', { retryable: true }));
    expect(c.state).toBe('backoff');
    await tick(BackoffMaxMs);
    server.replyError(server.last('hello'), makeError('brand_new_code', 'request', { retryable: false }));
    expect(c.state).toBe('stopped');
  });
});

describe('staleBuild (01 §6.1)', () => {
  it.each([
    ['0.1.0', '0.1.1', true],
    ['0.1.1', '0.1.0', true],
    ['0.3.0', '0.3.1-rc.1', true],
    ['0.1.0', '0.1.0', false],
    ['0.2.0-dev', '0.1.0', false],
    ['0.1.0', '0.0.0-dev+1a2b3c4', false],
    ['0.1.0', '0.1.1-dev.1a2b3c4', false],
    ['0.1.0', '', false],
  ])('build %s, server %s → staleBuild %s', async (build, serverVersion, stale) => {
    server.welcomeDefaults = { serverVersion };
    await readyClient({ buildVersion: build });
    expect(log.at(-1)).toEqual({ state: 'ready', info: { resumed: false, staleBuild: stale } });
    expect(isStaleBuild(serverVersion, build)).toBe(stale);
  });

  it('compares with this build version by default', async () => {
    server.welcomeDefaults = { serverVersion: '9.9.9' };
    await readyClient();
    expect(lastInfo().staleBuild).toBe(!isDevVersion(__ISSHONI_VERSION__) && __ISSHONI_VERSION__ !== '9.9.9');
  });

  it('reports it on a resumed ready too', async () => {
    server.welcomeDefaults = { serverVersion: '0.2.0' };
    await readyClient({ buildVersion: '0.1.0' });
    server.drop();
    await tick(BackoffMaxMs);
    expect(lastInfo()).toEqual({ resumed: true, staleBuild: true });
  });

  it.each([
    ['0.0.0-dev', true],
    ['0.0.0-dev+1a2b3c4', true],
    ['0.0.0-dev+1a2b3c4d5e6f-dirty', true],
    ['0.1.1-dev.1a2b3c4', true],
    ['0.3.0', false],
    ['0.3.1-rc.1', false],
    ['0.0.0-ci.42', false],
    ['0.0.0-e2e.local', false],
    ['0.0.0-dryrun.7', false],
    ['1.0.0+dev', false],
    ['', false],
  ])('isDevVersion(%j) is %s, like Go version.IsDev', (v, dev) => {
    expect(isDevVersion(v)).toBe(dev);
  });
});

describe('server.shutdown', () => {
  it('waits exactly reconnectInMs, then the normal sequence, with info.shutdown until ready', async () => {
    vi.spyOn(Math, 'random').mockReturnValue(0.999999);
    const c = await readyClient();
    await tick(10_000); // ready long enough: n = 0
    const shutdownMsgs: P.ServerShutdown[] = [];
    c.on('server.shutdown', (d) => shutdownMsgs.push(d));
    server.shutdown(1840);
    const shutdown = { reason: 'restart', reconnectInMs: 1840 };
    expect(shutdownMsgs).toEqual([shutdown]);
    expect(c.state).toBe('backoff');
    expect(lastInfo()).toMatchObject({
      delayMs: 1840,
      rateLimited: false,
      shutdown,
      error: { code: 'server_shutdown' },
    });
    server.autoOpen = false;
    await tick(1839);
    expect(server.sockets).toHaveLength(1);
    await tick(1);
    expect(server.sockets).toHaveLength(2);
    expect(log.at(-1)).toEqual({ state: 'connecting', info: { shutdown } });
    server.drop(); // the new process isn't listening yet
    expect(lastInfo()).toMatchObject({ delayMs: backoffDelay(0, () => 0.999999), shutdown });
    server.autoOpen = true;
    await tick(BackoffMaxMs);
    expect(c.state).toBe('ready');
    const handshaking = log.filter((e) => e.state === 'handshaking').at(-1);
    expect(handshaking?.info).toEqual({ shutdown });
    expect(lastInfo().shutdown).toBeUndefined();
    server.drop();
    expect(lastInfo().shutdown).toBeUndefined();
  });

  it('lets online skip a shutdown wait', async () => {
    const c = await readyClient();
    server.shutdown(3000);
    window.dispatchEvent(new Event('online'));
    expect(c.state).toBe('connecting');
  });
});

describe('page lifecycle', () => {
  it('closes with 1000 on pagehide and starts a new connection on a bfcache pageshow', async () => {
    const c = await readyClient();
    const r = track(c.request('room.join', { roomId: 'lounge' }));
    window.dispatchEvent(new Event('pagehide'));
    expect(c.state).toBe('stopped');
    expect(lastInfo()).toEqual({});
    expect(socketAt(0).clientClose?.code).toBe(1000);
    await tick();
    expect(r.error).toMatchObject({ code: LocalErrorCodeConnectionLost });
    window.dispatchEvent(pageShow(false));
    expect(c.state).toBe('stopped');
    window.dispatchEvent(pageShow(true));
    await tick();
    expect(c.state).toBe('ready');
    expect(server.hellos[1]?.resumeToken).toBeUndefined();
    expect(lastInfo()).toMatchObject({ resumed: false });
  });

  it('does not restart on pageshow after stop()', async () => {
    const c = await readyClient();
    c.stop();
    window.dispatchEvent(pageShow(true));
    await tick();
    expect(c.state).toBe('stopped');
    expect(server.sockets).toHaveLength(1);
  });

  it('stop() fails pending requests and the backoff timer', async () => {
    const c = await readyClient();
    server.drop();
    c.stop();
    await tick(BackoffMaxMs);
    expect(server.sockets).toHaveLength(1);
    expect(states().at(-1)).toBe('stopped');
  });
});

describe('ProtocolError', () => {
  it('reads a wire error', () => {
    const e = ProtocolError.fromWire(
      makeError('sdp_invalid', 'pc', { pc: 'sub', gen: 2, neg: 3, retryAfterMs: 0, params: { field: 'tracks' } }),
    );
    expect(e).toBeInstanceOf(Error);
    expect(e.name).toBe('ProtocolError');
    expect(e).toMatchObject({ code: 'sdp_invalid', scope: 'pc', retryable: false, pc: 'sub', gen: 2, neg: 3 });
    expect(e.params).toEqual({ field: 'tracks' });
    expect(e.local).toBe(false);
    expect(e.message).toContain('sdp_invalid');
  });

  it('tolerates a malformed payload', () => {
    for (const data of [undefined, null, 'x', [], { code: 5, scope: {}, retryable: 'yes', params: [1], gen: -1 }]) {
      const e = ProtocolError.fromWire(data);
      expect(e).toMatchObject({ code: 'internal', scope: 'request', retryable: false, params: {}, gen: undefined });
    }
  });

  it('makes the client-local errors of 01 §12.4', () => {
    expect(localErrorCodes).toEqual(['connection_lost', 'request_timeout', 'not_ready']);
    for (const code of localErrorCodes) {
      const e = ProtocolError.local(code);
      expect(e).toMatchObject({ code, scope: 'request', local: true, retryable: code !== 'not_ready' });
    }
  });

  it('maps the close codes of 01 §12.2 whose action is not a plain backoff', () => {
    const mapped = [1000, 1001, 1003, 1006, 1009, 1011, 1012, 4400, 4401, 4403, 4408, 4409, 4426, 4429, 4503].map(
      (code) => {
        const e = ProtocolError.fromCloseCode(code);
        return e === undefined
          ? `${String(code)}:backoff`
          : `${String(code)}:${e.code}/${e.scope}/${String(e.retryable)}`;
      },
    );
    expect(mapped).toEqual([
      '1000:backoff',
      '1001:backoff',
      '1003:bad_message/connection/false',
      '1006:backoff',
      '1009:backoff',
      '1011:backoff',
      '1012:backoff',
      '4400:bad_message/connection/false',
      '4401:unauthenticated/session/false',
      '4403:forbidden/connection/false',
      '4408:backoff',
      '4409:replaced/connection/false',
      '4426:protocol_unsupported/connection/false',
      '4429:rate_limited/connection/true',
      '4503:backoff',
    ]);
  });
});

describe('FakeWebSocket', () => {
  it('follows the browser rules for send and close', async () => {
    const s = new FakeWebSocket(url);
    expect(() => {
      s.send('x');
    }).toThrow(DOMException);
    s.accept();
    expect(() => {
      s.close(1001);
    }).toThrow(DOMException);
    const events: string[] = [];
    s.onclose = (ev) => events.push(`onclose ${String(ev.code)}`);
    s.addEventListener('close', () => events.push('listener'));
    s.close(4000);
    expect(s.readyState).toBe(FakeWebSocket.CLOSING);
    expect(events).toEqual([]);
    await tick();
    expect(events).toEqual(['onclose 4000', 'listener']);
    expect(s.closeEvent).toEqual({ code: 4000, reason: '', wasClean: true });
  });
});

describe('FakeSignalServer', () => {
  it('answers handled requests with ok or error, and fills in welcome overrides', async () => {
    server.welcomeDefaults = { defaultRoomId: 'den', features: ['agent.relay'] };
    const c = await readyClient();
    expect(c.welcome).toMatchObject({ defaultRoomId: 'den', features: ['agent.relay'], resumed: false });
    server.handle('share.stop', ({ shareId }) =>
      shareId === 's_known' ? { ok: {} } : { error: makeError('share_not_found', 'request') },
    );
    const ok = track(c.request('share.stop', { shareId: 's_known' }));
    const bad = track(c.request('share.stop', { shareId: 's_other' }));
    await tick();
    expect(ok.value).toEqual({});
    expect(bad.error).toMatchObject({ code: 'share_not_found', retryable: false });
  });

  it('puts globalThis.WebSocket back on uninstall', () => {
    const installed = globalThis.WebSocket;
    const inner = FakeSignalServer.install();
    expect(globalThis.WebSocket).not.toBe(installed);
    inner.uninstall();
    expect(globalThis.WebSocket).toBe(installed);
  });
});

describe('index.ts', () => {
  it('re-exports the protocol layer', async () => {
    const protocol = await import('./index');
    expect(protocol.SignalClient).toBe(SignalClient);
    expect(protocol.ProtocolError).toBe(ProtocolError);
    expect(typeof protocol.detectCaps).toBe('function');
    expect(typeof protocol.h264Key).toBe('function');
    expect(protocol.Version).toBe(P.Version);
    expect(protocol.clientRequestTypes).toContain('room.join');
    expect(typeof protocol.api).toBe('object');
  });
});

describe('generated types', () => {
  it('types map[string]any fields as unknown values, not any', () => {
    expectTypeOf<P.Error['params']>().toEqualTypeOf<Record<string, unknown> | undefined>();
    expectTypeOf<api.Error['params']>().toEqualTypeOf<Record<string, unknown> | undefined>();
    expectTypeOf<api.DoctorCheck['params']>().toEqualTypeOf<Record<string, unknown> | undefined>();
    expectTypeOf<api.Alert['params']>().toEqualTypeOf<Record<string, unknown> | undefined>();
    expectTypeOf<api.AuditEntry['detail']>().toEqualTypeOf<Record<string, unknown>>();
    expectTypeOf<ProtocolError['params']>().toEqualTypeOf<Readonly<Record<string, unknown>>>();
  });
});
