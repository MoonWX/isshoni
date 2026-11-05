// The desktop app's own pages (docs/m2/03 §7.5): the page folder `desktop/`, one lazy chunk that the router loads
// only inside the desktop app (app/router.tsx's desktop table, behind RequireDesktop). The routes:
//   /desktop/setup     SetupPage      the first-run wizard (§8.2)                       WA71
//   /desktop/relink    RelinkPage     the active server signed this PC out (§8.3)       WA71
//   /desktop/settings  SettingsPage   §14.4, with its anchors                           WA82
// WA42 declared the routes with stubs; each page slice replaces its stub in this folder and nothing in app/. The
// server switcher (ServerSwitcher.tsx, §8.1, §8.3) is a part of the relink page that Settings → Servers can reuse.
//
// The folder's texts are the lazy namespace `desktop` (i18n/lazy/desktop.ts, 03 §18.1): the import below adds them
// to the catalog before the router has a page (05 §16.5).
import '../i18n/lazy/desktop';

export { RelinkPage } from './RelinkPage';
export { SettingsPage } from './SettingsPage';
export { SetupPage } from './SetupPage';
