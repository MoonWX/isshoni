// Settings → App (#app, docs/m2/03 §14.4): Start isshoni when I sign in to Windows (autostart, off), Keep isshoni
// running in the tray when I close the window (closeToTray, off), Hide isshoni windows from screen capture
// (hideFromCapture, on, §11.5), Show notifications (on; the safety notices of §15 always show), Keep the tray icon
// visible (opens Windows settings: App.OpenOSSettings('taskbar')), and Quit isshoni, which asks "Stop sharing and
// quit?" while a share is live (§11.6) and then calls App.Quit(stopShare).
import { useState } from 'react';
import { useTranslation } from 'react-i18next';

import type { NativeBridge } from '../platform/desktop/bridge';
import { Button } from '../ui/Button';
import { Dialog } from '../ui/Dialog';
import { SettingsSection, SettingSwitch, useBridgeFailure } from './SettingsParts';
import { useSharing, type SettingsResult } from './settingsState';
import styles from './settings.module.css';

export function AppSettings({ bridge, settings }: { bridge: NativeBridge; settings: SettingsResult }) {
  const { t } = useTranslation();
  const fail = useBridgeFailure();
  const sharing = useSharing(bridge);
  const [asking, setAsking] = useState(false);
  const [quitting, setQuitting] = useState(false);
  const { view, update } = settings;

  const quit = (stopShare: boolean): void => {
    setAsking(false);
    setQuitting(true);
    bridge.app.quit(stopShare).catch((err: unknown) => {
      setQuitting(false);
      fail(err);
    });
  };

  return (
    <SettingsSection id="app" title={t('desktop.settings.app.title')}>
      <SettingSwitch
        label={t('desktop.settings.app.autostart')}
        checked={view?.autostart ?? null}
        onChange={(on) => update({ autostart: on })}
      />
      <SettingSwitch
        label={t('desktop.settings.app.closeToTray')}
        checked={view?.closeToTray ?? null}
        onChange={(on) => update({ closeToTray: on })}
      />
      <SettingSwitch
        label={t('desktop.settings.app.hideFromCapture')}
        hint={t('desktop.settings.app.hideFromCaptureHint')}
        checked={view?.hideFromCapture ?? null}
        onChange={(on) => update({ hideFromCapture: on })}
      />
      <SettingSwitch
        label={t('desktop.settings.app.notifications')}
        hint={t('desktop.settings.app.notificationsHint')}
        checked={view?.notifications ?? null}
        onChange={(on) => update({ notifications: on })}
      />
      <div className={styles.group}>
        <div className={styles.actions}>
          <Button
            size="sm"
            onClick={() => {
              bridge.app.openOSSettings('taskbar').catch(fail);
            }}
          >
            {t('desktop.settings.app.tray')}
          </Button>
        </div>
        <p className={styles.muted}>{t('desktop.settings.app.trayHint')}</p>
      </div>
      <div className={styles.actions}>
        <Button
          variant="danger"
          size="sm"
          loading={quitting}
          onClick={() => {
            if (sharing) setAsking(true);
            else quit(false);
          }}
        >
          {t('desktop.settings.app.quit')}
        </Button>
      </div>
      <Dialog
        open={asking}
        size="sm"
        onClose={() => {
          setAsking(false);
        }}
        title={t('desktop.settings.app.quitConfirm')}
        footer={
          <>
            <Button
              onClick={() => {
                setAsking(false);
              }}
            >
              {t('desktop.setup.cancel')}
            </Button>
            <Button
              variant="danger"
              onClick={() => {
                quit(true);
              }}
            >
              {t('desktop.settings.app.quitAction')}
            </Button>
          </>
        }
      >
        <p>{t('desktop.settings.app.quitBody')}</p>
      </Dialog>
    </SettingsSection>
  );
}
