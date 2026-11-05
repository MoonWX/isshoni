// The update UI of the desktop app (docs/m2/03 §16.2; 06 owns the updater and its states, 06 §8.9): the status line
// of Settings → Updates, Restart and update, and the banner of a required update at the top of every page.
//
// Per state (UpdateStatus.state):
//   up_to_date   "Up to date, checked <time>" (checkedAt)
//   downloading  "Downloading {available}… {progress} %"
//   ready        "isshoni {available} is ready" [Restart and update]; with `required` also the banner
//   error        manifest_expired: the out-of-date text, the clock line and the project site's
//                /download#windows-updates; install_failed: "The update didn't install" with the same link; any other
//                code: desktop.update.error.<code> (Check now is always next to the line)
//   off          "Turned off by your administrator" with policyOff, else "Turned off"
//   disabled     "Updates are off in this build" (not a release build)
//   idle         "Not checked yet"; checking: "Checking for updates…"
// Restart and update runs Updates.Install(); never while a share is live: the button says "Finish sharing first" and
// is disabled, and Go answers busy if a share starts meanwhile (03 enforces it, the updater isn't told).
import { RefreshCw } from 'lucide-react';
import { useState } from 'react';
import { useTranslation } from 'react-i18next';

import { usePlatform } from '../app/context';
import { DOCS_URL } from '../conntest/links';
import type { NativeBridge, UpdateStatus } from '../platform/desktop/bridge';
import { Button } from '../ui/Button';
import { failureOf } from './authText';
import { bridgeText, checkedAtText, useSharing, useUpdateStatus } from './settingsState';
import styles from './settings.module.css';

/** The project site's section on desktop updates (06 §8.5, §8.7). */
export const UPDATES_HELP = `${DOCS_URL}download#windows-updates`;

/** A link to the system browser (the desktop never navigates away, 03 §5.5). */
export function OutLink({ href, children }: { href: string; children: string }) {
  const platform = usePlatform();
  return (
    <a
      href={href}
      target="_blank"
      rel="noopener noreferrer"
      onClick={(e) => {
        e.preventDefault();
        platform.openExternal(href);
      }}
    >
      {children}
    </a>
  );
}

/** Restart and update (Updates.Install()), or "Finish sharing first" while a share is live. */
export function RestartButton({ bridge }: { bridge: NativeBridge }) {
  const { t } = useTranslation();
  const sharing = useSharing(bridge);
  const [busy, setBusy] = useState(false);
  const [refused, setRefused] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const blocked = sharing || refused;
  return (
    <>
      <Button
        variant="primary"
        size="sm"
        loading={busy}
        disabled={blocked && !busy}
        onClick={() => {
          setBusy(true);
          setError(null);
          bridge.updates.install().then(
            () => {
              // The app quits within 5 s and the installer starts it again: the button stays busy.
            },
            (err: unknown) => {
              setBusy(false);
              if (failureOf(err).code === 'busy') setRefused(true);
              else setError(bridgeText(t, err));
            },
          );
        }}
      >
        {blocked ? t('desktop.update.finishSharing') : t('desktop.update.restart')}
      </Button>
      {error !== null && (
        <p role="alert" className={styles.warning}>
          {error}
        </p>
      )}
    </>
  );
}

/** The status line's text and what goes with it. */
export interface UpdateLineView {
  text: string;
  /** Extra lines (manifest_expired's clock line). */
  more: string[];
  /** The project site's update help (manifest_expired, install_failed). */
  help: boolean;
  restart: boolean;
  error: boolean;
}

/** What the status line says for a status (03 §16.2). */
export function updateLineView(
  t: ReturnType<typeof useTranslation>['t'],
  s: UpdateStatus,
  lng: string,
): UpdateLineView {
  const base = { more: [], help: false, restart: false, error: false };
  switch (s.state) {
    case 'up_to_date': {
      const time = checkedAtText(s.checkedAt, lng);
      return {
        ...base,
        text: time === null ? t('desktop.update.state.upToDateNow') : t('desktop.update.state.upToDate', { time }),
      };
    }
    case 'downloading':
      return {
        ...base,
        text: t('desktop.update.state.downloading', {
          version: s.available,
          progress: Math.round(Math.min(1, Math.max(0, s.progress)) * 100),
        }),
      };
    case 'ready':
      return { ...base, text: t('desktop.update.state.ready', { version: s.available }), restart: true };
    case 'error':
      if (s.errorCode === 'manifest_expired') {
        return {
          ...base,
          text: t('desktop.update.expired.text'),
          more: [t('desktop.update.expired.clock')],
          help: true,
          error: true,
        };
      }
      if (s.errorCode === 'install_failed') {
        return { ...base, text: t('desktop.update.installFailed'), help: true, error: true };
      }
      return { ...base, text: updateErrorText(t, s.errorCode), error: true };
    case 'off':
      return { ...base, text: s.policyOff ? t('desktop.update.state.offPolicy') : t('desktop.update.state.off') };
    case 'disabled':
      return { ...base, text: t('desktop.update.state.disabled') };
    case 'checking':
      return { ...base, text: t('desktop.update.state.checking') };
    case 'idle':
      return { ...base, text: t('desktop.update.state.idle') };
  }
}

/** desktop.update.error.<code> for an updater code (06 §8.10), the generic one for a code this build doesn't know. */
export function updateErrorText(t: ReturnType<typeof useTranslation>['t'], code: string): string {
  return bridgeText(t, { code: `update.${code === '' ? 'unknown' : code}` });
}

/** The status line of Settings → Updates (and of the version screen's Update the app). */
export function UpdateStatusLine({ bridge, status }: { bridge: NativeBridge; status: UpdateStatus }) {
  const { t, i18n } = useTranslation();
  const view = updateLineView(t, status, i18n.resolvedLanguage ?? i18n.language);
  return (
    <div className={styles.group} data-testid="update-status" data-state={status.state}>
      <p role={view.error ? 'alert' : 'status'}>{view.text}</p>
      {view.more.map((line) => (
        <p key={line} className={styles.muted}>
          {line}
        </p>
      ))}
      {view.help && (
        <p className={styles.muted}>
          <OutLink href={UPDATES_HELP}>{t('desktop.update.help')}</OutLink>
        </p>
      )}
      {view.restart && (
        <div className={styles.actions}>
          <RestartButton bridge={bridge} />
        </div>
      )}
    </div>
  );
}

/**
 * The banner of a required update (03 §16.2): "An important update is ready" [Restart and update], at the top of
 * every page while the status is ready with `required`. It can't be dismissed. The app shell mounts it in the desktop
 * app only (app/App.tsx loads it on demand).
 */
export function RequiredUpdateBanner() {
  const platform = usePlatform();
  const desktop = platform.desktop ?? null;
  if (desktop === null) return null;
  return <RequiredBanner bridge={desktop.bridge} />;
}

function RequiredBanner({ bridge }: { bridge: NativeBridge }) {
  const { t } = useTranslation();
  const [status] = useUpdateStatus(bridge);
  if (status === null || status.state !== 'ready' || !status.required) return null;
  return (
    <section className={styles.banner} aria-label={t('desktop.update.required.label')}>
      <RefreshCw aria-hidden="true" width={16} height={16} />
      <p>{t('desktop.update.required.text')}</p>
      <RestartButton bridge={bridge} />
    </section>
  );
}
