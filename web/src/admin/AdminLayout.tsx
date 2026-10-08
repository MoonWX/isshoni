// The layout route around the admin pages (05 §5 "Page folders"): the way back to the room, the admin navigation,
// and the page. The guards sit outside it (app/router.tsx): RequireAdmin for every page but /admin/invites, which
// RequireInviter also opens to members who may create invites. Such a member gets the page without the navigation:
// the other admin pages don't exist for them (they render NotFound, so they aren't advertised).
//
// Each page renders its own <main> (AdminPage.tsx), so the router's stand-ins for a page that isn't built yet fit
// in here too.
import {
  ArrowLeft,
  DoorOpen,
  LayoutDashboard,
  ScrollText,
  Settings,
  Stethoscope,
  Ticket,
  UserCheck,
  Users,
  type LucideIcon,
} from 'lucide-react';
import { useEffect, useRef } from 'react';
import { useTranslation } from 'react-i18next';
import { Link, NavLink, Outlet, useLocation } from 'react-router';

import { isAdmin } from '../app/guards';
import { useMe } from '../auth/useMe';
import styles from './AdminLayout.module.css';

interface NavItem {
  readonly to: string;
  /** Only the exact path is "the current page" (the dashboard at /admin). */
  readonly end?: boolean;
  readonly icon: LucideIcon;
  readonly label: string;
  /** A count beside the label, and the link's whole name with it for screen readers ("Approvals, 3 waiting"). */
  readonly badge?: { readonly count: number; readonly name: string };
}

export function AdminLayout() {
  const { t } = useTranslation();
  const me = useMe().data;
  const admin = me ? isAdmin(me) : false;
  const pending = me?.badges?.pendingApprovals ?? 0;

  // On a phone the navigation is one row that scrolls sideways: the current page's link is brought into view.
  const nav = useRef<HTMLElement>(null);
  const { pathname } = useLocation();
  useEffect(() => {
    const current = nav.current?.querySelector('[aria-current="page"]');
    // jsdom has no scrollIntoView.
    if (current && typeof current.scrollIntoView === 'function') {
      current.scrollIntoView({ block: 'nearest', inline: 'center' });
    }
  }, [pathname, admin]);

  // Every page of 05 §15.3, in its order. The dashboard and the doctor page arrive in a later slice; until then
  // their routes show the router's "Not in this build yet".
  const items: readonly NavItem[] = [
    { to: '/admin', end: true, icon: LayoutDashboard, label: t('admin.nav.dashboard') },
    { to: '/admin/users', icon: Users, label: t('admin.nav.users') },
    {
      to: '/admin/approvals',
      icon: UserCheck,
      label: t('admin.nav.approvals'),
      ...(pending > 0 ? { badge: { count: pending, name: t('admin.nav.approvalsWaiting', { count: pending }) } } : {}),
    },
    { to: '/admin/invites', icon: Ticket, label: t('admin.nav.invites') },
    { to: '/admin/rooms', icon: DoorOpen, label: t('admin.nav.rooms') },
    { to: '/admin/settings', icon: Settings, label: t('admin.nav.settings') },
    { to: '/admin/audit', icon: ScrollText, label: t('admin.nav.audit') },
    { to: '/admin/doctor', icon: Stethoscope, label: t('admin.nav.doctor') },
  ];

  return (
    <div className={styles.shell}>
      <header className={styles.bar}>
        <Link to="/" className={styles.back}>
          <ArrowLeft aria-hidden="true" />
          <span>{t('admin.back')}</span>
        </Link>
        {admin && <p className={styles.where}>{t('admin.title')}</p>}
      </header>
      <div className={admin ? styles.columns : styles.single}>
        {admin && (
          <nav ref={nav} className={styles.nav} aria-label={t('admin.nav.label')}>
            <ul className={styles.items}>
              {items.map(({ to, end = false, icon: Icon, label, badge }) => (
                <li key={to}>
                  <NavLink to={to} end={end} className={styles.item} aria-label={badge?.name}>
                    <Icon aria-hidden="true" />
                    <span className={styles.label}>{label}</span>
                    {badge && <span className={styles.badge}>{badge.count}</span>}
                  </NavLink>
                </li>
              ))}
            </ul>
          </nav>
        )}
        <Outlet />
      </div>
    </div>
  );
}
