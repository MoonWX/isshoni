// What a connection-test result means (05 §14.2): the rows to show, the status, the round-trip label and the
// troubleshooting codes. Pure functions of ConnTestResult; fixText.ts adds the admin's fix text.
//
// Rows. One per transport, in the order UDP, TCP 443, TCP 7882:
// - ok ✓, failed ✗;
// - "off": the server has no listener for it (409 transport_disabled). An off TCP row is hidden. An off UDP row is
//   shown as ✗ with conntest.udpDisabled and counts as UDP ✗ in the table below, because UDP matters for quality;
// - "not_tested": the probe couldn't run (rate_limited, or a server or network error). Neither ✓ nor ✗; it is left
//   out of the table, and the panel says "Couldn't finish the test".
//
// Status.
//   | UDP | any TCP | status |
//   | ✓   | any     | green  | "Friends get the best quality."
//   | ✗   | ✓       | amber  | "Works, but video may stutter on weak networks."
//   | ✗   | ✗       | red    | "Friends can't receive video yet."
// With probes not tested, a status is given only when they can't change it: green needs just UDP ✓, amber UDP ✗ and
// one TCP ✓. Red says that nothing works, so it needs every TCP probe to be ✗ or off; with UDP not tested there is
// no status at all.
import type { CtCode } from './links';
import type { ConnTestResult, ProbeTransport, ProbeVerdict } from './runConnTest';

/** The port of both media listeners unless the admin changed listen.ice_udp or listen.ice_tcp (04 §4.3). */
export const DEFAULT_MEDIA_PORT = 7882;
/** The tcp443 transport: ICE-TCP through the HTTPS port's first-byte multiplexer (04 §7.2). */
export const HTTPS_PORT = 443;

/** 05 §14.2: ≤ 80 ms good, ≤ 200 ms OK, above that "high latency". */
export const RTT_GOOD_MS = 80;
export const RTT_OK_MS = 200;

export type RowState = 'ok' | 'failed' | 'off' | 'not_tested';

export interface TransportRow {
  transport: ProbeTransport;
  state: RowState;
  /** The port the row names; absent for UDP when the server has it off or didn't say. */
  port?: number;
}

export type ConnTestStatus = 'green' | 'amber' | 'red';
export type RttLabel = 'good' | 'ok' | 'high';

export interface ConnTestVerdict {
  /** The rows to show: an off TCP row is left out. */
  rows: TransportRow[];
  /** null: no status, because a probe that decides it wasn't tested. */
  status: ConnTestStatus | null;
  /** A probe was not tested: "Couldn't finish the test. Try again in a minute." */
  incomplete: boolean;
  /** The UDP row's state ("off": shown as ✗ with conntest.udpDisabled). */
  udp: RowState;
  /**
   * Red while the server doesn't know its public IPv4: the probe answers carried no IPv4 candidates (04 §7.7), so
   * no firewall is to blame. The only fix text is conntest.noPublicIp, and the code no_public_ip replaces no_media.
   */
  noPublicIp: boolean;
  /** The round trip to show: the UDP probe's when it worked, else the fastest working TCP probe's. */
  rttMs?: number;
  rttLabel?: RttLabel;
  /** The troubleshooting sections this result links to (`/troubleshooting#ct-<code>`); fix text adds its own. */
  codes: CtCode[];
  /** The media ports that texts name: the server's, or 7882 when it didn't say. */
  ports: { udp: number; tcp: number };
}

/** A probe's verdict as a row state. A transport without a verdict counts as not tested. */
export function rowState(probe: ProbeVerdict | undefined): RowState {
  if (!probe) return 'not_tested';
  switch (probe.result) {
    case 'ok':
      return 'ok';
    case 'disabled':
      return 'off';
    case 'failed':
      return probe.error === 'server' || probe.error === 'rate_limited' ? 'not_tested' : 'failed';
  }
}

export function rttLabelOf(rttMs: number): RttLabel {
  if (rttMs <= RTT_GOOD_MS) return 'good';
  return rttMs <= RTT_OK_MS ? 'ok' : 'high';
}

function statusOf(udp: RowState, tcp: readonly RowState[]): ConnTestStatus | null {
  if (udp === 'ok') return 'green';
  if (udp === 'not_tested') return null;
  if (tcp.includes('ok')) return 'amber';
  return tcp.includes('not_tested') ? null : 'red';
}

export function verdictOf(result: ConnTestResult): ConnTestVerdict {
  const probe = (t: ProbeTransport): ProbeVerdict | undefined => result.probes.find((p) => p.transport === t);
  const server = result.server;
  const ports = {
    udp: server && server.udpPort > 0 ? server.udpPort : DEFAULT_MEDIA_PORT,
    tcp: server?.tcpPorts.find((p) => p !== HTTPS_PORT) ?? DEFAULT_MEDIA_PORT,
  };

  const udp = rowState(probe('udp'));
  const tcp443 = rowState(probe('tcp443'));
  const tcp7882 = rowState(probe('tcp7882'));
  const all: TransportRow[] = [
    { transport: 'udp', state: udp, ...(server && server.udpPort > 0 ? { port: server.udpPort } : {}) },
    { transport: 'tcp443', state: tcp443, port: HTTPS_PORT },
    { transport: 'tcp7882', state: tcp7882, port: ports.tcp },
  ];
  const rows = all.filter((r) => r.transport === 'udp' || r.state !== 'off');

  const status = statusOf(udp, [tcp443, tcp7882]);
  const anyFailed = all.some((r) => r.state === 'failed');
  const noPublicIp = status === 'red' && anyFailed && server !== undefined && !server.publicIpKnown;

  const rtts = (['tcp443', 'tcp7882'] as const).flatMap((t) => {
    const p = probe(t);
    return p?.result === 'ok' && p.rttMs !== undefined ? [p.rttMs] : [];
  });
  const udpProbe = probe('udp');
  const rttMs =
    udpProbe?.result === 'ok' && udpProbe.rttMs !== undefined
      ? udpProbe.rttMs
      : rtts.length > 0
        ? Math.min(...rtts)
        : undefined;

  const codes: CtCode[] = [];
  if (status === 'amber' && udp === 'failed') codes.push('udp_blocked');
  if (status === 'red') codes.push(noPublicIp ? 'no_public_ip' : 'no_media');
  if (udp === 'ok' && tcp443 === 'failed') codes.push('tcp443_blocked');
  if (rttMs !== undefined && rttMs > RTT_OK_MS) codes.push('high_rtt');

  return {
    rows,
    status,
    incomplete: all.some((r) => r.state === 'not_tested'),
    udp,
    noPublicIp,
    ...(rttMs !== undefined ? { rttMs, rttLabel: rttLabelOf(rttMs) } : {}),
    codes,
    ports,
  };
}
