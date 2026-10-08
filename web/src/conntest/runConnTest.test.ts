// runConnTest end to end against MSW (POST /api/v1/conntest, 04 §7.7) and the fake RTCPeerConnection: the three
// probes in parallel, each ProbeVerdict, and the ConnTestResult with its derived server flags (05 §14.2).
import { http, HttpResponse } from 'msw';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { logLines } from '../lib/log';
import type { Platform } from '../platform/types';
import type { ConnTestRequest } from '../protocol/api.gen';
import { ApiError, configureApi } from '../protocol/rest';
import { FakeRTCPeerConnection } from '../test/FakeRTCPeerConnection';
import { apiError, apiPath, server } from '../test/msw';
import { createTestPlatform } from '../test/platform';
import { CONNECT_TIMEOUT_MS, ECHO_WAIT_MS, PING_COUNT, PING_INTERVAL_MS, type ProbeTransport } from './probe';
import { isPrivateAddress, runConnTest, summarizeServer, type ConnTestResult } from './runConnTest';
import { connTestReply, PUBLIC_IP, serverInfo } from './testing';

type Reply = () => Response;

interface Harness {
  platform: Platform;
  /** The request bodies the server got, in arrival order. */
  bodies: ConnTestRequest[];
  /** Request headers, by transport. */
  headers: Map<string, Headers>;
}

/** A server that answers each transport with its reply (default: 200 with an answer). */
function serve(replies: Partial<Record<ProbeTransport, Reply>> = {}, fallback: Reply = ok()): Harness {
  const h: Harness = { platform: createTestPlatform(), bodies: [], headers: new Map() };
  configureApi(h.platform);
  server.use(
    http.post(apiPath('/api/v1/conntest'), async ({ request }) => {
      const body = (await request.json()) as ConnTestRequest;
      h.bodies.push(body);
      h.headers.set(body.transport, request.headers);
      return (replies[body.transport] ?? fallback)();
    }),
  );
  return h;
}

function ok(info: Parameters<typeof connTestReply>[0] = {}): Reply {
  return () => HttpResponse.json(connTestReply(info));
}

const disabled = (transport: ProbeTransport): Reply => {
  return () => apiError(409, { code: 'transport_disabled', params: { transport } });
};

/** The probe PCs by transport: runConnTest makes them in the order udp, tcp443, tcp7882. */
function pcs(): Record<ProbeTransport, FakeRTCPeerConnection> {
  const [udp, tcp443, tcp7882] = FakeRTCPeerConnection.instances;
  if (!udp || !tcp443 || !tcp7882) throw new Error('expected three probe PCs');
  return { udp, tcp443, tcp7882 };
}

/** Waits until the probes of these transports have the server's answer, then connects them with an echoing channel. */
async function connect(...transports: ProbeTransport[]): Promise<void> {
  const all = pcs();
  await vi.waitFor(() => {
    for (const t of transports) expect(all[t].remoteDescription).not.toBeNull();
  });
  for (const t of transports) {
    const pc = all[t];
    const dc = pc.dataChannels[0];
    if (!dc) throw new Error(`the ${t} probe has no data channel`);
    dc.autoEcho = true;
    pc.setConnectionState('connected');
    dc.open();
  }
}

const PINGS_MS = (PING_COUNT - 1) * PING_INTERVAL_MS;

beforeEach(() => {
  // The clock also moves by itself, so MSW's replies arrive; the tests jump over the probe's waits.
  vi.useFakeTimers({ shouldAdvanceTime: true });
});

afterEach(() => {
  vi.useRealTimers();
});

describe('runConnTest', () => {
  it('runs the three probes in parallel and reports each transport, the client and the server', async () => {
    const h = serve();
    const before = Date.now();
    const promise = runConnTest(h.platform);
    // All three PCs exist before any probe has an answer: nothing waits for another probe.
    expect(FakeRTCPeerConnection.instances).toHaveLength(3);
    expect(FakeRTCPeerConnection.instances.every((pc) => pc.remoteDescription === null)).toBe(true);

    await connect('udp', 'tcp443', 'tcp7882');
    await vi.advanceTimersByTimeAsync(PINGS_MS);
    const result = await promise;

    expect(result.probes).toEqual([
      { transport: 'udp', result: 'ok', rttMs: 0 },
      { transport: 'tcp443', result: 'ok', rttMs: 0 },
      { transport: 'tcp7882', result: 'ok', rttMs: 0 },
    ]);
    expect(result.client).toEqual(h.platform.client);
    expect(result.server).toEqual({
      provider: 'hetzner',
      nat: 'none',
      udpPort: 7882,
      tcpPorts: [443, 7882],
      container: 'none',
      publicIpKnown: true,
      publicIpPrivate: false,
    });
    expect(result.at).toMatch(/^\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d\.\d{3}Z$/);
    expect(Date.parse(result.at)).toBeGreaterThanOrEqual(before);
    expect('retryAfterSec' in result).toBe(false);

    // One request per transport, each with its own PC's offer, as JSON (03 §7.5's CSRF rule).
    expect(h.bodies.map((b) => b.transport).sort()).toEqual(['tcp443', 'tcp7882', 'udp']);
    const all = pcs();
    for (const body of h.bodies) {
      expect(body.offer).toBe(all[body.transport].localDescription?.sdp);
      expect(body.offer).toContain('m=application');
      expect(h.headers.get(body.transport)?.get('Content-Type')).toBe('application/json');
    }
    // Step 6: every PC is closed.
    expect(FakeRTCPeerConnection.instances.map((pc) => pc.signalingState)).toEqual(['closed', 'closed', 'closed']);
  });

  it('"Copy result" data holds no IP address', async () => {
    const h = serve({}, ok({ publicIp: PUBLIC_IP }));
    const promise = runConnTest(h.platform);
    await connect('udp', 'tcp443', 'tcp7882');
    await vi.advanceTimersByTimeAsync(PINGS_MS);
    const json = JSON.stringify(await promise);
    expect(json).not.toContain(PUBLIC_IP);
    expect(json).not.toMatch(/\b\d{1,3}(\.\d{1,3}){3}\b/);
    expect(json).not.toContain('publicIp"');
    expect(json).not.toMatch(/candidate|ice-ufrag|m=application/);
  });

  it('an off-mode server: TCP 443 is disabled, UDP times out, TCP 7882 works', async () => {
    const h = serve({ tcp443: disabled('tcp443') }, ok({ tcpPorts: [7882], container: 'docker', nat: 'one_to_one' }));
    const promise = runConnTest(h.platform);
    await connect('tcp7882');
    await vi.advanceTimersByTimeAsync(CONNECT_TIMEOUT_MS);
    const result = await promise;
    expect(result.probes).toEqual([
      { transport: 'udp', result: 'failed', error: 'timeout' },
      { transport: 'tcp443', result: 'disabled' },
      { transport: 'tcp7882', result: 'ok', rttMs: 0 },
    ]);
    expect(result.server).toMatchObject({ tcpPorts: [7882], container: 'docker', nat: 'one_to_one' });
    // The in-memory log gets the outcomes, nothing else.
    expect(logLines().at(-1)).toMatchObject({
      component: 'conntest',
      msg: 'connection test finished',
      attrs: { probes: 'udp:failed(timeout) tcp443:disabled tcp7882:ok' },
    });
  });

  it('a rate-limited and a failing request are "not tested"; the longest Retry-After is kept', async () => {
    const h = serve({
      udp: () => apiError(429, { code: 'rate_limited', retryAfter: 12 }),
      tcp443: () => apiError(429, { code: 'rate_limited', retryAfter: 41 }),
      tcp7882: () => apiError(503, { code: 'not_ready' }),
    });
    const result = await runConnTest(h.platform);
    expect(result.probes).toEqual([
      { transport: 'udp', result: 'failed', error: 'rate_limited' },
      { transport: 'tcp443', result: 'failed', error: 'rate_limited' },
      { transport: 'tcp7882', result: 'failed', error: 'server' },
    ]);
    expect(result.retryAfterSec).toBe(41);
    // No probe got a reply, so nothing is known about the server.
    expect('server' in result).toBe(false);
    expect(FakeRTCPeerConnection.instances.every((pc) => pc.signalingState === 'closed')).toBe(true);
  });

  it('takes the server part from whichever probe got a reply', async () => {
    const h = serve({
      udp: disabled('udp'),
      tcp443: () => HttpResponse.error(),
      tcp7882: ok({ udpPort: 0, tcpPorts: [7882], provider: 'aws', nat: 'one_to_one' }),
    });
    const promise = runConnTest(h.platform);
    await connect('tcp7882');
    await vi.advanceTimersByTimeAsync(PINGS_MS);
    const result = await promise;
    expect(result.probes).toEqual([
      { transport: 'udp', result: 'disabled' },
      { transport: 'tcp443', result: 'failed', error: 'server' },
      { transport: 'tcp7882', result: 'ok', rttMs: 0 },
    ]);
    expect(result.server).toMatchObject({ provider: 'aws', udpPort: 0, tcpPorts: [7882] });
  });

  it('nothing connects and the server does not know its address: every probe times out', async () => {
    const h = serve({}, ok({ publicIp: '', nat: 'unknown' }));
    const promise = runConnTest(h.platform);
    await vi.waitFor(() => {
      expect(FakeRTCPeerConnection.instances.every((pc) => pc.remoteDescription !== null)).toBe(true);
    });
    await vi.advanceTimersByTimeAsync(CONNECT_TIMEOUT_MS);
    const result = await promise;
    expect(result.probes.map((p) => [p.result, p.error])).toEqual([
      ['failed', 'timeout'],
      ['failed', 'timeout'],
      ['failed', 'timeout'],
    ]);
    expect(result.server).toMatchObject({ publicIpKnown: false, publicIpPrivate: true, nat: 'unknown' });
  });

  it('measures each probe on its own', async () => {
    const h = serve();
    const promise = runConnTest(h.platform);
    const all = pcs();
    await vi.waitFor(() => {
      expect(FakeRTCPeerConnection.instances.every((pc) => pc.remoteDescription !== null)).toBe(true);
    });
    // UDP echoes after 20 ms, TCP 443 never echoes but has stats, TCP 7882 has neither.
    const udp = all.udp.dataChannels[0];
    if (!udp) throw new Error('no channel');
    const send = udp.send.bind(udp);
    udp.send = (data: unknown): void => {
      send(data);
      setTimeout(() => {
        udp.receive(data);
      }, 20);
    };
    all.tcp443.stats = new Map([
      ['cp', { type: 'candidate-pair', id: 'cp', nominated: true, state: 'succeeded', currentRoundTripTime: 0.05 }],
    ]);
    for (const pc of FakeRTCPeerConnection.instances) {
      pc.setConnectionState('connected');
      pc.dataChannels[0]?.open();
    }
    await vi.advanceTimersByTimeAsync(PINGS_MS + ECHO_WAIT_MS);
    expect((await promise).probes).toEqual([
      { transport: 'udp', result: 'ok', rttMs: 20 },
      { transport: 'tcp443', result: 'ok', rttMs: 50 },
      { transport: 'tcp7882', result: 'ok' },
    ]);
  });

  it('rejects with the 401 when the user is signed out, and stops the other probes', async () => {
    const h = serve({ tcp443: () => apiError(401, { code: 'unauthenticated' }) });
    const promise = runConnTest(h.platform);
    promise.catch(() => undefined);
    await expect(promise).rejects.toBeInstanceOf(ApiError);
    await expect(promise).rejects.toMatchObject({ status: 401, code: 'unauthenticated' });
    await vi.waitFor(() => {
      expect(FakeRTCPeerConnection.instances.map((pc) => pc.signalingState)).toEqual(['closed', 'closed', 'closed']);
    });
  });

  it('aborting rejects with the reason and closes every probe PC', async () => {
    const h = serve();
    const ctl = new AbortController();
    const promise = runConnTest(h.platform, { signal: ctl.signal });
    promise.catch(() => undefined);
    await connect('udp');
    await vi.advanceTimersByTimeAsync(PING_INTERVAL_MS);
    const reason = new DOMException('left the page', 'AbortError');
    ctl.abort(reason);
    await expect(promise).rejects.toBe(reason);
    await vi.waitFor(() => {
      expect(FakeRTCPeerConnection.instances.map((pc) => pc.signalingState)).toEqual(['closed', 'closed', 'closed']);
    });
  });

  it('with a signal that is already aborted, nothing starts', async () => {
    const h = serve();
    const ctl = new AbortController();
    ctl.abort();
    await expect(runConnTest(h.platform, { signal: ctl.signal })).rejects.toMatchObject({ name: 'AbortError' });
    expect(FakeRTCPeerConnection.instances).toEqual([]);
    expect(h.bodies).toEqual([]);
  });
});

describe('summarizeServer', () => {
  const flags = (
    publicIp: string,
  ): Pick<NonNullable<ConnTestResult['server']>, 'publicIpKnown' | 'publicIpPrivate'> => {
    const { publicIpKnown, publicIpPrivate } = summarizeServer(serverInfo({ publicIp }));
    return { publicIpKnown, publicIpPrivate };
  };

  it('reduces the public IP to two flags', () => {
    expect(flags(PUBLIC_IP)).toEqual({ publicIpKnown: true, publicIpPrivate: false });
    expect(flags('')).toEqual({ publicIpKnown: false, publicIpPrivate: true });
    expect(flags('192.168.1.20')).toEqual({ publicIpKnown: true, publicIpPrivate: true });
    expect(flags('127.0.0.1')).toEqual({ publicIpKnown: true, publicIpPrivate: true });
    expect(Object.keys(summarizeServer(serverInfo())).sort()).toEqual([
      'container',
      'nat',
      'provider',
      'publicIpKnown',
      'publicIpPrivate',
      'tcpPorts',
      'udpPort',
    ]);
  });

  it('reads a reply defensively: Go encodes an empty port list as null', () => {
    const wire = { publicIp: null, provider: '', container: undefined, udpPort: null, tcpPorts: null, nat: 7 };
    expect(summarizeServer(wire as unknown as Parameters<typeof summarizeServer>[0])).toEqual({
      provider: 'unknown',
      nat: 'unknown',
      udpPort: 0,
      tcpPorts: [],
      container: 'none',
      publicIpKnown: false,
      publicIpPrivate: true,
    });
    const odd = serverInfo({ udpPort: 70000, tcpPorts: [443, -1, 7882.5, 7882] });
    expect(summarizeServer(odd)).toMatchObject({ udpPort: 0, tcpPorts: [443, 7882] });
  });

  it('keeps ids this build does not know (a newer server)', () => {
    const info = serverInfo({ provider: 'newcloud' as never, nat: 'double' as never, container: 'lxc' as never });
    expect(summarizeServer(info)).toMatchObject({ provider: 'newcloud', nat: 'double', container: 'lxc' });
  });
});

describe('isPrivateAddress', () => {
  it.each([
    '127.0.0.1',
    '127.255.255.254',
    '10.0.0.1',
    '172.16.0.1',
    '172.31.255.255',
    '192.168.0.10',
    '169.254.10.20',
    '::1',
    'fe80::1',
    'fe80::1%en0',
    'febf::1',
    'fc00::1',
    'fd12:3456:789a::1',
    '::ffff:192.168.1.5',
    ' 10.1.2.3 ',
    'FD00::1',
  ])('%s is private or loopback', (ip) => {
    expect(isPrivateAddress(ip)).toBe(true);
  });

  it.each([
    PUBLIC_IP,
    '8.8.8.8',
    '172.15.0.1',
    '172.32.0.1',
    '192.169.0.1',
    '169.253.0.1',
    '100.64.0.1', // carrier-grade NAT: not this machine's own network
    '11.0.0.1',
    '2001:db8::1',
    '2606:4700::1111',
    'fec0::1',
    '::ffff:203.0.113.7',
    '',
    'not an address',
    '256.1.1.1',
    '10.0.0',
    'fe80',
  ])('%s is not', (ip) => {
    expect(isPrivateAddress(ip)).toBe(false);
  });
});
