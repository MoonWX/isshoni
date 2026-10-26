// The room page's main area (05 §11.2, §12.1): viewer/'s ViewerLayout on the page's viewer, with the room's empty
// state. It is part of the room's media chunk (rooms/media.ts): the room page renders it through lazyMedia.ts.
//
// Empty state (05 §11.2): "Nobody is sharing yet." plus [Share your screen] where the platform can share, or the
// note for phones, and account/'s one-time notifications card (05 §16.3: after 3 minutes of watching). Whether the
// platform can share is capabilities().canShare (docs/m2/03 §7.6); the desktop app (platform.sharing.mode 'native')
// gets share/native/'s button, and on a PC whose engine can't share, the reason in its place.
import { Suspense, type ReactNode } from 'react';
import { useTranslation } from 'react-i18next';

import { RoomNotificationsCard } from '../account/RoomPushSlots';
import { useApp } from '../app/context';
import { ShareButton, ShareUnavailableNote } from '../share/ShareButton';
import type { ShareElsewhere } from '../share/ShareSheet';
import type { StartShare } from '../share/shareStore';
import type { ViewerServices } from '../viewer/services';
import { ViewerLayout } from '../viewer/ViewerLayout';
import type { ShareAvailability } from './hooks';
import { NativeShareButton, NativeShareUnavailable } from './lazyNative';
import styles from './RoomStage.module.css';

export interface RoomStageProps {
  /** The page's viewer (the runtime's). */
  viewer: ViewerServices;
  /** The local preview of the share this page publishes, by shareId. */
  localPreviews: Readonly<Record<string, MediaStream | null | undefined>>;
  /**
   * What an empty stage shows instead of the room's empty state: "Joining…" until the first room.state, the reason
   * a join was refused. Not set once the page is in the room.
   */
  placeholder?: ReactNode;
  /** Publishes a picked source: the session's startShare (05 §11.1). */
  onStartShare: StartShare;
  /** This user's share from another tab or device, when room.state has one (05 §13.1). */
  shareElsewhere: ShareElsewhere | null;
  /**
   * Whether the platform can share (useShareAvailability): null while it hasn't said. Default: a platform with a
   * sharing provider can.
   */
  shareAvailability?: ShareAvailability | null | undefined;
  /** Opens the connection test (the "can't reach the media port" banner's button). */
  onTestConnection: () => void;
  /** The room's name, for the media notification of the share that plays (05 §12.8); the app's name without it. */
  roomName?: string | undefined;
}

export function RoomStage({
  viewer,
  localPreviews,
  placeholder,
  onStartShare,
  shareElsewhere,
  shareAvailability = { canShare: true },
  onTestConnection,
  roomName,
}: RoomStageProps) {
  return (
    <ViewerLayout
      viewer={viewer}
      localPreviews={localPreviews}
      empty={
        placeholder ?? (
          <EmptyRoom
            onStartShare={onStartShare}
            shareElsewhere={shareElsewhere}
            shareAvailability={shareAvailability}
          />
        )
      }
      onTestConnection={onTestConnection}
      roomName={roomName}
    />
  );
}

function EmptyRoom({
  onStartShare,
  shareElsewhere,
  shareAvailability,
}: Pick<RoomStageProps, 'onStartShare' | 'shareElsewhere' | 'shareAvailability'>) {
  const { t } = useTranslation();
  const { platform } = useApp();
  const sharing = platform.sharing;
  let share: ReactNode = null;
  if (sharing === null) {
    share = (
      <div className={styles.note}>
        <ShareUnavailableNote />
      </div>
    );
  } else if (shareAvailability?.canShare === true) {
    share =
      sharing.mode === 'native' ? (
        <Suspense fallback={null}>
          <NativeShareButton onStart={onStartShare} elsewhere={shareElsewhere} label={t('room.empty.share')} />
        </Suspense>
      ) : (
        <ShareButton onStart={onStartShare} elsewhere={shareElsewhere} label={t('room.empty.share')} />
      );
  } else if (shareAvailability?.canShare === false && sharing.mode === 'native') {
    share = (
      <div className={styles.note}>
        <Suspense fallback={null}>
          <NativeShareUnavailable reason={shareAvailability.reason} />
        </Suspense>
      </div>
    );
  }
  return (
    <>
      <p className={styles.title}>{t('room.empty.title')}</p>
      {share}
      <RoomNotificationsCard />
    </>
  );
}
