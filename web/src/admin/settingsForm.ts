// The Settings page's form logic (05 §15.3, 03 §9), without React.
//
// The page keeps only what the admin changed (SettingsEdits): a control shows the edit, or else the server's value,
// so a refetch while the form is open updates every field that wasn't touched. Saving sends a PATCH with exactly
// the fields whose value differs from the server's, and:
// - never a field in `locked` (pinned by config): the server answers 409 setting_locked for it whatever value is
//   sent, also the value in force;
// - never null (422, field code invalid): a field is reset by sending its default;
// - never setupWizardDone, which is not a field of this page (the setup wizard and the dashboard checklist own it).
// The checks here return the server's field codes (03 §9's table), so both sources read the same texts.
import {
  FieldInvalid,
  FieldOutOfRange,
  FieldRequired,
  FieldTooLong,
  RegistrationModeApproval,
  RegistrationModeClosed,
  RegistrationModeInvite,
  type RegistrationMode,
  type Settings,
} from '../protocol/api.gen';
import type { SettingsPatch } from './adminApi';
import { runeLength, type FieldCodes } from './formProblem';

/** Text settings. */
export type TextSetting = 'serverName' | 'minClientVersion';
/** Whole-number settings. */
export type NumberSetting =
  | 'inviteDefaultTtlHours'
  | 'inviteDefaultMaxUses'
  | 'maxParticipantsPerRoom'
  | 'maxSharesPerRoom'
  | 'maxShareBitrateKbps'
  | 'transferAlertGb';
/** On/off settings. */
export type BoolSetting = 'membersCanInvite' | 'updateCheck';
/** Every setting the page shows: all of Settings but setupWizardDone. */
export type SettingName = TextSetting | NumberSetting | BoolSetting | 'registrationMode';

/** The settings in page order (05 §15.3). */
export const SETTING_NAMES = [
  'serverName',
  'registrationMode',
  'inviteDefaultTtlHours',
  'inviteDefaultMaxUses',
  'membersCanInvite',
  'maxParticipantsPerRoom',
  'maxSharesPerRoom',
  'maxShareBitrateKbps',
  'transferAlertGb',
  'minClientVersion',
  'updateCheck',
] as const satisfies readonly SettingName[];

export const REGISTRATION_MODES: readonly RegistrationMode[] = [
  RegistrationModeInvite,
  RegistrationModeApproval,
  RegistrationModeClosed,
];

/** serverName holds at most 64 characters (03 §9). */
export const SERVER_NAME_MAX_LENGTH = 64;

export interface NumberRange {
  readonly min: number;
  readonly max: number;
  /** 0 is allowed although it is below min: "use the preset's own bitrate". */
  readonly zeroToo?: true;
}

/** 03 §9's ranges. */
export const NUMBER_RANGES: Readonly<Record<NumberSetting, NumberRange>> = {
  inviteDefaultTtlHours: { min: 1, max: 720 },
  inviteDefaultMaxUses: { min: 1, max: 1000 },
  maxParticipantsPerRoom: { min: 0, max: 10000 },
  maxSharesPerRoom: { min: 0, max: 1000 },
  maxShareBitrateKbps: { min: 500, max: 100000, zeroToo: true },
  transferAlertGb: { min: 0, max: 1000000 },
};

/**
 * The TOML key that pins a setting (04 §4.3, §4.6), for "Set in isshoni.toml". Only 04's policy keys can pin;
 * the other settings are never in `locked`.
 */
export const PINNING_KEYS: Readonly<Partial<Record<SettingName, string>>> = {
  registrationMode: 'registration.mode',
  maxParticipantsPerRoom: 'limits.max_participants_per_room',
  maxSharesPerRoom: 'limits.max_shares_per_room',
  maxShareBitrateKbps: 'limits.max_bitrate_kbps',
  transferAlertGb: 'limits.transfer_alert_gb',
  minClientVersion: 'clients.min_version',
  updateCheck: 'updates.release_check',
};

/**
 * What the admin changed, by field: the text of a text or number input as typed (a number is only parsed when
 * saving, so a half-typed value stays as it is), the state of a checkbox, the picked mode.
 */
export type SettingsEdits = { readonly [K in TextSetting | NumberSetting]?: string } & {
  readonly [K in BoolSetting]?: boolean;
} & { readonly registrationMode?: RegistrationMode };

/** SemVer 2.0.0 without a leading v, as the server checks minClientVersion (03 §9). */
const SEMVER =
  /^(0|[1-9]\d*)\.(0|[1-9]\d*)\.(0|[1-9]\d*)(-((0|[1-9]\d*|\d*[A-Za-z-][0-9A-Za-z-]*)(\.(0|[1-9]\d*|\d*[A-Za-z-][0-9A-Za-z-]*))*))?(\+([0-9A-Za-z-]+(\.[0-9A-Za-z-]+)*))?$/;

/** A whole number from an input's text, or the field code that says why it isn't one. */
export function parseNumberSetting(name: NumberSetting, text: string): number | string {
  const digits = text.trim();
  if (digits === '') return FieldRequired;
  if (!/^\d+$/.test(digits)) return FieldInvalid;
  // More digits than any range has: out of range, and never a number too large to send exactly.
  if (digits.length > 9) return FieldOutOfRange;
  const n = Number(digits);
  const { min, max, zeroToo } = NUMBER_RANGES[name];
  if (n === 0 && zeroToo === true) return n;
  return n < min || n > max ? FieldOutOfRange : n;
}

function parseText(name: TextSetting, text: string): { value: string } | { code: string } {
  const value = text.trim();
  if (name === 'serverName') {
    return runeLength(value) > SERVER_NAME_MAX_LENGTH ? { code: FieldTooLong } : { value };
  }
  return value !== '' && !SEMVER.test(value) ? { code: FieldInvalid } : { value };
}

function isNumberSetting(name: SettingName): name is NumberSetting {
  return Object.hasOwn(NUMBER_RANGES, name);
}

function isTextSetting(name: SettingName): name is TextSetting {
  return name === 'serverName' || name === 'minClientVersion';
}

export interface SettingsChanges {
  /** The PATCH body: the edited fields whose value differs from the server's. Empty: nothing to save. */
  readonly patch: SettingsPatch;
  /** The edited fields that can't be sent as they are, with their field code. */
  readonly errors: FieldCodes<SettingName>;
}

/**
 * What saving would send. Edits of a locked field are left out entirely (the field is read-only on the page, and a
 * field that became locked while the form was open must not turn the whole save into a 409).
 */
export function settingsChanges(edits: SettingsEdits, settings: Settings, locked: readonly string[]): SettingsChanges {
  const patch: SettingsPatch = {};
  const errors: FieldCodes<SettingName> = {};
  for (const name of SETTING_NAMES) {
    if (locked.includes(name)) continue;
    if (isNumberSetting(name)) {
      const text = edits[name];
      if (text === undefined) continue;
      const parsed = parseNumberSetting(name, text);
      if (typeof parsed === 'string') errors[name] = parsed;
      else if (parsed !== settings[name]) patch[name] = parsed;
    } else if (isTextSetting(name)) {
      const text = edits[name];
      if (text === undefined) continue;
      const parsed = parseText(name, text);
      if ('code' in parsed) errors[name] = parsed.code;
      else if (parsed.value !== settings[name]) patch[name] = parsed.value;
    } else if (name === 'registrationMode') {
      const mode = edits.registrationMode;
      if (mode !== undefined && mode !== settings.registrationMode) patch.registrationMode = mode;
    } else {
      const on = edits[name];
      if (on !== undefined && on !== settings[name]) patch[name] = on;
    }
  }
  return { patch, errors };
}

/** Whether saving has anything to do: a change to send, or an edit that must be fixed first. */
export function hasChanges({ patch, errors }: SettingsChanges): boolean {
  return Object.keys(patch).length > 0 || Object.keys(errors).length > 0;
}
