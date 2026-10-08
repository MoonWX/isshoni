// The sound controls (05 §10.3, §12.3). Exactly one share is audible at a time, by default the focused one:
// - SpeakerButton, on every other tile: "Listen to bo". It moves the audio to that tile without moving the video
//   focus; pressed again, the audio goes back to the stage.
// - StageSound, in the stage's toolbar: the muted-speaker indicator while the stage's share is not the one heard
//   (a button that brings the sound back), the mute button, and the volume slider where the browser has a volume.
// A change of what is heard is one subscribe.update with both shares (subscriptions.ts); the element that plays it
// follows viewerStore.audibleShareId by itself (services.ts).
import { Volume2, VolumeX } from 'lucide-react';
import { useState } from 'react';
import { useTranslation } from 'react-i18next';

import { cx } from '../ui/cx';
import styles from './AudioControls.module.css';
import { canSetVolume } from './audioOut';
import { useViewer, useViewerServices } from './context';
import type { ViewerShare } from './viewerStore';

/** Whether this page can play the share's sound: it receives it (not its own capture) and the share has audio. */
const hasSound = (share: ViewerShare): boolean => !share.local && share.info.audio;

export interface SpeakerButtonProps {
  share: ViewerShare;
  className?: string | undefined;
}

/** A tile's speaker button. Renders nothing for a share without sound and for the own preview. */
export function SpeakerButton({ share, className }: SpeakerButtonProps) {
  const { t } = useTranslation();
  const { store } = useViewerServices();
  const audible = useViewer((s) => s.audibleShareId === share.id);
  if (!hasSound(share)) return null;
  const label = share.own
    ? t('viewer.listen.own')
    : t('viewer.listen.to', { name: share.ownerName !== '' ? share.ownerName : t('viewer.someone') });
  return (
    <button
      type="button"
      className={cx(styles.speaker, className)}
      aria-label={label}
      aria-pressed={audible}
      title={label}
      onClick={() => {
        store.getState().toggleAudible(share.id);
      }}
    >
      <span className={styles.chip}>{audible ? <Volume2 aria-hidden="true" /> : <VolumeX aria-hidden="true" />}</span>
    </button>
  );
}

export interface StageSoundProps {
  /** The share on the stage. */
  share: ViewerShare;
}

/** Whether StageSound renders anything for this share and this state of the store. */
export function useStageSound(share: ViewerShare): boolean {
  const heard = useViewer((s) => s.audibleShareId !== null || s.audio === 'muted');
  return heard || hasSound(share);
}

/** The stage's sound controls. */
export function StageSound({ share }: StageSoundProps) {
  const { t } = useTranslation();
  const viewer = useViewerServices();
  const audibleId = useViewer((s) => s.audibleShareId);
  const audibleName = useViewer((s) => {
    const other = s.shares.find((x) => x.id === s.audibleShareId);
    return other === undefined ? null : other.own ? '' : other.ownerName;
  });
  const muted = useViewer((s) => s.audio === 'muted');
  const volume = useViewer((s) => s.volume);
  // Asked once: iOS ignores `volume`, and a slider that does nothing is hidden (05 §10.3).
  const [adjustable] = useState(() => canSetVolume());

  const elsewhere = hasSound(share) && audibleId !== share.id;
  let elsewhereLabel = t('viewer.sound.here');
  if (elsewhere && audibleId !== null) {
    elsewhereLabel =
      audibleName !== null && audibleName !== ''
        ? t('viewer.sound.elsewhere', { name: audibleName })
        : t('viewer.sound.elsewhereOwn');
  }
  const showMute = audibleId !== null || muted;

  return (
    <>
      {elsewhere && (
        <button
          type="button"
          className={styles.listenHere}
          aria-label={elsewhereLabel}
          title={elsewhereLabel}
          onClick={() => {
            viewer.store.getState().setAudible(share.id);
          }}
        >
          <VolumeX aria-hidden="true" />
          <span aria-hidden="true">{t('viewer.sound.listenHere')}</span>
        </button>
      )}
      {showMute && (
        <button
          type="button"
          className={styles.icon}
          aria-label={muted ? t('viewer.sound.unmute') : t('viewer.sound.mute')}
          aria-pressed={muted}
          title={muted ? t('viewer.sound.unmute') : t('viewer.sound.mute')}
          onClick={() => {
            // Inside the click: unmuting may have to start the element (05 §10.3).
            viewer.audio.setMuted(!muted);
          }}
        >
          {muted ? <VolumeX aria-hidden="true" /> : <Volume2 aria-hidden="true" />}
        </button>
      )}
      {showMute && adjustable && (
        <input
          type="range"
          className={styles.volume}
          min={0}
          max={1}
          step={0.05}
          value={volume}
          aria-label={t('viewer.sound.volume')}
          aria-valuetext={t('viewer.sound.volumeValue', { percent: Math.round(volume * 100) })}
          onChange={(e) => {
            viewer.store.getState().setVolume(Number(e.target.value));
          }}
        />
      )}
    </>
  );
}
