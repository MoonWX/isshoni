// How a doctor check reads on the Doctor page (05 §15.3, 04 §13.2, §13.5). The server sends each check's `code`,
// `params` and `fixCode`, plus English `message` and `fix` for the CLI; the page renders the codes from the catalog
// instead: `doctor.<code>` for the message and `doctor.fix.<fixCode>` for the fix, with the params as values.
//
// The rules mirror the CLI's templates (internal/server/ops/doctor/messages_en.go) one for one: where the CLI picks a
// sentence by the params (a path with or without its free space, an owner known by name or by number), the catalog
// has the variants as i18next contexts of the code's key (`doctor.data_dir.ok_free`), and counts as plurals
// (`doctor.config.warnings_one`). Shell commands are not text: a fix is a list of steps, each a sentence and maybe a
// command, which the page shows as a block to copy. The commands follow the report's env, as the CLI's do: Docker
// Compose in a container, systemd on a host.
//
// A code this build doesn't know (a newer server's) shows the server's English message, and an unknown fix code its
// English fix: they are still precise. A params value that is missing reads as empty, as in the CLI.
//
// doctorCatalog.node.test.ts holds the catalog to the CLI's list of codes, both ways.
import type { TFunction } from 'i18next';

import { CLOUD_PROVIDERS, DOCS_URL, knownProvider, vpsGuideUrl } from '../conntest';
import { i18n } from '../i18n';
import {
  CloudProviderGCP,
  CloudProviderOracle,
  CloudProviderUnknown,
  ContainerKindNone,
  DoctorStatusFail,
  DoctorStatusWarn,
  type CloudProvider,
  type DoctorCheck,
  type DoctorEnv,
} from '../protocol/api.gen';
import { Format } from './format';

/** One step of a fix: what to do, and the command that does it, when there is one. */
export interface FixStep {
  readonly text: string;
  readonly command?: string;
}

/** A link under a check: its troubleshooting section, the provider's install guide, a release's notes. */
export interface CheckLink {
  readonly kind: 'more' | 'vps' | 'release';
  readonly url: string;
}

/** A check as the page shows it. */
export interface CheckText {
  readonly message: string;
  /** Empty when there is nothing to do. */
  readonly fix: readonly FixStep[];
  readonly links: readonly CheckLink[];
}

/** The troubleshooting section of a check on the project site (06 §10.4: /troubleshooting#doctor-<id>). */
export function doctorDocsUrl(id: string): string {
  return `${DOCS_URL}troubleshooting#doctor-${id}`;
}

/** Whether the catalog has a plain message at this key (not a group of messages). */
function hasMessage(key: string): boolean {
  const lng = i18n.resolvedLanguage ?? i18n.language;
  return typeof i18n.getResource(lng, 'translation', key) === 'string';
}

// ---- reading params ----

type Params = Readonly<Record<string, unknown>>;

/** The params of one check, read the way the CLI reads them (messages_en.go `params`). */
class Reader {
  constructor(private readonly p: Params) {}

  has(key: string): boolean {
    return Object.hasOwn(this.p, key);
  }

  str(key: string): string {
    const v = this.p[key];
    if (typeof v === 'string') return v;
    if (typeof v === 'number' || typeof v === 'boolean') return String(v);
    return '';
  }

  num(key: string): number {
    const v = this.p[key];
    return typeof v === 'number' && Number.isFinite(v) ? v : 0;
  }

  /** Whole numbers, as the CLI's int(): 2.9 is 2. */
  int(key: string): number {
    return Math.trunc(this.num(key));
  }

  bool(key: string): boolean {
    return this.p[key] === true;
  }

  list(key: string): string[] {
    const v = this.p[key];
    if (!Array.isArray(v)) return [];
    return v.map((x: unknown) => (typeof x === 'string' ? x : String(x)));
  }

  /** Every param as a value for interpolation: lists joined, anything else as text. */
  values(fmt: Format): Record<string, string> {
    const out: Record<string, string> = {};
    for (const [key, v] of Object.entries(this.p)) {
      out[key] = Array.isArray(v) ? fmt.list(this.list(key)) : this.str(key);
    }
    return out;
  }
}

// ---- the environment ----

function inContainer(e: DoctorEnv): boolean {
  return (e.container as string) !== '' && e.container !== ContainerKindNone;
}

/** An isshoni command as the operator types it in this environment. */
function cli(e: DoctorEnv, args: string): string {
  return inContainer(e) ? `docker compose exec isshoni isshoni ${args}` : `sudo isshoni ${args}`;
}

/** The command that restarts the server in this environment, or '' where doctor can't name it. */
function restartCommand(e: DoctorEnv): string {
  if (inContainer(e)) return 'docker compose restart';
  if (e.systemd) return 'sudo systemctl restart isshoni';
  return '';
}

/** The command that shows the server's log. */
function logCommand(e: DoctorEnv): string {
  return inContainer(e) ? 'docker compose logs isshoni' : 'journalctl -u isshoni';
}

/** s as one word of a shell command: as it is when it has only plain characters, in single quotes otherwise. */
export function shellQuote(s: string): string {
  if (s !== '' && /^[A-Za-z0-9\-_./=:,@%+]+$/.test(s)) return s;
  return `'${s.replaceAll("'", `'\\''`)}'`;
}

/** 04 §6.1 step 4's restore command (doctor.RestoreCommand). */
export function restoreCommand(backup: string, container: boolean): string {
  const arg = shellQuote(backup);
  if (container) {
    return `docker compose stop && docker compose run --rm isshoni admin restore --offline ${arg} && docker compose up -d`;
  }
  return `sudo -u isshoni isshoni admin restore --offline ${arg} && sudo systemctl start isshoni`;
}

// ---- messages ----

/** What a rule asks of the catalog: values, an i18next context (a variant of the code's text), a count. */
interface Msg {
  readonly values?: Readonly<Record<string, string | number>>;
  readonly context?: string;
  readonly count?: number;
  /** Sentences after the code's text (the firewall hint's notes). */
  readonly extra?: readonly string[];
}

interface Ctx {
  readonly p: Reader;
  readonly e: DoctorEnv;
  /**
   * False where no report says how the server runs (certificateFix, the dashboard's): `e` is NO_ENV then, and a
   * step whose command depends on it has none.
   */
  readonly envKnown: boolean;
  readonly f: Format;
  readonly t: TFunction;
}

/** A rule returns null when the params can't make a sentence: the server's English message shows instead. */
type Rule = (c: Ctx) => Msg | null;

/**
 * The addresses that only this machine can reach (the param unreachable, README F21: a public_ip literal such as
 * 127.0.0.1), or "" (messages_en.go unreachableText).
 */
function unreachableText({ p, f, t }: Ctx): string {
  const addrs = p.list('unreachable');
  return addrs.length === 0 ? '' : t('doctor.part.unreachable', { addresses: f.list(addrs) });
}

/** The note where public_ip let a container's bridge NAT pass (the param dockerBridge, README F21; bridgeNATNote). */
function bridgeNATNote({ p, t }: Ctx): string[] {
  return p.bool('dockerBridge') ? [t('doctor.part.bridgeNAT')] : [];
}

/** The first part of the public_ip messages: the addresses and how they were found (messages_en.go publicIPText). */
function addressText({ p, f, t }: Ctx): string {
  const parts: string[] = [];
  const ip = p.str('ipv4');
  if (ip !== '') {
    const nat = p.str('nat');
    switch (p.str('ipv4Method')) {
      case 'config':
        parts.push(t('doctor.part.ip.config', { ip }));
        break;
      case 'interface': {
        const iface = p.str('iface');
        const confirmed = nat === 'none';
        if (iface !== '') {
          parts.push(
            confirmed
              ? t('doctor.part.ip.interfaceNamedConfirmed', { ip, iface })
              : t('doctor.part.ip.interfaceNamed', { ip, iface }),
          );
        } else {
          parts.push(
            confirmed ? t('doctor.part.ip.interfaceConfirmed', { ip }) : t('doctor.part.ip.interface', { ip }),
          );
        }
        break;
      }
      case 'stun':
        parts.push(
          nat === 'one_to_one' && p.has('local')
            ? t('doctor.part.ip.stunNat', { ip, local: p.str('local') })
            : t('doctor.part.ip.stun', { ip }),
        );
        break;
      default:
        parts.push(ip);
    }
  }
  const ipv6 = p.str('ipv6');
  if (ipv6 !== '') parts.push(ipv6);
  return parts.length === 0 ? t('doctor.part.noAddress') : f.list(parts);
}

/** "IP certificate for 203.0.113.7", "certificate for watch.example.com" (messages_en.go tlsSubject). */
function tlsSubject({ p, f, t }: Ctx): string {
  let kind: string;
  switch (p.str('mode')) {
    case 'ip':
      kind = t('doctor.part.cert.ip');
      break;
    case 'manual':
      kind = t('doctor.part.cert.manual');
      break;
    default:
      kind = t('doctor.part.cert.auto');
  }
  let names = p.list('names');
  if (names.length === 0 && p.has('name')) names = [p.str('name')];
  return names.length === 0 ? kind : t('doctor.part.cert.for', { kind, names: f.list(names) });
}

/** "TCP 80, 443, 7882 and UDP 7882" from tcpPorts and udpPorts; '' when there are none. */
function portsText({ p, f, t }: Ctx): string {
  const parts: string[] = [];
  const tcp = p.list('tcpPorts');
  const udp = p.list('udpPorts');
  if (tcp.length > 0) parts.push(t('doctor.part.tcpPorts', { ports: tcp.join(', ') }));
  if (udp.length > 0) parts.push(t('doctor.part.udpPorts', { ports: udp.join(', ') }));
  return f.list(parts);
}

/** The ports that are free, and those that weren't checked (messages_en.go portsFreeText), apart. */
function portsFree({ p, f, t }: Ctx): { free: string; unchecked: string } {
  const free = p.list('free');
  const freeText =
    free.length === 0
      ? t('doctor.part.noPortChecked')
      : t('doctor.part.portsFree', { ports: f.list(free), count: free.length });
  const unchecked = p.list('unchecked');
  let uncheckedText = '';
  if (unchecked.length > 0) {
    uncheckedText = p.bool('uncheckedDenied')
      ? t('doctor.part.uncheckedDenied', { ports: unchecked.join(', ') })
      : t('doctor.part.unchecked', { ports: unchecked.join(', ') });
  }
  return { free: freeText, unchecked: uncheckedText };
}

/** The owner findings of data_dir and secrets (messages_en.go wrongOwnerText). */
const wrongOwner: Rule = ({ p }) => {
  const values = { path: p.str('path'), owner: p.int('owner'), uid: p.int('uid'), user: p.str('user') };
  if (p.has('user')) return { values, context: 'user' };
  if (callerAsService(p)) return { values, context: 'caller' };
  return { values };
};

/**
 * The uid of an owner finding is the one doctor itself runs as: an offline run that did not find the service's user
 * by name (messages_en.go callerAsService).
 */
function callerAsService(p: Reader): boolean {
  return p.bool('offline') && !p.has('user');
}

const transferRule: Rule = ({ p, f }) => {
  const values = {
    egress: f.size(p.num('egressBytes')),
    limit: f.gigabytes(p.num('alertGb')),
    month: f.month(p.str('month')),
    percent: f.percent(p.num('pct')),
    projected: f.size(p.num('projectedEgressBytes')),
  };
  return p.has('projectedEgressBytes') ? { values, context: 'projected' } : { values };
};

const bandwidthRule: Rule = ({ p, f }) => {
  const hours = p.num('hours');
  // The whole gigabytes come from the wire rate, not from transferPerSessionGb, which is rounded to one place
  // already (as the CLI does it: rounding twice would print 100 for 99.49).
  const gb = (p.num('egressWireMbps') * hours * 3600) / 8 / 1000;
  return {
    values: {
      people: p.int('people'),
      sharing: p.int('sharing'),
      hours: f.number(hours, Number.isInteger(hours) ? 0 : 1),
      egress: f.number(p.num('egressMediaMbps'), 1),
      wire: f.number(p.num('egressWireMbps'), 0),
      gb: f.number(gb, 0),
      iface: p.str('iface'),
      nicMbps: p.int('nicMbps'),
    },
  };
};

const nofileRule: Rule = ({ p }) => {
  const values = { limit: p.int('limit'), want: p.int('want') };
  return p.bool('offline') ? { values, context: 'offline' } : { values };
};

const MESSAGES: Readonly<Record<string, Rule>> = {
  'config.ok': ({ p }) => {
    const file = p.str('file');
    if (p.bool('fileRead')) return { values: { file } };
    if (file !== '') return { values: { file }, context: 'noFile' };
    return { context: 'defaults' };
  },
  'config.warnings': ({ p }) => ({ values: { first: p.str('first') }, count: p.int('warnings') }),
  'config.invalid': ({ p }) => ({ values: { first: p.str('first') }, count: p.int('errors') }),

  'data_dir.ok': ({ p, f }) => {
    const values = { path: p.str('path'), mode: p.str('mode'), free: f.size(p.num('freeBytes')) };
    return p.has('freeBytes') ? { values, context: 'free' } : { values };
  },
  'data_dir.not_mounted': ({ p }) => {
    const values = { path: p.str('path'), error: p.str('error') };
    return p.has('error') ? { values, context: 'error' } : { values };
  },
  'data_dir.wrong_owner': wrongOwner,
  'data_dir.low_space': ({ p, f }) => ({ values: { path: p.str('path'), free: f.size(p.num('freeBytes')) } }),
  'secrets.missing': ({ p }) => ({
    values: { path: p.str('path') },
    ...(p.bool('offline') ? { context: 'offline' } : {}),
  }),
  'secrets.wrong_owner': wrongOwner,
  'schema.ok': ({ p }) => {
    const values = { version: p.int('version'), path: p.str('path') };
    return p.has('path') ? { values, context: 'path' } : { values };
  },

  'public_ip.ok': (c) => ({ values: { addresses: addressText(c) }, extra: bridgeNATNote(c) }),
  'public_ip.no_ipv6': (c): Msg => {
    const unreachable = unreachableText(c);
    if (unreachable !== '') {
      return { values: { addresses: addressText(c), unreachable }, context: 'unreachable', extra: bridgeNATNote(c) };
    }
    return {
      values: { addresses: addressText(c) },
      ...(inContainer(c.e) ? { context: 'container' } : {}),
      extra: bridgeNATNote(c),
    };
  },
  'public_ip.ipv6_only': (c): Msg => {
    const unreachable = unreachableText(c);
    return unreachable !== ''
      ? { values: { addresses: addressText(c), unreachable }, context: 'unreachable' }
      : { values: { addresses: addressText(c) } };
  },
  'public_ip.ephemeral': (c) => ({ values: { addresses: addressText(c) }, extra: bridgeNATNote(c) }),
  'public_ip.port_forward': (c) => {
    const values = { addresses: addressText(c), local: c.p.str('local') };
    return c.p.has('local') ? { values, context: 'local' } : { values };
  },
  'public_ip.symmetric': (c) => ({ values: { addresses: addressText(c) } }),
  'public_ip.none': (c) => {
    const unreachable = unreachableText(c);
    if (unreachable !== '') {
      return { values: { unreachable }, context: c.p.bool('siteIsAddress') ? 'unreachableSite' : 'unreachable' };
    }
    if (c.p.bool('siteIsAddress')) return { context: c.p.bool('running') ? 'running' : 'offline' };
    return {};
  },

  'dns.ok': ({ p, f }) => {
    const values = { domain: p.str('domain'), addresses: f.list([...p.list('a'), ...p.list('aaaa')]) };
    return p.str('caa') === 'allows' ? { values, context: 'caa' } : { values };
  },
  'dns.wrong': ({ p, f }) => ({
    values: { domain: p.str('domain'), others: f.list(p.list('others')), here: p.str('ip') || p.str('ipv6') },
  }),
  'dns.unverified': ({ p, f }) => ({
    values: { domain: p.str('domain'), addresses: f.list([...p.list('a'), ...p.list('aaaa')]) },
  }),

  'tls.ok': (c) => {
    const { p, f } = c;
    const values = {
      subject: tlsSubject(c),
      notAfter: f.time(p.str('notAfter')),
      nextRenewal: f.date(p.str('nextRenewal')),
    };
    if (p.has('notAfter') && p.has('nextRenewal')) return { values, context: 'renews' };
    if (p.has('notAfter')) return { values, context: 'valid' };
    return { values };
  },
  'tls.pending': (c) => ({ values: { subject: tlsSubject(c) } }),
  'tls.failing': (c) => ({ values: { subject: tlsSubject(c) } }),
  'tls.no_cert': (c) => ({
    values: { subject: tlsSubject(c), uptime: c.f.span(Math.trunc(c.p.int('uptimeS') / 60), 'minute') },
  }),
  'tls.renewal_failing': (c) => {
    const values = { subject: tlsSubject(c), notAfter: c.f.time(c.p.str('notAfter')) };
    return c.p.has('notAfter') ? { values, context: 'valid' } : { values };
  },
  'tls.reload_failed': (c) => ({ values: { subject: tlsSubject(c) } }),
  'tls.expires_soon': (c) => ({
    values: { subject: tlsSubject(c), date: c.f.date(c.p.str('notAfter')), days: c.f.span(c.p.int('days'), 'day') },
  }),
  'tls.expired': (c) => {
    const values = { subject: tlsSubject(c), date: c.f.date(c.p.str('notAfter')) };
    return c.p.has('notAfter') ? { values, context: 'date' } : { values };
  },

  'clock.ok': ({ p, f }) => {
    const synced = p.str('sync') === 'yes';
    const values = { skew: f.span(Math.max(Math.abs(p.int('skewS')), 1), 'second'), host: p.str('host') };
    if (p.has('skewS')) return { values, context: synced ? 'syncedSkew' : 'skew' };
    return synced ? { context: 'synced' } : {};
  },
  'clock.unknown': ({ p }) => {
    const values = { host: p.str('host'), error: p.str('error') };
    return p.has('error') ? { values, context: 'error' } : { values };
  },
  'clock.skew': ({ p, f, t }) => {
    const s = p.int('skewS');
    const abs = Math.abs(s);
    const duration = abs < 120 ? f.span(abs, 'second') : f.span(Math.round(abs / 60), 'minute');
    const skew = s < 0 ? t('doctor.part.behind', { duration }) : t('doctor.part.ahead', { duration });
    return { values: { skew, host: p.str('host') } };
  },
  'clock.cert_time': ({ p, f }) => ({ values: { now: f.time(p.str('now')), host: p.str('host') } }),

  'udp_buffers.ok': ({ p }) => {
    const values = {
      rmemMax: p.int('rmemMax'),
      wmemMax: p.int('wmemMax'),
      rcvBuf: p.int('rcvBuf'),
      sndBuf: p.int('sndBuf'),
    };
    if (p.has('rmemMax') && p.has('rcvBuf')) return { values, context: 'both' };
    if (p.has('rmemMax')) return { values, context: 'sysctl' };
    return { values };
  },
  'udp_buffers.low': ({ p }) => {
    const want = p.int('want');
    if (p.has('rmemMax') && p.int('rmemMax') < want)
      return { values: { value: p.int('rmemMax'), want }, context: 'rmem' };
    if (p.has('wmemMax') && p.int('wmemMax') < want)
      return { values: { value: p.int('wmemMax'), want }, context: 'wmem' };
    if (p.int('rcvBuf') < want) return { values: { value: p.int('rcvBuf'), want }, context: 'rcv' };
    return { values: { value: p.int('sndBuf'), want } };
  },

  'ports.listening': ({ p, f }) => ({ values: { ports: f.list(p.list('listening')) } }),
  'ports.free': (c) => {
    const { free, unchecked } = portsFree(c);
    const values = { free, unchecked };
    return unchecked === '' ? { values } : { values, context: 'unchecked' };
  },
  'ports.disabled': (c) => {
    const { p, f, t } = c;
    let first: string;
    if (p.has('listening')) {
      first = t('doctor.part.listeningOn', { ports: f.list(p.list('listening')) });
    } else {
      const { free, unchecked } = portsFree(c);
      first = unchecked === '' ? free : `${free}; ${unchecked}`;
    }
    const keys = p.list('disabledKeys');
    const off = p.list('disabled').map((port, i) => {
      const key = keys[i];
      return key === undefined ? port : t('doctor.part.portOff', { port, key });
    });
    return { values: { first, disabled: f.list(off) } };
  },
  'ports.not_listening': ({ p, f }) => {
    const values = { missing: f.list(p.list('missing')), listening: f.list(p.list('listening')) };
    return p.has('listening') ? { values, context: 'listening' } : { values };
  },
  'ports.in_use': ({ p, f }) => ({ values: { ports: f.list(p.list('inUse')) }, count: p.list('inUse').length }),

  'firewall_hint.open_ports': (c) => {
    const { p, t } = c;
    const ports = portsText(c);
    if (ports === '') return null;
    const provider = knownProvider(p.str('provider'));
    const extra: string[] = [];
    if (p.bool('proxy')) extra.push(t('doctor.part.proxyPorts'));
    if (p.bool('dockerBridge')) extra.push(t('doctor.part.dockerBridge'));
    if (provider === CloudProviderUnknown) return { values: { ports }, context: 'unknown', extra };
    const values = { where: t(`doctor.part.firewall.${provider}`), ports };
    return provider === CloudProviderOracle ? { values, context: 'oracle', extra } : { values, extra };
  },

  'container.detected': (c) => {
    const { p, t } = c;
    const kind = p.str('kind');
    const values = {
      kind:
        kind === 'docker' || kind === 'podman' ? t(`doctor.part.container.${kind}`) : t('doctor.part.container.other'),
      ports: portsText(c),
    };
    switch (p.str('network')) {
      case 'bridge':
        return { values, context: 'bridge' };
      case 'host':
        return { values, context: 'host' };
      default:
        return { values };
    }
  },

  'transfer.ok': transferRule,
  'transfer.no_limit': ({ p, f }) => ({
    values: { egress: f.size(p.num('egressBytes')), month: f.month(p.str('month')) },
  }),
  'transfer.high': transferRule,
  'transfer.over': transferRule,

  'nofile.ok': nofileRule,
  'nofile.low': nofileRule,

  'bandwidth.estimate': bandwidthRule,
  'bandwidth.near_nic': bandwidthRule,
};

/** Translates one key of the catalog with a rule's answer. */
function render(t: TFunction, key: string, msg: Msg): string {
  const options: Record<string, unknown> = { ...msg.values };
  if (msg.context !== undefined) options['context'] = msg.context;
  if (msg.count !== undefined) options['count'] = msg.count;
  const text = t(key, options);
  return msg.extra && msg.extra.length > 0 ? [text, ...msg.extra].join(' ') : text;
}

/** The message of a check: doctor.<code> with its params, or the server's English one for a code this build lacks. */
export function checkMessage(check: DoctorCheck, env: DoctorEnv, t: TFunction, lang: string): string {
  const key = `doctor.${check.code}`;
  if (!hasMessage(key)) return check.message;
  const f = new Format(lang);
  const p = new Reader(check.params ?? {});
  const rule = Object.hasOwn(MESSAGES, check.code) ? MESSAGES[check.code] : undefined;
  if (rule === undefined) return t(key, p.values(f));
  const msg = rule({ p, e: env, envKnown: true, f, t });
  return msg === null ? check.message : render(t, key, msg);
}

// ---- fixes ----

type FixRule = (c: Ctx, key: string) => readonly FixStep[];

/** A fix of one sentence, with its values and context. */
function step(t: TFunction, key: string, msg: Msg = {}, command?: string): FixStep {
  const text = render(t, key, msg);
  return command === undefined || command === '' ? { text } : { text, command };
}

/** The owner fixes of data_dir and secrets (messages_en.go ownerFix). */
function ownerFix(recursive: boolean): FixRule {
  return ({ p, e, t }, key) => {
    const r = recursive ? '-R ' : '';
    const path = p.str('path');
    if (!p.has('uid')) return [step(t, key, { values: { path }, context: 'unknown' })];
    const uid = String(p.int('uid'));
    if (inContainer(e)) {
      return [
        step(t, key, { values: { path, uid }, context: 'container' }, `sudo chown ${r}${uid}:${uid} <host path>`),
      ];
    }
    // The service's user by name where the report has one, else by number.
    let to = p.str('user');
    if (to === '') to = e.user === '' || e.user.startsWith('uid ') ? uid : e.user;
    const chown = `sudo chown ${r}${to} ${shellQuote(path)}`;
    if (callerAsService(p) && p.has('owner')) {
      // Doctor doesn't know which of the two the server runs as: the harmless step first.
      const owner = p.int('owner');
      const who = to === e.user ? to : `uid ${to}`;
      return [
        step(t, key, { values: { owner }, context: 'ifOwner' }, `sudo -u '#${String(owner)}' isshoni doctor`),
        step(t, key, { values: { who }, context: 'ifOther' }, chown),
      ];
    }
    const steps = [step(t, key, {}, chown)];
    if (p.bool('callerOwns') && e.user !== '')
      steps.push(step(t, key, { values: { user: e.user }, context: 'callerOwns' }));
    return steps;
  };
}

const sudoDoctor: FixRule = ({ t }, key) => [step(t, key, {}, 'sudo isshoni doctor')];

/** A certificate code of 04 §8.7: the server's own sentence for its last error when the result carries it. */
function tlsFix(rule: FixRule): FixRule {
  return (c, key) => {
    const lastError = c.p.str('lastError');
    return lastError !== '' ? [{ text: lastError }] : rule(c, key);
  };
}

/** The name a certificate fix is about. */
function certName(p: Reader): string {
  return p.str('name') || (p.list('names')[0] ?? '') || p.str('domain');
}

const tlsWithIP: FixRule = ({ p, t }, key) => {
  const values = { name: certName(p), ip: p.str('ip') };
  return [step(t, key, p.has('ip') ? { values, context: 'ip' } : { values })];
};

const tlsNamed: FixRule = ({ p, t }, key) => [step(t, key, { values: { name: certName(p) } })];

/**
 * The server's log has the answer. Without a report's env there is no command to name, and the sentence (the
 * code's noCommand variant) doesn't introduce one.
 */
const tlsLog: FixRule = ({ e, envKnown, t }, key) =>
  envKnown ? [step(t, key, {}, logCommand(e))] : [step(t, key, { context: 'noCommand' })];

const TLS_FIXES: Readonly<Record<string, FixRule>> = {
  'tls.acme_unreachable': tlsNamed,
  'tls.dns_missing': tlsWithIP,
  'tls.dns_wrong': tlsWithIP,
  'tls.rate_limited': tlsLog,
  'tls.ip_rejected': tlsNamed,
  'tls.caa_forbids': tlsNamed,
  'tls.acme_failed': tlsLog,
  'tls.cert_unreadable': ({ t }, key) => [step(t, key, {}, 'sudo chgrp isshoni <file> && sudo chmod 640 <file>')],
  'tls.cert_invalid': ({ t }, key) => [step(t, key)],
  'tls.cert_expired': ({ t }, key) => [step(t, key)],
};

const FIXES: Readonly<Record<string, FixRule>> = {
  'config.fix_check': ({ p, e, t }, key) => {
    const command = cli(e, 'config check').replace(/^sudo /, '');
    const first = p.str('firstFix');
    if (first === '') return [step(t, key, {}, command)];
    const one = step(t, key, { values: { firstFix: first }, context: 'first' });
    return p.int('errors') + p.int('warnings') > 1 ? [one, step(t, key, {}, command)] : [one];
  },

  'data_dir.fix_sudo': sudoDoctor,
  'secrets.fix_sudo': sudoDoctor,
  'schema.fix_sudo': sudoDoctor,
  'data_dir.fix_owner': ownerFix(true),
  'secrets.fix_owner': ownerFix(false),
  'data_dir.fix_mode': ({ p, t }, key) => [step(t, key, {}, `sudo chmod 700 ${shellQuote(p.str('path'))}`)],
  'secrets.fix_mode': ({ p, t }, key) => [step(t, key, {}, `sudo chmod 600 ${shellQuote(p.str('path'))}`)],
  'secrets.fix_restore': ({ e, t }, key) => [
    step(
      t,
      key,
      {},
      inContainer(e)
        ? 'docker compose stop && docker compose run --rm isshoni admin restore --offline <backup> && docker compose up -d'
        : 'sudo systemctl stop isshoni && sudo -u isshoni isshoni admin restore --offline <backup> && sudo systemctl start isshoni',
    ),
  ],

  'schema.fix_restore': ({ p, e, t }, key) => {
    const store = p.str('storeMessage');
    const version = p.str('lastAppVersion');
    const why: FixStep =
      store !== ''
        ? { text: store }
        : step(t, 'doctor.part.schemaNewer', version !== '' ? { values: { version }, context: 'version' } : {});
    const backup = p.str('backup');
    return [why, step(t, key, { values: { backup } }, restoreCommand(backup, inContainer(e)))];
  },
  'schema.fix_upgrade': ({ p, t }, key) => {
    const store = p.str('storeMessage');
    if (store !== '') return [{ text: store }];
    const version = p.str('lastAppVersion');
    return [step(t, key, version !== '' ? { values: { version }, context: 'version' } : {})];
  },
  'schema.fix_same_build': ({ p, t }, key) => {
    const version = p.str('lastAppVersion');
    return [
      step(
        t,
        key,
        version !== '' ? { values: { version }, context: 'version' } : {},
        'isshoni admin restore --offline <backup>',
      ),
    ];
  },
  'schema.fix_restore_backup': ({ e, t }, key) => [
    step(
      t,
      key,
      {},
      inContainer(e)
        ? 'docker compose run --rm isshoni admin restore --offline <backup>'
        : 'sudo -u isshoni isshoni admin restore --offline <backup>',
    ),
  ],

  'public_ip.fix_set': ({ e, t }, key) => {
    if (inContainer(e)) {
      return [
        step(t, key, { context: 'container' }, 'docker compose up -d'),
        step(t, 'doctor.part.notUpYet', {}, 'docker compose restart'),
      ];
    }
    const restart = restartCommand(e);
    return [
      step(t, key),
      restart !== '' ? step(t, 'doctor.part.notUpYet', {}, restart) : step(t, 'doctor.part.notUpYetPlain'),
    ];
  },
  'public_ip.fix_forward': (c, key) => {
    const values = { ports: portsText(c), local: c.p.str('local') };
    return [step(c.t, key, c.p.has('local') ? { values, context: 'local' } : { values })];
  },
  'public_ip.fix_static_ip': ({ p, t }, key) => [
    step(t, key, p.str('provider') === CloudProviderGCP ? { context: 'gcp' } : {}),
  ],

  'dns.fix_add_record': ({ p, t }, key) => {
    const values = { domain: p.str('domain'), ip: p.str('ip') };
    return [step(t, key, p.str('ip') !== '' ? { values, context: 'ip' } : { values })];
  },
  'dns.fix_point_here': ({ p, t }, key) => {
    const values = { domain: p.str('domain'), ip: p.str('ip'), ipv6: p.str('ipv6') };
    if (p.has('ip')) return [step(t, key, { values, context: 'ip' })];
    if (p.has('ipv6')) return [step(t, key, { values, context: 'ipv6' })];
    return [step(t, key, { values })];
  },
  'dns.fix_aaaa': ({ p, t }, key) => {
    const values = { domain: p.str('domain'), ipv6: p.str('ipv6') };
    return [step(t, key, p.has('ipv6') ? { values, context: 'ipv6' } : { values })];
  },

  'clock.fix_ntp': ({ e, t }, key) => [
    step(t, key, inContainer(e) ? { context: 'container' } : {}, 'sudo timedatectl set-ntp true'),
  ],

  'udp_buffers.fix_sysctl': ({ p, e, t }, key) => {
    const want = String(p.int('want'));
    const command =
      `printf 'net.core.rmem_max=${want}\\nnet.core.wmem_max=${want}\\n' | ` +
      'sudo tee /etc/sysctl.d/60-isshoni.conf && sudo sysctl --system';
    return [step(t, key, inContainer(e) ? { context: 'container' } : {}, command)];
  },
  'udp_buffers.fix_sysctl_reload': ({ t }, key) => [step(t, key, {}, 'sudo sysctl --system')],
  'udp_buffers.fix_restart': ({ e, t }, key) => {
    const restart = restartCommand(e);
    return [restart !== '' ? step(t, key, {}, restart) : step(t, key, { context: 'plain' })];
  },

  'ports.fix_free': ({ p, f, t }, key) => {
    const keys = p.list('inUseKeys');
    const web = keys.filter((k) => k === 'listen.https' || k === 'listen.http');
    const media = keys.filter((k) => k !== 'listen.https' && k !== 'listen.http');
    const steps: FixStep[] = [];
    if (web.length > 0) steps.push(step(t, key, { context: 'web' }));
    if (media.length > 0) steps.push(step(t, key, { values: { keys: f.list(media) } }));
    return steps;
  },

  'release.fix_upgrade': ({ e, t }, key) => [
    inContainer(e)
      ? step(t, key, { context: 'container' }, 'docker compose pull && docker compose up -d')
      : step(t, key),
  ],

  'nofile.fix_limit': ({ e, t }, key) => [step(t, key, inContainer(e) ? { context: 'container' } : {})],
};

/** The rule of a fix code: its own, a certificate code's, or none (the catalog text with every param). */
function fixRule(code: string): FixRule | undefined {
  if (Object.hasOwn(FIXES, code)) return FIXES[code];
  if (code === 'tls.fix_replace') return undefined;
  if (Object.hasOwn(TLS_FIXES, code)) return tlsFix(TLS_FIXES[code] ?? tlsLog);
  // A certificate code this build doesn't know (the tls check passes the server's code on as it is): the general
  // one's text, which points at the server's log.
  if (code.startsWith('tls.')) return tlsFix((c) => tlsLog(c, 'doctor.fix.tls.acme_failed'));
  return undefined;
}

/** The steps of a check's fix; none when the check has no fix code. */
export function checkFix(check: DoctorCheck, env: DoctorEnv, t: TFunction, lang: string): FixStep[] {
  const code = check.fixCode;
  if (code === undefined || code === '') return [];
  const key = `doctor.fix.${code}`;
  const rule = fixRule(code);
  const known = hasMessage(key) || (rule !== undefined && code.startsWith('tls.'));
  if (!known) return check.fix !== undefined && check.fix !== '' ? [{ text: check.fix }] : [];
  const f = new Format(lang);
  const p = new Reader(check.params ?? {});
  if (rule === undefined) return [{ text: t(key, p.values(f)) }];
  return [...rule({ p, e: env, envKnown: true, f, t }, key)];
}

/** The env of certificateFix, which has no report: no rule that reads it runs with it. */
const NO_ENV: DoctorEnv = {
  os: '',
  arch: '',
  kernel: '',
  container: ContainerKindNone,
  systemd: false,
  provider: CloudProviderUnknown,
  user: '',
};

/**
 * The fix of a certificate code of 04 §8.7 where no doctor report says how the server runs: the dashboard's, for
 * TLSInfo.lastErrorCode and the alert tls.renewal_failing. The steps are those of checkFix for the code, with
 * params like the tls check's ({mode, names, ip}, ip only when the address is known), except that a step whose
 * command depends on how the server runs (the server's log) has no command and a sentence that doesn't introduce
 * one. A certificate code this build doesn't know reads as tls.acme_failed's; any other code has no steps.
 */
export function certificateFix(code: string, params: Params, t: TFunction, lang: string): FixStep[] {
  if (!code.startsWith('tls.')) return [];
  const rule = fixRule(code);
  if (rule === undefined) return [];
  const ctx: Ctx = { p: new Reader(params), e: NO_ENV, envKnown: false, f: new Format(lang), t };
  return [...rule(ctx, `doctor.fix.${code}`)];
}

// ---- links ----

/** The links under a check (04 §13.2's text output; 05 builds them from id and env.provider). */
export function checkLinks(check: DoctorCheck, env: DoctorEnv): CheckLink[] {
  const links: CheckLink[] = [];
  const p = new Reader(check.params ?? {});
  if (check.status === DoctorStatusWarn || check.status === DoctorStatusFail) {
    links.push({ kind: 'more', url: doctorDocsUrl(check.id) });
  }
  let provider: CloudProvider | null = null;
  if (check.code === 'firewall_hint.open_ports') provider = knownProvider(p.str('provider'));
  if (check.code === 'public_ip.ephemeral') provider = knownProvider(p.str('provider') || env.provider);
  if (provider !== null && provider !== CloudProviderUnknown) links.push({ kind: 'vps', url: vpsGuideUrl(provider) });
  if (check.fixCode === 'release.fix_upgrade' && /^https:\/\//.test(p.str('url'))) {
    links.push({ kind: 'release', url: p.str('url') });
  }
  return links;
}

/** Everything the page shows for a check. */
export function checkText(check: DoctorCheck, env: DoctorEnv, t: TFunction, lang: string): CheckText {
  return {
    message: checkMessage(check, env, t, lang),
    fix: checkFix(check, env, t, lang),
    links: checkLinks(check, env),
  };
}

/** The provider ids that have a firewall text of their own (every one but unknown). */
export const FIREWALL_PROVIDERS: readonly CloudProvider[] = CLOUD_PROVIDERS.filter((p) => p !== CloudProviderUnknown);
