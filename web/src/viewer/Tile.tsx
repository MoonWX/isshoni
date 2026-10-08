// A tile of the "others" list (05 §12.1, §12.6, §16.6): one share's video (the low layer, or the local preview of
// a share this page publishes) with its sharer, what is shared, and how many watch. The main area is one <button>
// named "Watch alex's window, 3 watching"; whatever else a tile gets (the speaker and watchers buttons, S47) sits
// next to it, never inside.
import { Eye } from 'lucide-react';
import { useId, type ReactNode } from 'react';
import { useTranslation } from 'react-i18next';

import { Spinner } from '../ui/Spinner';
import { useShareMedia, useViewer } from './context';
import { ShareVideo } from './ShareVideo';
import { badgeText, overlayText, shareTitle, shareView, shareWhat, sharerName, watchingText } from './shareView';
import styles from './Tile.module.css';
import type { ViewerShare } from './viewerStore';

export interface TileProps {
  share: ViewerShare;
  /** The local preview, for a share this page publishes (share.local). */
  preview?: MediaStream | null | undefined;
  /** The user picked this tile (click, tap, Enter or Space). */
  onPick: (shareId: string) => void;
  /** Controls next to the main area (S47: the speaker and watchers buttons). */
  actions?: ReactNode;
}

export function Tile({ share, preview, onPick, actions }: TileProps) {
  const { t } = useTranslation();
  const media = useShareMedia(share.id);
  const status = useViewer((s) => s.status[share.id]);
  const stateId = useId();

  const stream = share.local ? preview : undefined;
  const track = share.local ? undefined : media.video;
  const view = shareView(share, status, stream != null || track !== undefined);
  const watching = watchingText(t, share);
  let label: string;
  if (share.local) label = t('viewer.tile.preview', { watching });
  else if (share.own) label = t('viewer.tile.watchOwn', { watching });
  else label = t('viewer.tile.watch', { title: shareTitle(t, share), watching });
  const state = view.overlay ? overlayText(t, view.overlay) : view.badge ? badgeText(t, view.badge) : null;

  return (
    <li className={styles.tile} data-share-id={share.id}>
      <button
        type="button"
        className={styles.main}
        aria-label={label}
        aria-describedby={state !== null ? stateId : undefined}
        onClick={() => {
          onPick(share.id);
        }}
      >
        <ShareVideo track={track} stream={stream} />
        {view.overlay && (
          <span className={styles.overlay} id={stateId}>
            {view.overlay === 'connecting' && <Spinner size="md" label={null} />}
            <span className={styles.overlayText}>{overlayText(t, view.overlay)}</span>
          </span>
        )}
        {!view.overlay && view.badge && (
          <span className={styles.badge} id={stateId}>
            {badgeText(t, view.badge)}
          </span>
        )}
        <span className={styles.caption}>
          <span className={styles.name}>{sharerName(t, share)}</span>
          <span className={styles.what}>{shareWhat(t, share)}</span>
        </span>
      </button>
      <div className={styles.aside}>
        {/* The count is in the button's name too; S47 turns this into the watchers popover's button. */}
        <span className={styles.watchers} aria-hidden="true">
          <Eye className={styles.eye} />
          {share.info.watchers.length}
        </span>
        {actions}
      </div>
    </li>
  );
}
