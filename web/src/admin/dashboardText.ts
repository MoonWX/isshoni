// How the dashboard's alerts read (04 §11.4). The server sends a code, a severity and params, never English; the
// texts are admin.dashboard.alert.<code>. The params as README S85 built them:
//   transfer.80, transfer.100       {month, alertGb}
//   release.update, …security_update {version}
//   public_ip.changed               {family: "ipv4" | "ipv6", from?, to}, one alert per family
//   tls.renewal_failing             {code, notAfter}: code is 04 §8.7's, whose fix doctor's catalog has
//   doctor.fail                     {fail, checks}: the number of failed checks and their ids
//   dashboard.source_failed         {source}: server, process, rooms, media, transfer, update, doctor or accounts
// An alert this build doesn't know (a newer server's) shows its code.
import type { TFunction } from 'i18next';

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
  ContainerKindNone,
  type Alert,
  type OpsDashboard,
} from '../protocol/api.gen';
import { isCheckId } from './doctorChecks';
import { checkFix } from './doctorText';
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

/** An alert as text: what happened, and for a failing renewal what to do about it. */
export interface AlertText {
  readonly text: string;
  /** The fix of tls.renewal_failing, from doctor's catalog; '' otherwise. */
  readonly fix: string;
}

/** The text of one alert; `data` is the snapshot it came with, for what the params leave out. */
export function alertText(alert: Alert, data: OpsDashboard, t: TFunction, lang: string): AlertText {
  const f = new Format(lang);
  const p = alert.params;
  const plain = (text: string): AlertText => ({ text, fix: '' });
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
    case AlertCodeTLSRenewalFailing: {
      const notAfter = f.time(str(p, 'notAfter'));
      const text =
        alert.severity === AlertSeverityError
          ? t('admin.dashboard.alert.tlsRenewalExpired', { notAfter })
          : t('admin.dashboard.alert.tlsRenewalFailing', { notAfter });
      // The certificate code's fix from doctor's catalog, with what the snapshot knows of the certificate. Only its
      // sentences: the commands depend on how the server runs, which the dashboard doesn't say.
      const code = str(p, 'code');
      const steps =
        code === ''
          ? []
          : checkFix(
              {
                id: 'tls',
                status: 'warn',
                code: 'tls.renewal_failing',
                params: { mode: data.server.tls.mode, names: data.server.tls.names, ip: data.server.publicIpv4 },
                message: '',
                fixCode: code,
                localOnly: false,
                durationMs: 0,
              },
              {
                os: '',
                arch: '',
                kernel: '',
                container: ContainerKindNone,
                systemd: false,
                provider: 'unknown',
                user: '',
              },
              t,
              lang,
            );
      return { text, fix: steps.map((s) => s.text).join(' ') };
    }
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
