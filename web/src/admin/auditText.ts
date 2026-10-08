// How an audit row reads (03 §10). The server sends identifiers, never English: the action ("user.role_changed"),
// the kinds of actor and target, and a small detail object. Actions have a text under admin.audit.action.<action>;
// one this build doesn't know (a newer server) shows as its identifier, which is still precise.
import type { TFunction } from 'i18next';

import { i18n } from '../i18n';
import type { AuditRef } from '../protocol/api.gen';

/** Whether en.json has a plain message at this key (not a group of messages). */
function hasMessage(key: string): boolean {
  const lng = i18n.resolvedLanguage ?? i18n.language;
  return typeof i18n.getResource(lng, 'translation', key) === 'string';
}

/** An action as the server writes it: lowercase words joined by dots, "user.role_changed". */
const ACTION = /^[a-z][a-z_]*(?:\.[a-z][a-z_]*)+$/;

/** "Changed a role" for "user.role_changed"; the identifier itself when there is no text for it. */
export function actionLabel(action: string, t: TFunction): string {
  const key = `admin.audit.action.${action}`;
  return ACTION.test(action) && hasMessage(key) ? t(key) : action;
}

/** Who did it. Kinds: user, cli, system, anonymous (03 §10); names are snapshots from when the row was written. */
export function actorLabel(actor: AuditRef, t: TFunction): string {
  switch (actor.kind) {
    case 'cli':
      return t('admin.audit.actor.cli');
    case 'system':
      return t('admin.audit.actor.system');
    case 'anonymous':
      // A sign-up request carries the name that was asked for.
      return actor.name !== undefined && actor.name !== ''
        ? t('admin.audit.actor.anonymousNamed', { name: actor.name })
        : t('admin.audit.actor.anonymous');
    default:
      return refName(actor, t);
  }
}

/** The name of an actor or target: its snapshot name, else its ID, else "Unknown". */
export function refName(ref: AuditRef, t: TFunction): string {
  if (ref.name !== undefined && ref.name !== '') return ref.name;
  if (ref.id !== undefined && ref.id !== '') return ref.id;
  return t('admin.unknown');
}

/** What kind of thing a target is, for the small print under its name; null for a user (the common case). */
export function targetKindLabel(kind: string, t: TFunction): string | null {
  switch (kind) {
    case 'user':
      return null;
    case 'session':
      return t('admin.audit.kind.session');
    case 'device':
      return t('admin.audit.kind.device');
    case 'invite':
      return t('admin.audit.kind.invite');
    case 'room':
      return t('admin.audit.kind.room');
    case 'settings':
      return t('admin.audit.kind.settings');
    case 'setup':
      return t('admin.audit.kind.setup');
    default:
      return kind;
  }
}

function isRecord(v: unknown): v is Record<string, unknown> {
  return typeof v === 'object' && v !== null && !Array.isArray(v);
}

/** One value of a detail object as text: an empty string shows as "" so a change to nothing is visible. */
function valueText(v: unknown): string {
  if (typeof v === 'string') return v === '' ? '""' : v;
  if (typeof v === 'number' || typeof v === 'boolean') return String(v);
  if (v === null || v === undefined) return '–';
  if (Array.isArray(v)) return v.map(valueText).join(', ');
  return JSON.stringify(v);
}

function isChange(v: unknown): v is { from: unknown; to: unknown } {
  return isRecord(v) && 'from' in v && 'to' in v;
}

/**
 * A row's detail as short lines (03 §10's Detail column): {from, to} reads "user → admin", settings.changed's
 * {changes: {field: {from, to}}} one line per field, anything else "key: value". The keys are the server's
 * identifiers (maxUses, reason, sessions): a log shows what was recorded.
 */
export function detailLines(detail: unknown): string[] {
  const lines: string[] = [];
  // The server sends {} when there is nothing to say; anything that isn't an object says nothing either.
  if (!isRecord(detail)) return lines;
  if (isChange(detail)) lines.push(`${valueText(detail.from)} → ${valueText(detail.to)}`);
  for (const [key, value] of Object.entries(detail)) {
    if (isChange(detail) && (key === 'from' || key === 'to')) continue;
    if (key === 'changes' && isRecord(value)) {
      for (const [field, change] of Object.entries(value)) {
        lines.push(
          isChange(change)
            ? `${field}: ${valueText(change.from)} → ${valueText(change.to)}`
            : `${field}: ${valueText(change)}`,
        );
      }
      continue;
    }
    lines.push(`${key}: ${valueText(value)}`);
  }
  return lines;
}
