// The admin page folder's entry module (app/router.tsx's page-folder contract, 05 §5): the router loads this file
// as one lazy chunk on the first visit to a route under /admin and picks each page, and the layout around them, by
// its export name. Only pages and the layout are exported here.
//
// The folder's exports are AdminLayout, DashboardPage, UsersPage, ApprovalsPage, InvitesPage, RoomsPage,
// SettingsPage, AuditPage and DoctorPage: every page of 05 §15.3 (README S49 and S91).
//
// The pages' texts, the `admin` namespace, come with this chunk and not with the main bundle (05 §16.5): the import
// below adds them to the catalog before the router has any of the pages. The Doctor page's `doctor` namespace is
// in en.json (05 §16.5 names account, admin and setup as M1's lazy namespaces).
import '../i18n/lazy/admin';

export { AdminLayout } from './AdminLayout';
export { ApprovalsPage } from './ApprovalsPage';
export { AuditPage } from './AuditPage';
export { DashboardPage } from './DashboardPage';
export { DoctorPage } from './DoctorPage';
export { InvitesPage } from './InvitesPage';
export { RoomsPage } from './RoomsPage';
export { SettingsPage } from './SettingsPage';
export { UsersPage } from './UsersPage';
