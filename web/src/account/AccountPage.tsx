import { CircleCheck, LogOut } from 'lucide-react';
import { useEffect, useId, useRef, useState } from 'react';
import { useTranslation } from 'react-i18next';

import { useApp } from '../app/context';
import { isAdmin } from '../app/guards';
import { useInfo } from '../app/info';
import { AuthForm } from '../auth/AuthForm';
import { checkNewPassword, fieldCodes } from '../auth/formErrors';
import { Notice } from '../auth/Notice';
import { PasswordField } from '../auth/PasswordField';
import { useLogout } from '../auth/useLogout';
import { useMe } from '../auth/useMe';
import { useAccountRules, useSubmit } from '../auth/useSubmit';
import {
  CodeLastAdmin,
  FieldRequired,
  type ChangePasswordRequest,
  type DeleteSelfRequest,
  type Me,
} from '../protocol/api.gen';
import { queryKeys } from '../protocol/queryKeys';
import { api, isApiError } from '../protocol/rest';
import { Button } from '../ui/Button';
import { Dialog } from '../ui/Dialog';
import styles from './AccountPage.module.css';
import { AccountShell, Section } from './AccountShell';
import { sessionEndedByServer } from './sessionEnded';
import { useWrongPassword } from './useWrongPassword';

/**
 * /account (05 §5, §15.2): who is signed in, with Sign out; change password; delete account. The route is under
 * RequireAuth, which redirects before this page renders without a user.
 */
export function AccountPage() {
  const { t } = useTranslation();
  const me = useMe().data;
  // Signed out in this very render (a logout, a 401): the guard above is about to redirect.
  if (!me) return null;
  return (
    <AccountShell title={t('account.title')}>
      <Profile me={me} />
      <ChangePassword username={me.user.username} />
      <DeleteAccount username={me.user.username} />
    </AccountShell>
  );
}

/** The username and role, and Sign out: the logout flow of 05 §15.1 (auth/useLogout.ts). */
function Profile({ me }: { me: Me }) {
  const { t } = useTranslation();
  const logout = useLogout();
  return (
    <Section heading={t('account.profile.heading')}>
      <dl className={styles.facts}>
        <div>
          <dt>{t('auth.username')}</dt>
          <dd>{me.user.username}</dd>
        </div>
        <div>
          <dt>{t('account.profile.role')}</dt>
          <dd>{isAdmin(me) ? t('account.profile.admin') : t('account.profile.member')}</dd>
        </div>
      </dl>
      <div className={styles.actions}>
        <Button
          icon={<LogOut />}
          loading={logout.pending}
          onClick={() => {
            void logout.logout();
          }}
        >
          {t('account.signOut')}
        </Button>
      </div>
    </Section>
  );
}

/**
 * Change password: POST /api/v1/me/password {currentPassword, newPassword} → 200 with this session's cookie
 * rotated; the user's other sessions and linked apps are signed out (03 §7.7), which the section says up front.
 * - 403 wrong_password shows under "Current password"; 422 names the fields; 429 and 503 server_busy count down.
 * - After a change the form starts over empty (a new instance: useSubmit keeps a succeeded form busy, because the
 *   auth pages navigate away then) and the confirmation takes focus, since the button that had it is gone.
 */
function ChangePassword({ username }: { username: string }) {
  const { t } = useTranslation();
  const [changes, setChanges] = useState(0);
  const done = useRef<HTMLParagraphElement>(null);
  useEffect(() => {
    if (changes > 0) done.current?.focus();
  }, [changes]);
  return (
    <Section heading={t('account.password.heading')} lead={t('account.password.note')}>
      {changes > 0 && (
        <p ref={done} tabIndex={-1} className={styles.done}>
          <CircleCheck aria-hidden="true" />
          <span>{t('account.password.done')}</span>
        </p>
      )}
      <div className={styles.form}>
        <ChangePasswordForm
          key={changes}
          username={username}
          onChanged={() => {
            setChanges((n) => n + 1);
          }}
        />
      </div>
    </Section>
  );
}

function ChangePasswordForm({ username, onChanged }: { username: string; onChanged: () => void }) {
  const { t } = useTranslation();
  const { queryClient } = useApp();
  const rules = useAccountRules();
  const [values, setValues] = useState<ChangePasswordRequest>({ currentPassword: '', newPassword: '' });
  const { inputRef, error: wrongPassword, onError: onWrongPassword, clear: clearWrongPassword } = useWrongPassword();

  const form = useSubmit<keyof ChangePasswordRequest, ChangePasswordRequest, unknown>({
    fields: ['currentPassword', 'newPassword'],
    validate: (v) =>
      fieldCodes({
        currentPassword: v.currentPassword === '' ? FieldRequired : null,
        newPassword: checkNewPassword(v.newPassword, rules),
      }),
    request: (v) => api('POST', '/api/v1/me/password', v),
    onSuccess: () => {
      // The other browsers and every linked app were just signed out: the devices page's lists are out of date.
      void queryClient.invalidateQueries({ queryKey: queryKeys.meSessions });
      void queryClient.invalidateQueries({ queryKey: queryKeys.meDevices });
      onChanged();
    },
    onError: onWrongPassword,
  });

  return (
    <AuthForm
      formRef={form.formRef}
      onSubmit={() => {
        clearWrongPassword();
        form.submit(values);
      }}
      submitLabel={t('account.password.submit')}
      busy={form.busy}
      waiting={form.waiting}
      error={form.formError}
      errorSpoken={form.formErrorSpoken}
    >
      {/* Not shown: tells a password manager which account the new password belongs to. */}
      <input type="text" name="username" autoComplete="username" value={username} readOnly hidden />
      <PasswordField
        ref={inputRef}
        label={t('account.password.current')}
        name="currentPassword"
        autoComplete="current-password"
        required
        value={values.currentPassword}
        onChange={(e) => {
          setValues({ ...values, currentPassword: e.target.value });
        }}
        error={form.fieldErrors.currentPassword ?? wrongPassword}
      />
      <PasswordField
        label={t('auth.newPassword')}
        name="newPassword"
        autoComplete="new-password"
        required
        value={values.newPassword}
        onChange={(e) => {
          setValues({ ...values, newPassword: e.target.value });
        }}
        hint={t('auth.passwordHint', { min: rules.passwordMinLength })}
        error={form.fieldErrors.newPassword}
      />
    </AuthForm>
  );
}

/**
 * Delete account: a button that opens the confirmation, which asks for the password. Each opening is a new dialog:
 * an empty field, no message from the time before.
 */
function DeleteAccount({ username }: { username: string }) {
  const { t } = useTranslation();
  const serverName = useInfo().data?.server.name ?? t('common.appName');
  const [dialog, setDialog] = useState({ open: false, opened: 0 });
  return (
    <Section
      tone="danger"
      heading={t('account.delete.heading')}
      lead={t('account.delete.body', { server: serverName })}
    >
      <div className={styles.actions}>
        <Button
          variant="danger"
          onClick={() => {
            setDialog((d) => ({ open: true, opened: d.opened + 1 }));
          }}
        >
          {t('account.delete.open')}
        </Button>
      </div>
      <DeleteDialog
        key={dialog.opened}
        open={dialog.open}
        username={username}
        onClose={() => {
          setDialog((d) => ({ ...d, open: false }));
        }}
      />
    </Section>
  );
}

/**
 * POST /api/v1/me/delete {password} → 204 with the cookie cleared (03 §12.3 #13); the server has ended every
 * session of the account by then (03 §7.7).
 * - 403 wrong_password shows under the field. 409 last_admin gets its own explanation: the only admin can't
 *   leave, and what to do about it.
 * - Afterwards a toast says that the account is gone and this tab's side of the sign-out runs (sessionEnded.ts).
 *   It ends with ['me'] null, so the guard sends the page to the login page like after any sign-out. The page
 *   doesn't navigate by itself: when the tab has a signaling connection, the server's session_revoked on it may
 *   already have made the guard do that, and two navigations racing would leave the address to chance.
 */
function DeleteDialog({ open, username, onClose }: { open: boolean; username: string; onClose: () => void }) {
  const { t } = useTranslation();
  const app = useApp();
  const formId = useId();
  const [password, setPassword] = useState('');
  const [lastAdmin, setLastAdmin] = useState(false);
  const { inputRef, error: wrongPassword, onError: onWrongPassword, clear: clearWrongPassword } = useWrongPassword();

  // The refs are taken out of the hooks' results: what is left is plain state, which rendering may read.
  const { formRef, ...form } = useSubmit<keyof DeleteSelfRequest, DeleteSelfRequest, undefined>({
    fields: ['password'],
    validate: (v) => fieldCodes({ password: v.password === '' ? FieldRequired : null }),
    request: (v) => api<undefined>('POST', '/api/v1/me/delete', v),
    onSuccess: async () => {
      app.ui.getState().toast({ kind: 'success', message: t('account.delete.done') });
      await sessionEndedByServer(app);
    },
    onError: (err) => {
      if (isApiError(err, CodeLastAdmin)) {
        setLastAdmin(true);
        return true;
      }
      return onWrongPassword(err);
    },
  });

  // The dialog's own first focus is its ✕ button; what the user came to do is type the password. This effect runs
  // after the Dialog's (a child's effects run first), so the dialog is open by now.
  useEffect(() => {
    if (open) inputRef.current?.focus();
  }, [open, inputRef]);

  return (
    <Dialog
      open={open}
      onClose={onClose}
      size="sm"
      title={t('account.delete.title')}
      // Not while the request runs: its answer decides whether there is still an account to come back to.
      dismissible={!form.busy}
      footer={
        <>
          <Button onClick={onClose} disabled={form.busy}>
            {t('common.cancel')}
          </Button>
          <Button type="submit" form={formId} variant="danger" loading={form.busy} disabled={form.waiting}>
            {t('account.delete.confirm')}
          </Button>
        </>
      }
    >
      <form
        id={formId}
        ref={formRef}
        className={styles.confirm}
        noValidate
        onSubmit={(e) => {
          e.preventDefault();
          clearWrongPassword();
          setLastAdmin(false);
          form.submit({ password });
        }}
      >
        <p>{t('account.delete.confirmBody', { username })}</p>
        {lastAdmin && <Notice>{t('account.delete.lastAdmin')}</Notice>}
        {form.formError !== null && <Notice spoken={form.formErrorSpoken}>{form.formError}</Notice>}
        <input type="text" name="username" autoComplete="username" value={username} readOnly hidden />
        <PasswordField
          ref={inputRef}
          label={t('auth.password')}
          name="password"
          autoComplete="current-password"
          required
          value={password}
          onChange={(e) => {
            setPassword(e.target.value);
          }}
          error={form.fieldErrors.password ?? wrongPassword}
        />
      </form>
    </Dialog>
  );
}
