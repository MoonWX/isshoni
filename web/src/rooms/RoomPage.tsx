// The room page, /r/:roomId (05 §5, §11.2): the app's main page. It makes the route's room the session's desired
// room (useRoomSession) and shows the session: the connection banner, the header (name, switcher, people, share
// controls, account menu), and the stage with its tiles (the viewer). It only reads the stores and calls the
// controllers (05 §3); leaving the page leaves nothing: the session goes on, and InRoomBar shows it elsewhere.
//
// What plugs the room, the viewer and the sharer together (05 §11.1, §12, §13.1):
// - the runtime made the viewer and keeps it in step with room.state (runtime.ts, connectViewer.ts); the page hands
//   it to the stage, with the local preview of this page's share;
// - the Share buttons publish through session.startShare, and know about this user's share on another tab or
//   device (findShareElsewhere), which the share sheet offers to stop;
// - "Test my connection" (the connection banner, the stage's media banner) opens the connection test.
//
// The stage, the Share button and the connection test are lazy chunks (lazyMedia.ts, ConnTestDialog.tsx). The page
// asks for the first two as it renders, with placeholders of their shape, so the header is there at once.
//
// Still to come, each with its slice: the in-app browser banner above the page and the notifications card in the
// empty state (05 §16.3); the share panel, which takes over the Stop button here (05 §13.7); ?focus=<shareId>
// (05 §12.2).
import { CircleStop, DoorClosed, MonitorUp } from 'lucide-react';
import { Suspense, useCallback, useMemo, useState } from 'react';
import { useTranslation } from 'react-i18next';
import { useParams } from 'react-router';

import { useApp } from '../app/context';
import { isAdmin } from '../app/guards';
import { useMeQuery } from '../app/me';
import { errorMessage } from '../lib/errorText';
import type { ProtocolError } from '../protocol/errors';
import { MessageTypeShareStop } from '../protocol/types.gen';
import { findShareElsewhere } from '../share/elsewhere';
import type { ShareElsewhere } from '../share/ShareSheet';
import type { StartShare } from '../share/shareStore';
import { Button } from '../ui/Button';
import { Spinner } from '../ui/Spinner';
import { ConnectionBanner } from './ConnectionBanner';
import { ConnTestDialog } from './ConnTestDialog';
import { useLocalShare, useRoom, useRoomSession } from './hooks';
import { RoomStage, ShareButton } from './lazyMedia';
import { PeoplePanel } from './PeoplePanel';
import { RoomHeader } from './RoomHeader';
import styles from './RoomPage.module.css';
import { roomOf, useRooms } from './roomsQuery';
import { liveShareCount, roomTitle, useDocumentTitle } from './roomTitle';

export function RoomPage() {
  const { t } = useTranslation();
  const { roomId } = useParams();
  const { platform, ui } = useApp();
  const { session, signal, viewer } = useRoomSession(roomId);

  const sessionRoomId = useRoom((s) => s.roomId);
  const joinState = useRoom((s) => s.joinState);
  const joinError = useRoom((s) => s.joinError);
  const joinedRoom = useRoom((s) => s.room);
  const roomState = useRoom((s) => s.state);
  const userId = useRoom((s) => s.userId);
  const connectionId = useRoom((s) => s.connectionId);
  // The session takes the route's room in an effect. For the one render before that, after a switch, the store
  // still describes the room of before: nothing of it is shown under the new room's name.
  const current = roomId !== undefined && sessionRoomId === roomId;
  const state = current ? roomState : null;
  const failed = current && joinState === 'failed';

  const rooms = useRooms();
  const me = useMeQuery();
  const share = useLocalShare(session);
  const sharing = share !== null;
  const [peopleOpen, setPeopleOpen] = useState(false);
  const [testOpen, setTestOpen] = useState(false);
  const [stopping, setStopping] = useState(false);

  // The people panel shows room.state: it goes when the snapshot does, and stays closed when one comes back.
  if (peopleOpen && state === null) setPeopleOpen(false);

  // The room list has the name of today (a rename refetches it); the join's answer has it before the list loaded.
  const name = roomOf(rooms.data, roomId)?.name ?? (current ? joinedRoom?.name : undefined);
  useDocumentTitle(roomTitle(t, { room: name ?? t('room.header.unnamed'), live: liveShareCount(state), sharing }));

  const startShare = useCallback<StartShare>((src, opts) => session.startShare(src, opts), [session]);

  // This user's share from another tab or device (05 §13.1): the share sheet offers to stop it.
  const elsewhereId =
    state !== null && userId !== null && connectionId !== null
      ? (findShareElsewhere(state.shares, { userId, connectionId })?.id ?? null)
      : null;
  const shareElsewhere = useMemo<ShareElsewhere | null>(
    () =>
      elsewhereId === null ? null : { onStop: () => signal.request(MessageTypeShareStop, { shareId: elsewhereId }) },
    [elsewhereId, signal],
  );

  // The preview's key follows the share's id, which a re-publish changes (01 §10.6; useLocalShare re-renders then).
  const shareId = share?.shareId;
  const preview = share?.preview;
  const localPreviews = useMemo(
    () => (shareId !== undefined && preview != null ? { [shareId]: preview } : {}),
    [shareId, preview],
  );

  const openTest = useCallback(() => {
    setTestOpen(true);
  }, []);

  const stopShare = (): void => {
    setStopping(true);
    session
      .stopShare()
      .catch((err: unknown) => {
        ui.getState().toast({ kind: 'error', message: errorMessage(err, t) });
      })
      .finally(() => {
        setStopping(false);
      });
  };

  return (
    <div className={styles.page}>
      <ConnectionBanner onTestConnection={openTest} />
      <RoomHeader
        roomId={roomId ?? ''}
        name={name}
        peopleCount={state?.participants.length}
        onOpenPeople={() => {
          setPeopleOpen(true);
        }}
        rooms={rooms.data}
        me={me.data}
        sharing={sharing}
        onOpenRooms={() => {
          void rooms.refetch();
        }}
      >
        {platform.sharing !== null && !failed && (
          // The same button until the chunk is there, so the header doesn't move. No button for a room that
          // refused the user: there is nothing to share into.
          <Suspense
            fallback={
              <Button variant="primary" icon={<MonitorUp />} disabled>
                {t('room.share.start')}
              </Button>
            }
          >
            <ShareButton onStart={startShare} elsewhere={shareElsewhere} label={t('room.share.start')} />
          </Suspense>
        )}
        {sharing && (
          <Button variant="danger" icon={<CircleStop />} loading={stopping} onClick={stopShare}>
            {t('room.share.stop')}
          </Button>
        )}
      </RoomHeader>
      <main className={styles.main}>
        {failed ? (
          <JoinFailed
            error={joinError}
            onRetry={() => {
              // A refusal is in the store again (joinState failed): nothing to catch here.
              session.join(roomId).catch(() => undefined);
            }}
          />
        ) : (
          <Suspense fallback={<div className={styles.stagePlaceholder} />}>
            <RoomStage
              viewer={viewer}
              localPreviews={localPreviews}
              placeholder={state === null ? <Joining /> : undefined}
              onStartShare={startShare}
              shareElsewhere={shareElsewhere}
              onTestConnection={openTest}
            />
          </Suspense>
        )}
      </main>
      {peopleOpen && state !== null && (
        <PeoplePanel
          open
          onClose={() => {
            setPeopleOpen(false);
          }}
          participants={state.participants}
          shares={state.shares}
          selfUserId={userId}
          selfIsAdmin={me.data != null && isAdmin(me.data)}
          onWatch={(id) => {
            // A pick, like a click on the tile: it holds until that share ends (05 §12.2).
            viewer.store.getState().focusShare(id);
          }}
        />
      )}
      {testOpen && (
        <ConnTestDialog
          open
          onClose={() => {
            setTestOpen(false);
          }}
        />
      )}
    </div>
  );
}

/** On the empty stage until the first room.state of the room is there. */
function Joining() {
  const { t } = useTranslation();
  return (
    <>
      <Spinner size="lg" label={null} />
      <p role="status">{t('room.join.joining')}</p>
    </>
  );
}

/**
 * The server refused the room (room_full, forbidden, …; 05 §6.3) and there was no other room to go to. The session
 * doesn't try again by itself (roomStore), so the page offers to.
 */
function JoinFailed({ error, onRetry }: { error: ProtocolError | null; onRetry: () => void }) {
  const { t } = useTranslation();
  return (
    <section className={styles.failed}>
      <DoorClosed className={styles.failedIcon} aria-hidden="true" />
      <h2 className={styles.failedTitle}>{t('room.join.failed')}</h2>
      {error !== null && <p role="alert">{errorMessage(error, t)}</p>}
      <Button variant="primary" onClick={onRetry}>
        {t('room.join.retry')}
      </Button>
    </section>
  );
}
