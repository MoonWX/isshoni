// ShareSheet (05 §13.1): what the Share button opens. A preset picker, the tip that steers towards a window with
// its own sound, and the Share button that opens the browser's picker.
import type { TFunction } from 'i18next';
import { Lightbulb, MonitorUp } from 'lucide-react';
import { useId, useState } from 'react';
import { Trans, useTranslation } from 'react-i18next';
import { Link } from 'react-router';

import { useApp, usePrefs } from '../app/context';
import { errorMessage } from '../lib/errorText';
import type { PickedSource, SharingProvider } from '../platform/types';
import { PresetGame, PresetMovie, PresetText, type Preset } from '../protocol/types.gen';
import { Button } from '../ui/Button';
import { Sheet } from '../ui/Sheet';
import { PRESETS } from './presets';
import styles from './ShareSheet.module.css';

/**
 * A share of this user from another connection (room.state, found with findShareElsewhere): the M1 UI starts at
 * most one.
 */
export interface ShareElsewhere {
  /** Sends share.stop for that share (allowed for the same user, 01 §4.1). */
  onStop: () => Promise<unknown>;
}

export interface ShareSheetProps {
  open: boolean;
  onClose: () => void;
  /** platform.sharing */
  sharing: SharingProvider;
  /**
   * The Share click. The browser's picker is already opening: `picking` is sharing.pick()'s promise, made as the
   * click handler's first statement, with the preset it was made for.
   */
  onPick: (picking: Promise<PickedSource | null>, preset: Preset) => void;
  /** Set while room.state has a share of this user from another tab or device. */
  elsewhere?: ShareElsewhere | null;
}

/** A preset's name and its one line. Literal keys, so check:i18n sees each one. */
function presetText(preset: Preset, t: TFunction): { name: string; hint: string } {
  switch (preset) {
    case PresetGame:
      return { name: t('share.preset.game.name'), hint: t('share.preset.game.hint') };
    case PresetMovie:
      return { name: t('share.preset.movie.name'), hint: t('share.preset.movie.hint') };
    case PresetText:
      return { name: t('share.preset.text.name'), hint: t('share.preset.text.hint') };
    default:
      return { name: t('share.preset.auto.name'), hint: t('share.preset.auto.hint') };
  }
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
      {error !== null && (
        <p role="alert" className={styles.error}>
          {errorMessage(error, t)}
        </p>
      )}
    </div>
  );
}

export function ShareSheet({ open, onClose, sharing, onPick, elsewhere = null }: ShareSheetProps) {
  const { t } = useTranslation();
  const { platform, prefs } = useApp();
  const preset = usePrefs((s) => s.preset);
  const group = useId();
  // Help text only (05 §13.8): every desktop browser that passes the capability probe may share, but only Chrome
  // and Edge are tested per release.
  const tested = platform.client.browser === 'chrome' || platform.client.browser === 'edge';

  const share = (): void => {
    // First statement, and nothing awaited before it: the picker needs this click's transient activation.
    const picking = sharing.pick({ preset });
    onPick(picking, preset);
  };

  return (
    <Sheet
      open={open}
      onClose={onClose}
      title={t('share.sheet.title')}
      footer={
        <>
          <Button variant="ghost" onClick={onClose}>
            {t('common.cancel')}
          </Button>
          <Button variant="primary" icon={<MonitorUp />} disabled={elsewhere !== null} onClick={share}>
            {t('share.sheet.share')}
          </Button>
        </>
      }
    >
      <div className={styles.body}>
        {elsewhere && <ElsewhereNotice elsewhere={elsewhere} />}
        <fieldset className={styles.presets}>
          <legend className={styles.legend}>{t('share.preset.legend')}</legend>
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
        <p className={styles.tip}>
          <Lightbulb aria-hidden="true" className={styles.tipIcon} />
          <span>
            <Trans i18nKey="share.sheet.tip" components={{ b: <strong /> }} />
          </span>
        </p>
        {!tested && <p className={styles.muted}>{t('share.sheet.bestInChrome')}</p>}
        <p className={styles.muted}>
          <Link to="/download">{t('share.sheet.desktopApp')}</Link>
        </p>
      </div>
    </Sheet>
  );
}
