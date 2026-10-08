// The frame every admin page shares (05 §15.3): the page's <main> with its <h1> (focus moves there after a
// navigation, 05 §16.6), a line about the page, and the states of the list it shows: loading, failed, empty.
import { useCallback, useEffect, useId, useRef, type ReactNode } from 'react';
import { useTranslation } from 'react-i18next';

import { useApp } from '../app/context';
import { focusPageHeading } from '../app/layouts/RootLayout';
import { Notice } from '../auth/Notice';
import { errorMessage } from '../lib/errorText';
import { CodeForbidden } from '../protocol/api.gen';
import { queryKeys } from '../protocol/queryKeys';
import { isApiError } from '../protocol/rest';
import { Button } from '../ui/Button';
import { cx } from '../ui/cx';
import { Spinner } from '../ui/Spinner';
import styles from './AdminPage.module.css';

export interface AdminPageProps {
  /** The page's <h1> (translated). */
  title: string;
  /** A line under the title (translated). */
  lead?: ReactNode;
  /** Buttons beside the title: the page's main action ("Reject all"). */
  actions?: ReactNode;
  children: ReactNode;
}

export function AdminPage({ title, lead, actions, children }: AdminPageProps) {
  return (
    <main className={styles.page}>
      <header className={styles.header}>
        <div className={styles.heading}>
          <h1 className={styles.title}>{title}</h1>
          {lead !== undefined && <p className={styles.lead}>{lead}</p>}
        </div>
        {actions !== undefined && <div className={styles.actions}>{actions}</div>}
      </header>
      {children}
    </main>
  );
}

export interface SectionProps {
  /** The section's <h2> (translated). */
  title: string;
  /** A line under the heading (translated). */
  lead?: ReactNode;
  children: ReactNode;
}

/** A titled card: a form ("New invite"), a group of settings. */
export function Section({ title, lead, children }: SectionProps) {
  const headingId = useId();
  return (
    <section className={styles.section} aria-labelledby={headingId}>
      <header className={styles.sectionHeader}>
        <h2 id={headingId} className={styles.sectionTitle}>
          {title}
        </h2>
        {lead !== undefined && <p className={styles.lead}>{lead}</p>}
      </header>
      {children}
    </section>
  );
}

/** The part of a query result the list states read; useQuery's and useInfiniteQuery's results both have it. */
export interface ListQuery<T> {
  readonly data: T | undefined;
  readonly error: unknown;
  readonly isError: boolean;
  readonly isFetching: boolean;
  refetch(): unknown;
}

export interface LoadedProps<T> {
  query: ListQuery<T>;
  /** Renders the data once it is there. A background refetch that fails keeps showing it. */
  children: (data: T) => ReactNode;
}

/**
 * A list's loading and failed states. A failure says why, with "Try again". A 403 forbidden means the role
 * changed since ['me'] was loaded (another admin removed it): ['me'] is asked for again, and the guard then shows
 * what a member sees (app/guards.tsx).
 */
export function Loaded<T>({ query, children }: LoadedProps<T>) {
  const { t } = useTranslation();
  const { queryClient } = useApp();
  const forbidden = isApiError(query.error, CodeForbidden);
  useEffect(() => {
    if (forbidden) void queryClient.invalidateQueries({ queryKey: queryKeys.me, exact: true });
  }, [forbidden, queryClient]);

  if (query.data !== undefined) return children(query.data);
  if (query.isError) {
    return (
      <Notice
        action={
          <Button
            size="sm"
            loading={query.isFetching}
            onClick={() => {
              void query.refetch();
            }}
          >
            {t('admin.tryAgain')}
          </Button>
        }
      >
        {t('admin.loadFailed', { reason: errorMessage(query.error, t) })}
      </Notice>
    );
  }
  return (
    <div className={styles.loading}>
      <Spinner />
    </div>
  );
}

/** What a list says when it has nothing to show. */
export function Empty({ children }: { children: ReactNode }) {
  return <p className={styles.empty}>{children}</p>;
}

export type TagTone = 'neutral' | 'accent' | 'success' | 'warning' | 'danger';

/** A short label next to a name: a role, a state. Never the only carrier of meaning: the text says it. */
export function Tag({ tone = 'neutral', children }: { tone?: TagTone; children: ReactNode }) {
  return <span className={cx(styles.tag, styles[tone])}>{children}</span>;
}

/**
 * For an action whose dialog was opened from a control that the action removes (a deleted row's menu, a revoked
 * invite's button): closing a dialog puts focus back on what opened it, and that is about to be gone. Call the
 * returned function when the action went through; once the dialog is closed, focus goes to the page's heading
 * instead of nowhere (05 §16.6).
 */
export function useHeadingFocusAfter(dialogOpen: boolean): () => void {
  const wanted = useRef(false);
  useEffect(() => {
    if (dialogOpen || !wanted.current) return;
    wanted.current = false;
    focusPageHeading();
  }, [dialogOpen]);
  return useCallback(() => {
    wanted.current = true;
  }, []);
}
