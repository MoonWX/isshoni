// /desktop/settings (docs/m2/03 §14.4): Servers, Sharing, Audio, App, Updates and About, with the anchors #servers,
// #sharing, #audio, #app, #updates and #about; the account menu's Switch server and the Offline screen's link open
// #servers. Outside the desktop app the page is M1's NotFound (RequireDesktop renders it only inside the app).
//
// The sections:
//   #servers   ServerSwitcher.tsx: the servers with Switch, Sign out, Link again, Remove; Add a server
//   #sharing   SharingSettings.tsx: Optimize for, the Sharing pill, the hotkeys, the first-share sheet, the encoders,
//              the Cisco OpenH264 opt-in (OpenH264OptIn.tsx)
//   #audio     AudioSettings.tsx (README WA77)
//   #app       AppSettings.tsx
//   #updates   UpdatesSettings.tsx with UpdateParts.tsx's status line
//   #about     AboutSettings.tsx
// settings.json's view (Settings.Get and settings.changed) and the update status are read once here and shared by the
// sections. A required update also shows its banner at the top of every page (the app shell's RequiredUpdateBanner).
import { useEffect, useMemo } from 'react';
import { useTranslation } from 'react-i18next';
import { useLocation } from 'react-router';

import { usePlatform } from '../app/context';
import { CenteredPage } from '../app/layouts/CenteredPage';
import { NotFound } from '../app/NotFound';
import type { DesktopFeatures } from '../platform/types';
import { AboutSettings } from './AboutSettings';
import { AppSettings } from './AppSettings';
import { AudioSettings } from './AudioSettings';
import { openH264Control, useOpenH264, type OpenH264Control } from './OpenH264OptIn';
import { ServerSwitcher } from './ServerSwitcher';
import { SharingSettings } from './SharingSettings';
import { useSettings, useUpdateStatus } from './settingsState';
import { UpdatesSettings } from './UpdatesSettings';
import styles from './settings.module.css';

/** The sections' anchors, in page order. */
export const SETTINGS_SECTIONS = ['servers', 'sharing', 'audio', 'app', 'updates', 'about'] as const;

/** The section links' texts. */
const SECTION_KEYS: Readonly<Record<(typeof SETTINGS_SECTIONS)[number], string>> = {
  servers: 'desktop.settings.servers.title',
  sharing: 'desktop.settings.sharing.title',
  audio: 'desktop.settings.audio.title',
  app: 'desktop.settings.app.title',
  updates: 'desktop.settings.updates.title',
  about: 'desktop.settings.about.title',
};

export interface SettingsPageProps {
  /** The OpenH264 opt-in's control; default: Go's (openH264Control over the bridge); null hides the opt-in. */
  openH264?: OpenH264Control | null;
}

export function SettingsPage({ openH264 }: SettingsPageProps = {}) {
  const platform = usePlatform();
  const desktop = platform.desktop ?? null;
  if (desktop === null) return <NotFound />;
  return <Settings desktop={desktop} origin={platform.serverOrigin} openH264={openH264} />;
}

function Settings({
  desktop,
  origin,
  openH264,
}: {
  desktop: DesktopFeatures;
  origin: string;
  openH264: OpenH264Control | null | undefined;
}) {
  const { t } = useTranslation();
  const { hash } = useLocation();
  const { bridge } = desktop;
  const settings = useSettings(bridge);
  const [status, setStatus] = useUpdateStatus(bridge);
  const control = useMemo(() => (openH264 === undefined ? openH264Control(bridge) : openH264), [openH264, bridge]);
  const h264 = useOpenH264(control);

  // An anchor in the address (#servers from the account menu or the Offline screen): show that section.
  useEffect(() => {
    const id = hash.replace(/^#/, '');
    if (id === '' || !(SETTINGS_SECTIONS as readonly string[]).includes(id)) return;
    const el = globalThis.document.getElementById(id);
    if (el && typeof el.scrollIntoView === 'function') el.scrollIntoView({ block: 'start' });
  }, [hash]);

  return (
    <CenteredPage title={t('desktop.settings.title')} width="lg">
      <div className={styles.page}>
        <nav className={styles.nav} aria-label={t('desktop.settings.nav')}>
          <ul>
            {SETTINGS_SECTIONS.map((id) => (
              <li key={id}>
                <a href={`#${id}`}>{t(SECTION_KEYS[id])}</a>
              </li>
            ))}
          </ul>
        </nav>
        <div id="servers" className={styles.section}>
          <ServerSwitcher bridge={bridge} appVersion={desktop.app.version} />
        </div>
        <SharingSettings
          bridge={bridge}
          origin={origin}
          settings={settings}
          openH264={control === null ? null : { control, state: h264 }}
        />
        <div className={styles.section}>
          <AudioSettings bridge={bridge} />
        </div>
        <AppSettings bridge={bridge} settings={settings} />
        <UpdatesSettings bridge={bridge} settings={settings} status={status} setStatus={setStatus} />
        <AboutSettings desktop={desktop} settings={settings} openH264={h264.status} />
      </div>
    </CenteredPage>
  );
}
