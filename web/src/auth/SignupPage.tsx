import { useState } from 'react';
import { useTranslation } from 'react-i18next';
import { Navigate, useNavigate } from 'react-router';

import { useApp } from '../app/context';
import { useInfo } from '../app/info';
import { CenteredPage } from '../app/layouts/CenteredPage';
import {
  RegistrationModeApproval,
  UserStatusPending,
  type RegisterRequest,
  type RegisterResponse,
} from '../protocol/api.gen';
import { api } from '../protocol/rest';
import { AccountFields, checkAccount, type AccountValues } from './AccountFields';
import { AuthForm, SmallPrint } from './AuthForm';
import { HaveAccount } from './LinkStates';
import { startSession } from './session';
import { useAccountRules, useSubmit } from './useSubmit';

/**
 * /signup (05 §5, §15.1, 03 §7.9): the invite form without a token, only while the server's registration mode is
 * `approval`; in every other mode the page redirects to /login. POST /api/v1/auth/register {username, password}
 * → 202 {status: "pending"}, no session → /pending. Sign-ups are limited to a few per hour per address
 * (rate_limited shows the wait), and the queue holds at most 50 (limit_reached).
 */
export function SignupPage() {
  const { t } = useTranslation();
  const { queryClient } = useApp();
  const navigate = useNavigate();
  const info = useInfo().data;
  const rules = useAccountRules();
  const [values, setValues] = useState<AccountValues>({ username: '', password: '' });

  const form = useSubmit<'username' | 'password', AccountValues, RegisterResponse>({
    fields: ['username', 'password'],
    validate: (v) => checkAccount(v, rules),
    request: (v) => {
      const body: RegisterRequest = { username: v.username, password: v.password };
      return api<RegisterResponse>('POST', '/api/v1/auth/register', body);
    },
    onSuccess: async (res) => {
      if (res.status === UserStatusPending) {
        await navigate('/pending', { replace: true });
        return;
      }
      // Not what approval mode answers (202 pending), but an active account comes with a session.
      await startSession(queryClient);
      await navigate('/', { replace: true });
    },
  });

  // After a submit the mode no longer matters: the admin may have changed it while the request ran.
  if (info !== undefined && info.registration !== RegistrationModeApproval && !form.busy) {
    return <Navigate to="/login" replace />;
  }

  return (
    <CenteredPage
      title={t('auth.signup.title')}
      lead={t('auth.signup.lead', { server: info?.server.name ?? t('common.appName') })}
      footer={
        <SmallPrint>
          <HaveAccount />
        </SmallPrint>
      }
    >
      <AuthForm
        formRef={form.formRef}
        onSubmit={() => {
          form.submit(values);
        }}
        submitLabel={t('auth.signup.submit')}
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
