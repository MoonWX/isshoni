// The boot sequence (05 §4). main.tsx calls startApp() unless /boot-check.js already flagged the browser as too old.
//
//  0. /setup, /invite, /reset with a fragment: stash the token in sessionStorage['isshoni.<kind>'] and drop the
//     fragment with history.replaceState, before any request (05 §20). The pages read it from there.
//  1. Not a secure context → NeedsHttps. No WebRTC at all → Unsupported (boot-check.js normally catches that first).
//  2. platform = detectPlatform(). It runs before step 0 here, because step 0 writes through platform.storage.session
//     (its memory fallback must be the one the pages read); it makes no request, so step 0 still precedes them all.
//  3. i18next with the bundled en.json; <html lang>.
//  4. GET /api/v1/info, retried after 1, 2 and 4 s: unreachable → Offline (retries on `online` and every 10 s);
//     setupRequired and the path isn't /setup → NotSetUp; any other failure → Fatal.
//  5. The QueryClient (seeded with info: app/info.ts, which pages read through useInfo()) and the router; render.
//  6. After the first render, production builds only: register the service worker when the browser is idle.
import { StrictMode, useCallback, useEffect, useRef, useState } from 'react';
import { createRoot, type Root } from 'react-dom/client';
import { I18nextProvider } from 'react-i18next';
import { createBrowserRouter, type DataRouter, type RouteObject } from 'react-router';

import { initI18n } from '../i18n';
import { createLogger } from '../lib/log';
import { sleep, whenIdle } from '../lib/time';
import { registerServiceWorker } from '../platform/browser/pwa';
import { detectPlatform } from '../platform/detect';
import type { KeyValueStore, Platform } from '../platform/types';
import type { Info } from '../protocol/api.gen';
import { ApiError, api, configureApi, isRetryableError } from '../protocol/rest';
import { PageSpinner } from '../ui/Spinner';
import { App } from './App';
import { createAppServices, type AppServices } from './context';
import { seedInfo } from './info';
import { clearMe } from './me';
import { createQueryClient } from './queryClient';
import { createAppRoutes } from './router';
import { AppScreenView } from './screens';
import { listenForLogout } from './session';
import type { AppScreen } from './uiStore';

const log = createLogger('boot');

/** Waits before the 2nd, 3rd and 4th GET /api/v1/info (05 §4). */
export const INFO_RETRY_DELAYS_MS: readonly number[] = [1000, 2000, 4000];
/** The Offline screen tries again this often (and on `online`). */
export const OFFLINE_RETRY_MS = 10_000;

// ---- step 0 ----

export type FragmentTokenKind = 'setup' | 'invite' | 'reset';

const FRAGMENT_TOKEN_PATHS: Readonly<Record<string, FragmentTokenKind>> = {
  '/setup': 'setup',
  '/invite': 'invite',
  '/reset': 'reset',
};

/** sessionStorage key of a stashed token: isshoni.setup, isshoni.invite, isshoni.reset (05 §4). */
export function fragmentTokenKey(kind: FragmentTokenKind): string {
  return `isshoni.${kind}`;
}

/**
 * Step 0: on /setup, /invite or /reset with a fragment, stores the fragment (the token) under
 * fragmentTokenKey(kind) and removes it from the address bar with history.replaceState. Returns the kind stored,
 * or null. auth/fragmentToken.ts (S33) reads and clears the stored token.
 */
export function stashFragmentToken(
  loc: Pick<Location, 'pathname' | 'search' | 'hash'>,
  history: Pick<History, 'replaceState' | 'state'>,
  session: KeyValueStore,
): FragmentTokenKind | null {
  const kind = Object.hasOwn(FRAGMENT_TOKEN_PATHS, loc.pathname) ? FRAGMENT_TOKEN_PATHS[loc.pathname] : undefined;
  const raw = loc.hash.startsWith('#') ? loc.hash.slice(1) : loc.hash;
  if (kind === undefined || raw === '') return null;
  let token = raw;
  try {
    token = decodeURIComponent(raw);
  } catch {
    // Not percent-encoded as expected: keep it as it came.
  }
  session.set(fragmentTokenKey(kind), token);
  history.replaceState(history.state, '', loc.pathname + loc.search);
  return kind;
}

// ---- step 4 ----

export type InfoResult =
  | { readonly kind: 'ok'; readonly info: Info }
  | { readonly kind: 'offline' }
  | { readonly kind: 'fatal'; readonly code: string };

function isInfo(v: unknown): v is Info {
  if (typeof v !== 'object' || v === null) return false;
  const r = v as Record<string, unknown>;
  return typeof r['setupRequired'] === 'boolean' && typeof r['server'] === 'object' && r['server'] !== null;
}

/**
 * GET /api/v1/info with retries: after each failure that may pass (no response, 5xx, server_busy, rate_limited)
 * it waits the next delay and tries again; when the delays run out it is offline. A failure that can't pass (4xx,
 * or a response that isn't Info) is fatal.
 */
export async function loadInfo(
  retryDelaysMs: readonly number[] = INFO_RETRY_DELAYS_MS,
  signal?: AbortSignal,
): Promise<InfoResult> {
  for (let attempt = 0; ; attempt++) {
    try {
      const info = await api<unknown>('GET', '/api/v1/info', undefined, signal ? { signal } : {});
      if (!isInfo(info)) {
        log.error('GET /api/v1/info returned something that is not Info');
        return { kind: 'fatal', code: 'unknown' };
      }
      return { kind: 'ok', info };
    } catch (err) {
      if (signal?.aborted) throw err;
      if (!isRetryableError(err)) {
        log.error('GET /api/v1/info failed', { error: err });
        return { kind: 'fatal', code: err instanceof ApiError ? err.code : 'unknown' };
      }
      const delay = retryDelaysMs[attempt];
      if (delay === undefined) {
        log.warn('server unreachable', { error: err });
        return { kind: 'offline' };
      }
      await sleep(delay, signal);
    }
  }
}

// ---- steps 4–5 in React ----

export interface BootOptions {
  /** Default: detectPlatform(). */
  platform?: Platform;
  /** Default: the page's location and history. */
  location?: Pick<Location, 'pathname' | 'search' | 'hash'>;
  history?: Pick<History, 'replaceState' | 'state'>;
  /** Default: INFO_RETRY_DELAYS_MS. */
  retryDelaysMs?: readonly number[];
  /** Default: OFFLINE_RETRY_MS. */
  offlineRetryMs?: number;
  /** Default: createBrowserRouter (tests: createMemoryRouter). */
  createRouter?: (routes: RouteObject[]) => DataRouter;
  /** Default: createAppRoutes(). */
  routes?: () => RouteObject[];
}

type Phase = { kind: 'loading' } | InfoResult;

/** Runs step 4 and shows its screens; on success builds the router (step 5) and renders the app. */
function BootGate({
  services,
  path,
  retryDelaysMs,
  offlineRetryMs,
  getRouter,
}: {
  services: AppServices;
  path: string;
  retryDelaysMs: readonly number[];
  offlineRetryMs: number;
  /** Creates the router on the first call and returns the same one after (StrictMode renders twice). */
  getRouter: () => DataRouter;
}) {
  const [phase, setPhase] = useState<Phase>({ kind: 'loading' });
  const [retrying, setRetrying] = useState(false);
  const running = useRef(false);

  const run = useCallback(
    async (delays: readonly number[]) => {
      if (running.current) return;
      running.current = true;
      setRetrying(true);
      try {
        const result = await loadInfo(delays);
        if (result.kind === 'ok') seedInfo(services.queryClient, result.info);
        setPhase(result);
      } finally {
        running.current = false;
        setRetrying(false);
      }
    },
    [services.queryClient],
  );

  useEffect(() => {
    void run(retryDelaysMs);
  }, [run, retryDelaysMs]);

  // Offline: one more try on `online` and every offlineRetryMs (no inner retries: the interval is the backoff).
  const offline = phase.kind === 'offline';
  useEffect(() => {
    if (!offline) return undefined;
    const retry = (): void => {
      void run([]);
    };
    globalThis.addEventListener('online', retry);
    const id = setInterval(retry, offlineRetryMs);
    return () => {
      globalThis.removeEventListener('online', retry);
      clearInterval(id);
    };
  }, [offline, offlineRetryMs, run]);

  switch (phase.kind) {
    case 'loading':
      return <PageSpinner />;
    case 'offline':
      return (
        <AppScreenView
          screen={{ kind: 'offline' }}
          platform={services.platform}
          onRetry={() => void run([])}
          retrying={retrying}
        />
      );
    case 'fatal':
      return <AppScreenView screen={{ kind: 'fatal', code: phase.code }} platform={services.platform} />;
    case 'ok':
      if (phase.info.setupRequired && path !== '/setup') {
        return <AppScreenView screen={{ kind: 'notSetUp' }} platform={services.platform} />;
      }
      return <App services={services} router={getRouter()} />;
  }
}

/** What startApp started: the React root and, past steps 1–2, the services. stop() unmounts and unhooks. */
export interface StartedApp {
  readonly root: Root;
  readonly services: AppServices | null;
  /** The screen boot stopped at before rendering the app (needsHttps, unsupported), else null. */
  readonly screen: AppScreen | null;
  stop(): void;
}

/** Runs the boot sequence into container (05 §4). */
export function startApp(container: Element, opts: BootOptions = {}): StartedApp {
  const loc = opts.location ?? globalThis.location;
  const history = opts.history ?? globalThis.history;
  // Step 2 (first, see the header), step 0, step 3.
  const platform = opts.platform ?? detectPlatform();
  stashFragmentToken(loc, history, platform.storage.session);
  const i18n = initI18n();
  const root = createRoot(container);

  // Step 1.
  const early: AppScreen | null = !globalThis.isSecureContext
    ? { kind: 'needsHttps' }
    : typeof globalThis.RTCPeerConnection !== 'function'
      ? { kind: 'unsupported' }
      : null;
  if (early) {
    root.render(
      <StrictMode>
        <I18nextProvider i18n={i18n}>
          <AppScreenView screen={early} platform={platform} />
        </I18nextProvider>
      </StrictMode>,
    );
    return {
      root,
      services: null,
      screen: early,
      stop: () => {
        root.unmount();
      },
    };
  }

  // Steps 4–5.
  configureApi(platform);
  const services = createAppServices(platform, createQueryClient());
  const stopLogoutListener = listenForLogout(() => {
    clearMe(services.queryClient);
  });
  // One router for the page's life, made once /info has answered (it starts loading the first route at once).
  let router: DataRouter | null = null;
  const createRouter = opts.createRouter ?? createBrowserRouter;
  const routes = opts.routes ?? createAppRoutes;
  const getRouter = (): DataRouter => (router ??= createRouter(routes()));
  root.render(
    <StrictMode>
      <I18nextProvider i18n={i18n}>
        <BootGate
          services={services}
          path={loc.pathname}
          retryDelaysMs={opts.retryDelaysMs ?? INFO_RETRY_DELAYS_MS}
          offlineRetryMs={opts.offlineRetryMs ?? OFFLINE_RETRY_MS}
          getRouter={getRouter}
        />
      </I18nextProvider>
    </StrictMode>,
  );

  // Step 6.
  let cancelIdle: (() => void) | null = null;
  if (import.meta.env.PROD && platform.pwa) {
    cancelIdle = whenIdle(() => {
      registerServiceWorker().catch((err: unknown) => {
        log.warn('service worker registration failed', { error: err });
      });
    });
  }

  return {
    root,
    services,
    screen: null,
    stop() {
      cancelIdle?.();
      stopLogoutListener();
      root.unmount();
      router?.dispose();
    },
  };
}
