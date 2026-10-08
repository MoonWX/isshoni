// The setup page folder's entry module (app/router.tsx's page-folder contract): the router loads this file as one
// lazy chunk on the first visit to /setup or /admin/welcome and picks each page by its export name. Only pages are
// exported here. WelcomePage (wizard steps 2–3, S87) joins SetupPage when it exists; until then /admin/welcome
// renders the router's PageUnavailable.
//
// The pages' texts, the `setup` namespace, come with this chunk and not with the main bundle (05 §16.5): the import
// below adds them to the catalog before the router has any of the pages.
import '../i18n/lazy/setup';

export { SetupPage } from './SetupPage';
