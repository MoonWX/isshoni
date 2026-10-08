import { useId, type ComponentPropsWithRef, type ReactNode } from 'react';

import { cx } from './cx';
import styles from './Field.module.css';

/** The attributes a form control inside a Field needs; spread them onto the control. */
export interface FieldControlProps {
  id: string;
  'aria-describedby'?: string;
  'aria-invalid'?: true;
}

export interface FieldProps {
  /** The visible label (translated). */
  label: ReactNode;
  /** Help under the control (translated), e.g. the password rules from /info's accountRules. */
  hint?: ReactNode;
  /** The error under the control (translated), e.g. from fieldErrors.<field>.<code>. Marks the control invalid. */
  error?: ReactNode;
  /** Renders the control with the ids that tie it to the label, hint and error. */
  children: (control: FieldControlProps) => ReactNode;
  className?: string;
}

/**
 * A labelled form control with optional hint and error text, wired for screen readers (label for, aria-describedby,
 * aria-invalid). The error is part of the control's description, read when it is focused; forms move focus to the
 * first invalid control after a failed submit.
 */
export function Field({ label, hint, error, children, className }: FieldProps) {
  const id = useId();
  const hintId = `${id}-hint`;
  const errorId = `${id}-error`;
  const hasError = error !== undefined && error !== null && error !== false && error !== '';
  const describedBy = cx(hint !== undefined && hintId, hasError && errorId);
  return (
    <div className={cx(styles.field, hasError && styles.invalid, className)}>
      <label className={styles.label} htmlFor={id}>
        {label}
      </label>
      {children({
        id,
        ...(describedBy !== '' ? { 'aria-describedby': describedBy } : {}),
        ...(hasError ? { 'aria-invalid': true as const } : {}),
      })}
      {hint !== undefined && (
        <p id={hintId} className={styles.hint}>
          {hint}
        </p>
      )}
      {hasError && (
        <p id={errorId} className={styles.error}>
          {error}
        </p>
      )}
    </div>
  );
}

export interface TextFieldProps extends Omit<ComponentPropsWithRef<'input'>, 'id' | 'children'> {
  label: ReactNode;
  hint?: ReactNode;
  error?: ReactNode;
}

/** A Field with a text input (text, password, url …). Other props go to the <input>. */
export function TextField({ label, hint, error, className, ...input }: TextFieldProps) {
  return (
    <Field label={label} hint={hint} error={error} className={className}>
      {(control) => <input {...input} {...control} className={styles.input} />}
    </Field>
  );
}

/** The input style, for controls that a Field wraps by hand (select, textarea). */
export const inputClass = styles.input ?? '';
