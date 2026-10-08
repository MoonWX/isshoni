// Fix text for a connection-test result (05 §14.2): which catalog lines an admin sees, in order. The server tells
// every signed-in user its provider and NAT kind, but only admins get fix text; everyone else gets "Send this to
// your admin" with the result to copy.
//
// For an amber or red result, in this order:
// - no public address (checked first): when the server doesn't know its public IPv4 and every probe that ran
//   failed, conntest.noPublicIp is the only line. No firewall is to blame, so nothing below is shown;
// - "Open UDP port N" on amber;
// - provider: fix.firewall.<provider> for 04 §13.3's ids, linking to the install guide's section;
// - host firewall: always both commands, labeled (ufw, firewalld);
// - nat: conntest.nat.<nat> (04 §7.4);
// - Docker: when the server runs in docker or podman;
// - macOS Local Network: when the admin's OS is macOS and the server's address is empty, private or loopback;
// - blocked network: under a red UDP row.
// The lines about opening the UDP port (the two host firewall commands, Docker's published port, the blocked
// network) need a UDP probe that ran and failed: with UDP turned off in the server config the row itself says so
// (conntest.udpDisabled), and opening a port nobody listens on fixes nothing. Without any failed probe there is no
// firewall line at all.
//
// On every result, green included: when the NAT kind is port_forward, "You may be testing from the server's own
// network".
//
// The keys are built from ids, so check:i18n can't see them as t('…') literals: its rule 3 checks that every
// CloudProvider and NATKind constant has its text, and fixText.test.ts that every key used here exists.
import {
  ContainerKindDocker,
  ContainerKindPodman,
  NATKindCGNATLikely,
  NATKindNone,
  NATKindOneToOne,
  NATKindPortForward,
  NATKindSymmetric,
  NATKindUnknown,
  type CloudProvider,
  type NATKind,
} from '../protocol/api.gen';
import { ClientOSMacOS } from '../protocol/types.gen';
import { knownProvider, type CtCode } from './links';
import type { ConnTestResult } from './runConnTest';
import type { ConnTestVerdict } from './verdict';

export type FixLineId =
  | 'noPublicIp'
  | 'openUdp'
  | 'provider'
  | 'ufw'
  | 'firewalld'
  | 'nat'
  | 'docker'
  | 'macLocalNetwork'
  | 'blockedNetwork'
  | 'ownNetwork';

/**
 * The numbers that fix texts name: {{udpPort}} and {{tcpPort}} (the media ports; 443 and 80 are fixed). A type
 * alias, not an interface, so it can be passed to t() as its options.
 */
export type FixValues = {
  udpPort: number;
  tcpPort: number;
};

export interface FixLine {
  id: FixLineId;
  /** The catalog key of the line's text. */
  key: string;
  values: FixValues;
  /** A shell command shown under the text (the host firewall lines). Commands are not translated. */
  command?: string;
  /** The line's troubleshooting section: `/troubleshooting#ct-<code>`. */
  code?: CtCode;
  /** The provider line's install guide section: `/install/vps#<provider>`. */
  provider?: CloudProvider;
}

export interface FixText {
  /** Admins only; empty for everyone else. */
  lines: FixLine[];
  /** A non-admin with an amber or red result: "Send this to your admin: [Copy result]" instead of fix text. */
  sendToAdmin: boolean;
}

/**
 * Every NATKind of 04 §7.4 with the troubleshooting section its line links to. A Record, so the build fails here
 * when `task gen` brings a new kind: it then needs its conntest.nat.<nat> text (check:i18n says so too).
 */
const NAT_CODES: Readonly<Record<NATKind, CtCode | null>> = {
  [NATKindNone]: null,
  [NATKindOneToOne]: null,
  [NATKindPortForward]: 'nat_port_forward',
  [NATKindSymmetric]: 'nat_cgnat',
  [NATKindCGNATLikely]: 'nat_cgnat',
  [NATKindUnknown]: null,
};

/** The NAT kinds this build has text for, in 04 §7.4's order. */
export const NAT_KINDS = Object.keys(NAT_CODES) as readonly NATKind[];

/** A NAT kind from the server as one this build knows: a newer server's unknown kind becomes "unknown". */
export function knownNat(id: string): NATKind {
  return Object.hasOwn(NAT_CODES, id) ? (id as NATKind) : NATKindUnknown;
}

export function providerKey(provider: CloudProvider): string {
  return `fix.firewall.${provider}`;
}

export function natKey(nat: NATKind): string {
  return `conntest.nat.${nat}`;
}

export function fixText(result: ConnTestResult, verdict: ConnTestVerdict, { admin }: { admin: boolean }): FixText {
  const notGreen = verdict.status === 'amber' || verdict.status === 'red';
  if (!admin) return { lines: [], sendToAdmin: notGreen };

  const server = result.server;
  const values: FixValues = { udpPort: verdict.ports.udp, tcpPort: verdict.ports.tcp };
  const line = (id: FixLineId, key: string, extra: Partial<FixLine> = {}): FixLine => ({ id, key, values, ...extra });

  if (verdict.noPublicIp) {
    return { lines: [line('noPublicIp', 'conntest.noPublicIp', { code: 'no_public_ip' })], sendToAdmin: false };
  }

  const lines: FixLine[] = [];
  const udpFailed = verdict.udp === 'failed';
  const anyFailed = verdict.rows.some((r) => r.state === 'failed');
  if (notGreen && anyFailed) {
    if (verdict.status === 'amber' && udpFailed) lines.push(line('openUdp', 'conntest.openUdp'));
    const provider = knownProvider(server?.provider ?? '');
    lines.push(line('provider', providerKey(provider), { provider }));
    if (udpFailed) {
      const port = String(values.udpPort);
      lines.push(
        line('ufw', 'fix.hostFirewall.ufw', { command: `sudo ufw allow ${port}/udp` }),
        line('firewalld', 'fix.hostFirewall.firewalld', {
          command: `sudo firewall-cmd --permanent --add-port=${port}/udp && sudo firewall-cmd --reload`,
        }),
      );
    }
    if (server) {
      const nat = knownNat(server.nat);
      const code = NAT_CODES[nat];
      lines.push(line('nat', natKey(nat), code ? { code } : {}));
      if (udpFailed && (server.container === ContainerKindDocker || server.container === ContainerKindPodman)) {
        lines.push(line('docker', 'conntest.docker', { code: 'docker_ports' }));
      }
      if (server.publicIpPrivate && result.client.os === ClientOSMacOS) {
        lines.push(line('macLocalNetwork', 'conntest.macLocalNetwork', { code: 'mac_local_network' }));
      }
    }
    if (udpFailed) lines.push(line('blockedNetwork', 'conntest.blockedNetwork'));
  }
  if (server && knownNat(server.nat) === NATKindPortForward) lines.push(line('ownNetwork', 'conntest.ownNetwork'));
  return { lines, sendToAdmin: false };
}
