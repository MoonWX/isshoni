// doctorText.ts (05 §15.3, 04 §13.2): a check's message, fix and links from the catalog, against doctor's own
// golden report (internal/server/ops/doctor/testdata/report.server.json, the CLI's text of which is
// report.server.txt) and against the cases where the CLI's templates pick a variant.
import { describe, expect, it } from 'vitest';

import { i18n } from '../i18n';
import type { DoctorCheck, DoctorEnv, DoctorReport } from '../protocol/api.gen';
import {
  certificateFix,
  checkFix,
  checkLinks,
  checkMessage,
  checkText,
  FIREWALL_PROVIDERS,
  restoreCommand,
  shellQuote,
} from './doctorText';
import { doctorCheck } from './testing/harness';

const golden = Object.values(
  import.meta.glob<DoctorReport>('../../../internal/server/ops/doctor/testdata/report.server.json', {
    eager: true,
    import: 'default',
  }),
)[0];

const t = i18n.t.bind(i18n);
const LANG = 'en';

const HOST: DoctorEnv = {
  os: 'linux',
  arch: 'amd64',
  kernel: '6.8.0',
  container: 'none',
  systemd: true,
  provider: 'hetzner',
  user: 'isshoni',
};
const DOCKER: DoctorEnv = { ...HOST, container: 'docker', systemd: false, user: 'uid 998' };

const message = (check: Partial<DoctorCheck>, env: DoctorEnv = HOST) => checkMessage(doctorCheck(check), env, t, LANG);
const fix = (check: Partial<DoctorCheck>, env: DoctorEnv = HOST) => checkFix(doctorCheck(check), env, t, LANG);

describe('the golden report of doctor', () => {
  if (golden === undefined) throw new Error('report.server.json not found');

  it('renders every check from the catalog, without a key or a placeholder left', () => {
    for (const check of golden.checks) {
      const text = checkText(check, golden.env, t, LANG);
      expect(text.message, check.code).not.toBe(check.message);
      expect(text.message, check.code).not.toMatch(/\{\{|doctor\./);
      for (const step of text.fix) expect(step.text, check.fixCode).not.toMatch(/\{\{|doctor\./);
    }
  });

  it('reads like the CLI’s text', () => {
    const byId = new Map(golden.checks.map((c) => [c.id, checkText(c, golden.env, t, LANG)]));
    expect(byId.get('config')?.message).toBe('Defaults and flags.');
    expect(byId.get('data_dir')?.message).toBe('/var/lib/isshoni (mode 0700, 37.2 GB free).');
    expect(byId.get('public_ip')?.message).toBe('203.0.113.7 (on interface eth0, confirmed by STUN) and 2001:db8::7.');
    expect(byId.get('tls')?.message).toMatch(
      /^The IP certificate for 203\.0\.113\.7 is valid until Oct 5, 2026, .+ and renews on Oct 2, 2026\.$/,
    );
    expect(byId.get('clock')?.message).toBe('Synchronized, within 1 second of acme-v02.api.letsencrypt.org.');
    expect(byId.get('udp_buffers')?.message).toBe('net.core.rmem_max is 212992, isshoni wants 8388608.');
    expect(byId.get('udp_buffers')?.fix).toEqual([
      {
        text: 'Raise the limits:',
        command:
          "printf 'net.core.rmem_max=8388608\\nnet.core.wmem_max=8388608\\n' | sudo tee /etc/sysctl.d/60-isshoni.conf && sudo sysctl --system",
      },
    ]);
    expect(byId.get('udp_buffers')?.links).toEqual([
      { kind: 'more', url: 'https://moonwx.github.io/isshoni/troubleshooting#doctor-udp_buffers' },
    ]);
    expect(byId.get('ports')?.message).toBe('Listening on 443/tcp, 80/tcp, 7882/udp, and 7882/tcp.');
    expect(byId.get('firewall_hint')?.message).toBe(
      'Hetzner: in the Cloud Console, under Firewalls: allow TCP 80, 443, 7882 and UDP 7882.',
    );
    expect(byId.get('firewall_hint')?.links).toEqual([
      { kind: 'vps', url: 'https://moonwx.github.io/isshoni/install/vps#hetzner' },
    ]);
    expect(byId.get('transfer')?.message).toBe('412.0 GB of 1,000 GB sent in September 2026 (41%).');
    expect(byId.get('bandwidth')?.message).toBe(
      '5 people, 2 sharing: about 41.5 Mbps egress (44 on the wire), about 39 GB per 2-hour session.',
    );
    // ok and info results have nothing to fix and no "more".
    expect(byId.get('config')?.fix).toEqual([]);
    expect(byId.get('config')?.links).toEqual([]);
  });
});

describe('checkMessage', () => {
  it('says why public_ip is not checked on a development server (README F04), with no params', () => {
    const check = { id: 'public_ip', status: 'skip', code: 'public_ip.loopback', message: 'english' } as const;
    expect(message(check)).toBe(
      'Not checked: network.include_loopback is on and network.stun_servers is empty ' +
        '(a development setup, which needs no public address).',
    );
    expect(fix(check)).toEqual([]);
    expect(checkLinks(doctorCheck(check), HOST)).toEqual([]);
  });

  it('says that the UDP buffers could not be read', () => {
    expect(message({ id: 'udp_buffers', status: 'skip', code: 'udp_buffers.unknown' })).toBe(
      "Not checked: net.core.rmem_max can't be read here, and isshoni is not running.",
    );
  });

  it('picks the variant the params ask for', () => {
    expect(message({ code: 'config.ok', params: { file: '/etc/isshoni/isshoni.toml', fileRead: true } })).toBe(
      'Loaded from /etc/isshoni/isshoni.toml.',
    );
    expect(message({ code: 'config.ok', params: { file: '/etc/isshoni/isshoni.toml', fileRead: false } })).toBe(
      'No config file at /etc/isshoni/isshoni.toml: defaults, environment and flags.',
    );
    expect(message({ code: 'config.warnings', params: { warnings: 1, first: 'x is deprecated' } })).toBe(
      '1 warning: x is deprecated',
    );
    expect(message({ code: 'config.warnings', params: { warnings: 3, first: 'x is deprecated' } })).toBe(
      '3 warnings: x is deprecated',
    );
    expect(message({ code: 'data_dir.not_mounted', params: { path: '/data', error: 'no mountinfo' } })).toBe(
      "Can't check that /data is a mounted volume: no mountinfo",
    );
    expect(message({ code: 'public_ip.no_ipv6', params: { ipv4: '203.0.113.7' } }, DOCKER)).toBe(
      '203.0.113.7. IPv6 media is off (Docker bridge).',
    );
    expect(message({ code: 'public_ip.none', params: { siteIsAddress: true, running: true } })).toMatch(
      /isshoni is running but not ready, and nobody can open it\.$/,
    );
    expect(message({ code: 'public_ip.none', params: { siteIsAddress: false } })).toMatch(
      /media has no address to offer\.$/,
    );
    expect(
      message({
        code: 'public_ip.port_forward',
        params: { ipv4: '198.51.100.4', ipv4Method: 'stun', local: '192.168.1.20', nat: 'port_forward' },
      }),
    ).toBe('198.51.100.4 (seen by STUN). This machine is 192.168.1.20 behind a router.');
    expect(message({ code: 'clock.skew', params: { skewS: -45, host: 'acme' } })).toBe(
      'The clock is 45 seconds behind (compared with acme).',
    );
    expect(message({ code: 'clock.skew', params: { skewS: 420, host: 'acme' } })).toBe(
      'The clock is 7 minutes ahead (compared with acme).',
    );
    expect(
      message({ code: 'ports.free', params: { free: ['443/tcp'], unchecked: ['80/tcp'], uncheckedDenied: true } }),
    ).toBe('443/tcp is free; not checked without root: 80/tcp.');
    expect(message({ code: 'ports.in_use', params: { inUse: ['443/tcp', '7882/udp'] } })).toBe(
      '443/tcp and 7882/udp are in use by another program.',
    );
    expect(
      message({
        code: 'transfer.high',
        params: { month: '2026-09', egressBytes: 850e9, alertGb: 1000, pct: 85, projectedEgressBytes: 1100e9 },
      }),
    ).toBe('850.0 GB of 1,000 GB sent in September 2026 (85%). At this rate the month ends at 1,100.0 GB.');
    expect(message({ code: 'tls.expired', params: { mode: 'manual', notAfter: '2026-09-01T00:00:00.000Z' } })).toBe(
      'The manual certificate expired on Sep 1, 2026.',
    );
    expect(message({ code: 'firewall_hint.open_ports', params: { provider: 'unknown', udpPorts: ['7882'] } })).toBe(
      "Open UDP 7882 in your provider's firewall or security group, and in this machine's own firewall if it has one.",
    );
    expect(
      message({
        code: 'firewall_hint.open_ports',
        params: { provider: 'oracle', tcpPorts: ['7882'], udpPorts: ['7882'], proxy: true },
      }),
    ).toBe(
      "Oracle Cloud: the ingress rules of the subnet's security list: allow TCP 7882 and UDP 7882. Oracle's images " +
        'also block these ports with their own iptables rules: open them on the server too. TCP 80 and 443 belong ' +
        'to the proxy in front: open them for it.',
    );
    expect(message({ code: 'container.detected', params: { kind: 'docker', network: 'host' } })).toBe(
      "Docker, host network: isshoni uses the host's ports directly.",
    );
  });

  it('has the firewall’s place for every provider id of 04 §13.3', () => {
    expect(FIREWALL_PROVIDERS.length).toBe(12);
    for (const provider of FIREWALL_PROVIDERS) {
      const text = message({ code: 'firewall_hint.open_ports', params: { provider, udpPorts: ['7882'] } });
      expect(text, provider).toMatch(/^[A-Z][^:]+: .+: allow UDP 7882\./);
      expect(text, provider).not.toContain('doctor.part');
    }
  });

  it('names the owner the way the CLI does', () => {
    const params = { path: '/var/lib/isshoni', owner: 0, uid: 998 };
    expect(message({ code: 'data_dir.wrong_owner', params: { ...params, user: 'isshoni' } })).toBe(
      '/var/lib/isshoni belongs to uid 0, but isshoni runs as the user isshoni (uid 998).',
    );
    expect(message({ code: 'data_dir.wrong_owner', params: { ...params, offline: true } })).toBe(
      '/var/lib/isshoni belongs to uid 0, but doctor runs as uid 998.',
    );
    expect(message({ code: 'secrets.wrong_owner', params })).toBe(
      '/var/lib/isshoni belongs to uid 0, but isshoni runs as uid 998.',
    );
  });

  it('shows the server’s English message for a code this build doesn’t know', () => {
    expect(message({ code: 'quantum.entangled', message: 'the qubits are tangled' })).toBe('the qubits are tangled');
  });

  it('shows the server’s message when the params can’t make a sentence', () => {
    expect(message({ code: 'firewall_hint.open_ports', params: { provider: 'aws' }, message: 'english' })).toBe(
      'english',
    );
  });
});

describe('checkFix', () => {
  it('has none without a fix code', () => {
    expect(fix({ code: 'config.ok' })).toEqual([]);
  });

  it('gives the commands of the environment', () => {
    expect(fix({ code: 'clock.unsynced', fixCode: 'clock.fix_ntp' })).toEqual([
      { text: 'Turn on time synchronization:', command: 'sudo timedatectl set-ntp true' },
    ]);
    expect(fix({ code: 'clock.unsynced', fixCode: 'clock.fix_ntp' }, DOCKER)).toEqual([
      { text: 'On the Docker host, turn on time synchronization:', command: 'sudo timedatectl set-ntp true' },
    ]);
    expect(fix({ code: 'udp_buffers.low', fixCode: 'udp_buffers.fix_restart' })).toEqual([
      {
        text: 'Restart isshoni so that its sockets get the larger buffers:',
        command: 'sudo systemctl restart isshoni',
      },
    ]);
    expect(fix({ code: 'udp_buffers.low', fixCode: 'udp_buffers.fix_restart' }, { ...HOST, systemd: false })).toEqual([
      { text: 'Restart isshoni so that its sockets get the larger buffers.' },
    ]);
    expect(fix({ code: 'config.warnings', fixCode: 'config.fix_check', params: { warnings: 2 } }, DOCKER)).toEqual([
      { text: 'This lists every problem with its fix:', command: 'docker compose exec isshoni isshoni config check' },
    ]);
    expect(
      fix({
        code: 'config.invalid',
        fixCode: 'config.fix_check',
        params: { errors: 2, warnings: 0, firstFix: 'set listen.https to a port' },
      }),
    ).toEqual([
      { text: 'set listen.https to a port' },
      { text: 'This lists every problem with its fix:', command: 'isshoni config check' },
    ]);
  });

  it('quotes paths for the shell', () => {
    expect(
      fix({ code: 'data_dir.mode_wide', fixCode: 'data_dir.fix_mode', params: { path: "/srv/it's here" } }),
    ).toEqual([{ text: 'Make it private to isshoni:', command: "sudo chmod 700 '/srv/it'\\''s here'" }]);
    expect(shellQuote('/var/lib/isshoni')).toBe('/var/lib/isshoni');
    expect(restoreCommand('/var/lib/isshoni/backups/pre-7.db', false)).toBe(
      'sudo -u isshoni isshoni admin restore --offline /var/lib/isshoni/backups/pre-7.db && sudo systemctl start isshoni',
    );
  });

  it('gives an owner to the service’s user, or names both cases when it can’t know which', () => {
    const params = { path: '/var/lib/isshoni', owner: 0, uid: 998 };
    expect(
      fix({ code: 'data_dir.wrong_owner', fixCode: 'data_dir.fix_owner', params: { ...params, user: 'isshoni' } }),
    ).toEqual([
      { text: 'Give it to the user that isshoni runs as:', command: 'sudo chown -R isshoni /var/lib/isshoni' },
    ]);
    expect(
      fix(
        { code: 'secrets.wrong_owner', fixCode: 'secrets.fix_owner', params: { ...params, offline: true } },
        { ...HOST, user: 'sam' },
      ),
    ).toEqual([
      {
        text: 'If isshoni runs as the owner (uid 0), run doctor as that user:',
        command: "sudo -u '#0' isshoni doctor",
      },
      { text: 'If it runs as sam:', command: 'sudo chown sam /var/lib/isshoni' },
    ]);
    expect(fix({ code: 'data_dir.wrong_owner', fixCode: 'data_dir.fix_owner', params }, DOCKER)).toEqual([
      {
        text: "On the container's host, give the host path of /var/lib/isshoni to uid 998:",
        command: 'sudo chown -R 998:998 <host path>',
      },
    ]);
  });

  it('gives the server’s own sentence for a certificate error when it has one', () => {
    const check = {
      id: 'tls',
      status: 'warn',
      code: 'tls.failing',
      params: { mode: 'auto', name: 'watch.example.com', ip: '203.0.113.7' },
      fixCode: 'tls.dns_wrong',
    } as const;
    expect(fix(check)).toEqual([
      {
        text: "watch.example.com doesn't point to this server (203.0.113.7). Change its A record (see the DNS check).",
      },
    ]);
    const lastError = 'watch.example.com points to 198.51.100.9, not to this server (203.0.113.7)';
    expect(fix({ ...check, params: { ...check.params, lastError } })).toEqual([{ text: lastError }]);
    // A certificate code this build doesn't know: the general text, which points at the log.
    expect(fix({ ...check, fixCode: 'tls.brand_new' })).toEqual([
      {
        text: "The certificate authority refused or could not be reached. The server's log has its answer:",
        command: 'journalctl -u isshoni',
      },
    ]);
  });

  it('gives a certificate code’s fix without a report, with no command that depends on how the server runs', () => {
    const cert = (code: string, params: Record<string, unknown> = { mode: 'auto', names: ['watch.example.com'] }) =>
      certificateFix(code, params, t, LANG);
    // The log's command is the env's: without one the sentence ends where the command would have come.
    expect(cert('tls.rate_limited')).toEqual([
      {
        text: "Let's Encrypt's rate limit was reached. If you reinstalled, restore certmagic/ from a backup. The server's log says until when.",
      },
    ]);
    expect(cert('tls.acme_failed')).toEqual([
      { text: "The certificate authority refused or could not be reached. The server's log has its answer." },
    ]);
    expect(cert('tls.brand_new')).toEqual(cert('tls.acme_failed'));
    // A command that is the same everywhere stays.
    expect(cert('tls.cert_unreadable', { mode: 'manual', names: [] })).toEqual([
      {
        text: "isshoni can't read tls.cert_file or tls.key_file. Check the paths, and let the user that isshoni runs as read both, for example:",
        command: 'sudo chgrp isshoni <file> && sudo chmod 640 <file>',
      },
    ]);
    // The address only where it is known.
    expect(cert('tls.dns_missing')).toEqual([
      { text: "watch.example.com doesn't resolve. Add an A record pointing to this server." },
    ]);
    expect(cert('tls.dns_missing', { mode: 'auto', names: ['watch.example.com'], ip: '203.0.113.7' })).toEqual([
      { text: "watch.example.com doesn't resolve. Add an A record pointing to 203.0.113.7." },
    ]);
    // Not a certificate code: nothing to say.
    expect(cert('public_ip.fix_set')).toEqual([]);
    expect(cert('')).toEqual([]);
    // checkFix, with a report, still names the command.
    expect(fix({ id: 'tls', code: 'tls.renewal_failing', fixCode: 'tls.rate_limited' }, DOCKER)).toEqual([
      {
        text: "Let's Encrypt's rate limit was reached. If you reinstalled, restore certmagic/ from a backup. The server's log says until when:",
        command: 'docker compose logs isshoni',
      },
    ]);
  });

  it('gives both ways out of a missing public address', () => {
    expect(fix({ code: 'public_ip.none', fixCode: 'public_ip.fix_set' })).toEqual([
      { text: "Set public_ip to this server's public address, or set domain, then restart isshoni." },
      {
        text: "If the network wasn't up yet when isshoni started, a restart is enough:",
        command: 'sudo systemctl restart isshoni',
      },
    ]);
    expect(fix({ code: 'public_ip.none', fixCode: 'public_ip.fix_set' }, DOCKER)).toEqual([
      {
        text: "Set ISSHONI_PUBLIC_IP in .env to this server's public address, or set a domain, then run:",
        command: 'docker compose up -d',
      },
      {
        text: "If the network wasn't up yet when isshoni started, a restart is enough:",
        command: 'docker compose restart',
      },
    ]);
  });

  it('shows the server’s English fix for a fix code this build doesn’t know', () => {
    expect(fix({ code: 'config.ok', fixCode: 'config.fix_magic', fix: 'wave a wand' })).toEqual([
      { text: 'wave a wand' },
    ]);
  });
});

describe('checkLinks', () => {
  it('links a warning or failure to its section, and the ephemeral address to the provider’s guide', () => {
    expect(checkLinks(doctorCheck({ id: 'dns', status: 'fail', code: 'dns.wrong' }), HOST)).toEqual([
      { kind: 'more', url: 'https://moonwx.github.io/isshoni/troubleshooting#doctor-dns' },
    ]);
    expect(
      checkLinks(
        doctorCheck({ id: 'public_ip', status: 'info', code: 'public_ip.ephemeral', params: { provider: 'aws' } }),
        HOST,
      ),
    ).toEqual([{ kind: 'vps', url: 'https://moonwx.github.io/isshoni/install/vps#aws' }]);
    expect(
      checkLinks(
        doctorCheck({
          id: 'release',
          status: 'warn',
          code: 'release.update',
          fixCode: 'release.fix_upgrade',
          params: { url: 'https://github.com/MoonWX/isshoni/releases/tag/v0.3.1' },
        }),
        HOST,
      ),
    ).toEqual([
      { kind: 'more', url: 'https://moonwx.github.io/isshoni/troubleshooting#doctor-release' },
      { kind: 'release', url: 'https://github.com/MoonWX/isshoni/releases/tag/v0.3.1' },
    ]);
  });
});
