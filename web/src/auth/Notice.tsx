import { CircleAlert, Info } from 'lucide-react';
import type { ReactNode } from 'react';

import { cx } from '../ui/cx';
import { VisuallyHidden } from '../ui/VisuallyHidden';
import styles from './Notice.module.css';

export interface NoticeProps {
  /** error: something failed (announced at once); info: why the user is here ("You were signed out"). */
  tone?: 'error' | 'info';
  /** The message (translated). */
  children: ReactNode;
  /**
   * What screen readers hear instead, for a message whose visible text keeps changing (a countdown): the visible
   * text is then hidden from them, so each tick isn't announced again.
   */
  spoken?: string;
  /** A button or link under the message ("Try again"). */
  action?: ReactNode;
}

/**
 * A message above a form: the error of a failed submit (role="alert") or a note about how the user got here
 * (role="status").
 */
export function Notice({ tone = 'error', children, spoken, action }: NoticeProps) {
  const Icon = tone === 'error' ? CircleAlert : Info;
  return (
    <div className={cx(styles.notice, styles[tone])}>
      <Icon className={styles.icon} aria-hidden="true" />
      <div className={styles.body}>
        <p role={tone === 'error' ? 'alert' : 'status'}>
          {spoken === undefined ? (
            children
          ) : (
            <>
              <VisuallyHidden>{spoken}</VisuallyHidden>
              <span aria-hidden="true">{children}</span>
            </>
          )}
        </p>
        {action}
      </div>
    </div>
  );
}
