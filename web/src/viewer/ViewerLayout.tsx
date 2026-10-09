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
// back to it (focusShare), which holds that share on the stage until it ends (05 §12.2). The stage and the tiles
// bring their own controls: the sound (the speaker button of a tile, mute and volume on the stage, 05 §12.3), the
// watchers popover (05 §12.6) and TapToStart over the stage while the browser wants a tap (05 §10.3).
//
// What the layout adds while it is mounted (05 §12.5, §12.7, §12.8), each only where the platform can do it
// (PlatformCapabilities, from the app's context; a layout outside the app's providers has the pseudo-fullscreen
// and the keyboard, and nothing that needs the platform):
// - fullscreen (F, a double click or tap on the stage, the stage's button) and picture-in-picture: the layout's
//   own element goes fullscreen, so the stage keeps everything that lies on it (fullscreen.ts, pip.ts);
// - the screen wake lock while a share is watched (wakeLock.ts);
// - the keyboard: the tiles are one tab stop that the arrow keys move, and the shortcuts of keyboard.ts, with
//   their dialog on "?";
// - `?focus=<shareId>` from the URL (FocusFromUrl.tsx), inside a router;
// - what plays, for the system's media notification (mediaSession.ts), and the one-time hint on iOS (IosHint.tsx).
import { Layers } from 'lucide-react';
import {
  useCallback,
  useContext,
  useEffect,
  useEffectEvent,
  useState,
  useSyncExternalStore,
  type ReactNode,
} from 'react';
import { flushSync } from 'react-dom';
import { useTranslation } from 'react-i18next';
import { useInRouterContext } from 'react-router';
import { useStore } from 'zustand';

import { AppContext } from '../app/context';
import { Button } from '../ui/Button';
import { cx } from '../ui/cx';
import { Sheet } from '../ui/Sheet';
import { ViewerContext } from './context';
import { FocusFromUrl } from './FocusFromUrl';
import { IosHint } from './IosHint';
import { attachShortcuts, moveTileFocus, TILE_LIST, TILE_MAIN, type ShortcutAction } from './keyboard';
import type { ViewerServices } from './services';
import { ShortcutsDialog } from './ShortcutsDialog';
import { Stage } from './Stage';
import { StageControls } from './StageControls';
import { Tile } from './Tile';
import { useCapabilities, useFullscreen, useMediaSession, usePip, useWakeLock } from './useControls';
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

/** The share of the tile an element is in; null outside the tiles. */
function tileShareId(el: Element | null): string | null {
  return el?.closest('li[data-share-id]')?.getAttribute('data-share-id') ?? null;
}

/** Whether the element has the focus because of the keyboard (the browser draws its focus ring). */
function hasKeyboardFocus(el: Element | null): boolean {
  try {
    return el?.matches(':focus-visible') === true;
  } catch {
    return false; // a DOM that doesn't know the selector
  }
}

export interface ViewerLayoutProps {
  /** The viewer's store, media registry and playback (createViewer()). */
  viewer: ViewerServices;
  /** The local preview streams of the shares this page publishes, by shareId (share/'s ActiveShare.preview). */
  localPreviews?: Readonly<Record<string, MediaStream | null | undefined>>;
  /** What the stage shows while nobody shares (the room page's empty state, 05 §11.2). Default: one line of text. */
  empty?: ReactNode;
  /** Opens the connection test. Without it, the "can't reach the media port" banner has no button. */
  onTestConnection?: () => void;
  /**
   * The room's name: the "artist" of what plays, for the system's media notification (05 §12.8). Without it the
   * app's name stands in.
   */
  roomName?: string | undefined;
  /** More controls for the stage's toolbar, after the layout's own (PiP, shortcuts, fullscreen). */
  stageControls?: ReactNode;
  /** More controls next to a tile's main area, after its watchers and speaker buttons. */
  tileActions?: (share: ViewerShare) => ReactNode;
}

export function ViewerLayout({
  viewer,
  localPreviews,
  empty,
  onTestConnection,
  roomName,
  stageControls,
  tileActions,
}: ViewerLayoutProps) {
  const { t } = useTranslation();
  const app = useContext(AppContext);
  const platform = app?.platform ?? null;
  const caps = useCapabilities(platform);
  const inRouter = useInRouterContext();
  const shares = useStore(viewer.store, (s) => s.shares);
  const focusedId = useStore(viewer.store, (s) => s.focusedShareId);
  const isFullscreen = useStore(viewer.store, (s) => s.fullscreen);
  const media = useStore(viewer.store, (s) => s.media);
  const phoneLandscape = useMediaQuery(PHONE_LANDSCAPE_QUERY);
  // The layout's element, in state: the controllers below are made for it.
  const [root, setRoot] = useState<HTMLDivElement | null>(null);
  const [sheetOpen, setSheetOpen] = useState(false);
  const [helpOpen, setHelpOpen] = useState(false);
  /** The tile the Tab key stops at (05 §12.7): the one that had the keyboard focus last. */
  const [rovingId, setRovingId] = useState<string | null>(null);

  const fullscreen = useFullscreen(viewer, root, caps?.fullscreen ?? 'none');
  const toggleFullscreen = useCallback(() => {
    fullscreen.toggle();
  }, [fullscreen]);
  const pip = usePip(viewer, root, caps?.pip === true);
  useWakeLock(viewer, platform, caps?.wakeLock === true);
  useMediaSession(viewer, roomName);

  const focused = shares.find((s) => s.id === focusedId) ?? null;
  const others = shares.filter((s) => s !== focused);
  const showOthers = !isFullscreen && others.length > 0;
  const inSheet = showOthers && phoneLandscape;
  // One tab stop for the list: the tile that had the focus, else the first.
  const tabStopId = others.some((s) => s.id === rovingId) ? rovingId : (others[0]?.id ?? null);

  // The sheet goes away with what it lists (fullscreen, a rotated phone, the last other share ending), and stays
  // closed when the list comes back. Adjusted while rendering, so no frame shows a stale sheet.
  if (sheetOpen && !inSheet) setSheetOpen(false);

  const pick = (shareId: string): void => {
    const store = viewer.store.getState();
    const active = document.activeElement;
    // The keyboard focus is on the tile that is picked, in the strip (a sheet closes and gives the focus back to
    // its button by itself). That tile goes to the stage, and the focus would go nowhere with it.
    const fromTile = !inSheet && root?.contains(active) === true && tileShareId(active) === shareId;
    if (!fromTile) {
      store.focusShare(shareId);
      setSheetOpen(false);
      return;
    }
    const before = store.focusedShareId;
    const byKeyboard = hasKeyboardFocus(active);
    flushSync(() => {
      store.focusShare(shareId);
      // The share that left the stage has a tile now: the focus goes there, so Enter swaps the two back.
      setRovingId(before);
    });
    // After a click or a tap the list stays where the user scrolled it; the keyboard's focus is brought into view.
    root.querySelector<HTMLElement>(`${TILE_LIST} ${TILE_MAIN}[tabindex="0"]`)?.focus({ preventScroll: !byKeyboard });
  };

  // The shortcuts (05 §12.7). True: the key press was used.
  const runShortcut = useEffectEvent((action: ShortcutAction): boolean => {
    const s = viewer.store.getState();
    switch (action.type) {
      case 'move':
        return root !== null && moveTileFocus(root, action.to, document.activeElement);
      case 'focusNth': {
        const share = s.shares[action.index];
        if (!share) return false;
        pick(share.id);
        return true;
      }
      case 'fullscreen':
        if (!s.fullscreen && s.focusedShareId === null) return false;
        fullscreen.toggle();
        return true;
      case 'mute':
        if (s.audio === 'blocked') {
          // The browser wants a gesture for the sound, and this key press is one: the tap of TapToStart.
          viewer.unlock();
          return true;
        }
        if (s.audibleShareId === null && s.audio !== 'muted') return false;
        viewer.audio.setMuted(s.audio !== 'muted');
        return true;
      case 'listen': {
        // The keyboard-focused tile: what its speaker button does.
        const active = document.activeElement;
        const id = root?.contains(active) === true ? tileShareId(active) : null;
        const share = s.shares.find((x) => x.id === id);
        if (!share || share.local || !share.info.audio) return false;
        s.toggleAudible(share.id);
        return true;
      }
      case 'escape':
        // A real fullscreen ends with the browser's own Esc; this is the pseudo one's.
        if (!s.fullscreen) return false;
        fullscreen.exit();
        return true;
      case 'help':
        setHelpOpen(true);
        return true;
      case 'debug': {
        if (app === null) return false;
        const prefs = app.prefs.getState();
        prefs.setDebug(!prefs.debug);
        return true;
      }
    }
  });
  useEffect(() => attachShortcuts({ run: (action) => runShortcut(action) }), []);

  // The roving tab stop follows the keyboard focus, whichever of a tile's controls has it.
  useEffect(() => {
    if (root === null) return undefined;
    const onFocusIn = (e: FocusEvent): void => {
      const id = e.target instanceof Element ? tileShareId(e.target) : null;
      if (id !== null) setRovingId(id);
    };
    root.addEventListener('focusin', onFocusIn);
    return () => {
      root.removeEventListener('focusin', onFocusIn);
    };
  }, [root]);

  const tiles = (
    <ul className={inSheet ? styles.sheetList : styles.others} aria-label={t('viewer.shares')} data-viewer-tiles="">
      {others.map((share) => (
        <Tile
          key={share.id}
          share={share}
          preview={share.local ? localPreviews?.[share.id] : undefined}
          onPick={pick}
          actions={tileActions?.(share)}
          tabStop={share.id === tabStopId}
        />
      ))}
    </ul>
  );

  return (
    <ViewerContext.Provider value={viewer}>
      <div
        ref={setRoot}
        className={cx(
          styles.layout,
          isFullscreen && styles.fullscreen,
          phoneLandscape && styles.phoneLandscape,
          showOthers && !inSheet && styles.withOthers,
        )}
      >
        {inRouter && <FocusFromUrl viewer={viewer} />}
        {media === 'unreachable' && (
          <div className={styles.banner} role="status">
            <p>{t('viewer.media.unreachable')}</p>
            {onTestConnection && <Button onClick={onTestConnection}>{t('viewer.media.test')}</Button>}
          </div>
        )}
        {/* One notice at a time: the banner says what is wrong now, the hint can wait. */}
        {!isFullscreen && media !== 'unreachable' && <IosHint className={styles.hint} />}
        <div className={styles.main}>
          <Stage
            className={styles.stage}
            share={focused}
            preview={focused?.local ? localPreviews?.[focused.id] : undefined}
            empty={<EmptyStage shares={shares} empty={empty} />}
            controls={
              focused && (
                <>
                  <StageControls
                    share={focused}
                    fullscreen={fullscreen}
                    pip={caps?.pip === true ? pip : null}
                    onShortcuts={() => {
                      setHelpOpen(true);
                    }}
                  />
                  {stageControls}
                </>
              )
            }
            onToggleFullscreen={toggleFullscreen}
            edge={isFullscreen || phoneLandscape}
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
        {/* Inside the layout, so it shows over a fullscreen stage too. Without the app there is no debug overlay. */}
        <ShortcutsDialog
          open={helpOpen}
          onClose={() => {
            setHelpOpen(false);
          }}
          without={app === null ? ['debug'] : []}
        />
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
