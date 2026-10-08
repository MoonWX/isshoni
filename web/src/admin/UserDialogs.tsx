// The dialogs of the Users page (05 §15.3; 03 §7.10, §7.11): one per action that needs a question, a field or the
// admin's own password. Each sends its request, refreshes the list, says what happened in a toast, and closes.
//
// The rules they follow:
// - making someone an admin asks for the acting admin's password (currentPassword), so a stolen admin cookie alone
//   can't mint a second admin; wrong_password shows under the field;
// - a reset link for an admin asks for it too (the link would hand over that admin account). The link is shown
//   once, in the dialog;
// - last_admin, self_action_forbidden and the rest show above the buttons, from errors.<code>.
import { useEffect, useRef, useState } from 'react';
import { useTranslation } from 'react-i18next';

import { useApp } from '../app/context';
import { checkUsername } from '../auth/formErrors';
import { PasswordField } from '../auth/PasswordField';
import { useAccountRules } from '../auth/useSubmit';
import { formatDateTime, parseWireTime } from '../lib/time';
import {
  CodeUsernameTaken,
  CodeWrongPassword,
  FieldRequired,
  RoleAdmin,
  RoleUser,
  UserStatusDisabled,
  type AdminUser,
  type PasswordResetRequest,
  type ResetLink,
} from '../protocol/api.gen';
import { TopicAdminUsers, TopicMe } from '../protocol/types.gen';
import { Button } from '../ui/Button';
import { Dialog } from '../ui/Dialog';
import { TextField } from '../ui/Field';
import { ActionDialog, DialogText } from './ActionDialog';
import { adminApi, refresh } from './adminApi';
import { fieldCodes } from './formProblem';
import { OneTimeLink } from './OneTimeLink';
import { useAction } from './useAction';

export interface UserDialogProps {
  user: AdminUser;
  /** The row is the admin's own account. */
  self: boolean;
  onClose: () => void;
  /** The action removed the row (the account was deleted): focus can't go back to its menu. */
  onRowGone: () => void;
}

/** What every user dialog does after its request went through. */
function useDone({ self, onClose }: Pick<UserDialogProps, 'self' | 'onClose'>): (message: string) => void {
  const { queryClient, ui } = useApp();
  return (message) => {
    // The admin's own name and role are in ['me'] too.
    if (self) refresh(queryClient, TopicAdminUsers, TopicMe);
    else refresh(queryClient, TopicAdminUsers);
    ui.getState().toast({ kind: 'success', message });
    onClose();
  };
}

/** The admin's own password, for the two actions that need it (03 §7.10, §7.11). */
function YourPassword({ value, onChange, error }: { value: string; onChange: (v: string) => void; error?: string }) {
  const { t } = useTranslation();
  return (
    <PasswordField
      label={t('admin.yourPassword')}
      name="currentPassword"
      autoComplete="current-password"
      required
      value={value}
      onChange={(e) => {
        onChange(e.target.value);
      }}
      error={error}
    />
  );
}

export function RenameUserDialog({ user, self, onClose }: UserDialogProps) {
  const { t } = useTranslation();
  const rules = useAccountRules();
  const done = useDone({ self, onClose });
  const [username, setUsername] = useState(user.username);
  const action = useAction({
    fields: ['username'],
    validate: (v: string) => fieldCodes({ username: checkUsername(v, rules) }),
    request: (v) => adminApi.patchUser(user.id, { username: v.trim() }),
    onSuccess: (res) => {
      done(t('admin.users.rename.done', { from: user.username, to: res.user.username }));
    },
    codeFields: { [CodeUsernameTaken]: 'username' },
  });
  return (
    <ActionDialog
      title={t('admin.users.rename.title', { username: user.username })}
      size="md"
      action={action}
      onClose={onClose}
      onSubmit={() => {
        // Nothing to change: the same name is not a request.
        if (username.trim() === user.username) onClose();
        else action.submit(username);
      }}
      submitLabel={t('admin.users.rename.submit')}
    >
      <TextField
        label={t('auth.username')}
        name="username"
        autoComplete="off"
        autoCapitalize="none"
        autoCorrect="off"
        spellCheck={false}
        required
        value={username}
        onChange={(e) => {
          setUsername(e.target.value);
        }}
        hint={t('auth.usernameHint', { min: rules.usernameMinLength, max: rules.usernameMaxLength })}
        error={action.fieldErrors.username}
      />
    </ActionDialog>
  );
}

export function MakeAdminDialog({ user, self, onClose }: UserDialogProps) {
  const { t } = useTranslation();
  const done = useDone({ self, onClose });
  const [password, setPassword] = useState('');
  const action = useAction({
    fields: ['currentPassword'],
    validate: (v: string) => fieldCodes({ currentPassword: v === '' ? FieldRequired : null }),
    request: (v) => adminApi.patchUser(user.id, { role: RoleAdmin, currentPassword: v }),
    onSuccess: () => {
      done(t('admin.users.makeAdmin.done', { username: user.username }));
    },
    codeFields: { [CodeWrongPassword]: 'currentPassword' },
  });
  return (
    <ActionDialog
      title={t('admin.users.makeAdmin.title', { username: user.username })}
      size="md"
      action={action}
      onClose={onClose}
      onSubmit={() => {
        action.submit(password);
      }}
      submitLabel={t('admin.users.makeAdmin.submit')}
    >
      <DialogText>{t('admin.users.makeAdmin.body')}</DialogText>
      <YourPassword value={password} onChange={setPassword} error={action.fieldErrors.currentPassword} />
    </ActionDialog>
  );
}

export function RemoveAdminDialog({ user, self, onClose }: UserDialogProps) {
  const { t } = useTranslation();
  const done = useDone({ self, onClose });
  const action = useAction({
    request: () => adminApi.patchUser(user.id, { role: RoleUser }),
    onSuccess: () => {
      done(t('admin.users.removeAdmin.done', { username: user.username }));
    },
  });
  return (
    <ActionDialog
      title={t('admin.users.removeAdmin.title', { username: user.username })}
      action={action}
      onClose={onClose}
      onSubmit={() => {
        action.submit(undefined);
      }}
      submitLabel={t('admin.users.removeAdmin.submit')}
      danger
    >
      <DialogText>
        {self ? t('admin.users.removeAdmin.bodySelf') : t('admin.users.removeAdmin.body', { username: user.username })}
      </DialogText>
    </ActionDialog>
  );
}

export function DisableUserDialog({ user, self, onClose }: UserDialogProps) {
  const { t } = useTranslation();
  const done = useDone({ self, onClose });
  const action = useAction({
    request: () => adminApi.patchUser(user.id, { status: UserStatusDisabled }),
    onSuccess: () => {
      done(t('admin.users.disable.done', { username: user.username }));
    },
  });
  return (
    <ActionDialog
      title={t('admin.users.disable.title', { username: user.username })}
      action={action}
      onClose={onClose}
      onSubmit={() => {
        action.submit(undefined);
      }}
      submitLabel={t('admin.users.disable.submit')}
      danger
    >
      <DialogText>{t('admin.users.disable.body', { username: user.username })}</DialogText>
    </ActionDialog>
  );
}

export function SignOutUserDialog({ user, self, onClose }: UserDialogProps) {
  const { t } = useTranslation();
  const done = useDone({ self, onClose });
  const action = useAction({
    request: () => adminApi.signOutUser(user.id),
    onSuccess: (res) => {
      const count = res.sessions + res.devices;
      done(
        count === 0
          ? t('admin.users.signOut.doneNone', { username: user.username })
          : t('admin.users.signOut.done', { username: user.username, count }),
      );
    },
  });
  return (
    <ActionDialog
      title={t('admin.users.signOut.title', { username: user.username })}
      action={action}
      onClose={onClose}
      onSubmit={() => {
        action.submit(undefined);
      }}
      submitLabel={t('admin.users.signOut.submit')}
    >
      <DialogText>{t('admin.users.signOut.body', { username: user.username })}</DialogText>
      {self && <DialogText>{t('admin.users.signOut.bodySelf')}</DialogText>}
    </ActionDialog>
  );
}

export function DeleteUserDialog({ user, self, onClose, onRowGone }: UserDialogProps) {
  const { t } = useTranslation();
  const done = useDone({ self, onClose });
  const action = useAction({
    request: () => adminApi.deleteUser(user.id),
    onSuccess: () => {
      onRowGone();
      done(t('admin.users.delete.done', { username: user.username }));
    },
  });
  return (
    <ActionDialog
      title={t('admin.users.delete.title', { username: user.username })}
      action={action}
      onClose={onClose}
      onSubmit={() => {
        action.submit(undefined);
      }}
      submitLabel={t('admin.users.delete.submit')}
      danger
    >
      <DialogText>{t('admin.users.delete.body')}</DialogText>
    </ActionDialog>
  );
}

/**
 * "Create reset link" (03 §7.10): the question first, with the admin's password when the target is an admin, then
 * the link, once. Closing the dialog forgets it.
 */
export function ResetLinkDialog({ user, onClose }: UserDialogProps) {
  const { t, i18n } = useTranslation();
  const { queryClient } = useApp();
  const targetIsAdmin = user.role === RoleAdmin;
  const [password, setPassword] = useState('');
  const [link, setLink] = useState<ResetLink | null>(null);
  const action = useAction({
    fields: ['currentPassword'],
    validate: (v: string) => fieldCodes({ currentPassword: targetIsAdmin && v === '' ? FieldRequired : null }),
    request: (v) => {
      const body: PasswordResetRequest = targetIsAdmin ? { currentPassword: v } : {};
      return adminApi.resetPassword(user.id, body);
    },
    onSuccess: (res) => {
      // The user's sessions are gone and a reset is pending: the list changed.
      refresh(queryClient, TopicAdminUsers);
      setLink(res);
    },
    codeFields: { [CodeWrongPassword]: 'currentPassword' },
  });

  // The link is why the dialog is open: focus goes to it, selected, ready to copy.
  const linkBox = useRef<HTMLDivElement>(null);
  useEffect(() => {
    if (link) linkBox.current?.querySelector('input')?.focus();
  }, [link]);

  if (link) {
    const expires = parseWireTime(link.expiresAt);
    return (
      <Dialog
        open
        onClose={onClose}
        // Only "Done" closes it: a stray Esc or click beside the dialog would lose a link that can't be shown again,
        // for a user whose old password is already gone.
        dismissible={false}
        title={t('admin.users.reset.linkTitle', { username: user.username })}
        footer={
          <Button variant="primary" onClick={onClose}>
            {t('admin.link.done')}
          </Button>
        }
      >
        <div ref={linkBox}>
          <OneTimeLink
            label={t('admin.users.reset.linkLabel')}
            url={link.url}
            hint={
              expires
                ? t('admin.users.reset.linkHint', {
                    username: user.username,
                    expires: formatDateTime(expires, i18n.language),
                  })
                : t('admin.users.reset.linkHintNoExpiry', { username: user.username })
            }
          />
        </div>
      </Dialog>
    );
  }

  return (
    <ActionDialog
      title={t('admin.users.reset.title', { username: user.username })}
      size="md"
      action={action}
      onClose={onClose}
      onSubmit={() => {
        action.submit(password);
      }}
      submitLabel={t('admin.users.reset.submit')}
    >
      <DialogText>{t('admin.users.reset.body', { username: user.username })}</DialogText>
      {targetIsAdmin && (
        <>
          <DialogText>{t('admin.users.reset.bodyAdmin', { username: user.username })}</DialogText>
          <YourPassword value={password} onChange={setPassword} error={action.fieldErrors.currentPassword} />
        </>
      )}
    </ActionDialog>
  );
}
