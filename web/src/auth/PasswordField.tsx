import { Eye, EyeOff } from 'lucide-react';
import { useState, type ComponentPropsWithRef, type ReactNode } from 'react';
import { useTranslation } from 'react-i18next';

import { cx } from '../ui/cx';
import { Field, inputClass } from '../ui/Field';
import styles from './PasswordField.module.css';

export interface PasswordFieldProps extends Omit<ComponentPropsWithRef<'input'>, 'id' | 'type' | 'children'> {
  /** The visible label (translated). */
  label: ReactNode;
  /** Help under the control (translated): the length rule from /info's accountRules. */
  hint?: ReactNode;
  /** The error under the control (translated). */
  error?: ReactNode;
}

/**
 * A password input with a show/hide button (05 §14.1, §15.1): there is no "repeat your password" field, so seeing
 * what was typed is how a friend checks it. Pasting and password managers work as usual (03 §7.2); pass
 * autoComplete "current-password" or "new-password". Other props go to the <input>.
 */
export function PasswordField({ label, hint, error, className, ...input }: PasswordFieldProps) {
  const { t } = useTranslation();
  const [shown, setShown] = useState(false);
  return (
    <Field label={label} hint={hint} error={error} className={className}>
      {(control) => (
        <div className={styles.wrap}>
          <input
            // Typed passwords are never capitalized, corrected or spell-checked (the last would send them to the
            // browser's spelling service while shown as text).
            autoCapitalize="none"
            autoCorrect="off"
            spellCheck={false}
            {...input}
            {...control}
            type={shown ? 'text' : 'password'}
            className={cx(inputClass, styles.input)}
          />
          <button
            type="button"
            className={styles.toggle}
            aria-label={t('auth.showPassword')}
            aria-pressed={shown}
            onClick={() => {
              setShown((s) => !s);
            }}
          >
            {shown ? <EyeOff aria-hidden="true" /> : <Eye aria-hidden="true" />}
          </button>
        </div>
      )}
    </Field>
  );
}
