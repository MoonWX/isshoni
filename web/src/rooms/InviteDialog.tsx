// "Invite friends" in the room's account menu (05 §11.2): a dialog that creates an invite and shows its link once,
// in setup/'s InviteLinkCard (05 §14.1), the card of the setup wizard's step 3.
//
// - Each opening creates one invite with the server's defaults (POST /api/v1/invites {}: 7 days, 10 uses unless an
//   admin changed them, 03 §12.4.5), as the wizard's step does. The account menu offers the entry only to users
//   who may create one (03 §7.9: admins, and members while membersCanInvite is on; nobody in the closed mode).
// - The link lives in this dialog's state for one opening (05 §20): never in the REST cache, never in storage.
//   Closing the dialog drops it: the server keeps only the token's hash, so the next opening makes a new invite.
//   An invite already created stays on the server; the user sees and revokes theirs under /admin/invites.
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
import type { CreateInviteRequest, CreateInviteResponse } from '../protocol/api.gen';
import { applyInvalidate } from '../protocol/invalidate';
import { queryKeys } from '../protocol/queryKeys';
import { api } from '../protocol/rest';
import { TopicAdminInvites } from '../protocol/types.gen';
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
          action={
            <Button
              size="sm"
              onClick={() => {
                createInvite();
              }}
            >
              {t('room.invite.retry')}
            </Button>
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
