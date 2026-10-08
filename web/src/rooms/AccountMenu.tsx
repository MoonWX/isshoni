// The account menu of the room header (05 §11.2): who is signed in, the ways to the account and admin pages, and
// Sign out (the logout flow of 05 §15.1, auth/useLogout.ts).
//
// - "Invite friends" shows to admins and to members with me.permissions.createInvites. It goes to /admin/invites,
//   where an invite is created and its link shown once; members see only their own invites there (05 §5,
//   RequireInviter), so the same entry is their "My invites". Creating the invite right here, with the
//   InviteLinkCard of 05 §14.1, arrives with that card (the setup wizard's step 3).
// - "Admin" shows to admins only: other users would get NotFound there (05 §5).
// - "Install app" shows while the browser offers an install prompt (05 §16.3).
import { Bell, CircleUser, Download, Info, LogOut, Settings, ShieldCheck, UserPlus } from 'lucide-react';
import type { ReactNode } from 'react';
import { useTranslation } from 'react-i18next';
import { Link } from 'react-router';

import { useApp } from '../app/context';
import { canInvite, isAdmin } from '../app/guards';
import { useLogout } from '../auth/useLogout';
import type { Me } from '../protocol/api.gen';
import { Popover } from '../ui/Popover';
import styles from './AccountMenu.module.css';

export interface AccountMenuProps {
  /** GET /api/v1/me; the guard around the room loaded it. */
  me: Me;
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

export function AccountMenu({ me }: AccountMenuProps) {
  const { t } = useTranslation();
  const { platform } = useApp();
  const { logout, pending } = useLogout();
  const name = me.user.username;

  return (
    <Popover
      label={t('room.menu.label')}
      align="end"
      trigger={(props) => (
        <button type="button" {...props} className={styles.trigger} aria-label={t('room.menu.open', { name })}>
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
            {canInvite(me) && (
              <MenuLink to="/admin/invites" icon={<UserPlus aria-hidden="true" />} onClick={close}>
                {t('room.menu.invite')}
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
  );
}
