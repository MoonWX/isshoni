// The room page's main area (05 §11.2, §12.1): viewer/'s ViewerLayout on the page's viewer, with the room's empty
// state. It is part of the room's media chunk (rooms/media.ts): the room page renders it through lazyMedia.ts.
//
// Empty state (05 §11.2): "Nobody is sharing yet." plus [Share your screen] where the platform can share, or the
// note for phones. The notifications card of 05 §16.3 joins it with the Web Push flow.
import type { ReactNode } from 'react';
import { useTranslation } from 'react-i18next';

import { useApp } from '../app/context';
import { ShareButton, ShareUnavailableNote } from '../share/ShareButton';
import type { ShareElsewhere } from '../share/ShareSheet';
import type { StartShare } from '../share/shareStore';
import type { ViewerServices } from '../viewer/services';
import { ViewerLayout } from '../viewer/ViewerLayout';
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
  /** Opens the connection test (the "can't reach the media port" banner's button). */
  onTestConnection: () => void;
}

export function RoomStage({
  viewer,
  localPreviews,
  placeholder,
  onStartShare,
  shareElsewhere,
  onTestConnection,
}: RoomStageProps) {
  return (
    <ViewerLayout
      viewer={viewer}
      localPreviews={localPreviews}
      empty={placeholder ?? <EmptyRoom onStartShare={onStartShare} shareElsewhere={shareElsewhere} />}
      onTestConnection={onTestConnection}
    />
  );
}

function EmptyRoom({ onStartShare, shareElsewhere }: Pick<RoomStageProps, 'onStartShare' | 'shareElsewhere'>) {
  const { t } = useTranslation();
  const { platform } = useApp();
  return (
    <>
      <p className={styles.title}>{t('room.empty.title')}</p>
      {platform.sharing !== null ? (
        <ShareButton onStart={onStartShare} elsewhere={shareElsewhere} label={t('room.empty.share')} />
      ) : (
        <div className={styles.note}>
          <ShareUnavailableNote />
        </div>
      )}
    </>
  );
}
