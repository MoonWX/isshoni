// ScreenAudioWarning (05 §13.3): the modal that stands between a whole-screen pick with system audio and the share.
//
// System audio is everything the computer plays, the sharer's voice call included, so the friends in that call
// would hear themselves. A browser can't leave single apps out of it; Discord Web's own screen share has the same
// echo (PLAN, "Discord Web's own screen share"). Keeping voice apps out is what isshoni's desktop app is for.
import { TriangleAlert } from 'lucide-react';
import { useTranslation } from 'react-i18next';
import { Link } from 'react-router';

import type { PickedSource, SharingProvider } from '../platform/types';
import type { Preset } from '../protocol/types.gen';
import { Button } from '../ui/Button';
import { Dialog } from '../ui/Dialog';
import styles from './ScreenAudioWarning.module.css';

export interface ScreenAudioWarningProps {
  open: boolean;
  /** platform.sharing, for "Pick something else". */
  sharing: SharingProvider;
  /** The preset of the pick this warning is about; a new pick keeps it. */
  preset: Preset;
  /** "Share without sound" (false: the audio track is stopped first) or "Share with sound anyway" (true). */
  onContinue: (withAudio: boolean) => void;
  /**
   * "Pick something else". The picker is already open again: `picking` is sharing.pick()'s promise, made as this
   * click's first statement. The receiver releases the stream this warning was about.
   */
  onPickAgain: (picking: Promise<PickedSource | null>) => void;
  /** Esc, ✕, the backdrop, or following the desktop-app link: give this pick up and release its stream. */
  onCancel: () => void;
}

export function ScreenAudioWarning({
  open,
  sharing,
  preset,
  onContinue,
  onPickAgain,
  onCancel,
}: ScreenAudioWarningProps) {
  const { t } = useTranslation();

  const pickAgain = (): void => {
    // First statement, and nothing awaited before it: the picker needs this click's transient activation.
    const picking = sharing.pick({ preset });
    onPickAgain(picking);
  };

  return (
    <Dialog
      open={open}
      onClose={onCancel}
      title={t('share.warning.title')}
      // Wide enough for the three buttons in one row; on phones they stack, the primary one on top.
      size="lg"
      footer={
        <>
          <Button variant="ghost" onClick={pickAgain}>
            {t('share.warning.pickAgain')}
          </Button>
          <Button
            onClick={() => {
              onContinue(true);
            }}
          >
            {t('share.warning.withSound')}
          </Button>
          <Button
            variant="primary"
            onClick={() => {
              onContinue(false);
            }}
          >
            {t('share.warning.withoutSound')}
          </Button>
        </>
      }
    >
      <div className={styles.body}>
        <p className={styles.lead}>
          <TriangleAlert aria-hidden="true" className={styles.icon} />
          <span>{t('share.warning.body')}</span>
        </p>
        <p className={styles.why}>{t('share.warning.why')}</p>
        <p className={styles.why}>
          <Link to="/download" onClick={onCancel}>
            {t('share.warning.desktopApp')}
          </Link>
        </p>
      </div>
    </Dialog>
  );
}
