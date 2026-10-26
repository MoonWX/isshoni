// The Do-not-disturb tip (docs/m2/03 §10.2): before the first share of a whole screen (settings tips.dndShown, which
// Go sets when the share starts). Notifications would show up for friends; isshoni's own warnings should still reach
// the user. "Open Do not disturb settings" opens Windows' page (App.OpenOSSettings('dnd')) and keeps the tip, so the
// user can come back and share: Go holds the pick for 300 s.
import { useTranslation } from 'react-i18next';

import { useApp } from '../../app/context';
import { Button } from '../../ui/Button';
import { Dialog } from '../../ui/Dialog';
import styles from './native.module.css';

export interface DndTipProps {
  open: boolean;
  /** Share now: the share starts. */
  onShareNow: () => void;
  /** Esc, ✕: the pick is given up (released). */
  onCancel: () => void;
}

export function DndTip({ open, onShareNow, onCancel }: DndTipProps) {
  const { t } = useTranslation();
  const { platform } = useApp();
  const bridge = platform.desktop?.bridge;
  return (
    <Dialog
      open={open}
      onClose={onCancel}
      title={t('desktop.share.dnd.title')}
      footer={
        <>
          {bridge && (
            <Button
              onClick={() => {
                bridge.app.openOSSettings('dnd').catch(() => undefined);
              }}
            >
              {t('desktop.share.dnd.openSettings')}
            </Button>
          )}
          <Button variant="primary" onClick={onShareNow}>
            {t('desktop.share.dnd.shareNow')}
          </Button>
        </>
      }
    >
      <p className={styles.lead}>{t('desktop.share.dnd.body')}</p>
    </Dialog>
  );
}
