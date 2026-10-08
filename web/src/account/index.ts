// The account page folder's entry module (app/router.tsx's page-folder contract, 05 §5): the router loads this file
// as one lazy chunk on the first visit to /account, /account/devices or /account/notifications, and picks each page
// by its export name. Only pages are exported here.
//
// NotificationsPage (Web Push on this device, 05 §16.3) joins these two when it exists; until then
// /account/notifications renders the router's PageUnavailable. The pages' shared frame, AccountShell.tsx, already
// links to it.
//
// For that page: useInAppBanner().showing (auth/inAppBanner.ts) is true in an in-app browser until the banner is
// dismissed, on every page, including one that renders no banner. If NotificationsPage hides its `needs-install` card
// on that flag, it must also render <InAppBrowserBanner /> (import it from '../auth/InAppBrowserBanner'); otherwise a
// friend in a chat app's browser gets neither the Home Screen steps nor a banner to dismiss.
export { AccountPage } from './AccountPage';
export { DevicesPage } from './DevicesPage';
