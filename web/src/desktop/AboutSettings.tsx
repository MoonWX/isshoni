// Settings → About (#about, docs/m2/03 §14.4, §17): the version, the Windows build, the WebView2 version and the
// engine summary (the usable encoders and the sound capture backend); the log detail (log.level: info, or debug for
// a bug report); Open logs folder (App.OpenLogsFolder); Copy version info, a text block with the app, Windows,
// WebView2 and engine versions and nothing else (no paths, no server, no names: it goes into bug reports); the
// third-party notices (App.OpenNotices opens the app's THIRD_PARTY_NOTICES.txt, and the bundled SPA's own
// /licenses.txt shows in a dialog: the desktop never navigates away, §5.5); Cisco's attribution while OpenH264 is
// installed (04 §9.9); and links to the project site's /privacy#desktop and /code-signing.
import { useEffect, useId, useState } from 'react';
import { useTranslation } from 'react-i18next';

import { useApp } from '../app/context';
import { DOCS_URL } from '../conntest/links';
import type { AppInfo, LogLevel, ShareCaps } from '../platform/desktop/bridge';
import type { DesktopFeatures } from '../platform/types';
import { Button } from '../ui/Button';
import { Dialog } from '../ui/Dialog';
import { Spinner } from '../ui/Spinner';
import type { OpenH264Status } from './OpenH264OptIn';
import { SettingsSection, useBridgeFailure } from './SettingsParts';
import { isObject, type SettingsResult } from './settingsState';
import { encoderName } from './SharingSettings';
import { OutLink } from './UpdateParts';
import styles from './settings.module.css';

export const PRIVACY_DESKTOP_URL = `${DOCS_URL}privacy#desktop`;
export const CODE_SIGNING_URL = `${DOCS_URL}code-signing`;
/** The bundled SPA's licenses (scripts/licenses.mjs writes it into the build). */
export const SPA_LICENSES_PATH = '/licenses.txt';

/** WebView2's version from its user agent ("… Edg/131.0.2903.70"); null when it doesn't say. */
export function webViewVersion(userAgent: string): string | null {
  const m = /\bEdg\/([0-9][0-9.]*)/.exec(userAgent);
  return m?.[1] ?? null;
}

export interface VersionFacts {
  version: string;
  arch: string;
  osBuild: number;
  webView: string | null;
  encoders: string[];
  audioBackend: ShareCaps['audioBackend'] | null;
}

/**
 * Copy version info's text (03 §14.4): app, Windows, WebView2 and engine, one per line. Plain English on purpose,
 * the same for every reader of a bug report; no paths, no server, no user or machine names.
 */
export function versionInfoText(f: VersionFacts): string {
  return [
    `isshoni ${f.version}${f.arch !== '' ? ` (${f.arch})` : ''}`,
    `Windows build ${f.osBuild > 0 ? String(f.osBuild) : 'unknown'}`,
    `WebView2 ${f.webView ?? 'unknown'}`,
    `Engine: encoders ${f.encoders.length > 0 ? f.encoders.join(', ') : 'none'}; sound ${f.audioBackend ?? 'unknown'}`,
  ].join('\n');
}

const LOG_LEVELS: readonly LogLevel[] = ['info', 'debug'];

/** The sound capture backend's name (ShareCaps.audioBackend, README §3 item 20). */
const BACKEND_KEYS: Readonly<Record<ShareCaps['audioBackend'], string>> = {
  'process-loopback': 'desktop.settings.about.audioBackend.processLoopback',
  endpoint: 'desktop.settings.about.audioBackend.endpoint',
  none: 'desktop.settings.about.audioBackend.none',
};

export interface AboutSettingsProps {
  desktop: DesktopFeatures;
  settings: SettingsResult;
  openH264: OpenH264Status | null;
}

export function AboutSettings({ desktop, settings, openH264 }: AboutSettingsProps) {
  const { t } = useTranslation();
  const { ui } = useApp();
  const fail = useBridgeFailure();
  const { bridge } = desktop;
  const [info, setInfo] = useState<AppInfo | null>(null);
  const [caps, setCaps] = useState<ShareCaps | null>(null);
  const [notices, setNotices] = useState(false);
  const levelId = useId();
  const { view, update } = settings;

  useEffect(() => {
    let live = true;
    bridge.app.info().then(
      (i) => {
        if (live && isObject(i)) setInfo(i);
      },
      () => undefined,
    );
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

  const unknown = t('desktop.settings.about.unknown');
  const webView = webViewVersion(globalThis.navigator.userAgent);
  const facts: VersionFacts = {
    version: info?.version ?? desktop.app.version,
    arch: info?.arch ?? '',
    osBuild: info?.osBuild ?? desktop.app.osBuild,
    webView,
    encoders: caps?.encoders ?? [],
    audioBackend: caps?.audioBackend ?? null,
  };
  const level = view?.log.level;

  return (
    <SettingsSection id="about" title={t('desktop.settings.about.title')}>
      <dl className={styles.facts}>
        <dt>{t('desktop.settings.about.version')}</dt>
        <dd>{facts.version}</dd>
        <dt>{t('desktop.settings.about.windows')}</dt>
        <dd>{facts.osBuild > 0 ? String(facts.osBuild) : unknown}</dd>
        <dt>{t('desktop.settings.about.webView')}</dt>
        <dd>{webView ?? unknown}</dd>
        <dt>{t('desktop.settings.about.encoders')}</dt>
        <dd>
          {caps === null
            ? unknown
            : caps.encoders.length > 0
              ? caps.encoders.map((e) => encoderName(t, e)).join(', ')
              : t('desktop.settings.about.none')}
        </dd>
        <dt>{t('desktop.settings.about.audio')}</dt>
        <dd>{caps === null ? unknown : t(BACKEND_KEYS[caps.audioBackend])}</dd>
      </dl>
      {openH264?.state === 'on' && <p className={styles.muted}>{t('desktop.settings.openh264.attribution')}</p>}
      <div className={styles.actions}>
        <Button
          size="sm"
          onClick={() => {
            const text = versionInfoText(facts);
            Promise.resolve()
              .then(() => globalThis.navigator.clipboard.writeText(text))
              .then(
                () => {
                  ui.getState().toast({ kind: 'success', message: t('desktop.settings.about.copied') });
                },
                () => {
                  ui.getState().toast({ kind: 'error', message: t('desktop.settings.about.copyFailed') });
                },
              );
          }}
        >
          {t('desktop.settings.about.copy')}
        </Button>
        <Button
          size="sm"
          onClick={() => {
            bridge.app.openLogsFolder().catch(fail);
          }}
        >
          {t('desktop.settings.about.logs')}
        </Button>
      </div>
      <div className={styles.row}>
        <label htmlFor={levelId}>{t('desktop.settings.about.logLevel')}</label>
        <select
          id={levelId}
          value={level !== undefined && LOG_LEVELS.includes(level) ? level : 'info'}
          disabled={view === null}
          onChange={(ev) => {
            update({ log: { level: ev.target.value as LogLevel } }).catch(fail);
          }}
        >
          {LOG_LEVELS.map((l) => (
            <option key={l} value={l}>
              {t(`desktop.settings.about.logLevels.${l}`)}
            </option>
          ))}
        </select>
      </div>
      <ul className={styles.links}>
        <li>
          <Button
            size="sm"
            variant="ghost"
            onClick={() => {
              bridge.app.openNotices().catch(fail);
            }}
          >
            {t('desktop.settings.about.notices')}
          </Button>
        </li>
        <li>
          <Button
            size="sm"
            variant="ghost"
            onClick={() => {
              setNotices(true);
            }}
          >
            {t('desktop.settings.about.webLicenses')}
          </Button>
        </li>
        <li>
          <OutLink href={PRIVACY_DESKTOP_URL}>{t('desktop.settings.about.privacy')}</OutLink>
        </li>
        <li>
          <OutLink href={CODE_SIGNING_URL}>{t('desktop.settings.about.codeSigning')}</OutLink>
        </li>
      </ul>
      <Dialog
        open={notices}
        title={t('desktop.settings.about.webLicenses')}
        onClose={() => {
          setNotices(false);
        }}
      >
        {notices && <SpaLicenses />}
      </Dialog>
    </SettingsSection>
  );
}

/** The bundled SPA's /licenses.txt, read from the app's own assets. */
function SpaLicenses() {
  const { t } = useTranslation();
  const [text, setText] = useState<string | null>(null);
  const [failed, setFailed] = useState(false);
  useEffect(() => {
    let live = true;
    fetch(SPA_LICENSES_PATH)
      .then((r) => (r.ok ? r.text() : Promise.reject(new Error(String(r.status)))))
      .then(
        (body) => {
          if (live) setText(body);
        },
        () => {
          if (live) setFailed(true);
        },
      );
    return () => {
      live = false;
    };
  }, []);
  if (failed) return <p role="alert">{t('desktop.settings.about.licensesFailed')}</p>;
  if (text === null) return <Spinner />;
  return (
    <textarea
      className={styles.notices}
      readOnly
      value={text}
      rows={20}
      aria-label={t('desktop.settings.about.webLicenses')}
    />
  );
}
