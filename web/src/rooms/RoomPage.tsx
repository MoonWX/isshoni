// The room page, /r/:roomId (05 §5, §11.2): the app's main page. It makes the route's room the session's desired
// room (useRoomSession) and shows the session: the connection banner, the header (name, switcher, people, share
// controls, account menu), and the stage with its tiles (the viewer). It only reads the stores and calls the
// controllers (05 §3); leaving the page leaves nothing: the session goes on, and InRoomBar shows it elsewhere.
//
// What plugs the room, the viewer and the sharer together (05 §11.1, §12, §13.1):
// - the runtime made the viewer and keeps it in step with room.state (runtime.ts, connectViewer.ts); the page hands
//   it to the stage, with the local preview of this page's share, and to the header's "Tap to unmute" pill
//   (05 §10.3);
// - the Share buttons publish through session.startShare, and know about this user's share on another tab or
//   device (findShareElsewhere), which the share sheet offers to stop;
// - the share panel under the stage (05 §13.7) shows this page's share, with who watches it (room.state), and its
//   Stop is session.stopShare;
// - "Test my connection" (the account menu, the connection banner, the stage's media banner, the share panel)
//   opens the connection test (05 §14.2).
//
// The stage, the Share button, the share panel, the pill and the connection test are lazy chunks (lazyMedia.ts,
// ConnTestDialog.tsx). The page asks for the media chunk as it renders, with placeholders of the stage's and the
// button's shape, so the header is there at once.
//
// The people panel and the connection test stay mounted and are closed through their `open` prop: a native
// <dialog> gives the focus back to the control that opened it only when it is closed while still in the page
// (05 §16.6).
//
// ?focus=<shareId> (05 §12.2) is the viewer's: its layout reads the link and picks the share. ?conntest=1 opens the
// connection test and is dropped from the URL (docs/m2/03 §7.6: the desktop app's "Test connection" opens the room
// with it in the browser).
//
// Share is offered by capabilities().canShare (docs/m2/03 §7.6, useShareAvailability), read asynchronously. In the
// desktop app (platform.sharing.mode 'native', docs/m2/03 §10) the Share button and the panel are share/native/'s
// (rooms/lazyNative.ts); the app's own share, which its C2 publishes, counts as this page's for "already sharing
// from another tab or device" (platform.sharing.localConnectionIds()), and that sheet's Stop it asks C2 to stop the
// other share (platform.sharing.stopOther), since this page's connection is a viewer.
//
// Web Push (05 §16.3, README S77): the in-app browser banner above the page (auth/InAppBrowserBanner.tsx, nothing
// in a real browser) and account/'s push prompts (RoomPushSlots.tsx: the push state, the iOS Home Screen sheet,
// notification clicks; the notifications card is the stage's, in the empty state).
import { DoorClosed, MonitorUp } from 'lucide-react';
import { Suspense, useCallback, useEffect, useMemo, useState } from 'react';
import { useTranslation } from 'react-i18next';
import { useParams, useSearchParams } from 'react-router';

import { RoomPushPrompts } from '../account/RoomPushSlots';
import { useApp } from '../app/context';
import { useMeQuery } from '../app/me';
import { InAppBrowserBanner } from '../auth/InAppBrowserBanner';
import { errorMessage } from '../lib/errorText';
import type { ProtocolError } from '../protocol/errors';
import { MessageTypeShareStop } from '../protocol/types.gen';
import { findShareElsewhere } from '../share/elsewhere';
import type { ShareElsewhere } from '../share/ShareSheet';
import type { StartShare } from '../share/shareStore';
import { shareWatchers } from '../share/watchers';
import { Button } from '../ui/Button';
import { Spinner } from '../ui/Spinner';
import { ConnectionBanner } from './ConnectionBanner';
import { ConnTestDialog } from './ConnTestDialog';
import { useLocalShare, useRoom, useRoomSession, useShareAvailability } from './hooks';
import { RoomStage, ShareButton, SharePanel, TapToStartPill } from './lazyMedia';
import { NativeShareButton, NativeSharePanel } from './lazyNative';
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
  const availability = useShareAvailability(platform);
  const native = platform.sharing?.mode === 'native';

  // ?conntest=1 (docs/m2/03 §7.6): the connection test, and the parameter goes from the URL.
  const [search, setSearch] = useSearchParams();
  const conntest = search.get('conntest') === '1';
  if (conntest && !testOpen) setTestOpen(true);
  useEffect(() => {
    if (!conntest) return;
    setSearch(
      (prev) => {
        const next = new URLSearchParams(prev);
        next.delete('conntest');
        return next;
      },
      { replace: true },
    );
  }, [conntest, setSearch]);

  // The people panel shows room.state: it goes when the snapshot does, and stays closed when one comes back.
  if (peopleOpen && state === null) setPeopleOpen(false);

  // The room list has the name of today (a rename refetches it); the join's answer has it before the list loaded.
  const name = roomOf(rooms.data, roomId)?.name ?? (current ? joinedRoom?.name : undefined);
  useDocumentTitle(roomTitle(t, { room: name ?? t('room.header.unnamed'), live: liveShareCount(state), sharing }));

  const startShare = useCallback<StartShare>((src, opts) => session.startShare(src, opts), [session]);

  // This user's share from another tab or device (05 §13.1): the share sheet offers to stop it. The platform's other
  // local connections (the desktop app's C2) are this page's own (docs/m2/03 §10.2).
  const localIds = platform.sharing?.localConnectionIds?.() ?? [];
  const elsewhereId =
    state !== null && userId !== null && connectionId !== null
      ? (findShareElsewhere(state.shares, { userId, connectionIds: [connectionId, ...localIds] })?.id ?? null)
      : null;
  const sharingProvider = platform.sharing;
  const stateRoomId = state?.roomId ?? null;
  const shareElsewhere = useMemo<ShareElsewhere | null>(() => {
    if (elsewhereId === null) return null;
    const stopOther = sharingProvider?.stopOther?.bind(sharingProvider);
    // The desktop's page is a viewer: C2 sends the share.stop (Share.StopOther).
    if (stopOther !== undefined && stateRoomId !== null) return { onStop: () => stopOther(stateRoomId, elsewhereId) };
    return { onStop: () => signal.request(MessageTypeShareStop, { shareId: elsewhereId }) };
  }, [elsewhereId, signal, sharingProvider, stateRoomId]);

  // The preview's key follows the share's id, which a re-publish changes (01 §10.6; useLocalShare re-renders then).
  const shareId = share?.shareId;
  const preview = share?.preview;
  const localPreviews = useMemo(
    () => (shareId !== undefined && preview != null ? { [shareId]: preview } : {}),
    [shareId, preview],
  );

  // Who watches this page's share (05 §13.7): the names of room.state, new only with a new snapshot or share.
  const watchers = useMemo(() => shareWatchers(state, shareId), [state, shareId]);

  const openTest = useCallback(() => {
    setTestOpen(true);
  }, []);

  // The share panel's Stop. The panel shows the share stopping (shareStore); a stop that failed is said here.
  const stopShare = useCallback(
    () =>
      session.stopShare().catch((err: unknown) => {
        ui.getState().toast({ kind: 'error', message: errorMessage(err, t) });
      }),
    [session, ui, t],
  );

  return (
    <div className={styles.page}>
      <div className={styles.inAppBanner}>
        <InAppBrowserBanner />
      </div>
      <RoomPushPrompts />
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
        onTestConnection={openTest}
      >
        {/* Nothing until the browser refuses to play, so no placeholder either. */}
        <Suspense fallback={null}>
          <TapToStartPill viewer={viewer} />
        </Suspense>
        {platform.sharing !== null && !failed && availability?.canShare !== false && (
          // The same button until the chunk is there and the platform said it can share, so the header doesn't
          // move. No button for a room that refused the user: there is nothing to share into.
          <Suspense
            fallback={
              <Button variant="primary" icon={<MonitorUp />} disabled>
                {t('room.share.start')}
              </Button>
            }
          >
            {availability === null ? (
              <Button variant="primary" icon={<MonitorUp />} disabled>
                {t('room.share.start')}
              </Button>
            ) : native ? (
              <NativeShareButton onStart={startShare} elsewhere={shareElsewhere} label={t('room.share.start')} />
            ) : (
              <ShareButton onStart={startShare} elsewhere={shareElsewhere} label={t('room.share.start')} />
            )}
          </Suspense>
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
              shareAvailability={availability}
              onTestConnection={openTest}
              roomName={name}
            />
          </Suspense>
        )}
      </main>
      {platform.sharing !== null && (
        // Mounted while the page is: it shows a share from "Starting…" on, and one that failed until it is
        // dismissed. Nothing while the page shares nothing, so no placeholder either. Also where the PC can't
        // share: the desktop app's share that outlived a reload still shows.
        <Suspense fallback={null}>
          {native ? (
            <NativeSharePanel share={share} onStop={stopShare} watchers={watchers} onStart={startShare} />
          ) : (
            <SharePanel share={share} onStop={stopShare} watchers={watchers} onTestConnection={openTest} />
          )}
        </Suspense>
      )}
      <PeoplePanel
        open={peopleOpen && state !== null}
        onClose={() => {
          setPeopleOpen(false);
        }}
        participants={state?.participants ?? []}
        shares={state?.shares ?? []}
        selfUserId={userId}
        onWatch={(id) => {
          // A pick, like a click on the tile: it holds until that share ends (05 §12.2).
          viewer.store.getState().focusShare(id);
        }}
      />
      <ConnTestDialog
        open={testOpen}
        onClose={() => {
          setTestOpen(false);
        }}
      />
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
