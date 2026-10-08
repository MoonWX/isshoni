// One probe against the fake RTCPeerConnection (05 §14.2, steps 1–6): what it sends, when it counts as connected,
// the RTT, and every way it can end. The REST call is a test double here; runConnTest.test.ts goes through api().
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { LocalError } from '../lib/errors';
import type { ConnTestRequest, ConnTestResponse } from '../protocol/api.gen';
import { ApiError, NetworkError } from '../protocol/rest';
import { FakeRTCPeerConnection, parseSdpSections, type FakeRTCDataChannel } from '../test/FakeRTCPeerConnection';
import { createTestPlatform } from '../test/platform';
import {
  CONNECT_TIMEOUT_MS,
  ECHO_WAIT_MS,
  median,
  PING_COUNT,
  PING_INTERVAL_MS,
  PROBE_TRANSPORTS,
  REQUEST_TIMEOUT_MS,
  runProbe,
  type ConnTestRequester,
  type ProbeOutcome,
  type ProbeTransport,
} from './probe';
import { connTestReply, serverInfo } from './testing';

beforeEach(() => {
  vi.useFakeTimers();
});

afterEach(() => {
  vi.useRealTimers();
});

/** Lets everything that is ready run, without moving the clock. */
const flush = (): Promise<unknown> => vi.advanceTimersByTimeAsync(0);

interface Started {
  promise: Promise<ProbeOutcome>;
  request: ReturnType<typeof vi.fn<ConnTestRequester>>;
  /** The probe's PC and channel, once it asked the server (and got its answer, when there is one). */
  pc: FakeRTCPeerConnection;
  dc: FakeRTCDataChannel;
}

/** Starts a probe and runs it up to the point where it waits for the connection (or has ended). */
async function start(
  transport: ProbeTransport = 'udp',
  requester: ConnTestRequester = () => Promise.resolve(connTestReply()),
  signal?: AbortSignal,
): Promise<Started> {
  const request = vi.fn<ConnTestRequester>(requester);
  const promise = runProbe(createTestPlatform(), transport, { request, ...(signal ? { signal } : {}) });
  promise.catch(() => undefined); // tests assert on it later; never an unhandled rejection
  await flush();
  const pc = FakeRTCPeerConnection.last;
  const dc = pc?.dataChannels[0];
  if (!pc || !dc) throw new Error('the probe made no PeerConnection with a data channel');
  return { promise, request, pc, dc };
}

/** The network side: ICE and DTLS connect and the channel opens; the probe then sends its first ping. */
async function connect({ pc, dc }: Started): Promise<void> {
  pc.setConnectionState('connected');
  dc.open();
  await flush();
}

/** Echoes each ping after its delay (ms, by ping index), like the server's probe PC; no delay = no echo. */
function echoAfter(dc: FakeRTCDataChannel, delays: readonly (number | undefined)[]): void {
  const send = dc.send.bind(dc);
  dc.send = (data: unknown): void => {
    send(data);
    const delay = delays[dc.sent.length - 1];
    if (delay !== undefined) {
      setTimeout(() => {
        dc.receive(data);
      }, delay);
    }
  };
}

const ALL_PINGS_MS = (PING_COUNT - 1) * PING_INTERVAL_MS;

describe('runProbe: the offer and the answer', () => {
  it.each(PROBE_TRANSPORTS)('%s: a PC without ICE servers, a "probe" channel, and the offer posted', async (t) => {
    const s = await start(t);
    expect(s.pc.config).toEqual({ iceServers: [], bundlePolicy: 'max-bundle' });
    expect(s.dc.label).toBe('probe');
    expect(FakeRTCPeerConnection.instances).toHaveLength(1);

    expect(s.request).toHaveBeenCalledTimes(1);
    const [body, signal] = s.request.mock.calls[0] ?? [];
    expect(body?.transport).toBe(t);
    // The offer is the local description as it is: one data-channel section, no media, nobody waited for gathering.
    expect(body?.offer).toBe(s.pc.localDescription?.sdp);
    expect(parseSdpSections(body?.offer ?? '').map((m) => m.kind)).toEqual(['application']);
    expect(signal).toBeInstanceOf(AbortSignal);

    expect(s.pc.remoteDescription).toMatchObject({ type: 'answer' });
    expect(s.pc.remoteDescription?.sdp).toContain('m=application');
    expect(s.pc.signalingState).toBe('stable');
    expect(s.pc.remoteCandidates).toEqual([]); // the answer is complete: no trickle
    s.dc.autoEcho = true;
    await connect(s);
    await vi.advanceTimersByTimeAsync(ALL_PINGS_MS);
    await expect(s.promise).resolves.toEqual({
      verdict: { transport: t, result: 'ok', rttMs: 0 },
      server: serverInfo(),
    });
  });
});

describe('runProbe: connected', () => {
  it('needs the PC connected and the channel open, in either order', async () => {
    const a = await start();
    a.pc.setConnectionState('connecting');
    a.dc.open();
    await vi.advanceTimersByTimeAsync(1000);
    expect(a.dc.sent).toEqual([]); // no ping before the PC is connected
    a.pc.setConnectionState('connected');
    a.dc.autoEcho = true;
    await vi.advanceTimersByTimeAsync(ALL_PINGS_MS);
    expect((await a.promise).verdict.result).toBe('ok');

    const b = await start();
    b.pc.setConnectionState('connected');
    await vi.advanceTimersByTimeAsync(1000);
    expect(b.dc.sent).toEqual([]); // nor before the channel is open
    b.dc.autoEcho = true;
    b.dc.open();
    await vi.advanceTimersByTimeAsync(ALL_PINGS_MS);
    expect((await b.promise).verdict.result).toBe('ok');
  });

  it('fails with "timeout" when it is not connected after 8 s, and closes the PC', async () => {
    const s = await start('tcp443');
    const done = vi.fn();
    void s.promise.then(done);
    s.pc.setConnectionState('connecting');
    await vi.advanceTimersByTimeAsync(CONNECT_TIMEOUT_MS - 1);
    expect(done).not.toHaveBeenCalled();
    expect(s.pc.signalingState).toBe('stable');
    await vi.advanceTimersByTimeAsync(1);
    await expect(s.promise).resolves.toEqual({
      verdict: { transport: 'tcp443', result: 'failed', error: 'timeout' },
      server: serverInfo(),
    });
    expect(s.pc.signalingState).toBe('closed');
    expect(s.dc.readyState).toBe('closed');
  });

  it('a connected PC whose channel never opens is a timeout too', async () => {
    const s = await start();
    s.pc.setConnectionState('connected');
    await vi.advanceTimersByTimeAsync(CONNECT_TIMEOUT_MS);
    expect((await s.promise).verdict).toEqual({ transport: 'udp', result: 'failed', error: 'timeout' });
  });

  it('fails at once, without an error, when ICE fails or the channel closes before the 8 s', async () => {
    const a = await start();
    a.pc.setConnectionState('failed');
    await flush();
    await expect(a.promise).resolves.toEqual({ verdict: { transport: 'udp', result: 'failed' }, server: serverInfo() });
    expect(a.pc.signalingState).toBe('closed');

    const b = await start('tcp7882');
    b.dc.close();
    await flush();
    expect((await b.promise).verdict).toEqual({ transport: 'tcp7882', result: 'failed' });
  });
});

describe('runProbe: round trip', () => {
  it(`sends ${String(PING_COUNT)} pings {n, t} ${String(PING_INTERVAL_MS)} ms apart and takes the median echo time`, async () => {
    const s = await start();
    echoAfter(s.dc, [10, 30, 20, 50, 40]);
    const t0 = performance.now();
    await connect(s);
    expect(s.dc.sent).toHaveLength(1);
    for (let i = 1; i < PING_COUNT; i++) {
      await vi.advanceTimersByTimeAsync(PING_INTERVAL_MS - 1);
      expect(s.dc.sent).toHaveLength(i);
      await vi.advanceTimersByTimeAsync(1);
      expect(s.dc.sent).toHaveLength(i + 1);
    }
    const pings = s.dc.sent.map((m) => JSON.parse(m as string) as { n: number; t: number });
    expect(pings.map((p) => p.n)).toEqual([0, 1, 2, 3, 4]);
    expect(pings.map((p) => p.t - t0)).toEqual([0, 200, 400, 600, 800]);

    await vi.advanceTimersByTimeAsync(40); // the last echo
    await expect(s.promise).resolves.toEqual({
      verdict: { transport: 'udp', result: 'ok', rttMs: 30 },
      server: serverInfo(),
    });
    expect(s.pc.signalingState).toBe('closed'); // step 6
    expect(vi.getTimerCount()).toBe(0); // and no timer is left behind
  });

  it('takes the median of the echoes that arrived within the wait after the last ping', async () => {
    const s = await start();
    echoAfter(s.dc, [12, undefined, 18, ECHO_WAIT_MS + ALL_PINGS_MS + 500, undefined]);
    await connect(s);
    const done = vi.fn();
    void s.promise.then(done);
    await vi.advanceTimersByTimeAsync(ALL_PINGS_MS + ECHO_WAIT_MS - 1);
    expect(done).not.toHaveBeenCalled();
    await vi.advanceTimersByTimeAsync(1);
    expect((await s.promise).verdict).toEqual({ transport: 'udp', result: 'ok', rttMs: 15 });
  });

  it('ignores messages that are not its echoes, and an echo that comes twice', async () => {
    const s = await start();
    await connect(s);
    const first = s.dc.sent[0] as string;
    s.dc.receive('hello');
    s.dc.receive('{"n":"0"}');
    s.dc.receive('{"n":99,"t":1}');
    s.dc.receive('[1]');
    s.dc.receive(new Blob(['{"n":0}']));
    await vi.advanceTimersByTimeAsync(7);
    s.dc.receive(first);
    await vi.advanceTimersByTimeAsync(50);
    s.dc.receive(first); // a duplicate must not count as a second, slower sample
    await vi.advanceTimersByTimeAsync(ALL_PINGS_MS + ECHO_WAIT_MS);
    expect((await s.promise).verdict.rttMs).toBe(7);
  });

  it('reads an echo that comes back as binary', async () => {
    const s = await start();
    await connect(s);
    await vi.advanceTimersByTimeAsync(5);
    const bytes = new TextEncoder().encode(s.dc.sent[0] as string);
    s.dc.receive(bytes.buffer);
    await vi.advanceTimersByTimeAsync(ALL_PINGS_MS + ECHO_WAIT_MS);
    expect((await s.promise).verdict.rttMs).toBe(5);
  });

  it("falls back to the selected candidate pair's currentRoundTripTime when nothing is echoed", async () => {
    const s = await start();
    s.pc.stats = new Map<string, Record<string, unknown>>([
      ['T1', { type: 'transport', id: 'T1', selectedCandidatePairId: 'CP2' }],
      ['CP1', { type: 'candidate-pair', id: 'CP1', nominated: true, state: 'succeeded', currentRoundTripTime: 0.9 }],
      ['CP2', { type: 'candidate-pair', id: 'CP2', nominated: true, state: 'succeeded', currentRoundTripTime: 0.0423 }],
    ]);
    await connect(s);
    await vi.advanceTimersByTimeAsync(ALL_PINGS_MS + ECHO_WAIT_MS);
    expect((await s.promise).verdict).toEqual({ transport: 'udp', result: 'ok', rttMs: 42.3 });
  });

  it.each([
    [
      'Firefox: the pair marked selected',
      [{ type: 'candidate-pair', id: 'a', selected: true, currentRoundTripTime: 0.02 }],
      20,
    ],
    [
      'no transport stats: the nominated pair that succeeded',
      [
        { type: 'candidate-pair', id: 'a', nominated: false, state: 'succeeded', currentRoundTripTime: 0.5 },
        { type: 'candidate-pair', id: 'b', nominated: true, state: 'succeeded', currentRoundTripTime: 0.031 },
      ],
      31,
    ],
    ['a pair without a round trip yet', [{ type: 'candidate-pair', id: 'a', selected: true }], undefined],
    ['no stats at all', [], undefined],
  ])('fallback, %s', async (_name, stats, want) => {
    const s = await start();
    s.pc.stats = new Map(stats.map((e) => [e.id, e as Record<string, unknown>]));
    await connect(s);
    await vi.advanceTimersByTimeAsync(ALL_PINGS_MS + ECHO_WAIT_MS);
    const { verdict } = await s.promise;
    expect(verdict.result).toBe('ok');
    expect(verdict.rttMs).toBe(want);
    expect('rttMs' in verdict).toBe(want !== undefined);
  });

  it('is ok without an RTT when getStats fails too', async () => {
    const s = await start();
    s.pc.getStats = () => Promise.reject(new Error('no stats'));
    await connect(s);
    await vi.advanceTimersByTimeAsync(ALL_PINGS_MS + ECHO_WAIT_MS);
    expect((await s.promise).verdict).toEqual({ transport: 'udp', result: 'ok' });
  });

  it('keeps what it measured when the channel closes during the pings', async () => {
    const s = await start();
    echoAfter(s.dc, [8, 8, 8, 8, 8]);
    await connect(s);
    await vi.advanceTimersByTimeAsync(PING_INTERVAL_MS + 10); // two pings echoed
    s.dc.close();
    await flush();
    expect((await s.promise).verdict).toEqual({ transport: 'udp', result: 'ok', rttMs: 8 });
    expect(s.dc.sent).toHaveLength(2);
  });
});

describe('runProbe: the server does not take the probe', () => {
  const rejectWith =
    (err: Error): ConnTestRequester =>
    () =>
      Promise.reject(err);

  it('409 transport_disabled → disabled', async () => {
    const err = new ApiError({ status: 409, code: 'transport_disabled', params: { transport: 'tcp443' } });
    const s = await start('tcp443', rejectWith(err));
    await expect(s.promise).resolves.toEqual({ verdict: { transport: 'tcp443', result: 'disabled' } });
    expect(s.pc.signalingState).toBe('closed');
    expect(s.pc.remoteDescription).toBeNull();
  });

  it('429 rate_limited → not tested, with the wait the server gave', async () => {
    const a = await start('udp', rejectWith(new ApiError({ status: 429, code: 'rate_limited', retryAfterSec: 37 })));
    await expect(a.promise).resolves.toEqual({
      verdict: { transport: 'udp', result: 'failed', error: 'rate_limited' },
      retryAfterSec: 37,
    });
    // A 429 from something in front of the server, without the envelope.
    const b = await start('udp', rejectWith(new ApiError({ status: 429, code: 'unknown' })));
    await expect(b.promise).resolves.toEqual({
      verdict: { transport: 'udp', result: 'failed', error: 'rate_limited' },
    });
  });

  it.each([
    ['503 not_ready', new ApiError({ status: 503, code: 'not_ready' })],
    ['400 bad_sdp', new ApiError({ status: 400, code: 'bad_sdp' })],
    ['500 internal', new ApiError({ status: 500, code: 'internal', requestId: 'abcd1234' })],
    ['403 forbidden', new ApiError({ status: 403, code: 'forbidden' })],
    ['a 401 that is not "signed out"', new ApiError({ status: 401, code: 'invalid_credentials' })],
    ['no network', new NetworkError()],
    ['anything else', new TypeError('boom')],
  ])('%s → not tested (error "server")', async (_name, err) => {
    const s = await start('tcp7882', rejectWith(err));
    await expect(s.promise).resolves.toEqual({ verdict: { transport: 'tcp7882', result: 'failed', error: 'server' } });
    expect(s.pc.signalingState).toBe('closed');
  });

  it(`a request without a reply after ${String(REQUEST_TIMEOUT_MS / 1000)} s is aborted → not tested`, async () => {
    let seen: AbortSignal | undefined;
    const s = await start('udp', (_body, signal) => {
      seen = signal;
      return new Promise<ConnTestResponse>((_resolve, reject) => {
        signal.addEventListener('abort', () => {
          reject(signal.reason as Error);
        });
      });
    });
    await vi.advanceTimersByTimeAsync(REQUEST_TIMEOUT_MS - 1);
    expect(seen?.aborted).toBe(false);
    await vi.advanceTimersByTimeAsync(1);
    expect(seen?.aborted).toBe(true);
    expect((seen?.reason as Error).name).toBe('TimeoutError');
    await expect(s.promise).resolves.toEqual({ verdict: { transport: 'udp', result: 'failed', error: 'server' } });
  });

  it.each([
    ['no body', undefined],
    ['no answer', { expiresInS: 20, server: serverInfo() }],
    ['an empty answer', { answer: '', expiresInS: 20, server: serverInfo() }],
    ['an answer that is not a string', { answer: 7, expiresInS: 20, server: serverInfo() }],
  ])('a 200 with %s → not tested', async (_name, reply) => {
    const s = await start('udp', () => Promise.resolve(reply as unknown as ConnTestResponse));
    const outcome = await s.promise;
    expect(outcome.verdict).toEqual({ transport: 'udp', result: 'failed', error: 'server' });
    expect(outcome.server).toEqual(reply === undefined ? undefined : serverInfo());
    expect(s.pc.signalingState).toBe('closed');
  });

  it('an answer the browser rejects → not tested', async () => {
    const s = await start('udp', () => {
      FakeRTCPeerConnection.last?.failNext('setRemoteDescription');
      return Promise.resolve(connTestReply());
    });
    await expect(s.promise).resolves.toEqual({
      verdict: { transport: 'udp', result: 'failed', error: 'server' },
      server: serverInfo(),
    });
  });
});

describe('runProbe: no result at all', () => {
  it.each([
    ['unauthenticated', new ApiError({ status: 401, code: 'unauthenticated' })],
    ['invalid_token', new ApiError({ status: 401, code: 'invalid_token' })],
  ])('rejects with the 401 %s, so the caller signs the user out', async (_name, err) => {
    const s = await start('udp', () => Promise.reject(err));
    await expect(s.promise).rejects.toBe(err);
    expect(s.pc.signalingState).toBe('closed');
  });

  it('rejects with webrtc_failed when the browser cannot make the offer', async () => {
    const platform = createTestPlatform({
      createPeerConnection: (config) => {
        const pc = new FakeRTCPeerConnection(config);
        pc.failNext('createOffer');
        return pc as unknown as RTCPeerConnection;
      },
    });
    const request = vi.fn<ConnTestRequester>();
    const failed = runProbe(platform, 'udp', { request });
    failed.catch(() => undefined);
    await flush();
    await expect(failed).rejects.toBeInstanceOf(LocalError);
    await expect(failed).rejects.toMatchObject({ code: 'webrtc_failed' });
    expect(request).not.toHaveBeenCalled();
    expect(FakeRTCPeerConnection.last?.signalingState).toBe('closed');

    const none = createTestPlatform({
      createPeerConnection: () => {
        throw new DOMException('no WebRTC here', 'NotSupportedError');
      },
    });
    await expect(runProbe(none, 'udp', { request })).rejects.toMatchObject({ code: 'webrtc_failed' });
  });

  it('an aborted signal: nothing starts', async () => {
    const ctl = new AbortController();
    ctl.abort();
    const request = vi.fn<ConnTestRequester>();
    await expect(runProbe(createTestPlatform(), 'udp', { request, signal: ctl.signal })).rejects.toMatchObject({
      name: 'AbortError',
    });
    expect(FakeRTCPeerConnection.instances).toEqual([]);
    expect(request).not.toHaveBeenCalled();
  });

  it('aborting during the request passes the abort on, closes the PC and rejects with the reason', async () => {
    const ctl = new AbortController();
    let seen: AbortSignal | undefined;
    const s = await start(
      'udp',
      (_body: ConnTestRequest, signal) => {
        seen = signal;
        return new Promise<ConnTestResponse>((_resolve, reject) => {
          signal.addEventListener('abort', () => {
            reject(new NetworkError());
          });
        });
      },
      ctl.signal,
    );
    const reason = new Error('left the page');
    ctl.abort(reason);
    await flush();
    expect(seen?.aborted).toBe(true);
    await expect(s.promise).rejects.toBe(reason);
    expect(s.pc.signalingState).toBe('closed');
  });

  it('aborting while it connects, and during the pings, closes the PC and rejects', async () => {
    const a = new AbortController();
    const connecting = await start('udp', undefined, a.signal);
    await vi.advanceTimersByTimeAsync(3000);
    a.abort();
    await flush();
    await expect(connecting.promise).rejects.toMatchObject({ name: 'AbortError' });
    expect(connecting.pc.signalingState).toBe('closed');

    const b = new AbortController();
    const pinging = await start('udp', undefined, b.signal);
    await connect(pinging);
    await vi.advanceTimersByTimeAsync(PING_INTERVAL_MS);
    b.abort();
    await flush();
    await expect(pinging.promise).rejects.toMatchObject({ name: 'AbortError' });
    expect(pinging.pc.signalingState).toBe('closed');
    expect(pinging.dc.sent).toHaveLength(2);
    expect(vi.getTimerCount()).toBe(0); // no timer left behind
  });
});

describe('median', () => {
  it.each([
    [[5], 5],
    [[3, 1, 2], 2],
    [[40, 10, 30, 20], 25],
    [[10, 30, 20, 50, 40], 30],
  ])('%j → %d', (values, want) => {
    expect(median(values)).toBe(want);
  });
});
