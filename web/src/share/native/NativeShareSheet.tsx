// The desktop ShareSheet (docs/m2/03 §10.2): what the Share button opens in the desktop app. It replaces the
// browser's tip with the "Optimize for" picker (Auto, Game, Movie, Text, one line each, as 05 §13.5), a Share sound
// checkbox (on), and Share, which opens the Windows picker owned by the app's window. The sheet stays open while the
// picker is: it says "Choose what to share in the Windows picker…", and a picker closed without a choice comes back
// to it. "You're already sharing from another tab or device" counts the app's own share as this page's
// (findShareElsewhere with platform.sharing.localConnectionIds()); its Stop it asks C2 to stop the other share.
//
// A pick that failed is said here, with what to do (engineErrors.ts): "Pick something other than isshoni" above the
// sheet while the picker opens again, Windows' capture settings, and so on.
//
// Later slices add to it: the one-line "What friends hear" summary with Audio.Watch(true) while open, and the
// volume note under Share sound (README WA77).
import { MonitorUp } from 'lucide-react';
import { useId, useState } from 'react';
import { useTranslation } from 'react-i18next';

import { useApp, usePrefs } from '../../app/context';
import type { Preset } from '../../protocol/types.gen';
import { Button } from '../../ui/Button';
import { Sheet } from '../../ui/Sheet';
import { PRESETS } from '../presets';
import { presetText } from '../presetText';
import type { ShareElsewhere } from '../ShareSheet';
import { ShareErrorNote } from './ShareErrorNote';
import styles from './native.module.css';

export interface NativeShareSheetProps {
  open: boolean;
  onClose: () => void;
  /** The Share click: the button opens the Windows picker with this preset and sound choice. */
  onShare: (preset: Preset, withAudio: boolean) => void;
  /** The Windows picker is open: "Choose what to share in the Windows picker…", and Share waits. */
  picking: boolean;
  /** A failed pick, said above the controls (null: none). */
  error?: unknown;
  /** Opens the picker again from the error's own button (Try again). */
  onRetry?: () => void;
  /** Set while room.state has a share of this user from another tab or device. */
  elsewhere?: ShareElsewhere | null;
}

function ElsewhereNotice({ elsewhere }: { elsewhere: ShareElsewhere }) {
  const { t } = useTranslation();
  const [stopping, setStopping] = useState(false);
  const [error, setError] = useState<unknown>(null);
  const stop = (): void => {
    setStopping(true);
    setError(null);
    elsewhere
      .onStop()
      .catch((err: unknown) => {
        setError(err);
      })
      .finally(() => {
        setStopping(false);
      });
  };
  return (
    <div className={styles.elsewhere}>
      <p>{t('share.sheet.elsewhere')}</p>
      <Button size="sm" loading={stopping} onClick={stop}>
        {t('share.sheet.stopElsewhere')}
      </Button>
      {error !== null && <ShareErrorNote error={error} />}
    </div>
  );
}

export function NativeShareSheet({
  open,
  onClose,
  onShare,
  picking,
  error = null,
  onRetry,
  elsewhere = null,
}: NativeShareSheetProps) {
  const { t } = useTranslation();
  const { prefs } = useApp();
  const preset = usePrefs((s) => s.preset);
  const [withAudio, setWithAudio] = useState(true);
  const group = useId();

  return (
    <Sheet
      open={open}
      onClose={onClose}
      title={t('desktop.share.sheet.title')}
      footer={
        <>
          <Button variant="ghost" onClick={onClose}>
            {t('common.cancel')}
          </Button>
          <Button
            variant="primary"
            icon={<MonitorUp />}
            loading={picking}
            disabled={elsewhere !== null || picking}
            onClick={() => {
              onShare(preset, withAudio);
            }}
          >
            {t('desktop.share.sheet.share')}
          </Button>
        </>
      }
    >
      <div className={styles.body}>
        {picking && (
          <p className={styles.picking} role="status">
            {t('desktop.share.sheet.picking')}
          </p>
        )}
        {error !== null && <ShareErrorNote error={error} onRetry={picking ? undefined : onRetry} />}
        {elsewhere && <ElsewhereNotice elsewhere={elsewhere} />}
        <fieldset className={styles.presets} disabled={picking}>
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
                    prefs.getState().setPreset(p);
                  }}
                />
                <span className={styles.presetName}>{text.name}</span>
                <span className={styles.presetHint}>{text.hint}</span>
              </label>
            );
          })}
        </fieldset>
        <label className={styles.switch}>
          <input
            type="checkbox"
            checked={withAudio}
            disabled={picking}
            onChange={(ev) => {
              setWithAudio(ev.target.checked);
            }}
          />
          <span>{t('desktop.share.sheet.sound')}</span>
        </label>
      </div>
    </Sheet>
  );
}
