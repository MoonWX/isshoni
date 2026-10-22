// The desktop SPA's access token (docs/m2/03 §5.8, §7.3; 02 §11.8). Created with the page's serverOrigin, which it
// passes in every Auth.AccessToken call, so a page that outlives a switch of the active server never gets another
// server's token (Go answers null for any origin but the active one).
//
// The token lives in this closure only: never in Web Storage, a store or a log. It is cached with its expiry and asked
// for again 60 s before it (Go hands out tokens with at least 120 s left); concurrent callers share one call.
import type { AuthState, NativeBridge } from './bridge';

/** Ask Go again this long before expiresAt. */
export const TokenRenewBeforeMs = 60_000;

export interface Tokens {
  /**
   * The access token for serverOrigin, or null (not linked, or serverOrigin isn't the active server any more).
   * force: Go drops the token it had and refreshes (after the server refused it; at most once per 10 s in Go).
   * Rejects with the bridge's error (server_unreachable, incompatible_server).
   */
  get(force?: boolean): Promise<string | null>;
  /** The server refused token: the cache forgets it (when it still holds it), and the next get() forces. */
  rejected(token: string): void;
  /** Stops listening to auth.state. */
  dispose(): void;
}

export interface TokensOptions {
  /** The page's serverOrigin (bridge.js's server.origin). */
  origin: string;
  bridge: Pick<NativeBridge, 'auth' | 'on'>;
  /** ms since the epoch (expiresAt's clock). Default Date.now. */
  now?: () => number;
}

interface Cached {
  token: string;
  expiresAt: number;
}

export function createTokens(opts: TokensOptions): Tokens {
  const { origin, bridge } = opts;
  const now = opts.now ?? Date.now;
  let cached: Cached | null = null;
  let forceNext = false;
  /** Bumped whenever the cache is cleared: an answer to an older call must not refill it. */
  let generation = 0;
  const inFlight: { plain: Promise<string | null> | null; forced: Promise<string | null> | null } = {
    plain: null,
    forced: null,
  };

  const clear = (): void => {
    cached = null;
    generation++;
  };

  const ask = (force: boolean): Promise<string | null> => {
    const gen = generation;
    const call = bridge.auth.accessToken(origin, force).then((t) => {
      if (gen === generation) cached = t === null ? null : { token: t.token, expiresAt: t.expiresAt };
      return t === null ? null : t.token;
    });
    const slot = force ? 'forced' : 'plain';
    const done = call.finally(() => {
      if (inFlight[slot] === done) inFlight[slot] = null;
    });
    inFlight[slot] = done;
    return done;
  };

  const unsubscribe = bridge.on('auth.state', (s: AuthState) => {
    // Another server's state means the window is about to reload (docs/m2/03 §8.2): nothing to do with it.
    if (s.origin !== origin) return;
    if (s.status !== 'linked') clear();
  });

  return {
    get(force = false) {
      if (force || forceNext) {
        forceNext = false;
        // A forced call replaces whatever the cache or a plain call would give.
        clear();
        return inFlight.forced ?? ask(true);
      }
      if (cached !== null && now() < cached.expiresAt - TokenRenewBeforeMs) return Promise.resolve(cached.token);
      return inFlight.forced ?? inFlight.plain ?? ask(false);
    },
    rejected(token) {
      if (cached?.token !== token) return;
      clear();
      forceNext = true;
    },
    dispose() {
      unsubscribe();
    },
  };
}
