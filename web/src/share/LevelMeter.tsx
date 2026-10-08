// LevelMeter (05 §13.7): the level of the sound this page shares, in the share panel, from a WebAudio AnalyserNode on
// the captured audio track (levelAudio.ts). "No sound captured" appears after 10 s of digital silence while sound
// is on: the capture has an audio track, but nothing plays into it (a muted tab, the wrong window).
import { useEffect, useState } from 'react';
import { useTranslation } from 'react-i18next';

import { openLevelTap } from './levelAudio';
import styles from './LevelMeter.module.css';

/** Digital silence for this long, with sound on → "No sound captured". */
export const SILENCE_AFTER_MS = 10_000;
/** The meter reads the level this often. */
export const LEVEL_INTERVAL_MS = 100;

export interface LevelMeterProps {
  /** The captured audio track; null when the share has none. */
  track: MediaStreamTrack | null;
  /** The panel's sound switch: while off nothing is measured, and silence is what was asked for. */
  active: boolean;
}

export function LevelMeter({ track, active }: LevelMeterProps) {
  const { t } = useTranslation();
  const [level, setLevel] = useState(0);
  const [silent, setSilent] = useState(false);

  useEffect(() => {
    if (track === null || !active) return undefined;
    const tap = openLevelTap(track);
    if (tap === null) return undefined;
    let silentSince: number | null = null;
    const timer = setInterval(() => {
      const peak = tap.read();
      const now = performance.now();
      if (peak > 0) silentSince = null;
      else silentSince ??= now;
      setLevel(peak);
      setSilent(silentSince !== null && now - silentSince >= SILENCE_AFTER_MS);
    }, LEVEL_INTERVAL_MS);
    return () => {
      clearInterval(timer);
      tap.close();
    };
  }, [track, active]);

  if (track === null) return null;
  const shown = active ? level : 0;
  return (
    <div className={styles.meter}>
      {/* The square root spreads quiet sound over more of the bar, as level meters do. */}
      <meter className={styles.bar} min={0} max={1} value={Math.sqrt(shown)} aria-label={t('share.panel.level')} />
      {active && silent && <p className={styles.silent}>{t('share.panel.noSound')}</p>}
    </div>
  );
}
