// The rarely used parts of DesktopPlatform, loaded on demand so that the entry chunk stays small (docs/m2/03 §7.2):
// what follows a stop of C1 (§7.3) and the "Update the app" action of the version screen (§16.3).
import type { AppUpdateOutcome, DesktopFeatures, SignalStopAction } from '../types';
import type { NativeBridge, UpdateStatus } from './bridge';
import { bridgeErrorCode } from './errors';

/**
 * C1 stopped with err: Auth.SessionEnded for session_revoked, account_disabled and unauthenticated, Auth.HelloRefused
 * for client_outdated and protocol_unsupported, both with this page's serverOrigin (desktop binds it); then what the
 * room runtime does next. not_found means this page's server isn't active any more: wait for the reload.
 */
export async function signalStopped(
  desktop: Pick<DesktopFeatures, 'sessionEnded' | 'helloRefused'>,
  err: { code: string; params?: Readonly<Record<string, unknown>> },
): Promise<SignalStopAction> {
  const { code } = err;
  if (code === 'session_revoked' || code === 'account_disabled' || code === 'unauthenticated') {
    try {
      return (await desktop.sessionEnded(code)).status === 'linked' ? 'redial' : 'relink';
    } catch (e) {
      return bridgeErrorCode(e) === 'not_found' ? 'wait' : 'relink';
    }
  }
  if (code === 'client_outdated' || code === 'protocol_unsupported') {
    try {
      await desktop.helloRefused(err.params ? { code, params: { ...err.params } } : { code });
      return 'version';
    } catch (e) {
      return bridgeErrorCode(e) === 'not_found' ? 'wait' : 'version';
    }
  }
  return 'none';
}

/** Updates.Check, then the first final update.state: install a ready update, or say why not (docs/m2/03 §16.3). */
export function updateApp(bridge: NativeBridge, openExternal: (url: string) => void): Promise<AppUpdateOutcome> {
  return new Promise((resolve) => {
    let done = false;
    let off = (): void => undefined;
    const finish = (o: AppUpdateOutcome): void => {
      done = true;
      off();
      resolve(o);
    };
    const fail = (e: unknown): void => {
      finish({ kind: 'error', code: bridgeErrorCode(e) ?? 'internal' });
    };
    const decide = (s: UpdateStatus): void => {
      if (done) return;
      if (s.state === 'ready') {
        done = true;
        off();
        bridge.updates.install().then(() => {
          resolve({ kind: 'installing' });
        }, fail);
      } else if (s.state === 'up_to_date') {
        finish({ kind: 'up-to-date' });
      } else if (s.state === 'error' && s.errorCode === 'not_installed') {
        openExternal(s.notesUrl);
        finish({ kind: 'not-installed', notesUrl: s.notesUrl });
      } else if (s.state === 'error' || s.state === 'off' || s.state === 'disabled') {
        finish({ kind: 'error', code: s.errorCode || s.state });
      }
    };
    // Subscribe first: the check's own answer may already be final, or the final state may come as an event.
    off = bridge.on('update.state', decide);
    bridge.updates.check().then(decide, (e: unknown) => {
      if (!done) fail(e);
    });
  });
}
