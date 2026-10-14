// recovery.ts (01 §10.5, §10.6): the wait for a resumed connection and share.start across a drop, against a
// stand-in signaling client whose state the test moves, and the real SignalClient on the fake server.
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { clearLog, createLogger } from '../lib/log';
import { isProtocolError, LocalErrorCodeConnectionLost, ProtocolError } from '../protocol/errors';
import type { SignalState, SignalStateInfo, StateListener } from '../protocol/signal-client';
import { SignalClient } from '../protocol/signal-client';
import { FakeSignalServer, makeError, makeWelcome } from '../protocol/testing';
import type { ShareStart, Welcome } from '../protocol/types.gen';
import { createTestPlatform } from '../test/platform';
import type { PublisherSignal } from './PublisherPC';
import { CAPTURE_HOLD_MS, isConnectionLost, newRef, requestShareStart, resumedInto } from './recovery';
import { shareParams } from './testing/publish';

const tick = async (ms = 0): Promise<void> => {
  await vi.advanceTimersByTimeAsync(ms);
};

/** A signaling client that is down, whose next states the test sends. */
class StandInSignal implements PublisherSignal {
  state: SignalState = 'backoff';
  welcome: Welcome | undefined;
  readonly listeners = new Set<StateListener>();
  readonly request = vi.fn();
  readonly notify = vi.fn(() => false);
  readonly on = vi.fn(() => () => undefined);
  readonly probe = vi.fn();

  onState(fn: StateListener): () => void {
    this.listeners.add(fn);
    return () => {
      this.listeners.delete(fn);
    };
  }

  /** The client reaches `state`, with welcome w when it is ready. */
  go(state: SignalState, info: SignalStateInfo = {}, w?: Partial<Welcome>): void {
    this.state = state;
    if (w !== undefined) this.welcome = makeWelcome(w);
    for (const fn of [...this.listeners]) fn(state, info);
  }
}

const START: ShareStart = { kind: 'window', preset: 'auto', audio: true, ref: 'r1' };

beforeEach(() => {
  vi.useFakeTimers();
  clearLog();
});

afterEach(() => {
  vi.restoreAllMocks();
  vi.useRealTimers();
});

describe('newRef and isConnectionLost', () => {
  it('a ref is 24 hex digits, new each time (01 §8.7: at most 32 characters)', () => {
    const a = newRef();
    expect(a).toMatch(/^[0-9a-f]{24}$/);
    expect(newRef()).not.toBe(a);
  });

  it('only the local connection_lost is a lost connection', () => {
    expect(isConnectionLost(ProtocolError.local(LocalErrorCodeConnectionLost))).toBe(true);
    expect(isConnectionLost(ProtocolError.fromWire(makeError('share_limit', 'request')))).toBe(false);
    expect(isConnectionLost(new Error('connection_lost'))).toBe(false);
  });
});

describe('resumedInto', () => {
  it('true once the next ready resumes the connection in the room', async () => {
    const signal = new StandInSignal();
    const kept = resumedInto(signal, 'lounge', CAPTURE_HOLD_MS);
    signal.go('connecting');
    signal.go('handshaking');
    signal.go('ready', { resumed: true }, { resumed: true, roomId: 'lounge' });
    await expect(kept).resolves.toBe(true);
    expect(signal.listeners.size).toBe(0);
  });

  it.each<[string, SignalStateInfo, Partial<Welcome>]>([
    ['a new connection', { resumed: false }, { resumed: false, roomId: 'lounge' }],
    ['a resumed connection in another room', { resumed: true }, { resumed: true, roomId: 'games' }],
    ['a resumed connection in no room', { resumed: true }, { resumed: true }],
  ])('false for %s', async (_name, info, w) => {
    const signal = new StandInSignal();
    const kept = resumedInto(signal, 'lounge', CAPTURE_HOLD_MS);
    signal.go('ready', info, w);
    await expect(kept).resolves.toBe(false);
  });

  it('false when the client stops, and when no ready comes within the limit', async () => {
    const stopping = new StandInSignal();
    const stopped = resumedInto(stopping, 'lounge', CAPTURE_HOLD_MS);
    stopping.go('stopped');
    await expect(stopped).resolves.toBe(false);

    const silent = new StandInSignal();
    const late = resumedInto(silent, 'lounge', 1_000);
    await tick(999);
    silent.go('connecting');
    await tick(1);
    await expect(late).resolves.toBe(false);
    expect(silent.listeners.size).toBe(0);
    // A ready after the limit changes nothing.
    silent.go('ready', { resumed: true }, { resumed: true, roomId: 'lounge' });
    await expect(late).resolves.toBe(false);
  });

  it('false at once for a client that is ready (no drop to wait out), stopped, or that can’t say (no onState)', async () => {
    const ready = new StandInSignal();
    ready.state = 'ready';
    await expect(resumedInto(ready, 'lounge', CAPTURE_HOLD_MS)).resolves.toBe(false);
    // stop() (a logout) under a request: only a new start() leaves `stopped`, and that is a new connection.
    const stopped = new StandInSignal();
    stopped.state = 'stopped';
    await expect(resumedInto(stopped, 'lounge', CAPTURE_HOLD_MS)).resolves.toBe(false);
    expect(stopped.listeners.size).toBe(0);
    const bare: PublisherSignal = {
      request: vi.fn(),
      notify: vi.fn(() => false),
      on: () => () => undefined,
      probe: vi.fn(),
    };
    await expect(resumedInto(bare, 'lounge', CAPTURE_HOLD_MS)).resolves.toBe(false);
  });
});

describe('requestShareStart (the real SignalClient on the fake server)', () => {
  let server: FakeSignalServer;
  let signal: SignalClient;
  let starts: ShareStart[];
  /** What share.start does with the n-th request: answer it, or drop the socket under it. */
  let plan: ('answer' | 'drop' | 'restart')[];

  beforeEach(async () => {
    vi.spyOn(Math, 'random').mockReturnValue(0.5);
    server = FakeSignalServer.install({ welcome: { roomId: 'lounge' } });
    starts = [];
    plan = [];
    server.handle('share.start', (data) => {
      starts.push(data);
      const step = plan.shift() ?? 'answer';
      if (step === 'restart') server.restart();
      if (step !== 'answer') server.drop();
      return { ok: shareParams(`s_${String(starts.length)}`) };
    });
    const platform = createTestPlatform();
    signal = new SignalClient({
      url: platform.signaling().url,
      client: platform.client,
      role: 'full',
      caps: () => platform.capsNow(),
    });
    signal.start();
    await tick();
    expect(signal.state).toBe('ready');
  });

  afterEach(() => {
    signal.stop();
    server.uninstall();
  });

  const log = createLogger('test');

  it('is share.start when nothing goes wrong', async () => {
    await expect(requestShareStart(signal, 'lounge', START, log)).resolves.toMatchObject({ shareId: 's_1' });
    expect(starts).toEqual([START]);
  });

  it('an answer lost with the socket: the same request, the same ref, once the connection is resumed in the room', async () => {
    plan = ['drop'];
    const params = requestShareStart(signal, 'lounge', START, log);
    await tick(1_000);
    expect(server.welcomes.at(-1)?.resumed).toBe(true);
    await expect(params).resolves.toMatchObject({ shareId: 's_2' });
    expect(starts).toEqual([START, START]);
  });

  it('a new connection has no such ref: connection_lost, and nothing is sent again', async () => {
    plan = ['restart'];
    const params = requestShareStart(signal, 'lounge', START, log);
    const outcome = params.catch((err: unknown) => err);
    await tick(1_000);
    expect(server.welcomes.at(-1)?.resumed).toBe(false);
    const err = await outcome;
    expect(isProtocolError(err) && err.code).toBe(LocalErrorCodeConnectionLost);
    expect(starts).toEqual([START]);
  });

  it('the server’s refusal is the answer: no wait, no second request', async () => {
    server.handle('share.start', (data) => {
      starts.push(data);
      return { error: makeError('share_limit', 'request') };
    });
    await expect(requestShareStart(signal, 'lounge', START, log)).rejects.toMatchObject({ code: 'share_limit' });
    expect(starts).toEqual([START]);
  });
});
