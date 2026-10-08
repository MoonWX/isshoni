// The states a token page (/invite, /reset, /setup) is in before its form shows: checking the link, a check that
// couldn't finish, a link that doesn't work.
import type { ReactNode } from 'react';
import { Trans, useTranslation } from 'react-i18next';
import { Link } from 'react-router';

import { CenteredPage, type CenteredPageProps } from '../app/layouts/CenteredPage';
import { errorMessage } from '../lib/errorText';
import { Button } from '../ui/Button';
import { Spinner } from '../ui/Spinner';
import { Prose } from './AuthForm';
import styles from './LinkStates.module.css';
import { Notice } from './Notice';

/** "Already have an account? Log in": the small print under the token pages' cards. */
export function HaveAccount() {
  return (
    <p>
      <Trans i18nKey="auth.haveAccount" components={{ a: <Link to="/login" /> }} />
    </p>
  );
}

/** The link check is running. The card keeps the page's title, so the layout doesn't jump when the form arrives. */
export function CheckingLink({ title, width }: Pick<CenteredPageProps, 'title' | 'width'>) {
  const { t } = useTranslation();
  return (
    <CenteredPage title={title} width={width}>
      <div className={styles.checking}>
        <Spinner label={t('auth.checkingLink')} />
      </div>
    </CenteredPage>
  );
}

export interface CheckFailedProps extends Pick<CenteredPageProps, 'title' | 'width'> {
  /** Why the check couldn't finish: offline, rate_limited, a server error. */
  error: unknown;
  onRetry: () => void;
  retrying: boolean;
}

/** The link check couldn't finish, which says nothing about the link: the message and "Try again". */
export function CheckFailed({ title, width, error, onRetry, retrying }: CheckFailedProps) {
  const { t } = useTranslation();
  return (
    <CenteredPage title={title} width={width}>
      <Notice
        action={
          <Button size="sm" loading={retrying} onClick={onRetry}>
            {t('auth.tryAgain')}
          </Button>
        }
      >
        {errorMessage(error, t)}
      </Notice>
    </CenteredPage>
  );
}

export interface LinkProblemProps extends Pick<CenteredPageProps, 'title' | 'width' | 'footer'> {
  /** Why it doesn't work and what to do (translated). */
  children: ReactNode;
  /** Buttons or links (AuthForm's Actions). */
  actions?: ReactNode;
}

/** The link doesn't work, or the page was opened without one: the reason, and where to go from here. */
export function LinkProblem({ title, width, footer, children, actions }: LinkProblemProps) {
  return (
    <CenteredPage title={title} width={width} footer={footer}>
      <Prose>{children}</Prose>
      {actions}
    </CenteredPage>
  );
}
