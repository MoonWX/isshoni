// The in-app browser banner's state (05 §16.3), apart from its component (InAppBrowserBanner.tsx) so that code
// which only needs to know whether the banner shows doesn't pull the component in.
//
// A chat app that opens an invite link in its own browser gives the friend a second cookie jar and no Add to Home
// Screen. platform.inAppBrowser() (lib/ua.ts, known UA tokens only) says whether this is such a browser; the invite,
// login and room pages then show the banner until the friend dismisses it. Dismissal lasts for the tab
// (platform.storage.session['isshoni.inAppBannerDismissed']).
//
// Until the banner is dismissed, the Home Screen sheet and card of the `needs-install` push state stay hidden; they
// appear once it is dismissed or the page is opened in a real browser. They read `useInAppBanner().showing` for that,
// and every reader re-renders when the banner is dismissed anywhere on the page.
//
// `showing` describes the tab, not the page: an in-app browser, and the banner not dismissed in this tab, whether or
// not the current page renders <InAppBrowserBanner />. A page that hides its needs-install sheet or card on this flag
// must render the banner itself (login, invite and room do), otherwise the user has nothing to dismiss.
import { useCallback, useSyncExternalStore } from 'react';

import { usePlatform } from '../app/context';
import type { KeyValueStore } from '../platform/types';
import { ClientOSAndroid, ClientOSIOS, type ClientOS } from '../protocol/types.gen';

/** platform.storage.session: set once the banner was dismissed in this tab. */
export const IN_APP_BANNER_DISMISSED_KEY = 'isshoni.inAppBannerDismissed';

/** Per session store, the readers to tell about a dismissal (each test's platform has its own store). */
const listeners = new WeakMap<KeyValueStore, Set<() => void>>();

function subscribe(session: KeyValueStore, fn: () => void): () => void {
  let set = listeners.get(session);
  if (!set) {
    set = new Set();
    listeners.set(session, set);
  }
  set.add(fn);
  return () => {
    set.delete(fn);
  };
}

/** Whether the banner was dismissed in this tab. */
export function inAppBannerDismissed(session: KeyValueStore): boolean {
  return session.get(IN_APP_BANNER_DISMISSED_KEY) !== null;
}

/** Dismisses the banner for the rest of this tab's life and tells every useInAppBanner() reader. */
export function dismissInAppBanner(session: KeyValueStore): void {
  session.set(IN_APP_BANNER_DISMISSED_KEY, '1');
  for (const fn of [...(listeners.get(session) ?? [])]) fn();
}

/** The browser the banner names: Safari on iOS and iPadOS, Chrome on Android, "your browser" elsewhere. */
export type ExternalBrowser = 'safari' | 'chrome' | 'other';

/** Which browser to name for a client OS (platform.client.os). It only chooses the text. */
export function externalBrowser(os: ClientOS): ExternalBrowser {
  if (os === ClientOSIOS) return 'safari';
  if (os === ClientOSAndroid) return 'chrome';
  return 'other';
}

/**
 * The invite link as it was opened: `<origin>/invite#<token>`. Boot step 0 moved the token out of the address bar
 * (05 §4), so location.href no longer has it; the invite page passes the token it keeps in memory. The token stays
 * in the fragment, which never reaches a server (05 §20).
 */
export function inviteLink(origin: string, token: string): string {
  return `${new URL('/invite', origin).href}#${encodeURIComponent(token)}`;
}

export interface InAppBanner {
  /** The chat or social app whose built-in browser this is (platform.inAppBrowser()), else null. */
  readonly app: string | null;
  /**
   * An in-app browser, and the banner not dismissed in this tab, whether or not the current page renders
   * <InAppBrowserBanner />. A page that hides its `needs-install` sheet or card on this flag (05 §16.3) must render
   * the banner itself (login, invite and room do), otherwise the user has nothing to dismiss.
   */
  readonly showing: boolean;
  /** Hides the banner for the rest of this tab's life. */
  readonly dismiss: () => void;
}

/** The banner's state for this tab. Every component that reads it follows a dismissal at once. */
export function useInAppBanner(): InAppBanner {
  const platform = usePlatform();
  const { session } = platform.storage;
  const dismissed = useSyncExternalStore(
    useCallback((fn: () => void) => subscribe(session, fn), [session]),
    () => inAppBannerDismissed(session),
  );
  const dismiss = useCallback(() => {
    dismissInAppBanner(session);
  }, [session]);
  const app = platform.inAppBrowser();
  return { app, showing: app !== null && !dismissed, dismiss };
}
