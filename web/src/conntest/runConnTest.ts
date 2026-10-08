// The connection test (05 §14.2): three probes in parallel, one per ICE transport (UDP, TCP 443, TCP 7882), each a
// real ICE check against the server's probe PeerConnection (probe.ts). Any signed-in user may run it; the wizard's
// step 2, the doctor page and "Test my connection" all use this one function, through ConnTestPanel.
//
// The result is plain data: verdict.ts turns it into rows and a status, fixText.ts into fix text, and "Copy result"
// serializes it as it is. It holds no IP address (05 §20): the server's public IP only becomes two flags.
import { createLogger } from '../lib/log';
import type { Platform } from '../platform/types';
import { CloudProviderUnknown, ContainerKindNone, NATKindUnknown, type ConnTestServerInfo } from '../protocol/api.gen';
import type { ClientInfo } from '../protocol/types.gen';
import { PROBE_TRANSPORTS, runProbe, type ProbeOutcome, type ProbeVerdict } from './probe';

export type { ProbeTransport, ProbeVerdict } from './probe';

const log = createLogger('conntest');

export interface ConnTestResult {
  /** When the run finished: an RFC 3339 UTC timestamp. */
  at: string;
  client: ClientInfo;
  /** One verdict per transport, in the order udp, tcp443, tcp7882. */
  probes: ProbeVerdict[];
  /** What the server said about itself; absent when no probe got a reply (every transport disabled or not tested). */
  server?: {
    /** A CloudProvider id of 04 §13.3 (a newer server may send one this build doesn't know). */
    provider: string;
    /** A NATKind of 04 §7.4. */
    nat: string;
    /** 0 when UDP is off. */
    udpPort: number;
    tcpPorts: number[];
    /** 04, as in the doctor's env.container: none | docker | podman | other. */
    container: string;
    /** Derived: server.publicIp is non-empty. */
    publicIpKnown: boolean;
    /** Derived: server.publicIp is empty, private or loopback (the IP itself isn't kept). */
    publicIpPrivate: boolean;
  };
  /**
   * The longest Retry-After (seconds) of the probes the server rate-limited, when it gave one: how long "Test again"
   * should wait. Not in 05 §14.2's interface; added so the panel can respect Retry-After.
   */
  retryAfterSec?: number;
}

export interface RunConnTestOptions {
  /** Stops the run: every probe PC is closed and the promise rejects with the signal's reason. */
  signal?: AbortSignal;
}

function ipv4Octets(ip: string): [number, number, number, number] | null {
  const m = /^(\d{1,3})\.(\d{1,3})\.(\d{1,3})\.(\d{1,3})$/.exec(ip);
  if (!m) return null;
  const [a, b, c, d] = [Number(m[1]), Number(m[2]), Number(m[3]), Number(m[4])];
  return a <= 255 && b <= 255 && c <= 255 && d <= 255 ? [a, b, c, d] : null;
}

/**
 * Whether an IP address is on the server's own machine or local network, where a friend on the internet can't reach
 * it: loopback, RFC 1918, link-local, or their IPv6 counterparts (::1, fc00::/7, fe80::/10). An IPv4-mapped IPv6
 * address is judged by the IPv4 address inside. Anything that doesn't parse is not private.
 */
export function isPrivateAddress(ip: string): boolean {
  let addr = ip.trim().toLowerCase();
  const zone = addr.indexOf('%');
  if (zone >= 0) addr = addr.slice(0, zone);
  const mapped = /^::ffff:(\d{1,3}(?:\.\d{1,3}){3})$/.exec(addr);
  if (mapped?.[1] !== undefined) addr = mapped[1];
  const v4 = ipv4Octets(addr);
  if (v4) {
    const [a, b] = v4;
    return (
      a === 127 || // loopback
      a === 10 || // RFC 1918
      (a === 172 && b >= 16 && b <= 31) ||
      (a === 192 && b === 168) ||
      (a === 169 && b === 254) // link-local
    );
  }
  if (!addr.includes(':')) return false;
  if (addr === '::1') return true;
  const first = addr.split(':')[0] ?? '';
  if (!/^[0-9a-f]{1,4}$/.test(first)) return false;
  const head = parseInt(first, 16);
  return (head & 0xfe00) === 0xfc00 || (head & 0xffc0) === 0xfe80; // unique local, link-local
}

function portOf(v: unknown): number {
  return typeof v === 'number' && Number.isInteger(v) && v > 0 && v <= 65535 ? v : 0;
}

function idOf(v: unknown, fallback: string): string {
  return typeof v === 'string' && v !== '' ? v : fallback;
}

/**
 * The server part of a result from a reply's `server` object. It comes off the wire, so every field is read
 * defensively (Go encodes an empty port list as null). The public IP is reduced to the two flags here.
 */
export function summarizeServer(info: ConnTestServerInfo): NonNullable<ConnTestResult['server']> {
  const raw = info as Partial<Record<keyof ConnTestServerInfo, unknown>>;
  const publicIp = typeof raw.publicIp === 'string' ? raw.publicIp.trim() : '';
  const tcpPorts = Array.isArray(raw.tcpPorts) ? raw.tcpPorts.map(portOf).filter((p) => p > 0) : [];
  return {
    provider: idOf(raw.provider, CloudProviderUnknown),
    nat: idOf(raw.nat, NATKindUnknown),
    udpPort: portOf(raw.udpPort),
    tcpPorts,
    container: idOf(raw.container, ContainerKindNone),
    publicIpKnown: publicIp !== '',
    publicIpPrivate: publicIp === '' || isPrivateAddress(publicIp),
  };
}

function buildResult(platform: Platform, outcomes: readonly ProbeOutcome[]): ConnTestResult {
  const server = outcomes.find((o) => o.server !== undefined)?.server;
  const waits = outcomes.flatMap((o) => (o.retryAfterSec !== undefined ? [o.retryAfterSec] : []));
  return {
    at: new Date().toISOString(),
    client: { ...platform.client },
    probes: outcomes.map((o) => o.verdict),
    ...(server ? { server: summarizeServer(server) } : {}),
    ...(waits.length > 0 ? { retryAfterSec: Math.max(...waits) } : {}),
  };
}

/**
 * Runs the three probes in parallel and resolves with the result, also when probes failed, are disabled or could not
 * run: those are verdicts (ProbeVerdict). It rejects only when there is nothing to show: the signal aborted (its
 * reason), the user is signed out (the ApiError, so the caller's 401 rule runs), or this browser can't make a
 * data-channel offer (LocalError "webrtc_failed"). When it rejects, the probes still running are stopped.
 */
export async function runConnTest(platform: Platform, { signal }: RunConnTestOptions = {}): Promise<ConnTestResult> {
  // One signal for the run, so a probe that rejects takes the others down with it.
  const ctl = new AbortController();
  const onAbort = (): void => {
    ctl.abort(signal?.reason);
  };
  if (signal?.aborted) onAbort();
  else signal?.addEventListener('abort', onAbort, { once: true });
  try {
    const outcomes = await Promise.all(
      PROBE_TRANSPORTS.map((transport) => runProbe(platform, transport, { signal: ctl.signal })),
    );
    const result = buildResult(platform, outcomes);
    // One line per run in the in-memory log: outcomes only, never an SDP or an address.
    log.info('connection test finished', {
      probes: result.probes.map((p) => `${p.transport}:${p.result}${p.error ? `(${p.error})` : ''}`).join(' '),
    });
    return result;
  } catch (err) {
    ctl.abort(err);
    throw err;
  } finally {
    signal?.removeEventListener('abort', onAbort);
  }
}
