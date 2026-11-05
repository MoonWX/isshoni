// The Cisco OpenH264 opt-in of Settings → Sharing (docs/m2/03 §14.4, §10.5; 04 §9.9, §12.5). OpenH264 is never
// shipped: the user turns it on here after reading Cisco's notice, Go downloads Cisco's pinned binary and checks its
// size and SHA-256 (native.InstallOpenH264; 04 owns the download and the checksum), and turning it off deletes the
// file. While it is installed, About shows Cisco's attribution "OpenH264 Video Codec provided by Cisco Systems, Inc."
//
// The control is the bound Settings.OpenH264, EnableOpenH264 and DisableOpenH264 (README WA84: Go installs and removes
// through WA51's helpers); Go answers state unavailable where 04 doesn't offer it, which hides the opt-in. The failures
// of 04 §9.9 have their texts here: dependency with reason download (the network: trying again may help), corrupt
// (Cisco's file didn't match the pinned size or hash), missing, version or blocked_by_policy; cancelled and timeout.
import { useCallback, useEffect, useState } from 'react';
import { useTranslation } from 'react-i18next';

import type { NativeBridge, OpenH264Status } from '../platform/desktop/bridge';
import { Button } from '../ui/Button';
import { failureOf } from './authText';
import { bridgeText, isObject } from './settingsState';
import { OutLink } from './UpdateParts';
import styles from './settings.module.css';

/** Cisco's binary license, which applies to the downloaded DLL (04 §17). */
export const OPENH264_LICENSE_URL = 'https://www.openh264.org/BINARY_LICENSE.txt';

export type { OpenH264Status };

/** What the opt-in needs from Go: the bound Settings methods (openH264Control), or a test's fake. */
export interface OpenH264Control {
  status(): Promise<OpenH264Status>;
  /** Downloads Cisco's binary, verifies it and turns it on; rejects with the bridge error of 04 §9.9. */
  enable(): Promise<OpenH264Status>;
  /** Turns it off and deletes the file. */
  disable(): Promise<OpenH264Status>;
}

/** The opt-in's control over the bound Settings methods (docs/m2/03 §14.4). */
export function openH264Control(bridge: NativeBridge): OpenH264Control {
  return {
    status: () => bridge.settings.openH264(),
    enable: () => bridge.settings.enableOpenH264(),
    disable: () => bridge.settings.disableOpenH264(),
  };
}

/** The reasons of 04 §9.9 that have a text of their own. */
const REASON_KEYS: Readonly<Record<string, string>> = {
  download: 'desktop.settings.openh264.error.download',
  corrupt: 'desktop.settings.openh264.error.corrupt',
  missing: 'desktop.settings.openh264.error.missing',
  version: 'desktop.settings.openh264.error.version',
  blocked_by_policy: 'desktop.settings.openh264.error.blockedByPolicy',
};

/** The text of a failed enable or disable (04 §9.9's codes and reasons). */
export function openH264ErrorText(t: ReturnType<typeof useTranslation>['t'], err: unknown): string {
  const { code, params } = failureOf(err);
  const reason = typeof params['reason'] === 'string' ? params['reason'] : '';
  const byReason = REASON_KEYS[reason];
  if (byReason !== undefined) return t(byReason);
  if (code === 'engine.cancelled') return t('desktop.settings.openh264.error.cancelled');
  if (code === 'engine.timeout') return t('desktop.settings.openh264.error.timeout');
  return bridgeText(t, err);
}

export interface OpenH264State {
  status: OpenH264Status | null;
  set: (s: OpenH264Status) => void;
}

/** The opt-in's status, read once; null without a control or until it answered. */
export function useOpenH264(control: OpenH264Control | null): OpenH264State {
  const [status, setStatus] = useState<OpenH264Status | null>(null);
  useEffect(() => {
    if (control === null) return undefined;
    let live = true;
    control.status().then(
      (s) => {
        if (live && isObject(s)) setStatus(s);
      },
      () => undefined,
    );
    return () => {
      live = false;
    };
  }, [control]);
  const set = useCallback((s: OpenH264Status) => {
    setStatus(s);
  }, []);
  return { status, set };
}

export interface OpenH264OptInProps {
  control: OpenH264Control;
  state: OpenH264State;
}

/** The opt-in: Cisco's notice, Enable Cisco OpenH264…, or Turn off and delete once installed. */
export function OpenH264OptIn({ control, state }: OpenH264OptInProps) {
  const { t } = useTranslation();
  const [busy, setBusy] = useState<'enable' | 'disable' | null>(null);
  const [error, setError] = useState<string | null>(null);
  const { status } = state;
  if (status === null || status.state === 'unavailable') return null;

  const run = (kind: 'enable' | 'disable'): void => {
    setBusy(kind);
    setError(null);
    (kind === 'enable' ? control.enable() : control.disable())
      .then(state.set, (err: unknown) => {
        setError(openH264ErrorText(t, err));
      })
      .finally(() => {
        setBusy(null);
      });
  };

  return (
    <div className={styles.group} data-testid="openh264">
      <h3 className={styles.subtitle}>{t('desktop.settings.openh264.title')}</h3>
      {status.state === 'on' ? (
        <>
          <p role="status">{t('desktop.settings.openh264.on', { version: status.version })}</p>
          <p className={styles.muted}>{t('desktop.settings.openh264.attribution')}</p>
        </>
      ) : (
        <>
          <p>{t('desktop.settings.openh264.notice', { version: status.version })}</p>
          <p className={styles.muted}>{t('desktop.settings.openh264.attribution')}</p>
        </>
      )}
      <p className={styles.muted}>
        <OutLink href={OPENH264_LICENSE_URL}>{t('desktop.settings.openh264.license')}</OutLink>
      </p>
      {busy === 'enable' && <p role="status">{t('desktop.settings.openh264.installing')}</p>}
      {error !== null && (
        <p role="alert" className={styles.warning}>
          {error}
        </p>
      )}
      <div className={styles.actions}>
        {status.state === 'on' ? (
          <Button
            size="sm"
            variant="danger"
            loading={busy === 'disable'}
            onClick={() => {
              run('disable');
            }}
          >
            {t('desktop.settings.openh264.disable')}
          </Button>
        ) : (
          <Button
            size="sm"
            loading={busy === 'enable'}
            onClick={() => {
              run('enable');
            }}
          >
            {t('desktop.settings.openh264.enable')}
          </Button>
        )}
      </div>
    </div>
  );
}
