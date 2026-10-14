// "Invite friends" in the room's account menu (05 §11.2): a dialog that creates an invite and shows its link once,
// in setup/'s InviteLinkCard (05 §14.1), the card of the setup wizard's step 3.
//
// - Each opening creates one invite with the server's defaults (POST /api/v1/invites {}: 7 days, 10 uses unless an
//   admin changed them, 03 §12.4.5), as the wizard's step does. The account menu offers the entry only to users
//   who may create one (03 §7.9: admins, and members while membersCanInvite is on; nobody in the closed mode).
// - The link lives in this dialog's state for one opening (05 §20): never in the REST cache, never in storage.
//   Closing the dialog drops it: the server keeps only the token's hash, so the next opening makes a new invite.
//   An invite already created stays on the server; the user sees and revokes theirs under /admin/invites.
// - A failure says why there is no link. "Try again" follows it, except after a refusal no retry ends (403
//   registration_closed or forbidden): that one marks ['info'] or ['me'] stale instead, so the menu, which read an
//   older state, stops offering the entry.
// - The card's texts are in the lazy `setup` namespace, which rooms/index.ts imports for this dialog (05 §16.5).
//
// The menu keeps this dialog mounted and closes it with `open`, so that the browser gives the focus back to the
// menu's button (05 §16.6). The content is there only while the dialog shows: a new opening starts afresh.
import { useMutation } from '@tanstack/react-query';
import { useEffect, useRef, useState } from 'react';
import { useTranslation } from 'react-i18next';

import { useApp } from '../app/context';
import { Notice } from '../auth/Notice';
import { errorMessage } from '../lib/errorText';
import {
  CodeForbidden,
  CodeRegistrationClosed,
  type CreateInviteRequest,
  type CreateInviteResponse,
} from '../protocol/api.gen';
import { applyInvalidate } from '../protocol/invalidate';
import { queryKeys } from '../protocol/queryKeys';
import { ApiError, api } from '../protocol/rest';
import { TopicAdminInvites, TopicMe } from '../protocol/types.gen';
import { InviteLinkCard } from '../setup/InviteLinkCard';
import { Button } from '../ui/Button';
import { Dialog } from '../ui/Dialog';
import { Spinner } from '../ui/Spinner';
import styles from './InviteDialog.module.css';

export interface InviteDialogProps {
  open: boolean;
  onClose: () => void;
}

export function InviteDialog({ open, onClose }: InviteDialogProps) {
  const { t } = useTranslation();
  return (
    <Dialog
      open={open}
      onClose={onClose}
      title={t('room.invite.title')}
      footer={
        <Button variant="primary" onClick={onClose}>
          {t('room.invite.done')}
        </Button>
      }
    >
      {open && <NewInvite />}
    </Dialog>
  );
}

/**
 * The server refuses this user any invite (03 §7.9): the permission went, or the mode is closed. Trying again can't
 * change that, unlike a network error or a full allowance (limit_reached: revoking an invite makes room).
 */
function isRefused(err: unknown): boolean {
  return err instanceof ApiError && (err.code === CodeForbidden || err.code === CodeRegistrationClosed);
}

/** One opening of the dialog: creates the invite when it mounts, and shows its link, or why there is none. */
function NewInvite() {
  const { t } = useTranslation();
  const { queryClient } = useApp();
  /** The new invite and its link, for this opening only. */
  const [created, setCreated] = useState<CreateInviteResponse | null>(null);
  const card = useRef<HTMLDivElement>(null);

  const create = useMutation({
    // The link is not kept after the dialog closes.
    gcTime: 0,
    // Sent at once, also when the browser says it is offline: the request then fails and the dialog says so.
    networkMode: 'always',
    mutationFn: () => {
      // Every field left out: the server's invite defaults (03 §12.4.5).
      const body: CreateInviteRequest = {};
      return api<CreateInviteResponse>('POST', '/api/v1/invites', body);
    },
    // A refusal says the state that offered the entry was out of date: the mode was closed meanwhile (a member gets
    // no `admin.settings` invalidation, 01 §8.12), or the permission went. The menu reads the fresh one when it
    // opens next.
    onError: (err) => {
      if (!(err instanceof ApiError)) return;
      if (err.code === CodeRegistrationClosed) void queryClient.invalidateQueries({ queryKey: queryKeys.info });
      else if (err.code === CodeForbidden) void applyInvalidate(queryClient, { topics: [TopicMe] });
    },
    onSuccess: (res) => {
      setCreated(res);
      // What the server's `invalidate` message does too (05 §6.2), at once: /admin/invites lists the new one; an
      // admin's audit log has a new row.
      void applyInvalidate(queryClient, { topics: [TopicAdminInvites] });
      void queryClient.invalidateQueries({ queryKey: queryKeys.adminAudit });
    },
  });
  const createInvite = create.mutate;
  // Opening the dialog creates the invite. On a timer, so React's StrictMode double mount in development creates
  // one invite, not two.
  useEffect(() => {
    const id = setTimeout(() => {
      createInvite();
    }, 0);
    return () => {
      clearTimeout(id);
    };
  }, [createInvite]);

  // The link is why the user is here: once it arrives, focus goes to it, selected, ready to copy (as on
  // /admin/invites).
  const url = created?.url;
  useEffect(() => {
    if (url !== undefined) card.current?.querySelector('input')?.focus();
  }, [url]);

  return (
    <div className={styles.content}>
      <p className={styles.lead}>{t('room.invite.lead')}</p>
      {created !== null ? (
        <div ref={card}>
          <InviteLinkCard created={created} />
        </div>
      ) : create.isError ? (
        <Notice
          // Another try makes no invite while the server refuses this user one: Done is the way on.
          action={
            isRefused(create.error) ? undefined : (
              <Button
                size="sm"
                onClick={() => {
                  createInvite();
                }}
              >
                {t('room.invite.retry')}
              </Button>
            )
          }
        >
          {t('room.invite.failed', { message: errorMessage(create.error, t) })}
        </Notice>
      ) : (
        <p className={styles.creating}>
          <Spinner size="sm" label={null} />
          <span>{t('room.invite.creating')}</span>
        </p>
      )}
    </div>
  );
}
