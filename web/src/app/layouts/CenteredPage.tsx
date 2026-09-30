import type { ReactNode } from 'react';
import { useTranslation } from 'react-i18next';
import { Link } from 'react-router';

import { cx } from '../../ui/cx';
import { AppMark } from '../AppMark';
import styles from './CenteredPage.module.css';

export interface CenteredPageProps {
  /** The page's <h1> (translated). */
  title: ReactNode;
  /** A line under the title (translated). */
  lead?: ReactNode;
  children: ReactNode;
  /** Small print under the card: links to /about, "Forgot your password?". */
  footer?: ReactNode;
  /** Above the card: the in-app browser banner (05 §16.3). */
  banner?: ReactNode;
  /** md: forms (26 rem); lg: the setup wizard (40 rem). */
  width?: 'md' | 'lg';
}

/**
 * The layout of single-purpose pages (login, invite, sign-up, reset, setup): the app mark, then one card with the
 * page title and its form, centered, full-width on phones.
 */
export function CenteredPage({ title, lead, children, footer, banner, width = 'md' }: CenteredPageProps) {
  const { t } = useTranslation();
  return (
    <main className={styles.page}>
      <Link to="/" className={styles.brand}>
        <AppMark size={28} />
        <span>{t('common.appName')}</span>
      </Link>
      <div className={cx(styles.column, styles[width])}>
        {banner}
        <section className={styles.card}>
          <header className={styles.header}>
            <h1 className={styles.title}>{title}</h1>
            {lead !== undefined && <p className={styles.lead}>{lead}</p>}
          </header>
          {children}
        </section>
        {footer !== undefined && <footer className={styles.footer}>{footer}</footer>}
      </div>
    </main>
  );
}
