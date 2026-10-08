// TapToStart (05 §10.3): the browser refused to play, and only a tap can start it. A large centred button over the
// stage, "Tap to unmute", plus a pill for the room's header. Both run the same one handler, viewer.unlock(): it
// calls audio.play() synchronously and play() on every tile video that was refused. iOS Low Power Mode blocks even
// muted autoplay; then the same button reads "Tap to start video".
//
// The tap picks nothing: it doesn't count as a manual focus (05 §12.2).
import { Play, VolumeX } from 'lucide-react';
import { useContext } from 'react';
import { useTranslation } from 'react-i18next';
import { useStore } from 'zustand';

import { Button } from '../ui/Button';
import { cx } from '../ui/cx';
import { ViewerContext } from './context';
import type { ViewerServices } from './services';
import styles from './TapToStart.module.css';

/** What the tap would start: the sound (and any refused video with it), only video, or nothing. */
function useBlocked(viewer: ViewerServices): 'audio' | 'video' | null {
  const audio = useStore(viewer.store, (s) => s.audio === 'blocked');
  const video = useStore(viewer.store, (s) => s.videoBlocked);
  return audio ? 'audio' : video ? 'video' : null;
}

export interface TapToStartProps {
  /** Default: the viewer of the ViewerLayout around it. */
  viewer?: ViewerServices | undefined;
  className?: string | undefined;
}

/** The large button over the stage. Renders nothing while everything plays. */
export function TapToStart({ viewer, className }: TapToStartProps) {
  const fromContext = useContext(ViewerContext);
  const services = viewer ?? fromContext;
  return services ? <StageButton viewer={services} className={className} /> : null;
}

function StageButton({ viewer, className }: { viewer: ViewerServices; className?: string | undefined }) {
  const { t } = useTranslation();
  const blocked = useBlocked(viewer);
  if (blocked === null) return null;
  return (
    <div className={cx(styles.cover, className)}>
      <button
        type="button"
        className={styles.tap}
        onClick={() => {
          viewer.unlock();
        }}
      >
        {blocked === 'audio' ? <VolumeX aria-hidden="true" /> : <Play aria-hidden="true" />}
        {blocked === 'audio' ? t('viewer.tap.unmute') : t('viewer.tap.video')}
      </button>
    </div>
  );
}

export interface TapToStartPillProps {
  /** The header is outside the ViewerLayout, so it names the viewer. */
  viewer: ViewerServices;
  className?: string | undefined;
}

/** The same tap as a small pill, for the room's header. Renders nothing while everything plays. */
export function TapToStartPill({ viewer, className }: TapToStartPillProps) {
  const { t } = useTranslation();
  const blocked = useBlocked(viewer);
  if (blocked === null) return null;
  return (
    <Button
      size="sm"
      variant="primary"
      className={cx(styles.pill, className)}
      icon={blocked === 'audio' ? <VolumeX /> : <Play />}
      onClick={() => {
        viewer.unlock();
      }}
    >
      {blocked === 'audio' ? t('viewer.tap.unmute') : t('viewer.tap.video')}
    </Button>
  );
}
