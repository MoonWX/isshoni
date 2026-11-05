// The active server's link state for this page (docs/m2/03 §6.3, §8.2): the isshoni:auth.state events of this page's
// server, subscribed first, then Auth.State() pulled, so nothing is missed. A state whose origin isn't this page's
// serverOrigin is ignored (03 §5.8): the window is about to reload for the new active server.
import { useEffect, useState } from 'react';

import type { AuthState, NativeBridge } from '../platform/desktop/bridge';

export interface AuthStateResult {
  /** null until the first state arrives. */
  state: AuthState | null;
  /** The pull failed: the bridge error's code. */
  failed: string | null;
  /** Sets the state from a method's own answer (LinkWithPassword, Recheck), when it is this page's server's. */
  set: (s: AuthState) => void;
}

export function useAuthState(bridge: NativeBridge, origin: string): AuthStateResult {
  const [state, setState] = useState<AuthState | null>(null);
  const [failed, setFailed] = useState<string | null>(null);
  useEffect(() => {
    if (origin === '') return undefined;
    let live = true;
    const off = bridge.on('auth.state', (s) => {
      if (live && s.origin === origin) setState(s);
    });
    bridge.auth.state().then(
      (s) => {
        // An event may have come first: it is the newer state.
        if (live && s.origin === origin) setState((prev) => prev ?? s);
      },
      (err: unknown) => {
        const code = (err as { code?: unknown } | null)?.code;
        if (live) setFailed(typeof code === 'string' ? code : 'internal');
      },
    );
    return () => {
      live = false;
      off();
    };
  }, [bridge, origin]);
  return {
    state,
    failed,
    set: (s) => {
      if (s.origin === origin) setState(s);
    },
  };
}
