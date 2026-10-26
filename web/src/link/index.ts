// The link page folder's entry module (app/router.tsx's page-folder contract): the router loads this file as one lazy
// chunk on the first visit to /link (docs/m2/02-device-auth.md §10.1), in the browser only (the desktop app opens
// /link in the system browser, RequireBrowser), and picks the page by its export name. Only the page is exported
// here; appName.ts is imported by account/DevicesPage.tsx from its own file.
//
// The page's texts, the `link` namespace, come with this chunk and not with the main bundle (05 §16.5): the import
// below adds them to the catalog before the router has the page.
import '../i18n/lazy/link';

export { DeviceLinkPage } from './DeviceLinkPage';
