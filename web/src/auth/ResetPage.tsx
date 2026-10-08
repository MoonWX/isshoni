import { useState } from 'react';
import { useTranslation } from 'react-i18next';
import { useNavigate } from 'react-router';

import { useApp } from '../app/context';
import { useInfo } from '../app/info';
import { CenteredPage } from '../app/layouts/CenteredPage';
import { errorMessage } from '../lib/errorText';
import { CodeResetTokenInvalid, type ResetCompleteRequest, type UserResponse } from '../protocol/api.gen';
import { ApiError, api, isApiError } from '../protocol/rest';
import { TextField } from '../ui/Field';
import { AuthForm, SmallPrint } from './AuthForm';
import { checkNewPassword, fieldCodes } from './formErrors';
import { useFragmentToken, type FragmentToken } from './fragmentToken';
import { CheckFailed, CheckingLink, HaveAccount, LinkProblem, useFocusHeadingWhen } from './LinkStates';
import { PasswordField } from './PasswordField';
import { useLinkCheck } from './useLinkCheck';
import { startSession } from './session';
import { useAccountRules, useSubmit } from './useSubmit';

function deadLink(err: unknown): ApiError | null {
  return isApiError(err, CodeResetTokenInvalid) ? err : null;
}

/**
 * /reset#<token> (05 §15.1, 03 §7.10): an admin made the link; boot moved the token out of the address bar
 * (fragmentToken.ts).
 * 1. POST /api/v1/auth/reset/check → {username}, or reset_token_invalid (unknown, used or expired).
 * 2. A new password (show/hide, the rules of /info) → POST /api/v1/auth/reset/complete {token, password} → 200
 *    with the session cookie → the stored token is cleared → "/".
 */
export function ResetPage() {
  const { token, clear } = useFragmentToken('reset');
  // Another reset link opened in this tab starts the page over.
  return <Reset key={token ?? ''} token={token} clear={clear} />;
}

function Reset({ token, clear }: FragmentToken) {
  const { t } = useTranslation();
  const { queryClient } = useApp();
  const navigate = useNavigate();
  const serverName = useInfo().data?.server.name ?? t('common.appName');
  const rules = useAccountRules();
  const [password, setPassword] = useState('');
  /** The link stopped working between the check and the submit. */
  const [diedOnSubmit, setDiedOnSubmit] = useState<ApiError | null>(null);
  useFocusHeadingWhen(diedOnSubmit !== null);
  const check = useLinkCheck('reset', token);

  const form = useSubmit<'password', string, UserResponse>({
    fields: ['password'],
    validate: (pw) => fieldCodes({ password: checkNewPassword(pw, rules) }),
    request: (pw) => {
      const body: ResetCompleteRequest = { token: token ?? '', password: pw };
      return api<UserResponse>('POST', '/api/v1/auth/reset/complete', body);
    },
    onSuccess: async () => {
      clear();
      await startSession(queryClient);
      await navigate('/', { replace: true });
    },
    onError: (err) => {
      const dead = deadLink(err);
      if (dead) setDiedOnSubmit(dead);
      return dead !== null;
    },
  });

  const title = t('auth.reset.title');
  const footer = (
    <SmallPrint>
      <HaveAccount />
    </SmallPrint>
  );

  if (token === null) {
    return (
      <LinkProblem title={t('auth.reset.noToken.title')} footer={footer}>
        <p>{t('auth.reset.noToken.body')}</p>
      </LinkProblem>
    );
  }
  const dead = diedOnSubmit ?? deadLink(check.error);
  if (dead) {
    return (
      <LinkProblem title={t('auth.reset.dead.title')} footer={footer}>
        <p>{errorMessage(dead, t)}</p>
      </LinkProblem>
    );
  }
  if (check.isError) {
    return (
      <CheckFailed
        title={title}
        error={check.error}
        retrying={check.isFetching}
        onRetry={() => {
          void check.refetch();
        }}
      />
    );
  }
  if (!check.isSuccess) return <CheckingLink title={title} />;

  return (
    <CenteredPage title={title} lead={t('auth.reset.lead', { server: serverName })} footer={footer}>
      <AuthForm
        formRef={form.formRef}
        onSubmit={() => {
          form.submit(password);
        }}
        submitLabel={t('auth.reset.submit')}
        busy={form.busy}
        waiting={form.waiting}
        error={form.formError}
        errorSpoken={form.formErrorSpoken}
      >
        {/* Read-only: it says whose password this is, and lets a password manager file the new one correctly. */}
        <TextField
          label={t('auth.username')}
          name="username"
          autoComplete="username"
          readOnly
          value={check.data.username}
        />
        <PasswordField
          label={t('auth.newPassword')}
          name="password"
          autoComplete="new-password"
          required
          value={password}
          onChange={(e) => {
            setPassword(e.target.value);
          }}
          hint={t('auth.passwordHint', { min: rules.passwordMinLength })}
          error={form.fieldErrors.password}
        />
      </AuthForm>
    </CenteredPage>
  );
}
