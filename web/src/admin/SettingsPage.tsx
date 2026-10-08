import { useQuery } from '@tanstack/react-query';
import { Lock } from 'lucide-react';
import { useId, useState, type ReactNode } from 'react';
import { Trans, useTranslation } from 'react-i18next';

import { useApp } from '../app/context';
import { Notice } from '../auth/Notice';
import {
  CodeSettingLocked,
  RegistrationModeApproval,
  RegistrationModeClosed,
  RegistrationModeInvite,
  type RegistrationMode,
  type Settings,
  type SettingsResponse,
} from '../protocol/api.gen';
import { queryKeys } from '../protocol/queryKeys';
import { isApiError } from '../protocol/rest';
import { Button } from '../ui/Button';
import { cx } from '../ui/cx';
import { TextField } from '../ui/Field';
import { adminApi, settingsQueryOptions } from './adminApi';
import { AdminPage, Loaded, Section } from './AdminPage';
import {
  hasChanges,
  NUMBER_RANGES,
  PINNING_KEYS,
  REGISTRATION_MODES,
  SERVER_NAME_MAX_LENGTH,
  SETTING_NAMES,
  settingsChanges,
  type BoolSetting,
  type NumberSetting,
  type SettingName,
  type SettingsChanges,
  type SettingsEdits,
  type TextSetting,
} from './settingsForm';
import styles from './SettingsPage.module.css';
import { useAction } from './useAction';

/**
 * /admin/settings (05 §15.3, 03 §9): the server name, the registration mode, the invite defaults and whether
 * members may invite, the optional limits, the transfer alert, the oldest app version allowed and the release
 * check. They apply at once, without a restart.
 *
 * A field that config pins (`locked`) is read-only, with "Set in isshoni.toml". Save sends only the fields the
 * admin changed, never a locked one and never null (settingsForm.ts). setupWizardDone is not a field here.
 */
export function SettingsPage() {
  const { t } = useTranslation();
  const settings = useQuery(settingsQueryOptions());
  return (
    <AdminPage title={t('admin.settings.title')} lead={t('admin.settings.lead')}>
      <Loaded query={settings}>{(data) => <SettingsForm data={data} />}</Loaded>
    </AdminPage>
  );
}

/** What the fields read and write. */
interface FormState {
  readonly settings: Settings;
  readonly defaults: Settings;
  readonly locked: readonly string[];
  readonly edits: SettingsEdits;
  /** Translated messages per field, from the last failed save. */
  readonly errors: Partial<Record<SettingName, string>>;
  edit<K extends keyof SettingsEdits>(name: K, value: NonNullable<SettingsEdits[K]>): void;
}

function SettingsForm({ data }: { data: SettingsResponse }) {
  const { t } = useTranslation();
  const { queryClient, ui } = useApp();
  const [edits, setEdits] = useState<SettingsEdits>({});
  const changes = settingsChanges(edits, data.settings, data.locked);
  const dirty = hasChanges(changes);

  const save = useAction({
    fields: SETTING_NAMES,
    validate: (sent: { changes: SettingsChanges; edits: SettingsEdits }) => sent.changes.errors,
    request: (sent) => adminApi.patchSettings(sent.changes.patch),
    onSuccess: (res, sent) => {
      queryClient.setQueryData(settingsQueryOptions().queryKey, res);
      // The edits that were sent are the server's values now. One made while the request ran stays an edit.
      setEdits((now) => {
        const sentEdits: Readonly<Record<string, unknown>> = sent.edits;
        return Object.fromEntries(Object.entries(now).filter(([name, value]) => sentEdits[name] !== value));
      });
      // What /info reports (the server name, the registration mode) and the audit log changed with the settings.
      // The server's `invalidate` message says the same, but only a tab with a room connection hears it.
      void queryClient.invalidateQueries({ queryKey: queryKeys.info });
      void queryClient.invalidateQueries({ queryKey: queryKeys.adminAudit });
      ui.getState().toast({ kind: 'success', message: t('admin.settings.saved') });
    },
    onError: (err) => {
      // A field was pinned after this page loaded it (the config changed): the fresh `locked` makes it read-only.
      if (isApiError(err, CodeSettingLocked)) {
        void queryClient.invalidateQueries({ queryKey: queryKeys.adminSettings });
      }
    },
  });
  const { formRef: saveFormRef } = save;

  const form: FormState = {
    settings: data.settings,
    defaults: data.defaults,
    locked: data.locked,
    edits,
    errors: save.fieldErrors,
    edit(name, value) {
      setEdits((prev) => ({ ...prev, [name]: value }));
      // The message under a field is about what it held when Save failed.
      save.clear(name);
    },
  };

  return (
    <form
      ref={saveFormRef}
      className={styles.form}
      noValidate
      onSubmit={(e) => {
        e.preventDefault();
        if (dirty) save.submit({ changes, edits });
      }}
    >
      <Section title={t('admin.settings.server.title')}>
        <TextSettingField
          form={form}
          name="serverName"
          label={t('admin.settings.serverName.label')}
          hint={t('admin.settings.serverName.hint')}
          // In UTF-16 units, so twice the limit in characters: never short of what the server takes.
          maxLength={2 * SERVER_NAME_MAX_LENGTH}
        />
      </Section>

      <Section title={t('admin.settings.registration.title')}>
        <RegistrationModeField form={form} />
      </Section>

      <Section title={t('admin.settings.invites.title')}>
        <div className={styles.pair}>
          <NumberSettingField
            form={form}
            name="inviteDefaultTtlHours"
            label={t('admin.settings.inviteDefaultTtlHours.label')}
            hint={t('admin.settings.inviteDefaultTtlHours.hint', range('inviteDefaultTtlHours'))}
          />
          <NumberSettingField
            form={form}
            name="inviteDefaultMaxUses"
            label={t('admin.settings.inviteDefaultMaxUses.label')}
            hint={t('admin.settings.inviteDefaultMaxUses.hint', range('inviteDefaultMaxUses'))}
          />
        </div>
        <BoolSettingField
          form={form}
          name="membersCanInvite"
          label={t('admin.settings.membersCanInvite.label')}
          hint={t('admin.settings.membersCanInvite.hint')}
        />
      </Section>

      <Section title={t('admin.settings.limits.title')} lead={t('admin.settings.limits.lead')}>
        <div className={styles.pair}>
          <NumberSettingField
            form={form}
            name="maxParticipantsPerRoom"
            label={t('admin.settings.maxParticipantsPerRoom.label')}
            hint={t('admin.settings.maxParticipantsPerRoom.hint', range('maxParticipantsPerRoom'))}
          />
          <NumberSettingField
            form={form}
            name="maxSharesPerRoom"
            label={t('admin.settings.maxSharesPerRoom.label')}
            hint={t('admin.settings.maxSharesPerRoom.hint', range('maxSharesPerRoom'))}
          />
        </div>
        <NumberSettingField
          form={form}
          name="maxShareBitrateKbps"
          label={t('admin.settings.maxShareBitrateKbps.label')}
          hint={t('admin.settings.maxShareBitrateKbps.hint', range('maxShareBitrateKbps'))}
        />
      </Section>

      <Section title={t('admin.settings.transfer.title')}>
        <NumberSettingField
          form={form}
          name="transferAlertGb"
          label={t('admin.settings.transferAlertGb.label')}
          hint={t('admin.settings.transferAlertGb.hint')}
        />
      </Section>

      <Section title={t('admin.settings.apps.title')}>
        <TextSettingField
          form={form}
          name="minClientVersion"
          label={t('admin.settings.minClientVersion.label')}
          hint={t('admin.settings.minClientVersion.hint')}
          maxLength={64}
        />
        <BoolSettingField
          form={form}
          name="updateCheck"
          label={t('admin.settings.updateCheck.label')}
          hint={t('admin.settings.updateCheck.hint')}
        />
      </Section>

      <div className={styles.bar}>
        {save.formError !== null && <Notice>{save.formError}</Notice>}
        <div className={styles.buttons}>
          {dirty && <p className={styles.unsaved}>{t('admin.settings.unsaved')}</p>}
          <Button
            disabled={!dirty || save.busy}
            onClick={() => {
              setEdits({});
              save.clear();
            }}
          >
            {t('admin.settings.discard')}
          </Button>
          <Button type="submit" variant="primary" loading={save.busy} disabled={!dirty}>
            {t('admin.settings.save')}
          </Button>
        </div>
      </div>
    </form>
  );
}

/** A number field's range, as its hint's {{min}} and {{max}}. */
function range(name: NumberSetting): { min: number; max: number } {
  const { min, max } = NUMBER_RANGES[name];
  return { min, max };
}

function isLocked(form: FormState, name: SettingName): boolean {
  return form.locked.includes(name);
}

/** The help under a control, with "Set in isshoni.toml" when config pins the field. */
function Hint({ form, name, children }: { form: FormState; name: SettingName; children: ReactNode }) {
  const { t } = useTranslation();
  if (!isLocked(form, name)) return children;
  const key = PINNING_KEYS[name];
  return (
    <>
      <span className={styles.locked}>
        <Lock aria-hidden="true" />
        <span>
          {key === undefined ? (
            t('admin.settings.locked')
          ) : (
            <Trans i18nKey="admin.settings.lockedKey" values={{ key }} components={{ code: <code /> }} />
          )}
        </span>
      </span>{' '}
      {children}
    </>
  );
}

interface FieldProps<N extends SettingName> {
  form: FormState;
  name: N;
  /** Translated. */
  label: string;
  /** Translated. */
  hint: string;
}

function TextSettingField({ form, name, label, hint, maxLength }: FieldProps<TextSetting> & { maxLength: number }) {
  const locked = isLocked(form, name);
  // A version number is not prose; a server name is.
  const machine = name === 'minClientVersion';
  return (
    <TextField
      className={styles.text}
      label={label}
      name={name}
      autoComplete="off"
      autoCapitalize={machine ? 'none' : undefined}
      spellCheck={machine ? false : undefined}
      maxLength={maxLength}
      readOnly={locked}
      value={locked ? form.settings[name] : (form.edits[name] ?? form.settings[name])}
      onChange={(e) => {
        form.edit(name, e.target.value);
      }}
      hint={
        <Hint form={form} name={name}>
          {hint}
        </Hint>
      }
      error={locked ? undefined : form.errors[name]}
    />
  );
}

function NumberSettingField({ form, name, label, hint }: FieldProps<NumberSetting>) {
  const { t } = useTranslation();
  const locked = isLocked(form, name);
  const current = String(form.settings[name]);
  return (
    <TextField
      className={styles.number}
      label={label}
      name={name}
      // Digits only, with the numeric keyboard on phones. Not type="number": that hides what was typed when it
      // isn't a number, and changes the value on a scroll wheel.
      inputMode="numeric"
      autoComplete="off"
      maxLength={9}
      readOnly={locked}
      value={locked ? current : (form.edits[name] ?? current)}
      onChange={(e) => {
        form.edit(name, e.target.value);
      }}
      hint={
        <Hint form={form} name={name}>
          {hint} {t('admin.settings.default', { value: form.defaults[name] })}
        </Hint>
      }
      error={locked ? undefined : form.errors[name]}
    />
  );
}

function BoolSettingField({ form, name, label, hint }: FieldProps<BoolSetting>) {
  const id = useId();
  const locked = isLocked(form, name);
  const error = locked ? undefined : form.errors[name];
  return (
    <div className={styles.check}>
      <input
        id={id}
        type="checkbox"
        name={name}
        // A checkbox has no read-only state: a locked one is disabled, and its description says why.
        disabled={locked}
        checked={locked ? form.settings[name] : (form.edits[name] ?? form.settings[name])}
        onChange={(e) => {
          form.edit(name, e.target.checked);
        }}
        aria-describedby={`${id}-hint`}
        aria-invalid={error !== undefined || undefined}
      />
      <div className={styles.checkText}>
        <label htmlFor={id}>{label}</label>
        <p id={`${id}-hint`} className={styles.hint}>
          <Hint form={form} name={name}>
            {hint}
          </Hint>
          {error !== undefined && <span className={styles.error}> {error}</span>}
        </p>
      </div>
    </div>
  );
}

function RegistrationModeField({ form }: { form: FormState }) {
  const { t } = useTranslation();
  const id = useId();
  const locked = isLocked(form, 'registrationMode');
  const value = locked
    ? form.settings.registrationMode
    : (form.edits.registrationMode ?? form.settings.registrationMode);
  const error = locked ? undefined : form.errors.registrationMode;
  const texts: Readonly<Record<RegistrationMode, { name: string; hint: string }>> = {
    [RegistrationModeInvite]: {
      name: t('admin.settings.registrationMode.invite.name'),
      hint: t('admin.settings.registrationMode.invite.hint'),
    },
    [RegistrationModeApproval]: {
      name: t('admin.settings.registrationMode.approval.name'),
      hint: t('admin.settings.registrationMode.approval.hint'),
    },
    [RegistrationModeClosed]: {
      name: t('admin.settings.registrationMode.closed.name'),
      hint: t('admin.settings.registrationMode.closed.hint'),
    },
  };
  return (
    // A disabled fieldset disables its radios: a locked mode can't be changed, and its description says why.
    <fieldset className={styles.modes} disabled={locked} aria-describedby={`${id}-note`}>
      <legend className={styles.legend}>{t('admin.settings.registrationMode.legend')}</legend>
      {REGISTRATION_MODES.map((mode) => (
        <div key={mode} className={cx(styles.check, styles.mode)}>
          <input
            id={`${id}-${mode}`}
            type="radio"
            name="registrationMode"
            value={mode}
            checked={value === mode}
            onChange={() => {
              form.edit('registrationMode', mode);
            }}
            aria-describedby={`${id}-${mode}-hint`}
          />
          <div className={styles.checkText}>
            <label htmlFor={`${id}-${mode}`}>{texts[mode].name}</label>
            <p id={`${id}-${mode}-hint`} className={styles.hint}>
              {texts[mode].hint}
            </p>
          </div>
        </div>
      ))}
      <p id={`${id}-note`} className={styles.hint}>
        <Hint form={form} name="registrationMode">
          {null}
        </Hint>
        {error !== undefined && <span className={styles.error}>{error}</span>}
      </p>
    </fieldset>
  );
}
