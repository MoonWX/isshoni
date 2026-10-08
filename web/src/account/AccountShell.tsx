// The frame every account page renders itself in (05 §15.2): a way back to the room, the page's <h1>, the links
// between the account pages, then the page's sections. The router has no layout route for /account (app/router.tsx
// declares the three pages side by side under RequireAuth), so each page wraps its content in <AccountShell>.
import { ArrowLeft } from 'lucide-react';
import { useId, type ReactNode, type Ref } from 'react';
import { useTranslation } from 'react-i18next';
import { Link, NavLink } from 'react-router';

import { cx } from '../ui/cx';
import styles from './AccountShell.module.css';

export interface AccountShellProps {
  /** The page's <h1> (translated). RootLayout moves focus to it after a navigation (05 §16.6). */
  title: string;
  /** The page's sections. */
  children: ReactNode;
}

function tabClass({ isActive }: { isActive: boolean }): string {
  return cx(styles.tab, isActive && styles.active);
}

export function AccountShell({ title, children }: AccountShellProps) {
  const { t } = useTranslation();
  return (
    <main className={styles.page}>
      <div className={styles.column}>
        {/* "/" is the room the user was last in (rooms/ RootRedirect). An installed app has no Back button. */}
        <Link to="/" className={styles.back}>
          <ArrowLeft aria-hidden="true" />
          <span>{t('account.back')}</span>
        </Link>
        <h1 className={styles.title}>{title}</h1>
        <nav className={styles.tabs} aria-label={t('account.nav.label')}>
          <NavLink to="/account" end className={tabClass}>
            {t('account.nav.account')}
          </NavLink>
          <NavLink to="/account/devices" className={tabClass}>
            {t('account.nav.devices')}
          </NavLink>
          {/* Web Push on this device (05 §16.3). Until that page is built the router shows its PageUnavailable. */}
          <NavLink to="/account/notifications" className={tabClass}>
            {t('account.nav.notifications')}
          </NavLink>
        </nav>
        {children}
      </div>
    </main>
  );
}

export interface SectionProps {
  /** The section's <h2> (translated); it names the section. */
  heading: string;
  /** A line under the heading (translated). */
  lead?: ReactNode;
  /** danger: something that can't be undone (delete account). */
  tone?: 'neutral' | 'danger';
  /**
   * For a section whose controls can disappear when they are used (a revoked row and its button): the heading
   * takes focus then, so it doesn't fall back to the top of the page.
   */
  headingRef?: Ref<HTMLHeadingElement>;
  children: ReactNode;
}

/** One card of an account page. */
export function Section({ heading, lead, tone = 'neutral', headingRef, children }: SectionProps) {
  const id = useId();
  return (
    <section className={cx(styles.section, tone === 'danger' && styles.danger)} aria-labelledby={id}>
      <header className={styles.header}>
        <h2 id={id} className={styles.heading} ref={headingRef} tabIndex={headingRef === undefined ? undefined : -1}>
          {heading}
        </h2>
        {lead !== undefined && <p className={styles.lead}>{lead}</p>}
      </header>
      {children}
    </section>
  );
}
