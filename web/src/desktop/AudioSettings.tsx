// The audio parts of the desktop pages (docs/m2/03 §8.2's voice step, §14.4's Settings → Audio; 05 §14.1–§14.5).
// Both watch the app list while shown (Audio.Watch(true) on mount, false on unmount), so the voice apps' "running"
// flags are current.
// - VoiceStep, the wizard's last step on the first install: "Friends won't hear:" with 05's built-in voice apps
//   (FriendsHear.voiceApps, "running" from their flag), "apps that use your microphone (you'll be asked)" with the
//   toggle Keep out apps that use my microphone (Audio.SetKeepOutMicUsers, on by default), and "isshoni itself"; the
//   note "Change this anytime in Settings → Audio"; Done.
// - AudioSettings, Settings → Audio (#audio): the built-in list as locked text, "Always kept out: Discord,
//   TeamSpeak, …"; the mic toggle; the per-app decisions (Audio.AppRules: Kept out, Included, with Change through
//   Audio.SetAppDecision, "Decide for me" forgetting it), the built-in voice apps listed as locked; the ducking tip's
//   text with [Open sound settings] (App.OpenOSSettings('sound-communications')); and Run the sound check again.
//   There is no sound-mode choice: M2 has no "All sound" mode (03 §6.2, README §3 item 15).
import { useCallback, useEffect, useId, useRef, useState } from 'react';
import { useTranslation } from 'react-i18next';

import { useApp } from '../app/context';
import { Actions } from '../auth/AuthForm';
import { errorMessage } from '../lib/errorText';
import type { AppRule, Decision, FriendsHear, NativeBridge } from '../platform/desktop/bridge';
import { useFriendsHear } from '../share/native/useAudio';
import { Button } from '../ui/Button';
import { SoundCheck } from './SoundCheck';
import styles from './wizard.module.css';

/** Toasts a failed bridge call with its text. */
function useFailure(): (err: unknown) => void {
  const { t } = useTranslation();
  const { ui } = useApp();
  return useCallback(
    (err: unknown) => {
      ui.getState().toast({ kind: 'error', message: errorMessage(err, t) });
    },
    [t, ui],
  );
}

/** "Keep out apps that use my microphone": FriendsHear.keepOutMicUsers, changed with Audio.SetKeepOutMicUsers. */
function MicToggle({ fh, bridge }: { fh: FriendsHear | null; bridge: NativeBridge }) {
  const { t } = useTranslation();
  const fail = useFailure();
  const [pending, setPending] = useState<boolean | null>(null);
  const checked = pending ?? fh?.keepOutMicUsers ?? true;
  return (
    <label className={styles.toggle}>
      <input
        type="checkbox"
        role="switch"
        checked={checked}
        disabled={fh === null || pending !== null}
        onChange={(ev) => {
          const on = ev.target.checked;
          setPending(on);
          bridge.audio
            .setKeepOutMicUsers(on)
            .catch(fail)
            .finally(() => {
              setPending(null);
            });
        }}
      />
      <span>{t('desktop.audio.keepOutMic')}</span>
    </label>
  );
}

/** The voice step (03 §8.2): what friends never hear, the mic toggle, Done. */
export function VoiceStep({ bridge, onDone }: { bridge: NativeBridge; onDone: () => void }) {
  const { t } = useTranslation();
  const fh = useFriendsHear(bridge, { watch: true });
  return (
    <div className={styles.stack}>
      <p>{t('desktop.setup.voice.lead')}</p>
      <ul className={styles.list} data-testid="voice-apps">
        {(fh?.voiceApps ?? []).map((app) => (
          <li key={app.id} data-running={app.running}>
            {app.running ? t('desktop.setup.voice.running', { app: app.name }) : app.name}
          </li>
        ))}
        <li>{t('desktop.setup.voice.micApps')}</li>
        <li>{t('desktop.setup.voice.isshoni')}</li>
      </ul>
      <MicToggle fh={fh} bridge={bridge} />
      <p className={styles.muted}>{t('desktop.setup.voice.note')}</p>
      <Actions>
        <Button variant="primary" onClick={onDone}>
          {t('desktop.setup.done')}
        </Button>
      </Actions>
    </div>
  );
}

/** One remembered decision with its Change select. */
function RuleRow({ rule, bridge, onChanged }: { rule: AppRule; bridge: NativeBridge; onChanged: () => void }) {
  const { t } = useTranslation();
  const fail = useFailure();
  const [busy, setBusy] = useState(false);
  return (
    <li className={styles.rule} data-app-id={rule.id}>
      <span>{rule.name}</span>
      <select
        value={rule.decision}
        disabled={busy}
        aria-label={t('desktop.settings.audio.change', { app: rule.name })}
        onChange={(ev) => {
          setBusy(true);
          bridge.audio
            .setAppDecision(rule.id, ev.target.value as Decision)
            .then(onChanged, fail)
            .finally(() => {
              setBusy(false);
            });
        }}
      >
        <option value="keep-out">{t('desktop.settings.audio.keptOut')}</option>
        <option value="include">{t('desktop.settings.audio.included')}</option>
        <option value="auto">{t('desktop.settings.audio.forget')}</option>
      </select>
    </li>
  );
}

/** What of a report can change the remembered decisions: each row's decision and the mic toggle. */
function decisionsKey(fh: FriendsHear): string {
  return (
    [...fh.heard, ...fh.keptOut, ...fh.notCaptured]
      .map((r) => `${r.id}:${r.decision}`)
      .sort()
      .join(',') + `|${String(fh.keepOutMicUsers)}`
  );
}

/** Settings → Audio (03 §14.4). */
export function AudioSettings({ bridge }: { bridge: NativeBridge }) {
  const { t } = useTranslation();
  const fail = useFailure();
  const fh = useFriendsHear(bridge, { watch: true });
  const [rules, setRules] = useState<AppRule[] | null>(null);
  const headingId = useId();

  const loadRules = useCallback((): void => {
    bridge.audio.appRules().then((r) => {
      setRules(Array.isArray(r) ? r : []);
    }, fail);
  }, [bridge, fail]);
  useEffect(loadRules, [loadRules]);
  // A decision made elsewhere (a row of the share panel, a prompt) changes the report's decisions: read the rules
  // again then. Not on every report: during a share 05 sends up to 4 a second for levels and playing changes alone.
  const key = fh === null ? null : decisionsKey(fh);
  const lastKey = useRef<string | null>(null);
  useEffect(() => {
    if (key === null) return;
    const prev = lastKey.current;
    lastKey.current = key;
    if (prev !== null && prev !== key) loadRules();
  }, [key, loadRules]);

  const voiceApps = fh?.voiceApps ?? [];
  return (
    <section id="audio" className={styles.section} aria-labelledby={headingId}>
      <h2 id={headingId} className={styles.sectionTitle}>
        {t('desktop.settings.audio.title')}
      </h2>
      {voiceApps.length > 0 && (
        <p data-testid="always-kept-out">
          {t('desktop.settings.audio.alwaysKeptOut', { apps: voiceApps.map((a) => a.name).join(', ') })}
        </p>
      )}
      <MicToggle fh={fh} bridge={bridge} />
      <h3 className={styles.sectionTitle}>{t('desktop.settings.audio.rules')}</h3>
      <ul className={styles.servers}>
        {(rules ?? []).map((rule) => (
          <RuleRow key={rule.id} rule={rule} bridge={bridge} onChanged={loadRules} />
        ))}
        {voiceApps.map((app) => (
          <li key={app.id} className={styles.rule} data-locked="true">
            <span>{app.name}</span>
            <span className={styles.muted}>{t('desktop.settings.audio.locked')}</span>
          </li>
        ))}
      </ul>
      {rules !== null && rules.length === 0 && <p className={styles.muted}>{t('desktop.settings.audio.noRules')}</p>}
      <p className={styles.muted}>{t('desktop.audio.tip.ducking')}</p>
      <Actions>
        <Button
          onClick={() => {
            bridge.app.openOSSettings('sound-communications').catch(fail);
          }}
        >
          {t('desktop.audio.openSoundSettings')}
        </Button>
      </Actions>
      <SoundCheck bridge={bridge} />
    </section>
  );
}
