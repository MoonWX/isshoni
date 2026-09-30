// SPA entry (05 §4). /boot-check.js runs first; if it flagged this browser as unsupported, it already wrote the
// message into #root and nothing here runs.
//
// Scaffold state (S09): i18n init (boot step 3) and a placeholder shell. S27 (05 W2) turns this into the full boot
// sequence: fragment token (step 0), NeedsHttps (1), detectPlatform (2), GET /api/v1/info (4), the QueryClient and
// the router (5), and service-worker registration (6).
import './ui/tokens.css';
import './ui/global.css';

import { StrictMode } from 'react';
import { createRoot } from 'react-dom/client';
import { I18nextProvider, useTranslation } from 'react-i18next';

import { initI18n } from './i18n';

function ScaffoldShell() {
  const { t } = useTranslation();
  return (
    <main>
      <h1>{t('common.appName')}</h1>
      <p>{t('common.tagline')}</p>
    </main>
  );
}

function main(): void {
  if (window.__ISSHONI_UNSUPPORTED__) return;
  const container = document.getElementById('root');
  if (!container) throw new Error('index.html has no #root element');
  const i18n = initI18n();
  createRoot(container).render(
    <StrictMode>
      <I18nextProvider i18n={i18n}>
        <ScaffoldShell />
      </I18nextProvider>
    </StrictMode>,
  );
}

main();
