// Why the user was sent to the login page (05 §6.3): the connection wiring navigates to /login?next=… "with a
// notice" when a `session`-scope error ends the session (session_revoked: "You were signed out"). The notice is an
// error code carried in the navigation's location state, not in the URL, so it shows once and a shared or
// bookmarked /login link never carries it:
//
//   navigate(loginPath(location), { state: loginState('session_revoked') });
//
// The login page shows errors.<code> for it (lib/errorText.ts); an unknown code falls back to errors.unknown.

/** The location state of a navigation to /login. */
export interface LoginLocationState {
  /** An error code of 01 §12.1 or 03 §12.2, shown above the form as errors.<code>. */
  readonly notice?: string;
}

/** The location state for a navigation to /login that explains itself. */
export function loginState(notice: string): LoginLocationState {
  return { notice };
}

/** The notice code in a location's state, or null. Location state is `unknown`: anything may be in history. */
export function loginNoticeCode(state: unknown): string | null {
  if (typeof state !== 'object' || state === null) return null;
  const notice = (state as { notice?: unknown }).notice;
  return typeof notice === 'string' && notice !== '' ? notice : null;
}
