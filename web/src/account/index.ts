// The account page folder's entry module (app/router.tsx's page-folder contract, 05 §5): the router loads this file
// as one lazy chunk on the first visit to /account, /account/devices or /account/notifications, and picks each page
// by its export name. Only pages are exported here.
//
// The folder also holds Web Push's parts for the room page (05 §16.3), which rooms/ imports from their own files,
// never through this one: RoomPushSlots.tsx (part of the room's chunk) loads RoomPush.tsx, the room's notifications
// card and Home Screen sheet, as a chunk of its own. pushState.ts is the push state that the room and the
// notifications page share.
//
// useInAppBanner().showing (auth/inAppBanner.ts) is true in an in-app browser until the banner is dismissed, on
// every page, including one that renders no banner. NotificationsPage hides its `needs-install` steps on that flag,
// so it renders <InAppBrowserBanner /> itself; otherwise a friend in a chat app's browser would get neither the Home
// Screen steps nor a banner to dismiss.
//
// The pages' texts, the `account` namespace, come with this chunk and not with the main bundle (05 §16.5): the
// import below adds them to the catalog before the router has any of the pages. RoomPush.tsx imports it too. The
// Devices page's app rows (M2 02 §10.2) name apps as the approval page does, link.app.<kind>.<os>, and their own
// texts are link.devices.*: the `link` namespace comes with this chunk as well.
import '../i18n/lazy/account';
import '../i18n/lazy/link';

export { AccountPage } from './AccountPage';
export { DevicesPage } from './DevicesPage';
export { NotificationsPage } from './NotificationsPage';
