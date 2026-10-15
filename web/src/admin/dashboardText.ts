// How the dashboard's alerts and wire values read (04 §11.4). The server sends a code, a severity and params, never
// English; the texts are admin.dashboard.alert.<code>. The params as README S85 built them:
//   transfer.80, transfer.100       {month, alertGb}
//   release.update, …security_update {version}
//   public_ip.changed               {family: "ipv4" | "ipv6", from?, to}, one alert per family
//   tls.renewal_failing             {code, notAfter}: code is 04 §8.7's, whose fix doctor's catalog has; warn while
//                                   the certificate is valid and served, error once it expired or isn't served
//   doctor.fail                     {fail, checks}: the number of failed checks and their ids
//   dashboard.source_failed         {source}: server, process, rooms, media, transfer, update, doctor or accounts
// An alert this build doesn't know (a newer server's) shows its code. The snapshot's other ids (the TLS mode, the
// NAT kind, a client's kind and OS) have short names here, and a value this build doesn't know shows as it is.
import type { TFunction } from 'i18next';

import { parseWireTime } from '../lib/time';
import {
  AlertCodeDashboardSourceFailed,
  AlertCodeDoctorFail,
  AlertCodePublicIPChanged,
  AlertCodeReleaseSecurityUpdate,
  AlertCodeReleaseUpdate,
  AlertCodeTLSRenewalFailing,
  AlertCodeTransfer100,
  AlertCodeTransfer80,
  AlertSeverityError,
  NATKindCGNATLikely,
  NATKindNone,
  NATKindOneToOne,
  NATKindPortForward,
  NATKindSymmetric,
  NATKindUnknown,
  TLSModeAuto,
  TLSModeIP,
  TLSModeManual,
  type Alert,
  type OpsDashboard,
} from '../protocol/api.gen';
import {
  ClientKindDesktop,
  ClientKindMobile,
  ClientKindTool,
  ClientKindWeb,
  ClientOSAndroid,
  ClientOSChromeOS,
  ClientOSIOS,
  ClientOSLinux,
  ClientOSMacOS,
  ClientOSOther,
  ClientOSWindows,
} from '../protocol/types.gen';
import { isCheckId } from './doctorChecks';
import { certificateFix, type FixStep } from './doctorText';
import { Format } from './format';

/** The dashboard's sources (ops.DashboardSource*): the parts a failing source leaves empty. */
export const DASHBOARD_SOURCES = [
  'server',
  'process',
  'rooms',
  'media',
  'transfer',
  'update',
  'doctor',
  'accounts',
] as const;

/** The name of a check on the page: admin.doctor.check.<id>, or the id for one this build doesn't know. */
export function checkName(id: string, t: TFunction): string {
  return isCheckId(id) ? t(`admin.doctor.check.${id}`) : id;
}

function str(params: Alert['params'], key: string): string {
  const v = params?.[key];
  return typeof v === 'string' ? v : typeof v === 'number' ? String(v) : '';
}

function num(params: Alert['params'], key: string): number {
  const v = params?.[key];
  return typeof v === 'number' && Number.isFinite(v) ? v : 0;
}

function list(params: Alert['params'], key: string): string[] {
  const v = params?.[key];
  return Array.isArray(v) ? v.map((x: unknown) => (typeof x === 'string' ? x : String(x))) : [];
}

/** A source of the dashboard. */
export type DashboardSource = (typeof DASHBOARD_SOURCES)[number];

/** For each source, whether it failed in a snapshot. */
export type FailedSources = Readonly<Record<DashboardSource, boolean>>;

/**
 * The sources whose dashboard.source_failed alert came with this snapshot: their sections hold zero values, not
 * facts (04 §11.4), so the page shows them as not available.
 */
export function failedSources(data: OpsDashboard): FailedSources {
  const failed = new Set(
    data.alerts.filter((a) => a.code === AlertCodeDashboardSourceFailed).map((a) => str(a.params, 'source')),
  );
  return Object.fromEntries(DASHBOARD_SOURCES.map((source) => [source, failed.has(source)])) as Record<
    DashboardSource,
    boolean
  >;
}

/**
 * The fix of a certificate code of 04 §8.7 (TLSInfo.lastErrorCode, tls.renewal_failing's params.code) from doctor's
 * catalog, with what the snapshot knows of the certificate and of the server's address. None for a code that is no
 * certificate code.
 */
export function certificateFixOf(code: string, data: OpsDashboard, t: TFunction, lang: string): FixStep[] {
  const { tls, publicIpv4 } = data.server;
  // The ip only when there is one: the fixes have a variant without it ("pointing to this server").
  const params = { mode: tls.mode, names: tls.names, ...(publicIpv4 !== '' ? { ip: publicIpv4 } : {}) };
  return certificateFix(code, params, t, lang);
}

/** An alert as text: what happened, and for a failing renewal what to do about it. */
export interface AlertText {
  readonly text: string;
  /** The fix of tls.renewal_failing, from doctor's catalog; empty otherwise. */
  readonly fix: readonly FixStep[];
}

/** The text of tls.renewal_failing: expired, no longer served while still valid, or valid and served. */
function renewalText(alert: Alert, data: OpsDashboard, t: TFunction, f: Format): string {
  const raw = str(alert.params, 'notAfter');
  const notAfter = f.time(raw);
  const expiry = parseWireTime(raw);
  // The server's clock, not this browser's: the snapshot's time is the one the server judged the certificate by.
  const now = parseWireTime(data.generatedAt);
  const expired = expiry !== undefined && now !== undefined && expiry.getTime() <= now.getTime();
  if (expired) return t('admin.dashboard.alert.tlsRenewalExpired', { notAfter });
  if (alert.severity === AlertSeverityError) {
    // An error while the certificate is still valid: the server no longer serves it. Without a time to compare,
    // the error is the expiry.
    return expiry !== undefined && now !== undefined
      ? t('admin.dashboard.alert.tlsRenewalNotServed', { notAfter })
      : t('admin.dashboard.alert.tlsRenewalExpired', { notAfter });
  }
  return t('admin.dashboard.alert.tlsRenewalFailing', { notAfter });
}

/** The text of one alert; `data` is the snapshot it came with, for what the params leave out. */
export function alertText(alert: Alert, data: OpsDashboard, t: TFunction, lang: string): AlertText {
  const f = new Format(lang);
  const p = alert.params;
  const plain = (text: string): AlertText => ({ text, fix: [] });
  switch (alert.code) {
    case AlertCodeTransfer80:
    case AlertCodeTransfer100:
      return plain(
        t(`admin.dashboard.alert.${alert.code === AlertCodeTransfer80 ? 'transfer80' : 'transfer100'}`, {
          limit: f.gigabytes(num(p, 'alertGb')),
          month: f.month(str(p, 'month')),
        }),
      );
    case AlertCodeReleaseUpdate:
      return plain(t('admin.dashboard.alert.releaseUpdate', { version: str(p, 'version') }));
    case AlertCodeReleaseSecurityUpdate:
      return plain(t('admin.dashboard.alert.releaseSecurityUpdate', { version: str(p, 'version') }));
    case AlertCodePublicIPChanged: {
      const family = str(p, 'family') === 'ipv6' ? t('admin.dashboard.family.ipv6') : t('admin.dashboard.family.ipv4');
      const values = { family, from: str(p, 'from'), to: str(p, 'to') };
      return plain(
        str(p, 'from') === ''
          ? t('admin.dashboard.alert.publicIpNew', values)
          : t('admin.dashboard.alert.publicIpChanged', values),
      );
    }
    case AlertCodeTLSRenewalFailing:
      // The certificate code's fix from doctor's catalog, with what the snapshot knows of the certificate.
      return { text: renewalText(alert, data, t, f), fix: certificateFixOf(str(p, 'code'), data, t, lang) };
    case AlertCodeDoctorFail: {
      const checks = list(p, 'checks').map((id) => checkName(id, t));
      return plain(t('admin.dashboard.alert.doctorFail', { count: num(p, 'fail'), checks: f.list(checks) }));
    }
    case AlertCodeDashboardSourceFailed: {
      const source = str(p, 'source');
      const known = (DASHBOARD_SOURCES as readonly string[]).includes(source);
      return plain(
        t('admin.dashboard.alert.sourceFailed', {
          source: known ? t(`admin.dashboard.source.${source}`) : source,
        }),
      );
    }
    default:
      // A newer server's alert: its code is still precise.
      return plain(String(alert.code));
  }
}

/** The TLS mode (04 §8.1) as the certificate's kind: "Let's Encrypt certificate". */
export function tlsModeName(mode: string, t: TFunction): string {
  switch (mode) {
    case TLSModeAuto:
      return t('admin.dashboard.tlsMode.auto');
    case TLSModeIP:
      return t('admin.dashboard.tlsMode.ip');
    case TLSModeManual:
      return t('admin.dashboard.tlsMode.manual');
    default:
      return mode;
  }
}

/** The NAT kind of the public IPv4 path (04 §7.4), in a few words. */
export function natName(nat: string, t: TFunction): string {
  switch (nat) {
    case NATKindNone:
      return t('admin.dashboard.natKind.none');
    case NATKindOneToOne:
      return t('admin.dashboard.natKind.one_to_one');
    case NATKindPortForward:
      return t('admin.dashboard.natKind.port_forward');
    case NATKindSymmetric:
      return t('admin.dashboard.natKind.symmetric');
    case NATKindCGNATLikely:
      return t('admin.dashboard.natKind.cgnat_likely');
    case NATKindUnknown:
      return t('admin.dashboard.natKind.unknown');
    default:
      return nat;
  }
}

/** A client's kind (01's ClientKind): "Web". */
export function clientKindName(kind: string, t: TFunction): string {
  switch (kind) {
    case ClientKindWeb:
      return t('admin.dashboard.clientKind.web');
    case ClientKindDesktop:
      return t('admin.dashboard.clientKind.desktop');
    case ClientKindMobile:
      return t('admin.dashboard.clientKind.mobile');
    case ClientKindTool:
      return t('admin.dashboard.clientKind.tool');
    default:
      return kind;
  }
}

/** A client's operating system (01's ClientOS): "iOS". */
export function osName(os: string, t: TFunction): string {
  switch (os) {
    case ClientOSWindows:
      return t('admin.dashboard.os.windows');
    case ClientOSMacOS:
      return t('admin.dashboard.os.macos');
    case ClientOSLinux:
      return t('admin.dashboard.os.linux');
    case ClientOSIOS:
      return t('admin.dashboard.os.ios');
    case ClientOSAndroid:
      return t('admin.dashboard.os.android');
    case ClientOSChromeOS:
      return t('admin.dashboard.os.chromeos');
    case ClientOSOther:
    case '':
      return t('admin.dashboard.os.other');
    default:
      return os;
  }
}
