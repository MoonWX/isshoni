// The sound check (docs/m2/03 §8.4, 05 §11.3): the wizard's sound step and Settings → Audio's "Run the sound check
// again". Audio.SoundCheck plays a test chime (05's helper, or the engine itself) and captures it for up to 3 s; the
// page shows "Playing a test sound…" with a spinner (no live meter: no levels exist during a check), then maps 05's
// SoundCheckResult {captured, levelDb, ms, backend, reason}:
// - captured over process loopback → "✓ isshoni can capture your PC's sound, without your voice apps" and the level;
// - captured over the endpoint → it can't keep voice apps out on this PC: set the voice app to another output device,
//   [Learn more] (the plan's degraded banner);
// - not captured → by reason: no_device (05's no-output-device text), device_busy (exclusive mode, with [Open sound
//   settings]), and everything else (silent, activation_failed, timeout, a reason this page doesn't know): 05's
//   audio.soundCheck.failed and "Friends will see your screen without sound".
// Try again runs it again. The step never blocks the wizard: Continue is always there.
import { useCallback, useEffect, useRef, useState } from 'react';
import { useTranslation } from 'react-i18next';

import { useApp } from '../app/context';
import { Actions } from '../auth/AuthForm';
import { Notice } from '../auth/Notice';
import { DOCS_URL } from '../conntest/links';
import { errorMessage } from '../lib/errorText';
import type { NativeBridge, SoundCheckResult } from '../platform/desktop/bridge';
import { Button } from '../ui/Button';
import { Spinner } from '../ui/Spinner';
import styles from './wizard.module.css';

/** The site's section on voice apps that can't be kept out on this PC (03 §8.4's [Learn more]). */
export const SOUND_HELP = `${DOCS_URL}troubleshooting#desktop-sound`;

type CheckState =
  | { kind: 'idle' }
  | { kind: 'running' }
  | { kind: 'done'; result: SoundCheckResult }
  | { kind: 'error'; error: unknown };

export interface SoundCheckProps {
  bridge: NativeBridge;
  /** Runs the check at once (the wizard's step); otherwise a button starts it (Settings → Audio). */
  auto?: boolean;
  /** The wizard's Continue: shown once the check has an answer (and while it runs, so it never blocks). */
  onContinue?: () => void;
}

/** One line of the result: the text and, for the good case, the measured level. */
function resultText(r: SoundCheckResult, t: (k: string, o?: Record<string, unknown>) => string): string {
  if (r.captured && r.backend === 'process-loopback') return t('desktop.setup.sound.ok');
  if (r.captured) return t('desktop.setup.sound.endpoint');
  switch (r.reason) {
    case 'no_device':
      return t('desktop.audio.banner.noOutputDevice');
    case 'device_busy':
      return t('desktop.audio.soundCheck.deviceBusy');
    default:
      return `${t('desktop.audio.soundCheck.failed')} ${t('desktop.setup.sound.withoutSound')}`;
  }
}

export function SoundCheck({ bridge, auto = false, onContinue }: SoundCheckProps) {
  const { t } = useTranslation();
  const { platform } = useApp();
  const [state, setState] = useState<CheckState>({ kind: auto ? 'running' : 'idle' });
  const alive = useRef(true);
  useEffect(
    () => () => {
      alive.current = false;
    },
    [],
  );

  const run = useCallback((): void => {
    setState({ kind: 'running' });
    bridge.audio.soundCheck().then(
      (result) => {
        if (alive.current) setState({ kind: 'done', result });
      },
      (error: unknown) => {
        if (alive.current) setState({ kind: 'error', error });
      },
    );
  }, [bridge]);

  const started = useRef(false);
  useEffect(() => {
    if (!auto || started.current) return;
    started.current = true;
    run();
  }, [auto, run]);

  const openSound = (): void => {
    bridge.app.openOSSettings('sound').catch(() => undefined);
  };

  let body;
  let retry = false;
  let busyDevice = false;
  let learnMore = false;
  switch (state.kind) {
    case 'idle':
      body = null;
      break;
    case 'running':
      body = (
        <p className={styles.checking} role="status">
          <Spinner size="sm" label={null} /> {t('desktop.setup.sound.playing')}
        </p>
      );
      break;
    case 'error':
      body = <Notice>{errorMessage(state.error, t)}</Notice>;
      retry = true;
      break;
    case 'done': {
      const r = state.result;
      const good = r.captured && r.backend === 'process-loopback';
      learnMore = r.captured && !good;
      busyDevice = !r.captured && r.reason === 'device_busy';
      retry = !good;
      body = (
        <div className={styles.stack} role="status" data-sound-check={good ? 'ok' : r.captured ? 'endpoint' : 'failed'}>
          <p>{resultText(r, t)}</p>
          {good && <p className={styles.muted}>{t('desktop.setup.sound.level', { level: Math.round(r.levelDb) })}</p>}
        </div>
      );
      break;
    }
  }

  return (
    <div className={styles.stack}>
      {body}
      <Actions>
        {state.kind === 'idle' && (
          <Button variant={onContinue ? 'primary' : 'secondary'} onClick={run}>
            {t('desktop.settings.audio.soundCheck')}
          </Button>
        )}
        {learnMore && (
          <Button
            onClick={() => {
              platform.openExternal(SOUND_HELP);
            }}
          >
            {t('desktop.setup.sound.learnMore')}
          </Button>
        )}
        {busyDevice && <Button onClick={openSound}>{t('desktop.audio.openSoundSettings')}</Button>}
        {retry && <Button onClick={run}>{t('desktop.setup.tryAgain')}</Button>}
        {onContinue && (
          <Button variant="primary" onClick={onContinue}>
            {t('desktop.setup.continue')}
          </Button>
        )}
      </Actions>
    </div>
  );
}
