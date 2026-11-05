// "Sign in with username and password instead" (docs/m2/03 §8.2, 02 §14): the plan's in-app fallback of the link
// step. [Sign in] calls Auth.LinkWithPassword once, which cancels a running device-flow link first; an error stays on
// the form with its text and clears the password field. The password lives only in the field until the call
// returns; both fields have autocomplete off, and WebView2's general autofill and password saving are off (§5.4).
import { useRef, useState, type SyntheticEvent } from 'react';
import { useTranslation } from 'react-i18next';

import { Notice } from '../auth/Notice';
import { PasswordField } from '../auth/PasswordField';
import type { AuthState, NativeBridge } from '../platform/desktop/bridge';
import { Button } from '../ui/Button';
import { TextField } from '../ui/Field';
import { failureOf, failureText, fieldTexts } from './authText';
import styles from './wizard.module.css';

export interface PasswordFormProps {
  bridge: NativeBridge;
  appVersion: string;
  /** The state LinkWithPassword answered: linked. */
  onLinked: (s: AuthState) => void;
  /** [Back]: the code again (Auth.Link()). */
  onBack: () => void;
}

export function PasswordForm({ bridge, appVersion, onLinked, onBack }: PasswordFormProps) {
  const { t } = useTranslation();
  const [username, setUsername] = useState('');
  const [password, setPassword] = useState('');
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [fields, setFields] = useState<Partial<Record<'username' | 'password', string>>>({});
  const formRef = useRef<HTMLFormElement>(null);
  const sending = useRef(false);

  const submit = (e: SyntheticEvent): void => {
    e.preventDefault();
    if (sending.current) return;
    sending.current = true;
    setBusy(true);
    setError(null);
    setFields({});
    bridge.auth
      .linkWithPassword(username, password)
      .then(
        (s) => {
          setPassword('');
          onLinked(s);
        },
        (err: unknown) => {
          const f = failureOf(err);
          const byField = fieldTexts(t, f);
          setFields(byField);
          setError(Object.keys(byField).length > 0 ? t('errors.validation_failed') : failureText(t, f, appVersion));
          setPassword('');
          // The first invalid field, else the password field, takes focus for the next try.
          const form = formRef.current;
          const target =
            form?.querySelector<HTMLInputElement>('input[aria-invalid="true"]') ??
            form?.querySelector<HTMLInputElement>('input[type="password"]');
          target?.focus();
        },
      )
      .finally(() => {
        sending.current = false;
        setBusy(false);
      });
  };

  return (
    <form ref={formRef} className={styles.form} noValidate onSubmit={submit}>
      <h2>{t('desktop.setup.password.title')}</h2>
      {error !== null && <Notice>{error}</Notice>}
      <TextField
        label={t('desktop.setup.password.username')}
        name="isshoni-link-username"
        autoComplete="off"
        autoCapitalize="none"
        autoCorrect="off"
        spellCheck={false}
        value={username}
        error={fields.username}
        onChange={(e) => {
          setUsername(e.target.value);
        }}
      />
      <PasswordField
        label={t('desktop.setup.password.password')}
        name="isshoni-link-password"
        autoComplete="off"
        value={password}
        error={fields.password}
        onChange={(e) => {
          setPassword(e.target.value);
        }}
      />
      <div className={styles.serverActions}>
        <Button type="submit" variant="primary" loading={busy}>
          {t('desktop.setup.password.signIn')}
        </Button>
        <Button onClick={onBack} disabled={busy}>
          {t('desktop.setup.back')}
        </Button>
      </div>
    </form>
  );
}
