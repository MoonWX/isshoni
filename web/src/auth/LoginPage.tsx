import { EyeOff, Server, ShieldCheck } from 'lucide-react';
import { useEffect, useId, useRef, useState } from 'react';
import { Trans, useTranslation } from 'react-i18next';
import { Link, Navigate, useLocation, useNavigate, useSearchParams } from 'react-router';

import { useApp } from '../app/context';
import { safeNext } from '../app/guards';
import { useInfo } from '../app/info';
import { CenteredPage } from '../app/layouts/CenteredPage';
import { focusPageHeading } from '../app/layouts/RootLayout';
import { errorMessage } from '../lib/errorText';
import {
  CodeAccountPending,
  FieldRequired,
  RegistrationModeApproval,
  type LoginRequest,
  type UserResponse,
} from '../protocol/api.gen';
import { api, isApiError } from '../protocol/rest';
import { TextField } from '../ui/Field';
import { PageSpinner } from '../ui/Spinner';
import { AuthForm, SmallPrint } from './AuthForm';
import { fieldCodes } from './formErrors';
import { InAppBrowserBanner } from './InAppBrowserBanner';
import { loginNoticeCode } from './loginNotice';
import styles from './LoginPage.module.css';
import { Notice } from './Notice';
import { PasswordField } from './PasswordField';
import { startSession } from './session';
import { useConfirmedMe } from './useMe';
import { useSubmit } from './useSubmit';

/**
 * Where a login goes: `?next=` when it is a same-origin path (app/guards.tsx safeNext), else "/". A next that points
 * back at the login page would keep a signed-in user on it, so that is "/" too.
 */
export function afterLogin(next: string | null): string {
  const path = safeNext(next);
  return /^\/login(?:[/?#]|$)/.test(path) ? '/' : path;
}

/**
 * /login (05 §5, §15.1): username and password, then on to `?next=` (afterLogin) or the default room.
 * - invalid_credentials, account_disabled and anything unexpected show above the form; rate_limited (429) and
 *   server_busy (503) show the wait and count it down; account_pending goes to /pending.
 * - A navigation here may carry a notice in its location state (loginNotice.ts): "You were signed out".
 * - Someone who is already signed in goes straight on, once the server has said so: a user this tab only has in its
 *   cache is asked for again first (useConfirmedMe), because the session may be what just ended.
 * - Below the form: where passwords and accounts come from (03 §7.10, §7.9), and the trust model in three lines
 *   with a link to /about (05 §6.3).
 */
export function LoginPage() {
  const { t } = useTranslation();
  const { queryClient } = useApp();
  const navigate = useNavigate();
  const location = useLocation();
  const [params] = useSearchParams();
  const next = afterLogin(params.get('next'));
  const info = useInfo().data;
  const me = useConfirmedMe();
  const [values, setValues] = useState<LoginRequest>({ username: '', password: '' });

  const form = useSubmit<'username' | 'password', LoginRequest, UserResponse>({
    fields: ['username', 'password'],
    validate: (v) =>
      fieldCodes({
        username: v.username.trim() === '' ? FieldRequired : null,
        password: v.password === '' ? FieldRequired : null,
      }),
    request: (v) => api<UserResponse>('POST', '/api/v1/auth/login', v),
    onSuccess: async () => {
      await startSession(queryClient);
      await navigate(next, { replace: true });
    },
    onError: (err) => {
      if (!isApiError(err, CodeAccountPending)) return false;
      // The password was right, but an admin hasn't approved the sign-up yet (03 §7.9).
      void navigate('/pending');
      return true;
    },
  });

  // Already signed in (a bookmark, the back button). After a submit, onSuccess navigates instead.
  const signedIn = Boolean(me.data) && !form.busy;
  // The user came from the cache and the server hasn't confirmed it yet: neither the form nor the way on.
  const confirming = signedIn && me.isFetching;
  // When the form follows the spinner, the focus move after the navigation (RootLayout, 05 §16.6) found no heading.
  const spinnerShown = useRef(false);
  useEffect(() => {
    if (confirming) {
      spinnerShown.current = true;
    } else if (spinnerShown.current && !signedIn) {
      spinnerShown.current = false;
      focusPageHeading();
    }
  }, [confirming, signedIn]);

  if (confirming) return <PageSpinner />;
  if (signedIn) return <Navigate to={next} replace />;

  const notice = loginNoticeCode(location.state);
  return (
    <CenteredPage
      title={t('auth.login.title', { server: info?.server.name ?? t('common.appName') })}
      banner={<InAppBrowserBanner />}
      footer={<Trust />}
    >
      <AuthForm
        formRef={form.formRef}
        onSubmit={() => {
          form.submit(values);
        }}
        submitLabel={t('auth.login.submit')}
        busy={form.busy}
        waiting={form.waiting}
        error={form.formError}
        errorSpoken={form.formErrorSpoken}
        notice={
          notice !== null &&
          form.formError === null && <Notice tone="info">{errorMessage({ code: notice, scope: 'session' }, t)}</Notice>
        }
      >
        <TextField
          label={t('auth.username')}
          name="username"
          autoComplete="username"
          autoCapitalize="none"
          autoCorrect="off"
          spellCheck={false}
          required
          value={values.username}
          onChange={(e) => {
            setValues({ ...values, username: e.target.value });
          }}
          error={form.fieldErrors.username}
        />
        <PasswordField
          label={t('auth.password')}
          name="password"
          autoComplete="current-password"
          required
          value={values.password}
          onChange={(e) => {
            setValues({ ...values, password: e.target.value });
          }}
          error={form.fieldErrors.password}
        />
      </AuthForm>
      <SmallPrint>
        <p>{t('auth.login.forgot')}</p>
        <p>
          {info?.registration === RegistrationModeApproval ? (
            <Trans i18nKey="auth.login.needAccountApproval" components={{ a: <Link to="/signup" /> }} />
          ) : (
            t('auth.login.needAccount')
          )}
        </p>
      </SmallPrint>
    </CenteredPage>
  );
}

/** The trust model, as the login page states it (05 §6.3, plan "Privacy and trust model"). */
function Trust() {
  const { t } = useTranslation();
  const headingId = useId();
  return (
    <section className={styles.trust} aria-labelledby={headingId}>
      <h2 id={headingId} className={styles.heading}>
        {t('auth.login.trust.heading')}
      </h2>
      <ul className={styles.lines}>
        <li>
          <Server aria-hidden="true" />
          <span>{t('auth.login.trust.server')}</span>
        </li>
        <li>
          <EyeOff aria-hidden="true" />
          <span>{t('auth.login.trust.noRecording')}</span>
        </li>
        <li>
          <ShieldCheck aria-hidden="true" />
          <span>{t('auth.login.trust.noTelemetry')}</span>
        </li>
      </ul>
      <Link to="/about">{t('auth.login.trust.more')}</Link>
    </section>
  );
}
