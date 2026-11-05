// /desktop/settings (docs/m2/03 §14.4): Servers, Sharing, Audio, App, Updates and About, with the anchors #servers,
// #sharing, #audio, #app, #updates and #about (the Offline screen's "Switch server" opens #servers). Audio (#audio)
// is here (README WA77: AudioSettings.tsx); WA82 adds the other sections around it. Outside the desktop app the page
// is M1's NotFound (RequireDesktop renders it only inside the app).
import { useTranslation } from 'react-i18next';

import { usePlatform } from '../app/context';
import { CenteredPage } from '../app/layouts/CenteredPage';
import { NotFound } from '../app/NotFound';
import { AudioSettings } from './AudioSettings';

export function SettingsPage() {
  const { t } = useTranslation();
  const platform = usePlatform();
  const desktop = platform.desktop ?? null;
  if (desktop === null) return <NotFound />;
  return (
    <CenteredPage title={t('desktop.settings.title')} width="lg">
      <AudioSettings bridge={desktop.bridge} />
    </CenteredPage>
  );
}
