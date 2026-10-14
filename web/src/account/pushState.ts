// Web Push on this device, as the notifications page, the room's card and the Home Screen sheet see it (05 §16.3).
// No React here but the hook at the end, so the room's lazy prompts and the account page share one state.
//
// The state is one store per platform (each test's platform has its own): what platform.notifications says
// (support(), then status() where push is supported) and whether an action runs. Every part of the page that shows
// push reads the same store, so the card in the room and the page's switch never disagree, and an action in one is
// seen by the other at once.
//
// status() is asked only on pages under RequireAuth (the room page, the account pages): each call is also the
// provider's start-up re-post of the subscription (05 §16.3, platform/browser/push.ts), which logged-out pages must
// never make. The room page and the notifications page read again on every mount, not once per page life: a login
// after a logout or a revoked session happens inside the same page, and the new session needs the re-post too
// (03 §12.4.6).
//
// The one-time prompts keep their dismissals in prefsStore (`dismissed`, per device): the room's card for 30 days,
// the iOS Home Screen sheet for 14, the first-launch offer of an iOS Home Screen app for good.
import type { TFunction } from 'i18next';
import { useEffect } from 'react';
import { useStore } from 'zustand';
import { createStore, type StoreApi } from 'zustand/vanilla';

import { useApp } from '../app/context';
import { LocalError } from '../lib/errors';
import { errorMessage } from '../lib/errorText';
import { createLogger } from '../lib/log';
import type { Platform, PushSupport } from '../platform/types';
import { ApiError, NetworkError } from '../protocol/rest';

const log = createLogger('push-ui');

/** prefsStore dismissal ids and how long each lasts. */
export const CARD_DISMISSAL = 'push.card';
export const CARD_DISMISSAL_MS = 30 * 24 * 60 * 60_000;
export const HOME_SCREEN_DISMISSAL = 'push.homeScreen';
export const HOME_SCREEN_DISMISSAL_MS = 14 * 24 * 60 * 60_000;
/** The first launch of an iOS Home Screen app offers notifications once (no maxAge: every dismissal counts). */
export const APP_LAUNCH_DISMISSAL = 'push.appLaunch';

export type PushStatus = 'on' | 'off';

/** What an enable() click came to. */
export type EnableOutcome = 'on' | 'off' | 'denied' | 'failed';

export interface PushState {
  /** platform.notifications.support(); null until it answered. */
  readonly support: PushSupport | null;
  /** status() where push is supported, 'off' everywhere else; null until known. */
  readonly status: PushStatus | null;
  /** A refresh or an action is running. */
  readonly busy: boolean;
  /** The last action's failure (enable, disable, test); null after a success or a new action. */
  readonly error: unknown;
}

/** The actions are plain functions (no `this`): components take them out of the state. */
export interface PushActions {
  /** Reads support() and status() again. Concurrent calls share one read. */
  readonly refresh: () => Promise<void>;
  /** platform.notifications.enable() (call it from a click), then a refresh. Never rejects. */
  readonly enable: () => Promise<EnableOutcome>;
  /** platform.notifications.disable(), then a refresh. Never rejects; false on a failure (in `error`). */
  readonly disable: () => Promise<boolean>;
  /** platform.notifications.test(). Never rejects; false on a failure (in `error`). */
  readonly test: () => Promise<boolean>;
}

export type PushStore = StoreApi<PushState & PushActions>;

const stores = new WeakMap<Platform, PushStore>();

/** The push state of this platform's page (created on first use). */
export function pushStore(platform: Platform): PushStore {
  let store = stores.get(platform);
  if (!store) {
    store = createPushStore(platform);
    stores.set(platform, store);
  }
  return store;
}

function createPushStore(platform: Platform): PushStore {
  const provider = platform.notifications;
  let reading: Promise<void> | null = null;

  return createStore<PushState & PushActions>()((set, get) => {
    const read = async (): Promise<void> => {
      let support: PushSupport;
      try {
        support = await provider.support();
      } catch (err) {
        log.warn('notifications.support() failed', { error: err });
        support = 'unsupported';
      }
      let status: PushStatus = 'off';
      if (support === 'supported') {
        try {
          status = await provider.status();
        } catch (err) {
          log.warn('notifications.status() failed', { error: err });
        }
      }
      set({ support, status });
    };

    const refresh = (): Promise<void> =>
      (reading ??= read().finally(() => {
        reading = null;
      }));

    /** Runs an action with busy set; a failure goes to `error`. */
    const run = async (action: () => Promise<void>): Promise<boolean> => {
      set({ busy: true, error: null });
      try {
        await action();
        return true;
      } catch (err) {
        set({ error: err });
        return false;
      } finally {
        set({ busy: false });
      }
    };

    return {
      support: null,
      status: null,
      busy: false,
      error: null,
      refresh,
      async enable() {
        // enable() first, in the click's own turn: the browser asks for permission only within the user activation.
        let enabling: Promise<void>;
        try {
          enabling = provider.enable();
        } catch (err) {
          enabling = Promise.reject(err instanceof Error ? err : new Error(String(err)));
        }
        const ok = await run(() => enabling);
        await refresh();
        if (!ok) return 'failed';
        const { support, status } = get();
        if (status === 'on') return 'on';
        return support === 'denied' ? 'denied' : 'off';
      },
      async disable() {
        const ok = await run(() => provider.disable());
        await refresh();
        return ok;
      },
      test: () => run(() => provider.test()),
    };
  });
}

/**
 * The message for a failed push action: the server's or the network's reason as everywhere (05 §6.3), and one
 * text for what went wrong in the browser itself (no worker, the push service refused, …: the platform's own
 * errors, which only its log tells apart).
 */
export function pushErrorMessage(err: unknown, t: TFunction): string {
  if (err instanceof ApiError || err instanceof NetworkError || err instanceof LocalError) return errorMessage(err, t);
  return t('account.push.setupFailed');
}

/**
 * The page's push state; the first reader of a page's life asks the platform. A page that needs a fresh read (and
 * with it the re-post) on every visit calls refresh() when it mounts, as the room page and the notifications page do.
 */
export function usePush(): PushState & PushActions {
  const { platform } = useApp();
  const store = pushStore(platform);
  const state = useStore(store);
  useEffect(() => {
    if (store.getState().support === null) void store.getState().refresh();
  }, [store]);
  return state;
}
