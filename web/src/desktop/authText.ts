// The texts of the wizard's and the relink page's outcomes (docs/m2/03 §8.1–§8.3): a bridge error of Auth.Probe,
// Auth.Link, Auth.LinkWithPassword or a server change, a notice of the AuthState, and 02 §13.3's version lines. The
// bridge sends codes and params only (03 §6.4); every word comes from the catalog here.
import type { TFunction } from 'i18next';

import type { AuthNotice, CompatCheck } from '../platform/desktop/bridge';

/** A bridge rejection read by shape (platform/desktop/errors.ts's BridgeError): its code and params. */
export interface BridgeFailure {
  code: string;
  params: Readonly<Record<string, unknown>>;
}

/** The code and params of a bridge call's rejection; `internal` for anything that isn't a BridgeError. */
export function failureOf(err: unknown): BridgeFailure {
  const e = err as { code?: unknown; params?: unknown } | null | undefined;
  const code = typeof e?.code === 'string' && e.code !== '' ? e.code : 'internal';
  const params =
    typeof e?.params === 'object' && e.params !== null ? (e.params as Readonly<Record<string, unknown>>) : {};
  return { code, params };
}

/** The probe's and the server changes' codes with a desktop text of their own (03 §8.1, §6.4). */
const DESKTOP_CODES: ReadonlySet<string> = new Set([
  'invalid_argument',
  'invalid_address',
  'server_unreachable',
  'tls_untrusted',
  'not_isshoni',
  'server_not_set_up',
  'too_many_servers',
  'not_found',
  'busy',
  'rate_limited',
  'not_linked',
  'not_implemented',
  'internal',
]);

/** The M1 REST codes that Auth.LinkWithPassword passes through (03 §6.4): their texts are M1's errors.<code>. */
const REST_CODES: ReadonlySet<string> = new Set([
  'invalid_credentials',
  'account_pending',
  'account_disabled',
  'server_busy',
  'validation_failed',
]);

function numberParam(params: Readonly<Record<string, unknown>>, name: string): number | undefined {
  const v = params[name];
  return typeof v === 'number' && Number.isFinite(v) && v > 0 ? v : undefined;
}

function stringParam(params: Readonly<Record<string, unknown>>, name: string): string {
  const v = params[name];
  return typeof v === 'string' ? v : '';
}

/**
 * 02 §13.3's line for an incompatible server: update_app ("This server needs a newer isshoni app…"), else
 * update_server ("…Ask your admin to update it.").
 */
export function compatText(
  t: TFunction,
  c: Pick<CompatCheck, 'verdict' | 'serverVersion'>,
  appVersion: string,
): string {
  return c.verdict === 'update_app'
    ? t('desktop.setup.version.updateApp', { app: appVersion, server: c.serverVersion || '?' })
    : t('desktop.setup.version.updateServer', { server: c.serverVersion || '?' });
}

/**
 * The text of a failed probe, server change or link (03 §8.1, §8.2): desktop.errors.<code>, incompatible_server by
 * its verdict, rate_limited with the wait when the server named one, M1's errors.<code> for the REST codes of the
 * password form, and the generic link error for anything else.
 */
export function failureText(t: TFunction, f: BridgeFailure, appVersion: string): string {
  if (f.code === 'incompatible_server') {
    return compatText(
      t,
      {
        verdict: stringParam(f.params, 'verdict') === 'update_app' ? 'update_app' : 'update_server',
        serverVersion: stringParam(f.params, 'serverVersion'),
      },
      appVersion,
    );
  }
  if (f.code === 'rate_limited') {
    const wait = numberParam(f.params, 'retryAfter');
    return wait !== undefined
      ? t('errors.rate_limited.wait', { count: Math.ceil(wait) })
      : t('desktop.errors.rate_limited');
  }
  if (REST_CODES.has(f.code)) return t(`errors.${f.code}`);
  if (DESKTOP_CODES.has(f.code)) return t(`desktop.errors.${f.code}`);
  return t('desktop.setup.link.failed');
}

/** validation_failed's params.fields (M1's {field: code}) as field → its fieldErrors text. */
export function fieldTexts(t: TFunction, f: BridgeFailure): Partial<Record<'username' | 'password', string>> {
  if (f.code !== 'validation_failed') return {};
  const fields = f.params['fields'];
  if (typeof fields !== 'object' || fields === null) return {};
  const out: Partial<Record<'username' | 'password', string>> = {};
  for (const field of ['username', 'password'] as const) {
    const code = (fields as Record<string, unknown>)[field];
    if (typeof code === 'string' && code !== '') {
      out[field] = t(`fieldErrors.${field}.${code}`, { defaultValue: t('errors.validation_failed') });
    }
  }
  return out;
}

/** The notices of a refused refresh (02 §11.3) that the relink page explains (03 §8.3). */
export type RelinkNotice = Extract<AuthNotice, 'signed_out' | 'refresh_reused' | 'account_disabled'>;

/** The relink page's text for a notice, naming the server. */
export function relinkText(t: TFunction, notice: RelinkNotice, server: string): string {
  switch (notice) {
    case 'signed_out':
      return t('desktop.relink.signedOut', { server });
    case 'refresh_reused':
      return t('desktop.relink.refreshReused', { server });
    case 'account_disabled':
      return t('desktop.relink.accountDisabled', { server });
  }
}
