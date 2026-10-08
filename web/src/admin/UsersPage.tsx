import { useMutation, useQuery } from '@tanstack/react-query';
import type { TFunction } from 'i18next';
import { useState } from 'react';
import { useTranslation } from 'react-i18next';
import { Link } from 'react-router';

import { useApp } from '../app/context';
import { useMe } from '../auth/useMe';
import { errorMessage } from '../lib/errorText';
import {
  CreatedViaCLI,
  CreatedViaInvite,
  CreatedViaSetup,
  CreatedViaSignup,
  RoleAdmin,
  UserStatusActive,
  UserStatusDisabled,
  UserStatusPending,
  type AdminUser,
} from '../protocol/api.gen';
import { TopicAdminUsers } from '../protocol/types.gen';
import { adminApi, refresh, usersQueryOptions } from './adminApi';
import { AdminPage, Loaded, Tag, useHeadingFocusAfter } from './AdminPage';
import { DataTable, Inline, Lines, Note, Row } from './DataTable';
import { RowMenu, type RowMenuItem } from './RowMenu';
import {
  DeleteUserDialog,
  DisableUserDialog,
  MakeAdminDialog,
  RemoveAdminDialog,
  RenameUserDialog,
  ResetLinkDialog,
  SignOutUserDialog,
  type UserDialogProps,
} from './UserDialogs';
import { When } from './When';

const DIALOGS = {
  rename: RenameUserDialog,
  makeAdmin: MakeAdminDialog,
  removeAdmin: RemoveAdminDialog,
  disable: DisableUserDialog,
  reset: ResetLinkDialog,
  signOut: SignOutUserDialog,
  delete: DeleteUserDialog,
} as const satisfies Record<string, (props: UserDialogProps) => unknown>;

type DialogKind = keyof typeof DIALOGS;

/**
 * /admin/users (05 §15.3): every account with its role, status, how it was created, when it was last seen and
 * whether it is online, and per row: rename, make or remove admin, disable or enable, a reset link, sign out
 * everywhere, delete.
 *
 * On the admin's own row, disable, delete and the reset link are missing: the server refuses them
 * (self_action_forbidden, 03 §7.11), and /account is the place for one's own account. A pending sign-up has no
 * actions here; it is answered on the Approvals page.
 */
export function UsersPage() {
  const { t } = useTranslation();
  const { queryClient, ui } = useApp();
  const me = useMe().data;
  const users = useQuery(usersQueryOptions());
  const [dialog, setDialog] = useState<{ kind: DialogKind; user: AdminUser } | null>(null);
  const rowGone = useHeadingFocusAfter(dialog !== null);

  /** Enabling an account needs no question: it only gives access back. */
  const enable = useMutation({
    mutationFn: (user: AdminUser) => adminApi.patchUser(user.id, { status: UserStatusActive }),
    onSuccess: (_res, user) => {
      ui.getState().toast({ kind: 'success', message: t('admin.users.enable.done', { username: user.username }) });
    },
    onError: (err) => {
      ui.getState().toast({ kind: 'error', message: errorMessage(err, t) });
    },
    onSettled: () => {
      refresh(queryClient, TopicAdminUsers);
    },
  });

  const menu = (user: AdminUser, self: boolean): RowMenuItem[] => {
    const open = (kind: DialogKind) => () => {
      setDialog({ kind, user });
    };
    const items: RowMenuItem[] = [{ label: t('admin.users.rename.action'), onSelect: open('rename') }];
    items.push(
      user.role === RoleAdmin
        ? { label: t('admin.users.removeAdmin.action'), onSelect: open('removeAdmin') }
        : { label: t('admin.users.makeAdmin.action'), onSelect: open('makeAdmin') },
    );
    if (user.status === UserStatusDisabled) {
      items.push({
        label: t('admin.users.enable.action'),
        onSelect: () => {
          enable.mutate(user);
        },
      });
    } else if (!self) {
      items.push({ label: t('admin.users.disable.action'), onSelect: open('disable') });
    }
    if (!self) items.push({ label: t('admin.users.reset.action'), onSelect: open('reset') });
    items.push({ label: t('admin.users.signOut.action'), onSelect: open('signOut') });
    if (!self) items.push({ label: t('admin.users.delete.action'), onSelect: open('delete'), danger: true });
    return items;
  };

  const ActiveDialog = dialog ? DIALOGS[dialog.kind] : null;

  return (
    <AdminPage title={t('admin.users.title')} lead={t('admin.users.lead')}>
      <Loaded query={users}>
        {(data) => (
          <DataTable
            caption={t('admin.users.title')}
            actions="corner"
            columns={[
              t('admin.users.col.user'),
              t('admin.users.col.role'),
              t('admin.users.col.status'),
              t('admin.users.col.joined'),
              t('admin.users.col.lastSeen'),
              '',
            ]}
          >
            {data.users.map((user) => {
              const self = me?.user.id === user.id;
              const pending = user.status === UserStatusPending;
              return (
                <Row
                  key={user.id}
                  cells={[
                    <Inline>
                      <span>{user.username}</span>
                      {self && <Tag tone="accent">{t('admin.users.you')}</Tag>}
                    </Inline>,
                    user.role === RoleAdmin ? t('admin.users.role.admin') : t('admin.users.role.user'),
                    <Status user={user} />,
                    <Joined user={user} />,
                    user.online ? (
                      <Tag tone="success">{t('admin.users.online')}</Tag>
                    ) : (
                      <When at={user.lastSeenAt} mode="relative" fallback={t('admin.never')} />
                    ),
                    pending ? null : (
                      <RowMenu
                        label={t('admin.users.actionsFor', { username: user.username })}
                        items={menu(user, self)}
                      />
                    ),
                  ]}
                />
              );
            })}
          </DataTable>
        )}
      </Loaded>
      {dialog && ActiveDialog && (
        <ActiveDialog
          user={dialog.user}
          self={me?.user.id === dialog.user.id}
          onClose={() => {
            setDialog(null);
          }}
          onRowGone={rowGone}
        />
      )}
    </AdminPage>
  );
}

function Status({ user }: { user: AdminUser }) {
  const { t } = useTranslation();
  if (user.status === UserStatusPending) {
    return (
      <Lines>
        <Tag tone="warning">{t('admin.users.status.pending')}</Tag>
        <Link to="/admin/approvals">{t('admin.users.review')}</Link>
      </Lines>
    );
  }
  return (
    <Lines>
      {user.status === UserStatusDisabled ? (
        <Tag tone="danger">{t('admin.users.status.disabled')}</Tag>
      ) : (
        <span>{t('admin.users.status.active')}</span>
      )}
      {user.resetPending && <Note>{t('admin.users.resetPending')}</Note>}
    </Lines>
  );
}

/** How the account came to be (03 §5 created_via), and when. */
function Joined({ user }: { user: AdminUser }) {
  const { t } = useTranslation();
  return (
    <Lines>
      <span>{createdVia(user, t)}</span>
      <Note>
        <When at={user.createdAt} mode="date" />
      </Note>
    </Lines>
  );
}

function createdVia(user: AdminUser, t: TFunction): string {
  switch (user.createdVia) {
    case CreatedViaSetup:
      return t('admin.users.via.setup');
    case CreatedViaInvite:
      return user.invitedBy
        ? t('admin.users.via.invite', { name: user.invitedBy.username })
        : t('admin.users.via.inviteUnknown');
    case CreatedViaSignup:
      return t('admin.users.via.signup');
    case CreatedViaCLI:
      return t('admin.users.via.cli');
    default:
      // A value from a newer server.
      return t('admin.unknown');
  }
}
