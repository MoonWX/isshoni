// Fixtures for connection-test tests: what POST /api/v1/conntest answers (04 §7.7), and finished results. Other
// folders' tests may use them too (the wizard's step 2 mocks the test with resultFixture).
import type { ConnTestResponse, ConnTestServerInfo } from '../../protocol/api.gen';
import type { ClientInfo } from '../../protocol/types.gen';
import { buildSdp } from '../../test/FakeRTCPeerConnection';
import type { ProbeTransport, ProbeVerdict } from '../probe';
import type { ConnTestResult } from '../runConnTest';

/** A public IPv4 from the documentation range (RFC 5737), standing in for a server's address. */
export const PUBLIC_IP = '203.0.113.7';

/** The server part of a reply: a Hetzner server with its public address on an interface and every transport on. */
export function serverInfo(overrides: Partial<ConnTestServerInfo> = {}): ConnTestServerInfo {
  return {
    publicIp: PUBLIC_IP,
    provider: 'hetzner',
    container: 'none',
    udpPort: 7882,
    tcpPorts: [443, 7882],
    nat: 'none',
    ...overrides,
  };
}

/** A probe answer: one data-channel section, like the server's probe PC sends. */
export function probeAnswer(): string {
  return buildSdp({ setup: 'active', sections: [{ kind: 'application', mid: '0', direction: 'sendrecv' }] });
}

/** A 200 reply of POST /api/v1/conntest. */
export function connTestReply(server: Partial<ConnTestServerInfo> = {}): ConnTestResponse {
  return { answer: probeAnswer(), expiresInS: 20, server: serverInfo(server) };
}

/** What a probe of each transport found, in short: a verdict's result, or how the probe could not run. */
export type ProbeShort = 'ok' | 'failed' | 'timeout' | 'disabled' | 'server' | 'rate_limited';

export function probeVerdict(transport: ProbeTransport, short: ProbeShort, rttMs?: number): ProbeVerdict {
  switch (short) {
    case 'ok':
      return { transport, result: 'ok', ...(rttMs !== undefined ? { rttMs } : {}) };
    case 'disabled':
      return { transport, result: 'disabled' };
    case 'failed':
      return { transport, result: 'failed' };
    default:
      return { transport, result: 'failed', error: short };
  }
}

export interface ResultFixtureOptions {
  udp?: ProbeShort;
  tcp443?: ProbeShort;
  tcp7882?: ProbeShort;
  /** rttMs of the ok probes, by transport. Default 24 for UDP, 31 for the TCP ones. */
  rtt?: Partial<Record<ProbeTransport, number | undefined>>;
  /** Overrides of the server part; null leaves it out (no probe got a reply). */
  server?: Partial<NonNullable<ConnTestResult['server']>> | null;
  client?: Partial<ClientInfo>;
  retryAfterSec?: number;
}

/** A finished result: every transport ok on a Hetzner server unless said otherwise. */
export function resultFixture({
  udp = 'ok',
  tcp443 = 'ok',
  tcp7882 = 'ok',
  rtt = {},
  server = {},
  client = {},
  retryAfterSec,
}: ResultFixtureOptions = {}): ConnTestResult {
  const rttOf = (t: ProbeTransport, fallback: number): number | undefined => (t in rtt ? rtt[t] : fallback);
  return {
    at: '2026-10-01T12:00:00.000Z',
    client: { kind: 'web', version: '0.0.0-test', os: 'windows', browser: 'chrome', ...client },
    probes: [
      probeVerdict('udp', udp, rttOf('udp', 24)),
      probeVerdict('tcp443', tcp443, rttOf('tcp443', 31)),
      probeVerdict('tcp7882', tcp7882, rttOf('tcp7882', 31)),
    ],
    ...(server === null
      ? {}
      : {
          server: {
            provider: 'hetzner',
            nat: 'none',
            udpPort: 7882,
            tcpPorts: [443, 7882],
            container: 'none',
            publicIpKnown: true,
            publicIpPrivate: false,
            ...server,
          },
        }),
    ...(retryAfterSec !== undefined ? { retryAfterSec } : {}),
  };
}
