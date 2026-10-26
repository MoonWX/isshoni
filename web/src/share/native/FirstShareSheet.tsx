// The first-share sheet (docs/m2/03 §10.2): once per install (settings tips.firstShareShown, which Go sets when the
// share starts), before the first share goes live, because it is the moment the user learns what leaves the PC:
// "Ready to go live?" · "Friends will see: <label>" · the "Optimize for" picker · Go live · Cancel. The label is the
// Windows picker's name of the source; it stays on this PC.
//
// Later slices add to it: the three groups of "What friends hear" with Audio.Watch(true) while open and the volume
// note (README WA77), and the line with the Pause and Stop hotkeys (WA80).
import { useId } from 'react';
import { useTranslation } from 'react-i18next';

import type { Preset } from '../../protocol/types.gen';
import { Button } from '../../ui/Button';
import { Dialog } from '../../ui/Dialog';
import { PRESETS } from '../presets';
import { presetText } from '../presetText';
import styles from './native.module.css';

export interface FirstShareSheetProps {
  open: boolean;
  /** The picker's name of what was picked (PickedSource.label). */
  label: string;
  preset: Preset;
  onPreset: (preset: Preset) => void;
  /** Go live: the next step, or the share. */
  onGoLive: () => void;
  /** Cancel, Esc, ✕: the pick is given up (released). */
  onCancel: () => void;
}

export function FirstShareSheet({ open, label, preset, onPreset, onGoLive, onCancel }: FirstShareSheetProps) {
  const { t } = useTranslation();
  const group = useId();
  return (
    <Dialog
      open={open}
      onClose={onCancel}
      title={t('desktop.share.first.title')}
      footer={
        <>
          <Button variant="ghost" onClick={onCancel}>
            {t('common.cancel')}
          </Button>
          <Button variant="primary" onClick={onGoLive}>
            {t('desktop.share.first.goLive')}
          </Button>
        </>
      }
    >
      <div className={styles.body}>
        <p className={styles.lead}>{t('desktop.share.first.friendsSee', { label })}</p>
        <fieldset className={styles.presets}>
          <legend className={styles.legend}>{t('desktop.share.sheet.presetHint')}</legend>
          {PRESETS.map((p) => {
            const text = presetText(p, t);
            return (
              <label key={p} className={styles.preset}>
                <input
                  type="radio"
                  name={group}
                  value={p}
                  checked={p === preset}
                  onChange={() => {
                    onPreset(p);
                  }}
                />
                <span className={styles.presetName}>{text.name}</span>
                <span className={styles.presetHint}>{text.hint}</span>
              </label>
            );
          })}
        </fieldset>
      </div>
    </Dialog>
  );
}
