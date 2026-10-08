// The setup page folder's entry module (app/router.tsx's page-folder contract): the router loads this file as one
// lazy chunk on the first visit to /setup or /admin/welcome and picks each page by its export name. Only pages are
// exported here. WelcomePage (wizard steps 2–3, S87) joins SetupPage when it exists; until then /admin/welcome
// renders the router's PageUnavailable.
export { SetupPage } from './SetupPage';
