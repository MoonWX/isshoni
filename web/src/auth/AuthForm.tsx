import type { ReactNode, RefObject } from 'react';

import { Button } from '../ui/Button';
import styles from './AuthForm.module.css';
import { Notice } from './Notice';

export interface AuthFormProps {
  /** useSubmit()'s formRef: focus moves to the first invalid control after a failed submit. */
  formRef: RefObject<HTMLFormElement | null>;
  onSubmit: () => void;
  /** The fields. */
  children: ReactNode;
  /** The submit button's label (translated). */
  submitLabel: string;
  /** The request is running: the button shows a spinner and ignores clicks. */
  busy: boolean;
  /** The server asked to wait: the button is off. */
  waiting?: boolean;
  /** The message above the fields (translated): useSubmit()'s formError. */
  error?: string | null;
  /** useSubmit()'s formErrorSpoken. */
  errorSpoken?: string | undefined;
  /** A note above the fields that isn't an error ("You were signed out"). */
  notice?: ReactNode;
}

/**
 * The form of an auth or setup page: a message, the fields, one submit button. The browser's own validation
 * bubbles are off (noValidate): every message comes from en.json, under its field, the same for the checks done
 * here and the server's (05 §6.3).
 */
export function AuthForm({
  formRef,
  onSubmit,
  children,
  submitLabel,
  busy,
  waiting = false,
  error,
  errorSpoken,
  notice,
}: AuthFormProps) {
  return (
    <form
      ref={formRef}
      className={styles.form}
      noValidate
      onSubmit={(e) => {
        e.preventDefault();
        onSubmit();
      }}
    >
      {notice}
      {error !== null && error !== undefined && <Notice spoken={errorSpoken}>{error}</Notice>}
      {children}
      <Button type="submit" variant="primary" block loading={busy} disabled={waiting}>
        {submitLabel}
      </Button>
    </form>
  );
}

/** A column of short paragraphs under or beside a form: hints, "Forgot your password?", links. */
export function SmallPrint({ children }: { children: ReactNode }) {
  return <div className={styles.small}>{children}</div>;
}

/** A row of buttons or button-like links under a message. */
export function Actions({ children }: { children: ReactNode }) {
  return <div className={styles.actions}>{children}</div>;
}

/** The text of a page that has no form: why a link doesn't work, what to do next. */
export function Prose({ children }: { children: ReactNode }) {
  return <div className={styles.prose}>{children}</div>;
}
