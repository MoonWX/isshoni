import { useState } from 'react';
import { useTranslation } from 'react-i18next';
import { Link, useNavigate } from 'react-router';

import { useApp } from '../app/context';
import { useInfo } from '../app/info';
import { CenteredPage } from '../app/layouts/CenteredPage';
import { SETUP_URL_COMMAND, SETUP_URL_DOCKER_COMMAND } from '../app/screens/NotSetUp';
import { CommandBlock } from '../app/screens/ScreenFrame';
import { AccountFields, checkAccount, type AccountValues } from '../auth/AccountFields';
import { Actions, AuthForm } from '../auth/AuthForm';
import { checkServerName, fieldCodes, SERVER_NAME_MAX_LENGTH } from '../auth/formErrors';
import { useFragmentToken, type FragmentToken } from '../auth/fragmentToken';
import { CheckFailed, CheckingLink, LinkProblem, useFocusHeadingWhen } from '../auth/LinkStates';
import { useLinkCheck } from '../auth/useLinkCheck';
import { startSession } from '../auth/session';
import { useAccountRules, useSubmit } from '../auth/useSubmit';
import {
  CodeSetupTokenInvalid,
  CodeSetupUnavailable,
  type SetupCompleteRequest,
  type UserResponse,
} from '../protocol/api.gen';
import { queryKeys } from '../protocol/queryKeys';
import { api, isApiError } from '../protocol/rest';
import { buttonClass } from '../ui/Button';
import { TextField } from '../ui/Field';
import { Stepper } from '../ui/Stepper';

/**
 * Where step 1 goes once the admin exists. The wizard's steps 2–3 (WelcomePage, S87) change it to
 * '/admin/welcome?step=2' (05 §14.1).
 */
export const AFTER_SETUP_PATH = '/';

interface SetupValues extends AccountValues {
  serverName: string;
}

/** Why the page can't create the admin: the link is dead, or the server already has one (03 §7.8). */
type SetupProblem = 'tokenInvalid' | 'alreadySetUp';

function setupProblem(err: unknown): SetupProblem | null {
  if (isApiError(err, CodeSetupTokenInvalid)) return 'tokenInvalid';
  if (isApiError(err, CodeSetupUnavailable)) return 'alreadySetUp';
  return null;
}

/** The two commands that print a setup link, as the NotSetUp screen shows them (04 §12.2). */
function SetupUrlCommands({ docker, after }: { docker: string; after: string }) {
  return (
    <>
      <CommandBlock>{SETUP_URL_COMMAND}</CommandBlock>
      <p>{docker}</p>
      <CommandBlock>{SETUP_URL_DOCKER_COMMAND}</CommandBlock>
      <p>{after}</p>
    </>
  );
}

/**
 * /setup#<token>, step 1 of the setup wizard: create the admin (05 §14.1, 03 §7.8). Public, with the one-time token
 * that `isshoni setup-url` printed; boot moved it out of the address bar (auth/fragmentToken.ts).
 * 1. POST /api/v1/auth/setup/check {token} → 204. setup_token_invalid: the link was replaced or has expired, with
 *    the command for a new one. setup_unavailable: an admin already exists → Log in.
 * 2. The form: username, password (show/hide, no repeat field, the rules of /info's accountRules) and an optional
 *    server name → POST /api/v1/auth/setup/complete → 201 with the session cookie → the stored token is cleared →
 *    AFTER_SETUP_PATH.
 * The stepper shows all three steps of the wizard; steps 2 and 3 live on /admin/welcome.
 */
export function SetupPage() {
  const { token, clear } = useFragmentToken('setup');
  // A newer setup link opened in this tab (the old one was replaced) starts the page over.
  return <Setup key={token ?? ''} token={token} clear={clear} />;
}

function Setup({ token, clear }: FragmentToken) {
  const { t } = useTranslation();
  const { queryClient } = useApp();
  const navigate = useNavigate();
  const info = useInfo().data;
  const rules = useAccountRules();
  const [values, setValues] = useState<SetupValues>({ username: '', password: '', serverName: '' });
  /** The link died, or another tab created the admin, between the check and the submit. */
  const [problemOnSubmit, setProblemOnSubmit] = useState<SetupProblem | null>(null);
  useFocusHeadingWhen(problemOnSubmit !== null);
  const check = useLinkCheck('setup', token);

  const form = useSubmit<'username' | 'password' | 'serverName', SetupValues, UserResponse>({
    fields: ['username', 'password', 'serverName'],
    validate: (v) => fieldCodes({ ...checkAccount(v, rules), serverName: checkServerName(v.serverName) }),
    request: (v) => {
      const serverName = v.serverName.trim();
      const body: SetupCompleteRequest = {
        token: token ?? '',
        username: v.username,
        password: v.password,
        ...(serverName !== '' ? { serverName } : {}),
      };
      return api<UserResponse>('POST', '/api/v1/auth/setup/complete', body);
    },
    onSuccess: async () => {
      clear();
      // The server is set up now and may have a new name: GET /api/v1/info changed.
      void queryClient.invalidateQueries({ queryKey: queryKeys.info });
      await startSession(queryClient);
      await navigate(AFTER_SETUP_PATH, { replace: true });
    },
    onError: (err) => {
      const problem = setupProblem(err);
      if (problem) setProblemOnSubmit(problem);
      return problem !== null;
    },
  });

  const title = t('setup.admin.title');
  const steps = (
    <Stepper
      label={t('setup.steps.label')}
      steps={[t('setup.steps.admin'), t('setup.steps.conntest'), t('setup.steps.invite')]}
      current={0}
    />
  );

  const alreadySetUp = (
    <LinkProblem
      width="lg"
      title={t('setup.done.title')}
      actions={
        <Actions>
          <Link to="/login" className={buttonClass({ variant: 'primary' })}>
            {t('setup.done.logIn')}
          </Link>
        </Actions>
      }
    >
      <p>{t('setup.done.body')}</p>
    </LinkProblem>
  );

  if (token === null) {
    // Without a link: the server either still waits for its admin, or has one.
    if (info?.setupRequired === false) return alreadySetUp;
    return (
      <LinkProblem width="lg" title={t('setup.noToken.title')}>
        <p>{t('setup.noToken.body')}</p>
        <SetupUrlCommands docker={t('setup.noToken.docker')} after={t('setup.noToken.after')} />
      </LinkProblem>
    );
  }

  const problem = problemOnSubmit ?? setupProblem(check.error);
  if (problem === 'alreadySetUp') return alreadySetUp;
  if (problem === 'tokenInvalid') {
    return (
      <LinkProblem width="lg" title={t('setup.tokenInvalid.title')}>
        <p>{t('setup.tokenInvalid.body')}</p>
        <SetupUrlCommands docker={t('setup.tokenInvalid.docker')} after={t('setup.tokenInvalid.after')} />
      </LinkProblem>
    );
  }
  if (check.isError) {
    return (
      <CheckFailed
        width="lg"
        title={title}
        error={check.error}
        retrying={check.isFetching}
        onRetry={() => {
          void check.refetch();
        }}
      />
    );
  }
  if (!check.isSuccess) return <CheckingLink width="lg" title={title} />;

  return (
    <CenteredPage width="lg" title={title} lead={t('setup.admin.lead')} banner={steps}>
      <AuthForm
        formRef={form.formRef}
        onSubmit={() => {
          form.submit(values);
        }}
        submitLabel={t('setup.admin.submit')}
        busy={form.busy}
        waiting={form.waiting}
        error={form.formError}
        errorSpoken={form.formErrorSpoken}
      >
        <AccountFields
          values={values}
          onChange={(account) => {
            setValues({ ...values, ...account });
          }}
          errors={form.fieldErrors}
        />
        <TextField
          label={t('setup.admin.serverName')}
          name="serverName"
          autoComplete="off"
          // The browser stops at the limit, so the "too long" answer is only for what gets past it (it counts
          // UTF-16 units, the server characters: never more than the server allows).
          maxLength={SERVER_NAME_MAX_LENGTH}
          value={values.serverName}
          // Left empty, the server's name is its host, which /info already reports.
          placeholder={info?.server.name}
          onChange={(e) => {
            setValues({ ...values, serverName: e.target.value });
          }}
          hint={t('setup.admin.serverNameHint')}
          error={form.fieldErrors.serverName}
        />
      </AuthForm>
    </CenteredPage>
  );
}
