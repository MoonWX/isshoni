// Small parts shared by the Settings page's sections (docs/m2/03 §14.4): the section frame with its anchor, a switch
// that saves through Settings.Update, and the failure toast.
import { useCallback, useId, useState, type ReactNode } from 'react';
import { useTranslation } from 'react-i18next';

import { useApp } from '../app/context';
import { bridgeText } from './settingsState';
import styles from './settings.module.css';

/** Toasts a failed bridge call with its text (desktop.errors.<code>, desktop.update.error.<code>). */
export function useBridgeFailure(): (err: unknown) => void {
  const { t } = useTranslation();
  const { ui } = useApp();
  return useCallback(
    (err: unknown) => {
      ui.getState().toast({ kind: 'error', message: bridgeText(t, err) });
    },
    [t, ui],
  );
}

/** A section of the page: its anchor id (#servers, #sharing, …) and heading. */
export function SettingsSection({ id, title, children }: { id: string; title: string; children: ReactNode }) {
  const headingId = useId();
  return (
    <section id={id} className={styles.section} aria-labelledby={headingId}>
      <h2 id={headingId} className={styles.title}>
        {title}
      </h2>
      {children}
    </section>
  );
}

export interface SettingSwitchProps {
  label: string;
  /** A line under the switch, tied to it with aria-describedby. */
  hint?: string;
  /** The saved value; null while unknown (the switch is disabled then). */
  checked: boolean | null;
  disabled?: boolean;
  /** Saves the new value; the switch shows it until this settles, and a failure is toasted. */
  onChange: (on: boolean) => Promise<unknown>;
}

/** A setting's on/off switch (role="switch"). */
export function SettingSwitch({ label, hint, checked, disabled = false, onChange }: SettingSwitchProps) {
  const fail = useBridgeFailure();
  const [pending, setPending] = useState<boolean | null>(null);
  const hintId = useId();
  return (
    <div>
      <label className={styles.switch}>
        <input
          type="checkbox"
          role="switch"
          checked={pending ?? checked ?? false}
          disabled={disabled || checked === null || pending !== null}
          aria-describedby={hint !== undefined ? hintId : undefined}
          onChange={(ev) => {
            const on = ev.target.checked;
            setPending(on);
            onChange(on)
              .catch(fail)
              .finally(() => {
                setPending(null);
              });
          }}
        />
        <span>{label}</span>
      </label>
      {hint !== undefined && (
        <p id={hintId} className={styles.muted}>
          {hint}
        </p>
      )}
    </div>
  );
}
