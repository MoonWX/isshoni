// The admin page folder's entry module (app/router.tsx's page-folder contract, 05 §5): the router loads this file
// as one lazy chunk on the first visit to a route under /admin and picks each page, and the layout around them, by
// its export name. Only pages and the layout are exported here.
//
// The folder's full set of exports is AdminLayout, DashboardPage, UsersPage, ApprovalsPage, InvitesPage, RoomsPage,
// SettingsPage, AuditPage and DoctorPage. DashboardPage (/admin) and DoctorPage (/admin/doctor) join the others
// when they exist (the dashboard, doctor and bandwidth slice); until then their routes render the router's
// PageUnavailable inside AdminLayout, whose navigation already lists them.
//
// The pages' texts, the `admin` namespace, come with this chunk and not with the main bundle (05 §16.5): the import
// below adds them to the catalog before the router has any of the pages.
import '../i18n/lazy/admin';

export { AdminLayout } from './AdminLayout';
export { ApprovalsPage } from './ApprovalsPage';
export { AuditPage } from './AuditPage';
export { InvitesPage } from './InvitesPage';
export { RoomsPage } from './RoomsPage';
export { SettingsPage } from './SettingsPage';
export { UsersPage } from './UsersPage';
