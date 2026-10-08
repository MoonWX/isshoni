import { useState } from 'react';
import { useTranslation } from 'react-i18next';
import { Link, useNavigate } from 'react-router';

import { useApp } from '../app/context';
import { useInfo } from '../app/info';
import { CenteredPage } from '../app/layouts/CenteredPage';
import { errorMessage } from '../lib/errorText';
import {
  CodeInviteExpired,
  CodeInviteInvalid,
  CodeInviteRevoked,
  CodeInviteUsedUp,
  CodeRegistrationClosed,
  UserStatusPending,
  type RegisterRequest,
  type RegisterResponse,
} from '../protocol/api.gen';
import { ApiError, api } from '../protocol/rest';
import { Button, buttonClass } from '../ui/Button';
import { AccountFields, checkAccount, type AccountValues } from './AccountFields';
import { Actions, AuthForm, Prose, SmallPrint } from './AuthForm';
import { useFragmentToken, type FragmentToken } from './fragmentToken';
import { InAppBrowserBanner } from './InAppBrowserBanner';
import { CheckFailed, CheckingLink, HaveAccount, LinkProblem, useFocusHeadingWhen } from './LinkStates';
import { useLinkCheck } from './useLinkCheck';
import { useLogout } from './useLogout';
import { startSession } from './session';
import { useMe } from './useMe';
import { useAccountRules, useSubmit } from './useSubmit';

/** The codes that say the link itself can't be used (03 §7.9); each has its own errors.<code> text. */
const DEAD_LINK_CODES: ReadonlySet<string> = new Set([
  CodeInviteInvalid,
  CodeInviteExpired,
  CodeInviteUsedUp,
  CodeInviteRevoked,
  CodeRegistrationClosed,
]);

function deadLink(err: unknown): ApiError | null {
  return err instanceof ApiError && DEAD_LINK_CODES.has(err.code) ? err : null;
}

/**
 * /invite#<token> (05 §15.1, 03 §7.9). Boot moved the token out of the address bar (fragmentToken.ts).
 * 1. A signed-in user sees who they are, with "Go to isshoni" and "Sign out and create another account".
 * 2. Otherwise POST /api/v1/auth/invite/check says who invited them to which server, or why the link doesn't work
 *    (invite_invalid, invite_expired, invite_used_up, invite_revoked, registration_closed).
 * 3. One form (username, password with show/hide) → POST /api/v1/auth/register {inviteToken, username, password}
 *    → 201 with the session cookie → the stored token is cleared → "/" (the default room).
 */
export function InvitePage() {
  const { token, clear } = useFragmentToken('invite');
  // Another invite link opened in this tab starts the page over: its check, and whatever was typed.
  return <Invite key={token ?? ''} token={token} clear={clear} />;
}

function Invite({ token, clear }: FragmentToken) {
  const { t } = useTranslation();
  const { queryClient } = useApp();
  const navigate = useNavigate();
  const serverName = useInfo().data?.server.name ?? t('common.appName');
  const rules = useAccountRules();
  const me = useMe();
  const logout = useLogout();
  const [values, setValues] = useState<AccountValues>({ username: '', password: '' });
  /** The invite stopped working between the check and the submit (used up, revoked, expired). */
  const [diedOnSubmit, setDiedOnSubmit] = useState<ApiError | null>(null);
  useFocusHeadingWhen(diedOnSubmit !== null);

  const form = useSubmit<'username' | 'password', AccountValues, RegisterResponse>({
    fields: ['username', 'password'],
    validate: (v) => checkAccount(v, rules),
    request: (v) => {
      const body: RegisterRequest = { inviteToken: token ?? '', username: v.username, password: v.password };
      return api<RegisterResponse>('POST', '/api/v1/auth/register', body);
    },
    onSuccess: async (res) => {
      clear();
      if (res.status === UserStatusPending) {
        // An invite makes the account active at once (03 §7.9); a server that answers "pending" anyway gets the
        // page that explains it.
        await navigate('/pending', { replace: true });
        return;
      }
      await startSession(queryClient);
      await navigate('/', { replace: true });
    },
    onError: (err) => {
      const dead = deadLink(err);
      if (dead) setDiedOnSubmit(dead);
      return dead !== null;
    },
  });

  // Signed-in users don't need the check; everyone else gets it as soon as GET /api/v1/me has answered.
  const account = me.data ?? null;
  const check = useLinkCheck('invite', token, !me.isPending && account === null);

  const genericTitle = t('auth.invite.titleNoInviter', { server: serverName });
  const footer = (
    <SmallPrint>
      <HaveAccount />
    </SmallPrint>
  );

  if (token === null) {
    return (
      <LinkProblem title={t('auth.invite.noToken.title')} footer={footer}>
        <p>{t('auth.invite.noToken.body')}</p>
      </LinkProblem>
    );
  }
  if (me.isPending) return <CheckingLink title={genericTitle} />;

  // After a successful submit ['me'] is the new user; the form stays (busy) until the navigation.
  if (account !== null && !form.busy) {
    return (
      <CenteredPage title={t('auth.invite.signedIn.title')}>
        <Prose>
          <p>{t('auth.invite.signedIn.body', { username: account.user.username })}</p>
        </Prose>
        <Actions>
          <Link to="/" className={buttonClass({ variant: 'primary' })}>
            {t('auth.invite.signedIn.go')}
          </Link>
          <Button
            loading={logout.pending}
            onClick={() => {
              void logout.logout();
            }}
          >
            {t('auth.invite.signedIn.switch')}
          </Button>
        </Actions>
      </CenteredPage>
    );
  }

  const dead = diedOnSubmit ?? deadLink(check.error);
  if (dead) {
    return (
      <LinkProblem title={t('auth.invite.dead.title')} footer={footer}>
        <p>{errorMessage(dead, t)}</p>
      </LinkProblem>
    );
  }
  if (check.isError) {
    return (
      <CheckFailed
        title={genericTitle}
        error={check.error}
        retrying={check.isFetching}
        onRetry={() => {
          void check.refetch();
        }}
      />
    );
  }
  if (!check.isSuccess) return <CheckingLink title={genericTitle} />;

  const invite = check.data;
  return (
    <CenteredPage
      title={
        invite.invitedBy !== undefined && invite.invitedBy !== ''
          ? t('auth.invite.title', { inviter: invite.invitedBy, server: invite.serverName })
          : t('auth.invite.titleNoInviter', { server: invite.serverName })
      }
      lead={t('auth.invite.lead')}
      banner={<InAppBrowserBanner inviteToken={token} />}
      footer={footer}
    >
      <AuthForm
        formRef={form.formRef}
        onSubmit={() => {
          form.submit(values);
        }}
        submitLabel={t('auth.invite.submit')}
        busy={form.busy}
        waiting={form.waiting}
        error={form.formError}
        errorSpoken={form.formErrorSpoken}
      >
        <AccountFields values={values} onChange={setValues} errors={form.fieldErrors} />
      </AuthForm>
    </CenteredPage>
  );
}
