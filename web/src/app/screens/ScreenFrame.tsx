import { Check, Copy, type LucideIcon } from 'lucide-react';
import { useEffect, useId, useRef, useState, type ReactNode } from 'react';
import { useTranslation } from 'react-i18next';

import { copyText } from '../../lib/clipboard';
import { cx } from '../../ui/cx';
import { AppMark } from '../AppMark';
import styles from './ScreenFrame.module.css';

export interface ScreenFrameProps {
  /** A lucide icon for the situation. */
  icon: LucideIcon;
  /** neutral: waiting or informational; warning: the admin must act; danger: it stopped. */
  tone?: 'neutral' | 'warning' | 'danger';
  /** The heading (translated). */
  title: string;
  /** What happened and what to do (translated). */
  children: ReactNode;
  /** Buttons. */
  actions?: ReactNode;
  /** Small print: an error code, version numbers. */
  footnote?: ReactNode;
}

/**
 * The layout of an app-level screen (05 §4): the whole page, one message, at most two actions. The heading takes
 * focus when the screen appears, so screen readers start there.
 */
export function ScreenFrame({ icon: Icon, tone = 'neutral', title, children, actions, footnote }: ScreenFrameProps) {
  const { t } = useTranslation();
  const titleId = useId();
  const heading = useRef<HTMLHeadingElement>(null);
  useEffect(() => {
    heading.current?.focus();
  }, []);
  return (
    <main className={styles.screen}>
      <div className={styles.brand}>
        <AppMark size={28} />
        <span>{t('common.appName')}</span>
      </div>
      <section className={styles.card} aria-labelledby={titleId}>
        <span className={cx(styles.icon, styles[tone])} aria-hidden="true">
          <Icon />
        </span>
        <h1 id={titleId} className={styles.title} ref={heading} tabIndex={-1}>
          {title}
        </h1>
        <div className={styles.body}>{children}</div>
        {actions !== undefined && <div className={styles.actions}>{actions}</div>}
        {footnote !== undefined && <p className={styles.footnote}>{footnote}</p>}
      </section>
    </main>
  );
}

/** A shell command with a Copy button (NotSetUp). It wraps on narrow screens; copying gives the one-line command. */
export function CommandBlock({ children }: { children: string }) {
  const { t } = useTranslation();
  const [copied, setCopied] = useState(false);
  useEffect(() => {
    if (!copied) return undefined;
    const id = setTimeout(() => {
      setCopied(false);
    }, 2000);
    return () => {
      clearTimeout(id);
    };
  }, [copied]);
  return (
    <div className={styles.command}>
      <pre>
        <code>{children}</code>
      </pre>
      <button
        type="button"
        className={styles.copy}
        onClick={() => {
          void copyText(children).then(setCopied);
        }}
      >
        {copied ? <Check aria-hidden="true" /> : <Copy aria-hidden="true" />}
        <span>{copied ? t('common.copied') : t('common.copy')}</span>
      </button>
    </div>
  );
}
