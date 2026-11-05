// Settings → Sharing (#sharing, docs/m2/03 §14.4): the default "Optimize for" on this server
// (serverPrefs[origin].preset), the Sharing pill (Only when Windows shows no border / Always, §11.4), the Pause and
// Stop hotkeys (HotkeyField with their status, §11.2), "Show the first-share sheet again" (tips.firstShareShown),
// the video encoders 04 found (Share.Capabilities: NVENC, Quick Sync, AMF, Media Foundation, OpenH264) or why none
// works, and the Cisco OpenH264 opt-in when it is offered (OpenH264OptIn.tsx).
//
// The quiet per-encoder notes of §10.5 ("Update your graphics driver (NVIDIA 531.61 or newer)" for an NVENC with
// driver_too_old while another encoder works) need the probe's per-encoder statuses, which ShareCaps doesn't carry
// yet (§10.5, §22.3), so until the bridge sends them the section lists the usable encoders and, when none works,
// why (ShareCaps.reason).
import { useCallback, useEffect, useId, useState } from 'react';
import { useTranslation } from 'react-i18next';

import { i18n } from '../i18n';
import type { HotkeyStatus, NativeBridge, SettingsView, ShareCaps } from '../platform/desktop/bridge';
import type { Preset } from '../protocol/types.gen';
import { Button } from '../ui/Button';
import { HotkeyField } from './HotkeyField';
import type { HotkeyId } from './hotkeys';
import { OpenH264OptIn, type OpenH264Control, type OpenH264State } from './OpenH264OptIn';
import { SettingsSection, useBridgeFailure } from './SettingsParts';
import { isObject, type SettingsResult } from './settingsState';
import styles from './settings.module.css';

const PRESETS: readonly Preset[] = ['auto', 'game', 'movie', 'text'];
const PILL_MODES: readonly SettingsView['pill']['mode'][] = ['auto', 'always'];
const HOTKEY_IDS: readonly HotkeyId[] = ['pause', 'stop'];

/** The i18n key of an encoder's kind token (media.EncoderInfo.Name, 04 §9.1). */
const ENCODER_KEYS: Readonly<Record<string, string>> = {
  nvenc: 'desktop.settings.encoder.names.nvenc',
  qsv: 'desktop.settings.encoder.names.qsv',
  amf: 'desktop.settings.encoder.names.amf',
  'mf-hw': 'desktop.settings.encoder.names.mfHw',
  'mf-sw': 'desktop.settings.encoder.names.mfSw',
  openh264: 'desktop.settings.encoder.names.openh264',
  fake: 'desktop.settings.encoder.names.fake',
};

/** An encoder's display name; an unknown token as it is. */
export function encoderName(t: ReturnType<typeof useTranslation>['t'], token: string): string {
  const key = ENCODER_KEYS[token];
  return key === undefined ? token : t(key);
}

/** Why sharing can't work here (ShareCaps.reason, a media code), as desktop.errors.engine.<code>. */
function capsReasonText(t: ReturnType<typeof useTranslation>['t'], reason: string): string {
  const key = `desktop.errors.engine.${reason}`;
  return reason !== '' && i18n.exists(key) ? t(key) : t('desktop.errors.engine.no_encoder');
}

export interface SharingSettingsProps {
  bridge: NativeBridge;
  /** The active server's origin, whose "Optimize for" this section sets ('' before the first server). */
  origin: string;
  settings: SettingsResult;
  openH264: { control: OpenH264Control; state: OpenH264State } | null;
}

export function SharingSettings({ bridge, origin, settings, openH264 }: SharingSettingsProps) {
  const { t } = useTranslation();
  const fail = useBridgeFailure();
  const { view, update } = settings;
  const [statuses, setStatuses] = useState<HotkeyStatus[]>([]);
  const [caps, setCaps] = useState<ShareCaps | null>(null);
  const [sheetAgain, setSheetAgain] = useState(false);
  const presetId = useId();

  const loadHotkeys = useCallback(() => {
    bridge.settings.hotkeys().then(
      (s) => {
        setStatuses(Array.isArray(s) ? s : []);
      },
      () => undefined,
    );
  }, [bridge]);
  useEffect(loadHotkeys, [loadHotkeys]);
  useEffect(() => {
    let live = true;
    bridge.share.capabilities().then(
      (c) => {
        if (live && isObject(c)) setCaps(c);
      },
      () => undefined,
    );
    return () => {
      live = false;
    };
  }, [bridge]);

  const preset: Preset = (origin !== '' ? view?.serverPrefs[origin]?.preset : undefined) ?? 'auto';
  const statusOf = (id: string): HotkeyStatus | null => statuses.find((s) => s.id === id) ?? null;
  const setPill = (mode: SettingsView['pill']['mode']): void => {
    update({ pill: { mode } }).catch(fail);
  };

  return (
    <SettingsSection id="sharing" title={t('desktop.settings.sharing.title')}>
      {origin !== '' && (
        <div className={styles.row}>
          <label htmlFor={presetId}>{t('desktop.settings.sharing.preset')}</label>
          <select
            id={presetId}
            value={preset}
            disabled={view === null}
            onChange={(ev) => {
              update({ serverPrefs: { [origin]: { preset: ev.target.value as Preset } } }).catch(fail);
            }}
          >
            {PRESETS.map((p) => (
              <option key={p} value={p}>
                {t(`share.preset.${p}.name`)}
              </option>
            ))}
          </select>
        </div>
      )}

      <fieldset className={styles.group}>
        <legend>{t('desktop.settings.sharing.pill.legend')}</legend>
        {PILL_MODES.map((mode) => (
          <label key={mode} className={styles.choice}>
            <input
              type="radio"
              name="pill-mode"
              value={mode}
              checked={view?.pill.mode === mode}
              disabled={view === null}
              onChange={() => {
                setPill(mode);
              }}
            />
            <span>{t(`desktop.settings.sharing.pill.${mode}`)}</span>
          </label>
        ))}
        <p className={styles.muted}>{t('desktop.settings.sharing.pill.hint')}</p>
      </fieldset>

      <div className={styles.group}>
        <h3 className={styles.subtitle}>{t('desktop.settings.hotkeys.title')}</h3>
        {HOTKEY_IDS.map((id) => (
          <HotkeyField
            key={id}
            id={id}
            bridge={bridge}
            combo={view?.hotkeys[id] ?? null}
            other={view?.hotkeys[id === 'pause' ? 'stop' : 'pause'] ?? null}
            status={statusOf(id)}
            save={update}
            onSaved={loadHotkeys}
          />
        ))}
      </div>

      <div className={styles.group}>
        <div className={styles.actions}>
          <Button
            size="sm"
            disabled={view === null || !view.tips.firstShareShown}
            onClick={() => {
              update({ tips: { firstShareShown: false } }).then(() => {
                setSheetAgain(true);
              }, fail);
            }}
          >
            {t('desktop.settings.sharing.firstShare')}
          </Button>
        </div>
        {(sheetAgain || view?.tips.firstShareShown === false) && (
          <p className={styles.muted} role="status">
            {t('desktop.settings.sharing.firstShareNext')}
          </p>
        )}
      </div>

      <div className={styles.group} data-testid="encoders">
        <h3 className={styles.subtitle}>{t('desktop.settings.encoder.title')}</h3>
        {caps === null ? (
          <p className={styles.muted}>{t('desktop.settings.encoder.unknown')}</p>
        ) : caps.encoders.length > 0 ? (
          <p>
            {t('desktop.settings.encoder.found', { encoders: caps.encoders.map((e) => encoderName(t, e)).join(', ') })}
          </p>
        ) : (
          <p role="status">{capsReasonText(t, caps.reason ?? '')}</p>
        )}
        {caps !== null && !caps.canShare && caps.encoders.length > 0 && (
          <p role="status">{capsReasonText(t, caps.reason ?? '')}</p>
        )}
      </div>

      {openH264 !== null && <OpenH264OptIn control={openH264.control} state={openH264.state} />}
    </SettingsSection>
  );
}
