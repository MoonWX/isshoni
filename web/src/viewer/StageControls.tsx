// The stage's own controls after the sound (05 §12.5, §12.7), in its toolbar: picture-in-picture, the keyboard
// shortcuts, and fullscreen. Each is there only where it works:
// - PiP where the platform has it (never on iOS in M1, 05 §12.8) and the stage shows a received share: a floating
//   window of one's own capture shows nothing new;
// - the shortcuts button where there is a keyboard to use them with (a device that can hover);
// - fullscreen always: every device has at least the CSS pseudo-fullscreen (fullscreen.ts).
// The buttons say what a press does ("Fullscreen", "Exit fullscreen"), so they need no pressed state.
import { Keyboard, Maximize, Minimize, PictureInPicture2 } from 'lucide-react';
import { useTranslation } from 'react-i18next';

import { useViewer } from './context';
import type { FullscreenController } from './fullscreen';
import type { PipController } from './pip';
import styles from './StageControls.module.css';
import type { ViewerShare } from './viewerStore';

export interface StageControlsProps {
  /** The share on the stage. */
  share: ViewerShare;
  fullscreen: Pick<FullscreenController, 'toggle'>;
  /** null: no picture-in-picture on this device. */
  pip: Pick<PipController, 'toggle'> | null;
  /** Opens the shortcuts dialog. */
  onShortcuts: () => void;
}

export function StageControls({ share, fullscreen, pip, onShortcuts }: StageControlsProps) {
  const { t } = useTranslation();
  const isFullscreen = useViewer((s) => s.fullscreen);
  const inPip = useViewer((s) => s.pipShareId === share.id);
  const pipLabel = inPip ? t('viewer.pip.exit') : t('viewer.pip.enter');
  const fullscreenLabel = isFullscreen ? t('viewer.fullscreen.exit') : t('viewer.fullscreen.enter');

  return (
    <>
      {pip !== null && !share.local && (
        <button
          type="button"
          className={styles.icon}
          aria-label={pipLabel}
          title={pipLabel}
          data-on={inPip || undefined}
          onClick={() => {
            // Inside the click: the browser opens the window only for a user gesture.
            pip.toggle();
          }}
        >
          <PictureInPicture2 aria-hidden="true" />
        </button>
      )}
      <button
        type="button"
        className={styles.keyboard}
        aria-label={t('viewer.shortcuts.open')}
        aria-haspopup="dialog"
        title={t('viewer.shortcuts.open')}
        onClick={onShortcuts}
      >
        <Keyboard aria-hidden="true" />
      </button>
      <button
        type="button"
        className={styles.icon}
        aria-label={fullscreenLabel}
        title={fullscreenLabel}
        onClick={() => {
          // Inside the click, for the same reason.
          fullscreen.toggle();
        }}
      >
        {isFullscreen ? <Minimize aria-hidden="true" /> : <Maximize aria-hidden="true" />}
      </button>
    </>
  );
}
