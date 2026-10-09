// The stage (05 §12.1, §16.6): the focused share, large. A <section> named "Now watching: alex's window" with the
// video, the share's state over it, and a bar with who shares what and who watches (the eye opens the watchers
// popover, 05 §12.6). Its toolbar has the sound controls (05 §10.3, §12.3: the muted-speaker indicator, mute,
// volume) and whatever the caller adds: ViewerLayout adds PiP, the shortcuts and fullscreen (StageControls.tsx).
// TapToStart lies over it while the browser wants a tap. Without a share it shows the empty state.
//
// A double click or double tap on it, outside its controls, toggles fullscreen (05 §12.5), like the F key and the
// toolbar's button; it is a pointer's shortcut for those, so the listeners are native ones.
import { useCallback, useEffect, useRef, type ReactNode } from 'react';
import { useTranslation } from 'react-i18next';

import { cx } from '../ui/cx';
import { Spinner } from '../ui/Spinner';
import { StageSound, useStageSound } from './AudioControls';
import { useShareMedia, useViewer } from './context';
import { onDoublePress } from './fullscreen';
import { ShareVideo } from './ShareVideo';
import { badgeText, overlayText, shareTitle, shareView, shareWhat, sharerName } from './shareView';
import styles from './Stage.module.css';
import { TapToStart } from './TapToStart';
import { useVisibility } from './useVisibility';
import type { ViewerShare } from './viewerStore';
import { WatchersPopover } from './WatchersPopover';

export interface StageProps {
  /** The focused share; null shows `empty`. */
  share: ViewerShare | null;
  /** The local preview, when the focused share is one this page publishes (share.local). */
  preview?: MediaStream | null | undefined;
  /** What an empty stage shows (05 §11.2): "Nobody is sharing yet." and the like. */
  empty?: ReactNode;
  /** More controls for the toolbar, after the sound controls. */
  controls?: ReactNode;
  /** A double click or double tap on the stage (05 §12.5). Call fullscreen's toggle: it runs inside the press. */
  onToggleFullscreen?: (() => void) | undefined;
  /**
   * The stage reaches the screen's edges (fullscreen, a phone held sideways): its bar keeps clear of the notch and
   * the home indicator.
   */
  edge?: boolean | undefined;
  className?: string | undefined;
}

export function Stage({ share, preview, empty, controls, onToggleFullscreen, edge, className }: StageProps) {
  const { t } = useTranslation();
  if (!share) {
    return (
      <section className={cx(styles.stage, className)} aria-label={t('viewer.stage.none')} data-viewer-stage="">
        <div className={styles.empty}>{empty}</div>
        {/* Sound can play without a share on the stage: one that is heard through its tile's speaker button. */}
        <TapToStart />
      </section>
    );
  }
  return (
    <FocusedStage
      share={share}
      preview={preview}
      controls={controls}
      onToggleFullscreen={onToggleFullscreen}
      edge={edge}
      className={className}
    />
  );
}

function FocusedStage({
  share,
  preview,
  controls,
  onToggleFullscreen,
  edge,
  className,
}: StageProps & { share: ViewerShare }) {
  const { t } = useTranslation();
  const media = useShareMedia(share.id);
  const status = useViewer((s) => s.status[share.id]);
  const frozen = useViewer((s) => s.frozen[share.id] === true);
  const sound = useStageSound(share);
  // The stage shows this share: the layer policy gives it the high layer only while it does (05 §12.4).
  const shown = useVisibility(share.id);
  const section = useRef<HTMLElement | null>(null);
  const ref = useCallback(
    (el: HTMLElement | null) => {
      section.current = el;
      const off = shown(el);
      return () => {
        section.current = null;
        if (typeof off === 'function') off();
      };
    },
    [shown],
  );

  useEffect(() => {
    const el = section.current;
    if (!el || !onToggleFullscreen) return undefined;
    return onDoublePress(el, onToggleFullscreen);
  }, [onToggleFullscreen]);

  const stream = share.local ? preview : undefined;
  const track = share.local ? undefined : media.video;
  const view = shareView(share, status, stream != null || track !== undefined, frozen);
  const title = shareTitle(t, share);
  const extra = controls != null && controls !== false;
  const waiting = view.overlay === 'connecting' || view.overlay === 'frozen';

  return (
    <section
      ref={ref}
      className={cx(styles.stage, edge && styles.edge, className)}
      aria-label={share.local ? t('viewer.stage.preview', { title }) : t('viewer.stage.watching', { title })}
      data-share-id={share.id}
      data-viewer-stage=""
    >
      {/* The key gives each share its own element, so the last frame of the previous one never shows. */}
      <ShareVideo key={share.id} track={track} stream={stream} />
      {/* One live region for the share's state: screen readers hear "Connecting…" and "Connection unstable". */}
      <div className={styles.status} role="status">
        {view.overlay && (
          <div className={styles.overlay}>
            {waiting && <Spinner size="lg" label={null} />}
            <p className={styles.overlayText}>{overlayText(t, view.overlay)}</p>
          </div>
        )}
        {!view.overlay && view.badge && <p className={styles.badge}>{badgeText(t, view.badge)}</p>}
      </div>
      <div className={styles.bar}>
        <p className={styles.title}>
          <span className={styles.name}>{sharerName(t, share)}</span>
          <span className={styles.what}>{shareWhat(t, share)}</span>
        </p>
        <WatchersPopover share={share} withText />
        {(sound || extra) && (
          <div className={styles.controls} role="toolbar" aria-label={t('viewer.stage.controls')}>
            {sound && <StageSound share={share} />}
            {controls}
          </div>
        )}
      </div>
      <TapToStart />
    </section>
  );
}
