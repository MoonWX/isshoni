// The logout flow for React (05 §15.1): the account menu, the account page, the devices page's "Sign out" on this
// browser's row and the invite page's "Sign out and create another account" all call useLogout().logout().
//
// No navigation happens here. logout() ends with ['me'] set to null, and that alone moves the page: a route under
// RequireAuth goes to /login?next=<path>, a public page stays where it is. It is the same rule the user's other tabs
// follow when the BroadcastChannel message reaches them (05 §6.2).
import { useCallback, useRef, useState } from 'react';
import { useTranslation } from 'react-i18next';

import { useApp } from '../app/context';
import { errorMessage } from '../lib/errorText';
import { logout } from './logout';

export interface UseLogout {
  /**
   * Logs out. Resolves true when the session has ended, false when it couldn't be ended (the server was unreachable:
   * an error toast says so, and the user is still signed in). A second call while one runs resolves false.
   */
  readonly logout: () => Promise<boolean>;
  /** A logout is running: show the button as busy. */
  readonly pending: boolean;
}

export function useLogout(): UseLogout {
  const app = useApp();
  const { t } = useTranslation();
  const [pending, setPending] = useState(false);
  const running = useRef(false);

  const run = useCallback(async (): Promise<boolean> => {
    if (running.current) return false;
    running.current = true;
    setPending(true);
    try {
      await logout(app);
      return true;
    } catch (err) {
      app.ui.getState().toast({
        kind: 'error',
        message: t('auth.logout.failed', { reason: errorMessage(err, t) }),
      });
      return false;
    } finally {
      running.current = false;
      setPending(false);
    }
  }, [app, t]);

  return { logout: run, pending };
}
