// The account menu of the room header (05 §11.2): who is signed in, the ways to the account and admin pages, and
// Sign out (the logout flow of 05 §15.1, auth/useLogout.ts).
//
// - "Invite friends" opens InviteDialog, which creates an invite and shows its link once in setup/'s
//   InviteLinkCard (05 §14.1). It shows to the users who may create one (03 §7.9): admins, and members with
//   me.permissions.createInvites, and nobody while registration is closed (/info says so; the server answers
//   registration_closed then, to admins too).
// - "My invites" shows to members with me.permissions.createInvites, also while registration is closed: a link to
//   /admin/invites, where they see and revoke their own invites (05 §5, RequireInviter). Admins get there through
//   Admin.
// - "Admin" shows to admins only: other users would get NotFound there (05 §5).
// - "Install app" shows while the browser offers an install prompt (05 §16.3).
// - "Test my connection" opens the page's connection test (05 §14.2): the way to it while no banner offers it,
//   for a user whose video is poor on a connection that is up.
//
// An entry that opens a dialog closes the menu and puts the focus on the menu's button first: the dialog gives the
// focus back to where it was when it opened (05 §16.6), and that has to be something that stays.
import {
  Activity,
  Bell,
  CircleUser,
  Download,
  Info,
  LogOut,
  Settings,
  ShieldCheck,
  Ticket,
  UserPlus,
} from 'lucide-react';
import { useRef, useState, type ReactNode } from 'react';
import { useTranslation } from 'react-i18next';
import { Link } from 'react-router';

import { useApp } from '../app/context';
import { canInvite, isAdmin } from '../app/guards';
import { useInfo } from '../app/info';
import { useLogout } from '../auth/useLogout';
import { RegistrationModeClosed, type Me } from '../protocol/api.gen';
import { Popover } from '../ui/Popover';
import styles from './AccountMenu.module.css';
import { InviteDialog } from './InviteDialog';

export interface AccountMenuProps {
  /** GET /api/v1/me; the guard around the room loaded it. */
  me: Me;
  /** Opens the connection test (05 §14.2). The "Test my connection" entry shows only when this is given. */
  onTestConnection?: () => void;
}

/** A link of the menu; a click on it closes the menu. */
function MenuLink({
  to,
  icon,
  onClick,
  children,
}: {
  to: string;
  icon: ReactNode;
  onClick: () => void;
  children: ReactNode;
}) {
  return (
    <li>
      <Link to={to} className={styles.item} onClick={onClick}>
        {icon}
        {children}
      </Link>
    </li>
  );
}

export function AccountMenu({ me, onTestConnection }: AccountMenuProps) {
  const { t } = useTranslation();
  const { platform } = useApp();
  const { logout, pending } = useLogout();
  const registration = useInfo().data?.registration;
  const name = me.user.username;
  /** The menu's button, which is also the Popover's: an entry that opens a dialog hands the focus back to it. */
  const menuButton = useRef<HTMLButtonElement | null>(null);
  /** "Invite friends" is open. */
  const [inviting, setInviting] = useState(false);
  // 03 §7.9: the permission first, then the mode, which refuses new invites to everyone while it is closed.
  const mayInvite = canInvite(me) && registration !== RegistrationModeClosed;
  // A member's way to their invites. An admin's is Admin → Invites.
  const myInvites = !isAdmin(me) && me.permissions.createInvites;

  /** Closes the menu and opens a dialog, with the focus on the menu's button to come back to. */
  const openDialog = (close: () => void, open: () => void): void => {
    close();
    menuButton.current?.focus();
    open();
  };

  return (
    <>
      <Popover
        label={t('room.menu.label')}
        align="end"
        trigger={({ ref, ...props }) => (
          <button
            type="button"
            {...props}
            ref={(el) => {
              ref.current = el;
              menuButton.current = el;
            }}
            className={styles.trigger}
            aria-label={t('room.menu.open', { name })}
          >
            <CircleUser aria-hidden="true" />
            <span className={styles.who}>{name}</span>
          </button>
        )}
      >
        {(close) => (
          <>
            <p className={styles.signedIn}>{t('room.menu.signedInAs', { name })}</p>
            <ul className={styles.list}>
              <MenuLink to="/account" icon={<Settings aria-hidden="true" />} onClick={close}>
                {t('room.menu.account')}
              </MenuLink>
              <MenuLink to="/account/notifications" icon={<Bell aria-hidden="true" />} onClick={close}>
                {t('room.menu.notifications')}
              </MenuLink>
              {mayInvite && (
                <li>
                  <button
                    type="button"
                    className={styles.item}
                    aria-haspopup="dialog"
                    onClick={() => {
                      openDialog(close, () => {
                        setInviting(true);
                      });
                    }}
                  >
                    <UserPlus aria-hidden="true" />
                    {t('room.menu.invite')}
                  </button>
                </li>
              )}
              {myInvites && (
                <MenuLink to="/admin/invites" icon={<Ticket aria-hidden="true" />} onClick={close}>
                  {t('room.menu.myInvites')}
                </MenuLink>
              )}
              {isAdmin(me) && (
                <MenuLink to="/admin" icon={<ShieldCheck aria-hidden="true" />} onClick={close}>
                  {t('room.menu.admin')}
                </MenuLink>
              )}
              {/* Read when the menu opens: the browser may have offered (or withdrawn) the prompt since. */}
              {platform.pwa?.installState() === 'prompt' && (
                <li>
                  <button
                    type="button"
                    className={styles.item}
                    onClick={() => {
                      close();
                      void platform.pwa?.promptInstall();
                    }}
                  >
                    <Download aria-hidden="true" />
                    {t('room.menu.install')}
                  </button>
                </li>
              )}
              {onTestConnection && (
                <li>
                  <button
                    type="button"
                    className={styles.item}
                    aria-haspopup="dialog"
                    onClick={() => {
                      openDialog(close, onTestConnection);
                    }}
                  >
                    <Activity aria-hidden="true" />
                    {t('room.connection.testConnection')}
                  </button>
                </li>
              )}
              <MenuLink to="/about" icon={<Info aria-hidden="true" />} onClick={close}>
                {t('room.menu.about')}
              </MenuLink>
              <li className={styles.last}>
                <button
                  type="button"
                  className={styles.item}
                  disabled={pending}
                  onClick={() => {
                    // The page follows by itself: ['me'] becomes null and the guard goes to the login page.
                    void logout();
                  }}
                >
                  <LogOut aria-hidden="true" />
                  {t('room.menu.signOut')}
                </button>
              </li>
            </ul>
          </>
        )}
      </Popover>
      {/*
        Always mounted, whoever may invite: closed through `open`, it gives the focus back to the menu's button,
        also after the permission went while it was open (a link it made stays on screen until then).
      */}
      <InviteDialog
        open={inviting}
        onClose={() => {
          setInviting(false);
        }}
      />
    </>
  );
}
