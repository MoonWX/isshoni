// The account page folder's entry module (app/router.tsx's page-folder contract, 05 §5): the router loads this file
// as one lazy chunk on the first visit to /account, /account/devices or /account/notifications, and picks each page
// by its export name. Only pages are exported here.
//
// NotificationsPage (Web Push on this device, 05 §16.3) joins these two when it exists; until then
// /account/notifications renders the router's PageUnavailable. The pages' shared frame, AccountShell.tsx, already
// links to it.
export { AccountPage } from './AccountPage';
export { DevicesPage } from './DevicesPage';
