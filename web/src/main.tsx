// SPA entry (05 §4). /boot-check.js runs first; if it flagged this browser as unsupported, it already wrote the
// message into #root and nothing here runs. Otherwise startApp() runs the boot sequence (app/boot.tsx): the fragment
// token, NeedsHttps, the platform, i18n, GET /api/v1/info, the router, and the service worker.
import './ui/tokens.css';
import './ui/global.css';

import { startApp } from './app/boot';

function main(): void {
  if (window.__ISSHONI_UNSUPPORTED__) return;
  const container = document.getElementById('root');
  if (!container) throw new Error('index.html has no #root element');
  startApp(container);
}

main();
