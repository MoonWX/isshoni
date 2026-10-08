// What this tab does after the server ended its session as part of another request (03 §7.7): "Log out everywhere"
// (POST /api/v1/auth/logout-everywhere) and deleting the account (POST /api/v1/me/delete) both answer 204 with the
// cookie cleared. The session is gone then, but the tab still shows the user, its signaling connection may still be
// open, its push subscription still exists in the browser, and the user's other tabs don't know.
//
// That is exactly the logout flow's own list (05 §15.1, auth/logout.ts): stop local shares, stop the SignalClient,
// drop the push subscription locally, tell the other tabs, forget the user. So the flow runs as it is. Its request,
// POST /api/v1/auth/logout, has nothing left to end and answers 204 all the same (public and idempotent, 03 §12.3
// #3). When even that request can't be made (the network dropped right after the first one), the two steps that
// need no server still happen: the other tabs are told and this one forgets the user.
import type { AppServices } from '../app/context';
import { broadcastLogout } from '../app/session';
import { logout } from '../auth/logout';
import { endSession } from '../auth/session';
import { createLogger } from '../lib/log';

const log = createLogger('account');

/**
 * Finishes a sign-out that the server already did. Resolves once this tab has forgotten the user: ['me'] is null
 * then, so a guarded route goes to the login page (app/guards.tsx). Never rejects.
 */
export async function sessionEndedByServer(app: Pick<AppServices, 'queryClient'>): Promise<void> {
  try {
    await logout(app);
  } catch (err) {
    log.warn('could not run the logout flow after the server ended the session', { error: err });
    broadcastLogout();
    endSession(app.queryClient);
  }
}
