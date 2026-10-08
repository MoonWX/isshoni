// ViewerLayout (05 §12.1): a stage with the focused share and "others": every other share that has a tile, plus
// the local preview of a share this page publishes, newest first. Shares in `starting` have no tile (01 §4.4).
//
// | Viewport              | Stage                    | Others                                                  |
// |-----------------------|--------------------------|---------------------------------------------------------|
// | ≥ 1024 px wide        | fills the main area      | right column, 280 px, vertical scroll, 16:9 tiles       |
// | < 1024 px, portrait   | full width, 16:9, on top | horizontal scroll strip below, tiles min(44vw, 220px)   |
// | phone landscape       | fills the viewport       | hidden; a "Shares (N)" button opens a bottom sheet      |
// | fullscreen            | only the stage           | not rendered                                            |
//
// It is a view of viewerStore: RoomSession gives the store each room.state (syncRoom), and a pick on a tile goes
// back to it (focusShare), which holds that share on the stage until it ends (05 §12.2).
import { Layers } from 'lucide-react';
import { useState, useSyncExternalStore, type ReactNode } from 'react';
import { useTranslation } from 'react-i18next';
import { useStore } from 'zustand';

import { Button } from '../ui/Button';
import { cx } from '../ui/cx';
import { Sheet } from '../ui/Sheet';
import { ViewerContext } from './context';
import type { ViewerServices } from './services';
import { Stage } from './Stage';
import { Tile } from './Tile';
import styles from './ViewerLayout.module.css';
import type { ViewerShare } from './viewerStore';

/** A phone held sideways: too low for a strip of tiles under the stage. */
export const PHONE_LANDSCAPE_QUERY = '(orientation: landscape) and (max-height: 500px)';

/** Whether a media query matches, live. False where matchMedia is missing. */
function useMediaQuery(query: string): boolean {
  return useSyncExternalStore(
    (onChange) => {
      if (typeof window.matchMedia !== 'function') return () => undefined;
      const list = window.matchMedia(query);
      list.addEventListener('change', onChange);
      return () => {
        list.removeEventListener('change', onChange);
      };
    },
    () => typeof window.matchMedia === 'function' && window.matchMedia(query).matches,
  );
}

export interface ViewerLayoutProps {
  /** The viewer's store and media registry (createViewer()). */
  viewer: ViewerServices;
  /** The local preview streams of the shares this page publishes, by shareId (share/'s ActiveShare.preview). */
  localPreviews?: Readonly<Record<string, MediaStream | null | undefined>>;
  /** What the stage shows while nobody shares (the room page's empty state, 05 §11.2). Default: one line of text. */
  empty?: ReactNode;
  /** Opens the connection test. Without it, the "can't reach the media port" banner has no button. */
  onTestConnection?: () => void;
  /** The stage toolbar's controls (S47, S56). */
  stageControls?: ReactNode;
  /** Controls next to a tile's main area (S47: the speaker and watchers buttons). */
  tileActions?: (share: ViewerShare) => ReactNode;
}

export function ViewerLayout({
  viewer,
  localPreviews,
  empty,
  onTestConnection,
  stageControls,
  tileActions,
}: ViewerLayoutProps) {
  const { t } = useTranslation();
  const shares = useStore(viewer.store, (s) => s.shares);
  const focusedId = useStore(viewer.store, (s) => s.focusedShareId);
  const fullscreen = useStore(viewer.store, (s) => s.fullscreen);
  const media = useStore(viewer.store, (s) => s.media);
  const phoneLandscape = useMediaQuery(PHONE_LANDSCAPE_QUERY);
  const [sheetOpen, setSheetOpen] = useState(false);

  const focused = shares.find((s) => s.id === focusedId) ?? null;
  const others = shares.filter((s) => s !== focused);
  const showOthers = !fullscreen && others.length > 0;
  const inSheet = showOthers && phoneLandscape;

  // The sheet goes away with what it lists (fullscreen, a rotated phone, the last other share ending), and stays
  // closed when the list comes back. Adjusted while rendering, so no frame shows a stale sheet.
  if (sheetOpen && !inSheet) setSheetOpen(false);

  const pick = (shareId: string): void => {
    viewer.store.getState().focusShare(shareId);
    setSheetOpen(false);
  };

  const tiles = (
    <ul className={inSheet ? styles.sheetList : styles.others} aria-label={t('viewer.shares')}>
      {others.map((share) => (
        <Tile
          key={share.id}
          share={share}
          preview={share.local ? localPreviews?.[share.id] : undefined}
          onPick={pick}
          actions={tileActions?.(share)}
        />
      ))}
    </ul>
  );

  return (
    <ViewerContext.Provider value={viewer}>
      <div
        className={cx(
          styles.layout,
          fullscreen && styles.fullscreen,
          phoneLandscape && styles.phoneLandscape,
          showOthers && !inSheet && styles.withOthers,
        )}
      >
        {media === 'unreachable' && (
          <div className={styles.banner} role="status">
            <p>{t('viewer.media.unreachable')}</p>
            {onTestConnection && <Button onClick={onTestConnection}>{t('viewer.media.test')}</Button>}
          </div>
        )}
        <div className={styles.main}>
          <Stage
            className={styles.stage}
            share={focused}
            preview={focused?.local ? localPreviews?.[focused.id] : undefined}
            empty={<EmptyStage shares={shares} empty={empty} />}
            controls={stageControls}
          />
          {showOthers && !inSheet && tiles}
        </div>
        {inSheet && (
          <>
            <Button
              className={styles.sharesButton}
              icon={<Layers />}
              aria-haspopup="dialog"
              onClick={() => {
                setSheetOpen(true);
              }}
            >
              {t('viewer.sharesButton', { count: others.length })}
            </Button>
            <Sheet
              open={sheetOpen}
              onClose={() => {
                setSheetOpen(false);
              }}
              title={t('viewer.shares')}
            >
              {/* Only while it is open: tiles that nobody sees must not count as visible (05 §12.4). */}
              {sheetOpen && tiles}
            </Sheet>
          </>
        )}
      </div>
    </ViewerContext.Provider>
  );
}

/**
 * An empty stage (05 §12.1): "You're live · 3 watching" while this page shares, else the room's empty state. With
 * tiles but nothing focused (only shares of this user, which are never focused automatically), it asks for a pick.
 */
function EmptyStage({ shares, empty }: { shares: readonly ViewerShare[]; empty: ReactNode }) {
  const { t } = useTranslation();
  const local = shares.filter((s) => s.local);
  if (local.length > 0) {
    const count = local.reduce((n, s) => n + s.info.watchers.length, 0);
    return <p>{t('viewer.live', { watching: t('viewer.watching', { count }) })}</p>;
  }
  if (shares.length > 0) return <p>{t('viewer.pick')}</p>;
  return empty ?? <p>{t('viewer.empty')}</p>;
}
