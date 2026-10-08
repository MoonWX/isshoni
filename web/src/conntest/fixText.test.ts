// fixText.ts (05 §14.2, §19.1): which lines an admin sees for a result, that every provider id of 04 §13.3 and every
// NAT kind of 04 §7.4 has its text, and what everyone else gets instead.
import { describe, expect, it } from 'vitest';

import { i18n } from '../i18n';
import en from '../i18n/en.json';
import { fixText, knownNat, NAT_KINDS, natKey, providerKey, type FixLine } from './fixText';
import { CLOUD_PROVIDERS, knownProvider, type CtCode } from './links';
import type { ConnTestResult } from './runConnTest';
import { resultFixture, type ResultFixtureOptions } from './testing';
import { verdictOf } from './verdict';

/** 04 §13.3's provider ids, written out: the list this slice must cover. */
const PROVIDER_IDS = [
  'aws',
  'gcp',
  'azure',
  'oracle',
  'hetzner',
  'digitalocean',
  'vultr',
  'linode',
  'scaleway',
  'ovh',
  'alibaba',
  'tencent',
  'unknown',
];
/** 04 §7.4's NAT kinds. */
const NAT_IDS = ['none', 'one_to_one', 'port_forward', 'symmetric', 'cgnat_likely', 'unknown'];

const admin = { admin: true };

function fix(opts: ResultFixtureOptions, who = admin) {
  const result = resultFixture(opts);
  return fixText(result, verdictOf(result), who);
}

const ids = (lines: readonly FixLine[]): string[] => lines.map((l) => l.id);
const byId = (lines: readonly FixLine[], id: FixLine['id']): FixLine | undefined => lines.find((l) => l.id === id);

/** The line as the panel shows it, without the <code> tags. */
function text(line: FixLine): string {
  return i18n.t(line.key, line.values).replace(/<\/?code>/g, '');
}

function catalogString(key: string): unknown {
  let node: unknown = en;
  for (const part of key.split('.')) {
    if (typeof node !== 'object' || node === null) return undefined;
    node = (node as Record<string, unknown>)[part];
  }
  return node;
}

/** UDP ✗ with TCP ✓: the result the fix text is mostly for. */
const udpBlocked: ResultFixtureOptions = { udp: 'timeout' };
const allBlocked: ResultFixtureOptions = { udp: 'timeout', tcp443: 'timeout', tcp7882: 'timeout' };

describe('provider fix text', () => {
  it('covers exactly the provider ids of 04 §13.3', () => {
    expect([...CLOUD_PROVIDERS]).toEqual(PROVIDER_IDS);
  });

  it.each(PROVIDER_IDS)('%s has fix text that names the ports and links to its install guide section', (id) => {
    const { lines } = fix({ ...udpBlocked, server: { provider: id } });
    const line = byId(lines, 'provider');
    expect(line).toMatchObject({ key: `fix.firewall.${id}`, provider: id });
    const raw = catalogString(`fix.firewall.${id}`);
    expect(typeof raw).toBe('string');
    // UDP 7882, TCP 443 and 7882, TCP 80 for certificates; the two media ports come from the server.
    expect(raw).toContain('UDP {{udpPort}}');
    expect(raw).toContain('TCP 443 and {{tcpPort}}');
    expect(raw).toContain('TCP 80 for certificates');
    const shown = line ? text(line) : '';
    expect(shown).toContain('UDP 7882, TCP 443 and 7882, and TCP 80 for certificates');
    expect(shown).not.toMatch(/\{\{|\}\}/);
  });

  it('says what is special about each kind of provider firewall', () => {
    const shown = (id: string): string => i18n.t(providerKey(knownProvider(id)), { udpPort: 7882, tcpPort: 7882 });
    expect(shown('aws')).toMatch(/security group/);
    expect(shown('gcp')).toMatch(/VPC firewall rule/);
    expect(shown('azure')).toMatch(/network security group/);
    expect(shown('oracle')).toMatch(/security list/);
    expect(shown('oracle')).toMatch(/iptables/);
    for (const id of ['hetzner', 'digitalocean', 'vultr', 'linode']) expect(shown(id)).toMatch(/Cloud Firewall/);
    for (const id of ['scaleway', 'ovh', 'alibaba', 'tencent']) expect(shown(id)).toMatch(/security group/);
    expect(shown('unknown')).toMatch(/your provider's firewall or security group/);
  });

  it('uses the generic text for a provider id this build does not know', () => {
    const { lines } = fix({ ...udpBlocked, server: { provider: 'newcloud' } });
    expect(byId(lines, 'provider')).toMatchObject({ key: 'fix.firewall.unknown', provider: 'unknown' });
    expect(knownProvider('constructor')).toBe('unknown');
    expect(knownProvider('')).toBe('unknown');
  });
});

describe('NAT text', () => {
  it('covers exactly the NAT kinds of 04 §7.4', () => {
    expect([...NAT_KINDS]).toEqual(NAT_IDS);
  });

  it.each([
    ['none', undefined],
    ['one_to_one', undefined],
    ['port_forward', 'nat_port_forward'],
    ['symmetric', 'nat_cgnat'],
    ['cgnat_likely', 'nat_cgnat'],
    ['unknown', undefined],
  ] as const)('%s has its line (troubleshooting code %s)', (nat, code) => {
    const { lines } = fix({ ...udpBlocked, server: { nat } });
    const line = byId(lines, 'nat');
    expect(line?.key).toBe(`conntest.nat.${nat}`);
    expect(line?.code).toBe(code);
    expect(typeof catalogString(`conntest.nat.${nat}`)).toBe('string');
    expect(line ? text(line) : '').not.toMatch(/\{\{|\}\}/);
  });

  it('says what 05 §14.2 says for the kinds an admin must act on', () => {
    const shown = (nat: string): string => i18n.t(natKey(knownNat(nat)), { udpPort: 7882, tcpPort: 7882 });
    expect(shown('port_forward')).toBe('Forward TCP 80, 443, 7882 and UDP 7882 on your router to this machine.');
    for (const nat of ['cgnat_likely', 'symmetric']) {
      expect(shown(nat)).toBe('This server has no public address; isshoni needs one (a VPS or a router port forward).');
    }
  });

  // 04 §7.4 and §7.5 put three servers under one_to_one: a cloud VM, a container on a VPS (a bridge passes only
  // published ports) and a container at home behind a router. The text must not rule any of them out.
  it('one_to_one names everything that can be in the way: the firewall, published ports, a home router', () => {
    const server = { udpPort: 5000, tcpPorts: [443, 5001], nat: 'one_to_one', container: 'docker' };
    const shown = text(byId(fix({ ...udpBlocked, server }).lines, 'nat') as FixLine);
    expect(shown).toMatch(/provider's firewall/);
    expect(shown).toMatch(/In a container, also publish them/);
    expect(shown).toContain('forward TCP 80, 443, 5001 and UDP 5000 on your router to this machine');
    expect(shown).not.toMatch(/\bonly\b/);
  });

  it('falls back to "unknown" for a kind this build does not know', () => {
    expect(byId(fix({ ...udpBlocked, server: { nat: 'double' } }).lines, 'nat')?.key).toBe('conntest.nat.unknown');
    expect(knownNat('toString')).toBe('unknown');
  });
});

describe('fixText for admins', () => {
  it('amber (UDP ✗, TCP ✓): open the UDP port, the provider, both host firewalls, the NAT kind, the network', () => {
    const { lines, sendToAdmin } = fix(udpBlocked);
    expect(sendToAdmin).toBe(false);
    expect(ids(lines)).toEqual(['openUdp', 'provider', 'ufw', 'firewalld', 'nat', 'blockedNetwork']);
    expect(text(lines[0] as FixLine)).toBe('Open UDP port 7882.');
    // Host firewall: always both lines, labeled.
    expect(byId(lines, 'ufw')).toMatchObject({ key: 'fix.hostFirewall.ufw', command: 'sudo ufw allow 7882/udp' });
    expect(byId(lines, 'firewalld')).toMatchObject({
      key: 'fix.hostFirewall.firewalld',
      command: 'sudo firewall-cmd --permanent --add-port=7882/udp && sudo firewall-cmd --reload',
    });
    expect(text(byId(lines, 'ufw') as FixLine)).toMatch(/ufw/);
    expect(text(byId(lines, 'firewalld') as FixLine)).toMatch(/firewalld/);
    expect(text(byId(lines, 'blockedNetwork') as FixLine)).toBe(
      "If this network blocks UDP (some offices and hotels do), test from home or from your phone's mobile data.",
    );
  });

  it('red (everything ✗): the same lines without "Open UDP port"', () => {
    expect(ids(fix(allBlocked).lines)).toEqual(['provider', 'ufw', 'firewalld', 'nat', 'blockedNetwork']);
  });

  it('green: no fix text', () => {
    expect(fix({})).toEqual({ lines: [], sendToAdmin: false });
    expect(fix({ tcp443: 'timeout' })).toEqual({ lines: [], sendToAdmin: false });
  });

  it("names the server's own ports when the admin changed them", () => {
    const { lines } = fix({ ...udpBlocked, server: { udpPort: 5000, tcpPorts: [443, 5001], nat: 'port_forward' } });
    expect(text(byId(lines, 'openUdp') as FixLine)).toBe('Open UDP port 5000.');
    expect(byId(lines, 'ufw')?.command).toBe('sudo ufw allow 5000/udp');
    expect(byId(lines, 'firewalld')?.command).toBe(
      'sudo firewall-cmd --permanent --add-port=5000/udp && sudo firewall-cmd --reload',
    );
    expect(text(byId(lines, 'provider') as FixLine)).toContain('UDP 5000, TCP 443 and 5001, and TCP 80');
    expect(text(byId(lines, 'nat') as FixLine)).toBe(
      'Forward TCP 80, 443, 5001 and UDP 5000 on your router to this machine.',
    );
  });

  describe('Docker', () => {
    it.each(['docker', 'podman'])('%s: check the published UDP port', (container) => {
      const { lines } = fix({ ...udpBlocked, server: { container } });
      const line = byId(lines, 'docker');
      expect(line).toMatchObject({ key: 'conntest.docker', code: 'docker_ports' });
      expect(text(line as FixLine)).toBe('Check that compose publishes 7882:7882/udp; published ports bypass ufw.');
      expect(ids(lines)).toEqual(['openUdp', 'provider', 'ufw', 'firewalld', 'nat', 'docker', 'blockedNetwork']);
    });

    it.each(['none', 'other', ''])('container "%s": no Docker line', (container) => {
      expect(byId(fix({ ...udpBlocked, server: { container } }).lines, 'docker')).toBeUndefined();
    });
  });

  describe('macOS Local Network', () => {
    const local = { publicIpKnown: true, publicIpPrivate: true };

    it("the admin's OS is macOS and the server's address is private or loopback", () => {
      const { lines } = fix({ ...udpBlocked, client: { os: 'macos' }, server: local });
      const line = byId(lines, 'macLocalNetwork');
      expect(line).toMatchObject({ key: 'conntest.macLocalNetwork', code: 'mac_local_network' });
      expect(text(line as FixLine)).toBe(
        'On a Mac, allow your browser Local Network access (System Settings → Privacy & Security → Local Network).',
      );
    });

    it('also when the address is empty but a probe worked', () => {
      const { lines } = fix({
        ...udpBlocked,
        client: { os: 'macos' },
        server: { publicIpKnown: false, publicIpPrivate: true },
      });
      expect(ids(lines)).toContain('macLocalNetwork');
    });

    it.each([
      ['another OS', { client: { os: 'windows' }, server: local }],
      ['iOS', { client: { os: 'ios' }, server: local }],
      ['a public address', { client: { os: 'macos' }, server: { publicIpPrivate: false } }],
    ] as const)('not for %s', (_name, opts) => {
      expect(ids(fix({ ...udpBlocked, ...opts }).lines)).not.toContain('macLocalNetwork');
    });
  });

  describe('no public address', () => {
    const noIp = { publicIpKnown: false, publicIpPrivate: true };

    it('every probe that ran failed: conntest.noPublicIp is the only line', () => {
      const { lines } = fix({
        ...allBlocked,
        client: { os: 'macos' },
        server: { ...noIp, provider: 'aws', container: 'docker', nat: 'port_forward' },
      });
      // No provider, host firewall, NAT, Docker, macOS or blocked-network line, and no own-network hint.
      expect(lines).toHaveLength(1);
      expect(lines[0]).toMatchObject({ id: 'noPublicIp', key: 'conntest.noPublicIp', code: 'no_public_ip' });
      expect(text(lines[0] as FixLine)).toBe(
        "The server doesn't know its public address, so browsers can't reach its media ports. Set public_ip in " +
          '/etc/isshoni/isshoni.toml (Docker: ISSHONI_PUBLIC_IP in .env) and restart.',
      );
    });

    it('a probe ✓: the normal text', () => {
      const { lines } = fix({ ...udpBlocked, server: { ...noIp, container: 'docker' } });
      expect(ids(lines)).toEqual(['openUdp', 'provider', 'ufw', 'firewalld', 'nat', 'docker', 'blockedNetwork']);
    });
  });

  describe('UDP turned off in the server config', () => {
    it('with TCP ✓ there is nothing to open: no firewall line', () => {
      const result = resultFixture({ udp: 'disabled', server: { udpPort: 0 } });
      const verdict = verdictOf(result);
      expect(verdict.status).toBe('amber');
      expect(fixText(result, verdict, admin)).toEqual({ lines: [], sendToAdmin: false });
    });

    it('with TCP ✗ the firewall may block TCP: the provider and NAT lines, nothing about the UDP port', () => {
      const { lines } = fix({
        udp: 'disabled',
        tcp443: 'timeout',
        tcp7882: 'timeout',
        client: { os: 'macos' },
        server: { udpPort: 0, container: 'docker', publicIpPrivate: true },
      });
      expect(ids(lines)).toEqual(['provider', 'nat', 'macLocalNetwork']);
    });
  });

  describe('probes that were not tested', () => {
    it('no status, no fix text', () => {
      expect(fix({ udp: 'rate_limited', tcp443: 'rate_limited', tcp7882: 'rate_limited', server: null })).toEqual({
        lines: [],
        sendToAdmin: false,
      });
      expect(fix({ udp: 'timeout', tcp443: 'server', tcp7882: 'timeout' }).lines).toEqual([]);
    });

    it('a status the untested probe cannot change keeps its fix text', () => {
      expect(ids(fix({ udp: 'timeout', tcp443: 'server' }).lines)).toEqual([
        'openUdp',
        'provider',
        'ufw',
        'firewalld',
        'nat',
        'blockedNetwork',
      ]);
    });
  });

  describe('the server is behind a home router (port_forward)', () => {
    const server = { nat: 'port_forward' };

    it('"You may be testing from the server\'s own network" on every result, green included', () => {
      const green = fix({ server });
      expect(ids(green.lines)).toEqual(['ownNetwork']);
      expect(text(green.lines[0] as FixLine)).toBe(
        "You may be testing from the server's own network. Test again from your phone's mobile data.",
      );
      expect(ids(fix({ ...udpBlocked, server }).lines)).toEqual([
        'openUdp',
        'provider',
        'ufw',
        'firewalld',
        'nat',
        'blockedNetwork',
        'ownNetwork',
      ]);
      expect(ids(fix({ ...allBlocked, server }).lines).at(-1)).toBe('ownNetwork');
      expect(ids(fix({ udp: 'server', server }).lines)).toEqual(['ownNetwork']);
    });

    it('not for other NAT kinds', () => {
      for (const nat of NAT_IDS.filter((n) => n !== 'port_forward')) {
        expect(ids(fix({ server: { nat } }).lines)).toEqual([]);
      }
    });
  });

  it('gives fix text without a server part too (a reply without one)', () => {
    const { lines } = fix({ ...udpBlocked, server: null });
    expect(ids(lines)).toEqual(['openUdp', 'provider', 'ufw', 'firewalld', 'blockedNetwork']);
    expect(byId(lines, 'provider')?.key).toBe('fix.firewall.unknown');
  });
});

describe('fixText for everyone else', () => {
  const user = { admin: false };

  it.each([
    ['amber', udpBlocked, true],
    ['red', allBlocked, true],
    ['red without a public address', { ...allBlocked, server: { publicIpKnown: false, publicIpPrivate: true } }, true],
    ['green', {}, false],
    ['green behind a home router', { server: { nat: 'port_forward' } }, false],
    ['not tested', { udp: 'rate_limited' }, false],
  ] as const)('%s: no fix text; "Send this to your admin" = %s', (_name, opts, sendToAdmin) => {
    expect(fix(opts, user)).toEqual({ lines: [], sendToAdmin });
  });
});

describe('every key fixText uses', () => {
  it('is a message in en.json, and every troubleshooting code is a known one', () => {
    const shorts = ['ok', 'timeout', 'failed', 'disabled', 'server'] as const;
    const servers: ResultFixtureOptions['server'][] = [null];
    for (const provider of PROVIDER_IDS) servers.push({ provider });
    for (const nat of NAT_IDS) servers.push({ nat, container: 'docker', publicIpPrivate: true });
    servers.push({ publicIpKnown: false, publicIpPrivate: true });
    const keys = new Set<string>();
    const codes = new Set<CtCode>();
    const results: ConnTestResult[] = [];
    for (const udp of shorts) {
      for (const tcp443 of shorts) {
        for (const tcp7882 of shorts) {
          for (const server of servers) {
            results.push(resultFixture({ udp, tcp443, tcp7882, server, client: { os: 'macos' } }));
          }
        }
      }
    }
    for (const result of results) {
      for (const line of fixText(result, verdictOf(result), admin).lines) {
        keys.add(line.key);
        if (line.code) codes.add(line.code);
      }
    }
    for (const key of keys) expect(typeof catalogString(key), key).toBe('string');
    // 13 providers, 6 NAT kinds, and the 8 fixed lines.
    expect(keys.size).toBe(PROVIDER_IDS.length + NAT_IDS.length + 8);
    expect([...codes].sort()).toEqual([
      'docker_ports',
      'mac_local_network',
      'nat_cgnat',
      'nat_port_forward',
      'no_public_ip',
    ]);
  });
});
