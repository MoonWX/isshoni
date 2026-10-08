import { useMutation, useQuery } from '@tanstack/react-query';
import { useEffect, useRef, useState } from 'react';
import { Trans, useTranslation } from 'react-i18next';
import { Link } from 'react-router';

import { useApp } from '../app/context';
import { useInfo } from '../app/info';
import { focusPageHeading } from '../app/layouts/RootLayout';
import { Notice } from '../auth/Notice';
import { errorMessage } from '../lib/errorText';
import { RegistrationModeApproval, type ApprovalsResponse, type PendingUser } from '../protocol/api.gen';
import { TopicAdminApprovals, TopicAdminUsers } from '../protocol/types.gen';
import { Button } from '../ui/Button';
import { ActionDialog, DialogText } from './ActionDialog';
import { adminApi, approvalsQueryOptions, refresh } from './adminApi';
import { AdminPage, Empty, Loaded } from './AdminPage';
import styles from './ApprovalsPage.module.css';
import { DataTable, Row } from './DataTable';
import { useAction } from './useAction';
import { When } from './When';

interface Decision {
  readonly user: PendingUser;
  readonly approve: boolean;
  /** The row's place in the list, for where focus goes once the row is gone. */
  readonly index: number;
}

/**
 * /admin/approvals (05 §15.3, 03 §7.9): the sign-ups that wait for an admin in approval mode, each with Approve
 * and Reject. The sign-up IP is shown here (and in the audit log) so a flood from one place is easy to spot.
 *
 * "Reject all" is for that flood: it asks first, then empties the queue with one request and says how many it
 * rejected. A real sign-up caught in it loses nothing but a minute: its username is free again.
 */
export function ApprovalsPage() {
  const { t } = useTranslation();
  const { queryClient, ui } = useApp();
  const info = useInfo().data;
  const approvals = useQuery(approvalsQueryOptions());
  const { queryKey } = approvalsQueryOptions();
  const [confirmAll, setConfirmAll] = useState(false);
  /** The count of the last "Reject all", shown until the next decision. */
  const [rejectedAll, setRejectedAll] = useState<number | null>(null);
  const list = useRef<HTMLDivElement>(null);
  /**
   * Set when a decision removes rows: where focus goes once they are gone. The place in the list, and which of the
   * two buttons was used, so that focus never moves from a Reject button to an Approve button.
   */
  const focusAfter = useRef<{ index: number; approve: boolean } | null>(null);

  const stale = (): void => {
    // An approved sign-up is a user now, and the navigation's badge counts the queue (Me.badges).
    refresh(queryClient, TopicAdminApprovals, TopicAdminUsers);
  };

  const decide = useMutation({
    mutationFn: ({ user, approve }: Decision) => (approve ? adminApi.approve(user.id) : adminApi.reject(user.id)),
    onSuccess: (_res, { user, approve, index }) => {
      setRejectedAll(null);
      focusAfter.current = { index, approve };
      queryClient.setQueryData<ApprovalsResponse>(queryKey, (old) =>
        old ? { ...old, pending: old.pending.filter((p) => p.id !== user.id) } : old,
      );
      ui.getState().toast({
        kind: 'success',
        message: approve
          ? t('admin.approvals.approved', { username: user.username })
          : t('admin.approvals.rejected', { username: user.username }),
      });
    },
    onError: (err) => {
      // Most likely another admin answered it first (404): the refreshed list shows what is left.
      ui.getState().toast({ kind: 'error', message: errorMessage(err, t) });
    },
    onSettled: stale,
  });

  const rejectAll = useAction({
    request: () => adminApi.rejectAll(),
    onSuccess: (res) => {
      // The queue is empty now, so this ends on the page's heading.
      focusAfter.current = { index: 0, approve: false };
      queryClient.setQueryData<ApprovalsResponse>(queryKey, (old) => (old ? { ...old, pending: [] } : old));
      stale();
      setRejectedAll(res.rejected);
      setConfirmAll(false);
    },
  });

  const count = approvals.data?.pending.length ?? 0;
  // The button that was used is gone with its row. Focus goes to the same button of the row that moved up (or of
  // the last row), so a queue can be worked through from the keyboard: Enter on Reject, again and again, rejects
  // one sign-up after the other and never approves one. With no row left, focus goes to the page's heading.
  useEffect(() => {
    const want = focusAfter.current;
    if (want === null) return;
    focusAfter.current = null;
    // The row buttons carry data-decision="approve" or "reject".
    const kind = want.approve ? 'approve' : 'reject';
    const buttons = list.current?.querySelectorAll<HTMLElement>(`[data-decision="${kind}"]`) ?? [];
    const next = buttons[Math.min(want.index, buttons.length - 1)];
    if (next) next.focus();
    else focusPageHeading();
    // Every change of the list, also one that leaves its length as it was (a new sign-up arrived meanwhile).
  }, [approvals.dataUpdatedAt]);

  return (
    <AdminPage
      title={t('admin.approvals.title')}
      lead={t('admin.approvals.lead')}
      actions={
        count > 0 ? (
          <Button
            onClick={() => {
              rejectAll.clear();
              setConfirmAll(true);
            }}
          >
            {t('admin.approvals.rejectAll.action')}
          </Button>
        ) : undefined
      }
    >
      {rejectedAll !== null && (
        <Notice tone="info">{t('admin.approvals.rejectAll.done', { count: rejectedAll })}</Notice>
      )}
      <Loaded query={approvals}>
        {(data) =>
          data.pending.length === 0 ? (
            <Empty>
              {info?.registration === RegistrationModeApproval ? (
                t('admin.approvals.empty')
              ) : (
                <Trans i18nKey="admin.approvals.emptyOtherMode" components={{ a: <Link to="/admin/settings" /> }} />
              )}
            </Empty>
          ) : (
            <div ref={list} className={styles.list}>
              <DataTable
                caption={t('admin.approvals.title')}
                columns={[
                  t('admin.approvals.col.user'),
                  t('admin.approvals.col.requested'),
                  t('admin.approvals.col.ip'),
                  '',
                ]}
              >
                {data.pending.map((user, index) => {
                  const deciding = decide.isPending && decide.variables.user.id === user.id;
                  return (
                    <Row
                      key={user.id}
                      cells={[
                        user.username,
                        <When at={user.requestedAt} />,
                        <span className={styles.ip}>{user.ip}</span>,
                        <>
                          <Button
                            size="sm"
                            variant="primary"
                            data-decision="approve"
                            aria-label={t('admin.approvals.approveName', { username: user.username })}
                            loading={deciding && decide.variables.approve}
                            disabled={decide.isPending && !deciding}
                            onClick={() => {
                              if (!decide.isPending) decide.mutate({ user, approve: true, index });
                            }}
                          >
                            {t('admin.approvals.approve')}
                          </Button>
                          <Button
                            size="sm"
                            data-decision="reject"
                            aria-label={t('admin.approvals.rejectName', { username: user.username })}
                            loading={deciding && !decide.variables.approve}
                            disabled={decide.isPending && !deciding}
                            onClick={() => {
                              if (!decide.isPending) decide.mutate({ user, approve: false, index });
                            }}
                          >
                            {t('admin.approvals.reject')}
                          </Button>
                        </>,
                      ]}
                    />
                  );
                })}
              </DataTable>
            </div>
          )
        }
      </Loaded>
      {confirmAll && (
        <ActionDialog
          title={t('admin.approvals.rejectAll.title')}
          action={rejectAll}
          onClose={() => {
            setConfirmAll(false);
          }}
          onSubmit={() => {
            rejectAll.submit(undefined);
          }}
          submitLabel={t('admin.approvals.rejectAll.submit')}
          danger
        >
          <DialogText>{t('admin.approvals.rejectAll.body')}</DialogText>
        </ActionDialog>
      )}
    </AdminPage>
  );
}
