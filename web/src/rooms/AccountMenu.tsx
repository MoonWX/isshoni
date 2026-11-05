// The account menu of the room header (05 §11.2): who is signed in, the ways to the account and admin pages, and
// Sign out (the logout flow of 05 §15.1, auth/useLogout.ts).
//
// - "Invite friends" opens InviteDialog, which creates an invite and shows its link once in setup/'s
//   InviteLinkCard (05 §14.1). It shows to the users who may create one (03 §7.9): admins, and members with
//   me.permissions.createInvites, and nobody while registration is closed (/info says so, read again when the menu
//   opens; the server answers registration_closed then, to admins too).
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
//
// In the desktop app (M2: docs/m2/03 §7.5, §8.3; platform.desktop is set) the menu is the desktop's: "Settings"
// (/desktop/settings), "Switch server" (Settings → Servers, only with more than one server in Auth.Servers()),
// "Account ↗", "My invites ↗" and, for admins, "Admin ↗" (the browser's pages, opened in the system browser: the
// desktop has no cookie session for them), "Invite friends" (its dialog is a REST call), "Test my connection",
// "About isshoni" and "Sign out of <server>". Sign out asks first, then calls Auth.Unlink(): Go stops a share on that
// server, revokes this PC's device (POST /api/v1/device/revoke) and deletes the credential, and keeps the server as
// Not linked. The desktop never POSTs /auth/logout (02 §11.8) and has no Notifications entry (web push is the
// browser's). Then ['me'] is forgotten here, and RequireAuth's desktop branch follows Auth.State() to the link step.
// When the server wasn't reached ({confirmed: false}), a toast that stays until dismissed says "Remove {device} on
// your Devices page to finish": a toast, so that it outlives the room page that the sign-out leaves.
import {
  Activity,
  ArrowLeftRight,
  Bell,
  CircleUser,
  Download,
  ExternalLink,
  Info,
  LogOut,
  Settings,
  ShieldCheck,
  SlidersHorizontal,
  Ticket,
  UserPlus,
} from 'lucide-react';
import { useEffect, useRef, useState, type ReactNode } from 'react';
import { useTranslation } from 'react-i18next';
import { Link } from 'react-router';

import { useApp } from '../app/context';
import { canInvite, DesktopPaths, isAdmin } from '../app/guards';
import { useInfo } from '../app/info';
import { endSession } from '../auth/session';
import { useLogout, type UseLogout } from '../auth/useLogout';
import { errorMessage } from '../lib/errorText';
import type { DesktopFeatures } from '../platform/types';
import { RegistrationModeClosed, type Me } from '../protocol/api.gen';
import { Button } from '../ui/Button';
import { Dialog } from '../ui/Dialog';
import { Popover } from '../ui/Popover';
import { VisuallyHidden } from '../ui/VisuallyHidden';
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

/** An entry of the desktop menu for a page that opens in the system browser ("Account ↗"). */
function MenuExternal({ icon, onClick, children }: { icon: ReactNode; onClick: () => void; children: ReactNode }) {
  const { t } = useTranslation();
  return (
    <li>
      <button type="button" className={styles.item} onClick={onClick}>
        {icon}
        {children}
        <ExternalLink aria-hidden="true" /> <VisuallyHidden>{t('room.menu.inBrowser')}</VisuallyHidden>
      </button>
    </li>
  );
}

/** The desktop app's active server as the menu names it, and the device that "Sign out of <server>" unlinks. */
interface DesktopServer {
  /** Its name, or its host when it has none. */
  name: string;
  /** This PC's device name on it; '' when unknown. */
  device: string;
}

/** The desktop menu's view of Auth.Servers(). */
interface DesktopServers {
  active: DesktopServer;
  /** More than one server: "Switch server" shows. */
  several: boolean;
}

/** The active server before Auth.Servers() answers: bridge.js's name, else the host of the page's server. */
function bootServer(desktop: DesktopFeatures, serverOrigin: string): DesktopServer {
  const name = desktop.server?.name ?? '';
  if (name !== '') return { name, device: '' };
  try {
    return { name: new URL(serverOrigin).host, device: '' };
  } catch {
    return { name: serverOrigin, device: '' };
  }
}

/**
 * Auth.Servers(), read each time the menu opens (the entries mount then): the active server's name and device, and
 * whether "Switch server" shows. A failed read keeps bridge.js's name and shows no "Switch server".
 */
function useDesktopServers(desktop: DesktopFeatures, serverOrigin: string): DesktopServers {
  const [servers, setServers] = useState<DesktopServers>(() => ({
    active: bootServer(desktop, serverOrigin),
    several: false,
  }));
  useEffect(() => {
    let live = true;
    desktop.bridge.auth.servers().then(
      (list) => {
        if (!live) return;
        const active = list.find((s) => s.active);
        setServers((prev) => ({
          active:
            active === undefined
              ? prev.active
              : { name: active.name !== '' ? active.name : prev.active.name, device: active.device?.name ?? '' },
          several: list.length > 1,
        }));
      },
      () => undefined,
    );
    return () => {
      live = false;
    };
  }, [desktop]);
  return servers;
}

interface DesktopSignOut {
  /** Auth.Unlink(); resolves true when this PC was signed out of the server. */
  readonly unlink: (server: DesktopServer) => Promise<boolean>;
  /** An Unlink is running. */
  readonly pending: boolean;
}

/**
 * The desktop's "Sign out of <server>" (docs/m2/03 §8.3): Auth.Unlink(), never POST /auth/logout. Once it resolved,
 * ['me'] is forgotten here, and the page follows Auth.State() (RequireAuth's desktop branch: the link step). A
 * refusal is an error toast, and the user stays signed in.
 */
function useDesktopSignOut(desktop: DesktopFeatures | null): DesktopSignOut {
  const app = useApp();
  const { t } = useTranslation();
  const [pending, setPending] = useState(false);
  const running = useRef(false);
  const unlink = async (server: DesktopServer): Promise<boolean> => {
    if (desktop === null || running.current) return false;
    running.current = true;
    setPending(true);
    try {
      const { confirmed } = await desktop.bridge.auth.unlink();
      if (!confirmed) {
        app.ui.getState().toast({
          kind: 'info',
          message: t('room.menu.signOutUnconfirmed', {
            device: server.device !== '' ? server.device : t('room.menu.thisPC'),
          }),
          durationMs: null,
        });
      }
      endSession(app.queryClient);
      return true;
    } catch (err) {
      app.ui.getState().toast({ kind: 'error', message: t('auth.logout.failed', { reason: errorMessage(err, t) }) });
      return false;
    } finally {
      running.current = false;
      setPending(false);
    }
  };
  return { unlink, pending };
}

export function AccountMenu({ me, onTestConnection }: AccountMenuProps) {
  const { t } = useTranslation();
  const { platform } = useApp();
  const desktop = platform.desktop ?? null;
  // Here, not in the entries: the busy state outlives a menu that closes meanwhile.
  const signOut = useLogout();
  const desktopSignOut = useDesktopSignOut(desktop);
  const name = me.user.username;
  /** The menu's button, which is also the Popover's: an entry that opens a dialog hands the focus back to it. */
  const menuButton = useRef<HTMLButtonElement | null>(null);
  /** "Invite friends" is open. */
  const [inviting, setInviting] = useState(false);
  /** The desktop's "Sign out of <server>?" is open, for this server. */
  const [signingOut, setSigningOut] = useState<DesktopServer | null>(null);

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
        {(close) => {
          const common = {
            me,
            close,
            openDialog: (open: () => void) => {
              close();
              menuButton.current?.focus();
              open();
            },
            onInvite: () => {
              setInviting(true);
            },
            ...(onTestConnection ? { onTestConnection } : {}),
          };
          return desktop !== null ? (
            <DesktopMenuEntries
              {...common}
              desktop={desktop}
              signOutPending={desktopSignOut.pending}
              onSignOut={(server) => {
                setSigningOut(server);
              }}
            />
          ) : (
            <MenuEntries {...common} signOut={signOut} />
          );
        }}
      </Popover>
      {/*
        Always mounted, whoever may invite: closed through `open`, it gives the focus back to the menu's button,
        also after the permission went while it was open (a link it made stays on screen until then).
      */}
      <InviteDialog
        me={me}
        open={inviting}
        onClose={() => {
          setInviting(false);
        }}
      />
      {desktop !== null && (
        <Dialog
          open={signingOut !== null}
          size="sm"
          onClose={() => {
            setSigningOut(null);
          }}
          title={signingOut === null ? '' : t('room.menu.signOutConfirm', { server: signingOut.name })}
          footer={
            signingOut !== null && (
              <>
                <Button
                  onClick={() => {
                    setSigningOut(null);
                  }}
                >
                  {t('common.cancel')}
                </Button>
                <Button
                  variant="danger"
                  loading={desktopSignOut.pending}
                  onClick={() => {
                    void desktopSignOut.unlink(signingOut).then(() => {
                      setSigningOut(null);
                    });
                  }}
                >
                  {t('room.menu.signOut')}
                </Button>
              </>
            )
          }
        >
          <p>{t('room.menu.signOutBody')}</p>
        </Dialog>
      )}
    </>
  );
}

interface EntriesProps {
  me: Me;
  /** Closes the menu. */
  close: () => void;
  /** Closes the menu and runs open, with the focus on the menu's button to come back to. */
  openDialog: (open: () => void) => void;
  /** Opens "Invite friends". */
  onInvite: () => void;
  onTestConnection?: () => void;
}

interface MenuEntriesProps extends EntriesProps {
  signOut: UseLogout;
}

/** "Invite friends", for the users who may invite while registration is open (both menus). */
function InviteEntry({ me, openDialog, onInvite }: Pick<EntriesProps, 'me' | 'openDialog' | 'onInvite'>) {
  const { t } = useTranslation();
  const registration = useInfo().data?.registration;
  // 03 §7.9: the permission first, then the mode, which refuses new invites to everyone while it is closed.
  if (!canInvite(me) || registration === RegistrationModeClosed) return null;
  return (
    <li>
      <button
        type="button"
        className={styles.item}
        aria-haspopup="dialog"
        onClick={() => {
          openDialog(onInvite);
        }}
      >
        <UserPlus aria-hidden="true" />
        {t('room.menu.invite')}
      </button>
    </li>
  );
}

/** "Test my connection" (both menus), when the page offers the test. */
function TestConnectionEntry({ openDialog, onTestConnection }: Pick<EntriesProps, 'openDialog' | 'onTestConnection'>) {
  const { t } = useTranslation();
  if (!onTestConnection) return null;
  return (
    <li>
      <button
        type="button"
        className={styles.item}
        aria-haspopup="dialog"
        onClick={() => {
          openDialog(onTestConnection);
        }}
      >
        <Activity aria-hidden="true" />
        {t('room.connection.testConnection')}
      </button>
    </li>
  );
}

/**
 * The open menu's content in the browser. It mounts on each opening, and so does its ['info'] observer: TanStack
 * refetches /info then when it is older than staleTime. A member gets no `admin.settings` invalidation (01 §8.12:
 * that topic goes to admins only), so this is how the closed mode reaches the menu of a member who stays in the room.
 */
function MenuEntries({ me, close, openDialog, onInvite, onTestConnection, signOut }: MenuEntriesProps) {
  const { t } = useTranslation();
  const { platform } = useApp();
  const name = me.user.username;
  // A member's way to their invites. An admin's is Admin → Invites.
  const myInvites = !isAdmin(me) && me.permissions.createInvites;

  return (
    <>
      <p className={styles.signedIn}>{t('room.menu.signedInAs', { name })}</p>
      <ul className={styles.list}>
        <MenuLink to="/account" icon={<Settings aria-hidden="true" />} onClick={close}>
          {t('room.menu.account')}
        </MenuLink>
        <MenuLink to="/account/notifications" icon={<Bell aria-hidden="true" />} onClick={close}>
          {t('room.menu.notifications')}
        </MenuLink>
        <InviteEntry me={me} openDialog={openDialog} onInvite={onInvite} />
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
        <TestConnectionEntry openDialog={openDialog} {...(onTestConnection ? { onTestConnection } : {})} />
        <MenuLink to="/about" icon={<Info aria-hidden="true" />} onClick={close}>
          {t('room.menu.about')}
        </MenuLink>
        <li className={styles.last}>
          <button
            type="button"
            className={styles.item}
            disabled={signOut.pending}
            onClick={() => {
              // The page follows by itself: ['me'] becomes null and the guard goes to the login page.
              void signOut.logout();
            }}
          >
            <LogOut aria-hidden="true" />
            {t('room.menu.signOut')}
          </button>
        </li>
      </ul>
    </>
  );
}

interface DesktopMenuEntriesProps extends EntriesProps {
  desktop: DesktopFeatures;
  /** Opens "Sign out of <server>?" for the active server. */
  onSignOut: (server: DesktopServer) => void;
  /** Auth.Unlink() is running. */
  signOutPending: boolean;
}

/**
 * The open menu's content in the desktop app (docs/m2/03 §7.5). Like the browser's, it mounts on each opening, so
 * ['info'] and Auth.Servers() are read again then.
 */
function DesktopMenuEntries({
  me,
  close,
  openDialog,
  onInvite,
  onTestConnection,
  desktop,
  onSignOut,
  signOutPending,
}: DesktopMenuEntriesProps) {
  const { t } = useTranslation();
  const { platform } = useApp();
  const origin = platform.serverOrigin;
  const servers = useDesktopServers(desktop, origin);
  const name = me.user.username;
  const myInvites = !isAdmin(me) && me.permissions.createInvites;
  /** Opens one of the server's browser pages in the system browser. */
  const openInBrowser = (path: string) => () => {
    close();
    platform.openExternal(origin + path);
  };

  return (
    <>
      <p className={styles.signedIn}>{t('room.menu.signedInAs', { name })}</p>
      <ul className={styles.list}>
        <MenuLink to="/desktop/settings" icon={<SlidersHorizontal aria-hidden="true" />} onClick={close}>
          {t('room.menu.settings')}
        </MenuLink>
        {servers.several && (
          <MenuLink to={DesktopPaths.servers} icon={<ArrowLeftRight aria-hidden="true" />} onClick={close}>
            {t('room.menu.switchServer')}
          </MenuLink>
        )}
        <MenuExternal icon={<Settings aria-hidden="true" />} onClick={openInBrowser('/account')}>
          {t('room.menu.account')}
        </MenuExternal>
        <InviteEntry me={me} openDialog={openDialog} onInvite={onInvite} />
        {myInvites && (
          <MenuExternal icon={<Ticket aria-hidden="true" />} onClick={openInBrowser('/admin/invites')}>
            {t('room.menu.myInvites')}
          </MenuExternal>
        )}
        {isAdmin(me) && (
          <MenuExternal icon={<ShieldCheck aria-hidden="true" />} onClick={openInBrowser('/admin')}>
            {t('room.menu.admin')}
          </MenuExternal>
        )}
        <TestConnectionEntry openDialog={openDialog} {...(onTestConnection ? { onTestConnection } : {})} />
        <MenuLink to="/about" icon={<Info aria-hidden="true" />} onClick={close}>
          {t('room.menu.about')}
        </MenuLink>
        <li className={styles.last}>
          <button
            type="button"
            className={styles.item}
            aria-haspopup="dialog"
            disabled={signOutPending}
            onClick={() => {
              openDialog(() => {
                onSignOut(servers.active);
              });
            }}
          >
            <LogOut aria-hidden="true" />
            {t('room.menu.signOutOf', { server: servers.active.name })}
          </button>
        </li>
      </ul>
    </>
  );
}
