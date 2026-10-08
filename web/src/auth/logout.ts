// The logout flow (05 §15.1), in the order the spec gives:
//   1. stop local shares;
//   2. SignalClient.stop() (close 1000, so the server drops this tab's presence at once, 05 §7);
//   3. POST /api/v1/auth/logout (the server deletes this session and its push subscriptions, 03 §12.4.2);
//   4. PushSubscription.unsubscribe() locally;
//   5. BroadcastChannel logout, so the user's other tabs go to the login page too (05 §6.2);
//   6. forget the user in this tab's REST cache (session.ts endSession).
//
// Steps 1 and 2 belong to the realtime parts (share/, rooms/), which register them with addLogoutStep('shares', …)
// and addLogoutStep('signal', …); this module never imports them, and it uses no React (controllers register from
// plain TypeScript). Pages call useLogout() (useLogout.ts), which adds the error toast; the redirect follows
// from ['me'] becoming null.
//
// When the server can't be reached the user is NOT signed out: the session cookie is HttpOnly, so only the server
// can end the session, and pretending otherwise would leave a shared computer signed in. logout() then undoes what
// the steps allow (the signaling connection starts again) and rejects.
import type { QueryClient } from '@tanstack/react-query';

import { broadcastLogout } from '../app/session';
import { createLogger } from '../lib/log';
import { api, isSignedOutError } from '../protocol/rest';
import { endSession } from './session';

const log = createLogger('auth');

/** What a step may return: a function that undoes it, called when the logout request failed. */
export type LogoutUndo = () => void;

/**
 * A part of the logout flow that runs before the request: stop the local shares, stop the SignalClient. It may
 * return (or resolve with) an undo function. A step that throws is logged and doesn't stop the logout.
 */
export type LogoutStep = () => LogoutUndo | undefined | Promise<LogoutUndo | undefined>;

/**
 * When a step runs. 05 §15.1 fixes the order, whoever registered first: every 'shares' step (stop local shares,
 * while the signaling connection can still say share.stop), then every 'signal' step (SignalClient.stop()).
 */
export type LogoutPhase = 'shares' | 'signal';

const PHASES: readonly LogoutPhase[] = ['shares', 'signal'];

const steps = new Map<LogoutStep, LogoutPhase>();

/**
 * Adds a step to every later logout() of this page, in its phase; within a phase, steps run in the order they were
 * added. Returns the function that removes it.
 */
export function addLogoutStep(phase: LogoutPhase, step: LogoutStep): () => void {
  steps.set(step, phase);
  return () => {
    steps.delete(step);
  };
}

/** The registered steps in the order they run. */
function orderedSteps(): LogoutStep[] {
  return PHASES.flatMap((phase) => [...steps].filter(([, p]) => p === phase).map(([step]) => step));
}

/**
 * Step 4: drops this browser's push subscription. The server already deleted its copy with the session, so there is
 * no request here (NotificationsProvider.disable() would make one, and get a 401). Browsers without a service
 * worker or PushManager, and the desktop app, have nothing to drop.
 */
async function unsubscribePushLocally(): Promise<void> {
  try {
    const sw = (globalThis.navigator as Partial<Navigator> | undefined)?.serviceWorker;
    const registration = await sw?.getRegistration();
    // Safari before 16.4 has service workers without push.
    if (!registration || !('pushManager' in registration)) return;
    const subscription = await registration.pushManager.getSubscription();
    await subscription?.unsubscribe();
  } catch (err) {
    log.warn('could not drop the push subscription', { error: err });
  }
}

/** What logout() needs from the app's services. */
export interface LogoutServices {
  readonly queryClient: QueryClient;
}

/**
 * Logs this browser session out. Resolves when the server has ended the session and this tab has forgotten the
 * user; ['me'] is then null, so guarded routes show the login page. Rejects (NetworkError, ApiError) when the server
 * couldn't end the session; the user is then still signed in.
 */
export async function logout({ queryClient }: LogoutServices): Promise<void> {
  const undo: LogoutUndo[] = [];
  for (const step of orderedSteps()) {
    try {
      const u = await step();
      if (typeof u === 'function') undo.push(u);
    } catch (err) {
      log.warn('a logout step failed', { error: err });
    }
  }

  try {
    await api('POST', '/api/v1/auth/logout');
  } catch (err) {
    // 401 unauthenticated can't come from this public, idempotent endpoint (03 §12.3 #3); if a proxy or a later
    // server version sends it, the session is gone all the same.
    if (!isSignedOutError(err)) {
      for (const u of undo.reverse()) {
        try {
          u();
        } catch (undoErr) {
          log.warn('could not undo a logout step', { error: undoErr });
        }
      }
      throw err;
    }
  }

  await unsubscribePushLocally();
  broadcastLogout();
  endSession(queryClient);
}
