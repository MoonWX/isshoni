// Settings → Updates (#updates, docs/m2/03 §14.4, §16.2): Check for updates automatically (updates.check, on;
// greyed out with "Turned off by your administrator" when policyOff), Get release candidates (beta channel)
// (updates.channel, off), Check now (Updates.Check(), which always works, 06 §8.6), the status line of §16.2 with
// Restart and update when ready (UpdateParts.tsx), and "Updates come only from GitHub Releases" (never from the
// connected server, PLAN).
import { useState } from 'react';
import { useTranslation } from 'react-i18next';

import type { NativeBridge, UpdateStatus } from '../platform/desktop/bridge';
import { Button } from '../ui/Button';
import { SettingsSection, SettingSwitch } from './SettingsParts';
import { bridgeText, isObject, type SettingsResult } from './settingsState';
import { UpdateStatusLine } from './UpdateParts';
import styles from './settings.module.css';

export interface UpdatesSettingsProps {
  bridge: NativeBridge;
  settings: SettingsResult;
  status: UpdateStatus | null;
  setStatus: (s: UpdateStatus) => void;
}

export function UpdatesSettings({ bridge, settings, status, setStatus }: UpdatesSettingsProps) {
  const { t } = useTranslation();
  const [checking, setChecking] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const { view, update } = settings;
  const policyOff = view?.updates.policyOff === true || status?.policyOff === true;

  return (
    <SettingsSection id="updates" title={t('desktop.settings.updates.title')}>
      <SettingSwitch
        label={t('desktop.settings.updates.check')}
        hint={policyOff ? t('desktop.update.state.offPolicy') : undefined}
        checked={view === null ? null : view.updates.check && !policyOff}
        disabled={policyOff}
        onChange={(on) => update({ updates: { check: on } })}
      />
      <SettingSwitch
        label={t('desktop.settings.updates.beta')}
        hint={t('desktop.settings.updates.betaHint')}
        checked={view === null ? null : view.updates.channel === 'beta'}
        onChange={(on) => update({ updates: { channel: on ? 'beta' : 'stable' } })}
      />
      {status !== null && <UpdateStatusLine bridge={bridge} status={status} />}
      {error !== null && (
        <p role="alert" className={styles.warning}>
          {error}
        </p>
      )}
      <div className={styles.actions}>
        <Button
          size="sm"
          loading={checking || status?.state === 'checking'}
          onClick={() => {
            setChecking(true);
            setError(null);
            bridge.updates
              .check()
              .then(
                (s) => {
                  if (isObject(s)) setStatus(s);
                },
                (err: unknown) => {
                  setError(bridgeText(t, err));
                },
              )
              .finally(() => {
                setChecking(false);
              });
          }}
        >
          {t('desktop.settings.updates.checkNow')}
        </Button>
      </div>
      <p className={styles.muted}>{t('desktop.settings.updates.source')}</p>
    </SettingsSection>
  );
}
