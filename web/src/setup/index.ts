// The setup page folder's entry module (app/router.tsx's page-folder contract): the router loads this file as one
// lazy chunk on the first visit to /setup or /admin/welcome and picks each page by its export name. Only pages are
// exported here: SetupPage (wizard step 1, S33) and WelcomePage (steps 2–3, S87). InviteLinkCard, which another
// folder may show too, is imported from its own file (05 §3).
//
// The pages' texts, the `setup` namespace, come with this chunk and not with the main bundle (05 §16.5): the import
// below adds them to the catalog before the router has any of the pages.
import '../i18n/lazy/setup';

export { SetupPage } from './SetupPage';
export { WelcomePage } from './WelcomePage';
