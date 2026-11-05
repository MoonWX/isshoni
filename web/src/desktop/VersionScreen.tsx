// The version screen in the desktop app (docs/m2/03 §16.3, 02 §13.3): VersionMismatch's desktop variant, which
// app/screens/VersionMismatch.tsx loads on demand when the platform's versionActions() has the desktop's actions.
//   update_app     "This server needs a newer isshoni app. You have {app}; the server runs {server}."
//                  [Update the app] (versionActions().updateApp) and [Open in browser]
//   update_server  "This server runs isshoni {server}, which is too old for this app. Ask your admin to update it."
//                  [Open in browser]
// Both have [Try again] (Auth.Recheck(); a server that is compatible now reloads the page). Open in browser opens the
// current room on the server in the system browser (versionActions().openInBrowser). Update the app says what
// happened (AppUpdateOutcome): installing (the app quits and the installer starts it again), "No newer version is
// available yet", a build that can't update itself (its release notes opened), or the update error's text; each but
// installing offers "Get it from your server's Download page" (serverOrigin + '/download' in the system browser,
// never in the webview: the bundled SPA's /download has no buttons in the app, 06 §7.5).
import { RefreshCw } from 'lucide-react';
import { useState } from 'react';
import { useTranslation } from 'react-i18next';

import { usePlatform } from '../app/context';
import { ScreenFrame } from '../app/screens/ScreenFrame';
import { i18n } from '../i18n';
import type { AppUpdateOutcome, VersionActions } from '../platform/types';
import { Button } from '../ui/Button';
import { bridgeText } from './settingsState';
import styles from './settings.module.css';

export interface DesktopVersionMismatchProps {
  serverVersion?: string;
  /** update_server: only the admin can fix it. */
  serverOlder: boolean;
  actions: VersionActions;
}

/** The text of an outcome other than installing. */
export function outcomeText(t: ReturnType<typeof useTranslation>['t'], o: AppUpdateOutcome): string {
  switch (o.kind) {
    case 'installing':
      return t('desktop.version.installing');
    case 'up-to-date':
      return t('desktop.version.upToDate');
    case 'not-installed':
      return t('desktop.version.notInstalled');
    case 'error': {
      const key = `desktop.update.error.${o.code}`;
      if (i18n.exists(key)) return t(key);
      if (o.code === 'off') return t('desktop.update.state.off');
      if (o.code === 'disabled') return t('desktop.update.state.disabled');
      return bridgeText(t, { code: o.code });
    }
  }
}

export function DesktopVersionMismatch({ serverVersion, serverOlder, actions }: DesktopVersionMismatchProps) {
  const { t } = useTranslation();
  const platform = usePlatform();
  const desktop = platform.desktop ?? null;
  const [updating, setUpdating] = useState(false);
  const [outcome, setOutcome] = useState<AppUpdateOutcome | null>(null);
  const [rechecking, setRechecking] = useState(false);
  const appVersion = desktop?.app.version ?? __ISSHONI_VERSION__;
  const server = serverVersion !== undefined && serverVersion !== '' ? serverVersion : '?';
  const { updateApp, openInBrowser } = actions;

  const update = (): void => {
    if (!updateApp) return;
    setUpdating(true);
    setOutcome(null);
    updateApp()
      .then(setOutcome, () => {
        setOutcome({ kind: 'error', code: 'internal' });
      })
      .finally(() => {
        setUpdating(false);
      });
  };

  const recheck = (): void => {
    if (desktop === null) return;
    setRechecking(true);
    desktop.bridge.auth.recheck().then(
      (s) => {
        setRechecking(false);
        // Compatible now: the page starts again with this server.
        if (s.compat.verdict === 'ok') globalThis.location.reload();
      },
      () => {
        setRechecking(false);
      },
    );
  };

  const downloadPage = (): void => {
    if (platform.serverOrigin !== '') platform.openExternal(`${platform.serverOrigin}/download`);
  };

  return (
    <ScreenFrame
      icon={RefreshCw}
      tone="warning"
      title={serverOlder ? t('desktop.version.titleServer') : t('desktop.version.titleApp')}
      actions={
        <>
          {!serverOlder && updateApp && (
            <Button variant="primary" loading={updating} disabled={outcome?.kind === 'installing'} onClick={update}>
              {t('desktop.version.updateApp')}
            </Button>
          )}
          {openInBrowser && (
            <Button variant={serverOlder ? 'primary' : 'secondary'} onClick={openInBrowser}>
              {t('desktop.version.openInBrowser')}
            </Button>
          )}
        </>
      }
      footnote={
        <Button size="sm" variant="ghost" loading={rechecking} disabled={desktop === null} onClick={recheck}>
          {t('desktop.version.tryAgain')}
        </Button>
      }
    >
      <p>
        {serverOlder
          ? t('desktop.setup.version.updateServer', { server })
          : t('desktop.setup.version.updateApp', { app: appVersion, server })}
      </p>
      {updating && <p role="status">{t('desktop.version.checking')}</p>}
      {outcome !== null && (
        <div className={styles.outcome}>
          <p role={outcome.kind === 'error' ? 'alert' : 'status'}>{outcomeText(t, outcome)}</p>
          {outcome.kind !== 'installing' && platform.serverOrigin !== '' && (
            <Button size="sm" onClick={downloadPage}>
              {t('desktop.version.downloadPage')}
            </Button>
          )}
        </div>
      )}
    </ScreenFrame>
  );
}
