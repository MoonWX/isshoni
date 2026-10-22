// What the app's own code reads of the desktop bridge's rejections (docs/m2/03 §6.4). The transports reject with a
// BridgeError (platform/desktop/errors.ts); its code is read by shape, because importing errors.ts here would split it
// into one more chunk of the first page load.
import { NetworkError } from '../protocol/rest';

/** The code of a bridge call's rejection, or 'unknown'. */
export function rejectionCode(err: unknown): string {
  const code = (err as { code?: unknown } | null | undefined)?.code;
  return typeof code === 'string' && code !== '' ? code : 'unknown';
}

/**
 * A request the desktop app didn't send because Go refused it a token with incompatible_server (Auth.AccessToken,
 * docs/m2/03 §6.2; 02's ErrIncompatible): apiFetch passes that rejection on, and api() wraps it in a NetworkError. The
 * server wasn't asked, so it isn't unreachable and trying again changes nothing: the active server's AuthState is
 * `incompatible`, and the link step shows its version texts with Try again (03 §8.2, §16.3).
 */
export function isIncompatibleServer(err: unknown): boolean {
  return err instanceof NetworkError && rejectionCode(err.cause) === 'incompatible_server';
}
