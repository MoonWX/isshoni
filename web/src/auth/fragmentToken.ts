// The one-time tokens of /setup, /invite and /reset (05 §4 step 0, §14.1, §15.1, §20; 03 §7.8–§7.10).
//
// A token arrives in the URL fragment, which never reaches the server. Boot step 0 (app/boot.tsx,
// stashFragmentToken) moves it into sessionStorage['isshoni.<kind>'] and drops the fragment with
// history.replaceState before the first request, GET /api/v1/info included. This module is the pages' side: read the
// stashed token, keep it in memory while the page is open, and clear the stored copy once the form has succeeded.
//
// Rules (05 §20): the token travels only in JSON request bodies. Nothing here puts it into a URL, a log line or
// localStorage, and it stays in sessionStorage only until the form succeeds. A reload of the tab before that still
// finds it, so a friend who reloads halfway through signing up doesn't need the link again.
//
// A second link in the same tab: after step 0 the address bar shows /invite, so a new /invite#<token> pasted there
// differs only in its fragment. The browser then doesn't load the page again (it fires `hashchange`), and boot doesn't
// run. useFragmentToken() gives that fragment the same treatment (stash it, drop it from the address bar) and hands
// the page the new token. Without it, a friend who got "this invite has expired" and then a fresh link would keep
// seeing the old answer, with the new token left in the address bar.
import { useCallback, useEffect, useState } from 'react';

import { fragmentTokenKey, stashFragmentToken, type FragmentTokenKind } from '../app/boot';
import { usePlatform } from '../app/context';
import type { KeyValueStore } from '../platform/types';

export type { FragmentTokenKind };

/** The token boot stashed for this kind of page in this tab, or null when the page was opened without one. */
export function readFragmentToken(session: KeyValueStore, kind: FragmentTokenKind): string | null {
  const token = session.get(fragmentTokenKey(kind));
  return token === null || token === '' ? null : token;
}

/** Forgets the stored token. Pages call it when their form has succeeded: the link is used up (05 §20). */
export function clearFragmentToken(session: KeyValueStore, kind: FragmentTokenKind): void {
  session.remove(fragmentTokenKey(kind));
}

export interface FragmentToken {
  /**
   * The token, kept in memory while the page is open; null when there is none. It changes when another link of
   * the same kind is opened in this tab: pages start over then (they key their content by it).
   */
  readonly token: string | null;
  /** Removes the stored copy; the one in memory stays until the page unmounts. */
  readonly clear: () => void;
}

/**
 * The page's token: read from platform.storage.session when the page mounts and kept in component state, so the
 * form still has it after clear(). While the page is open, a fragment that appears in the address bar on this
 * page's path replaces it (see the header).
 */
export function useFragmentToken(kind: FragmentTokenKind): FragmentToken {
  const { session } = usePlatform().storage;
  const [token, setToken] = useState(() => readFragmentToken(session, kind));

  useEffect(() => {
    const onHashChange = (): void => {
      // Boot's own step 0: it acts only on /setup, /invite and /reset with a non-empty fragment.
      if (stashFragmentToken(globalThis.location, globalThis.history, session) === kind) {
        setToken(readFragmentToken(session, kind));
      }
    };
    globalThis.addEventListener('hashchange', onHashChange);
    return () => {
      globalThis.removeEventListener('hashchange', onHashChange);
    };
  }, [session, kind]);

  const clear = useCallback(() => {
    clearFragmentToken(session, kind);
  }, [session, kind]);
  return { token, clear };
}
