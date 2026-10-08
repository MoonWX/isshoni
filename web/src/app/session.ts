// Cross-tab logout (05 §6.2): a BroadcastChannel('isshoni') message {type: 'logout'} clears ['me'] in the user's
// other tabs, with the same redirect rule as a 401 (guarded routes go to the login page, public pages stay).
// The logout flow (S33) calls broadcastLogout() after POST /api/v1/auth/logout succeeds; boot listens.

export const CHANNEL_NAME = 'isshoni';

export interface LogoutMessage {
  readonly type: 'logout';
}

function isLogoutMessage(data: unknown): data is LogoutMessage {
  return typeof data === 'object' && data !== null && (data as { type?: unknown }).type === 'logout';
}

/** Tells the other tabs of this origin that the user logged out. A no-op without BroadcastChannel. */
export function broadcastLogout(): void {
  if (typeof globalThis.BroadcastChannel !== 'function') return;
  const channel = new BroadcastChannel(CHANNEL_NAME);
  channel.postMessage({ type: 'logout' } satisfies LogoutMessage);
  channel.close();
}

/** Calls onLogout when another tab logs out. Returns the function that stops listening. */
export function listenForLogout(onLogout: () => void): () => void {
  if (typeof globalThis.BroadcastChannel !== 'function') return () => undefined;
  const channel = new BroadcastChannel(CHANNEL_NAME);
  channel.onmessage = (e: MessageEvent<unknown>) => {
    if (isLogoutMessage(e.data)) onLogout();
  };
  return () => {
    channel.close();
  };
}
