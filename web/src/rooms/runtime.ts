// The room runtime: the tab's one signaling connection and its room session, with their stores (05 §7 "One
// connection per tab", §11.1 "The session is app-level"). It is made once per app (getRoomRuntime), on the first
// room page, and kept until logout or tab close; pages reach it through the hooks in rooms/hooks.ts.
//
// Besides creating the parts it wires what belongs to none of them:
// - 01's `invalidate` messages refetch REST data (protocol/invalidate.ts, 05 §6.2);
// - signing out stops the connection: when ['me'] becomes null (a logout here or in another tab, a 401, a session
//   the server revoked: app/me.ts), the client stops, which closes with 1000 so the server skips the grace period
//   (05 §7 "Intentional leave"), and the session lets go of its room. This is the net under every way of being
//   signed out, not the logout flow: a logout started in this tab makes ['me'] null only after its request, and by
//   then the server has closed the socket itself (session_revoked). 05 §15.1's order (stop the share, stop the
//   client, then the request) is the logout flow's, which runs session.stopShare() and signal.stop() of this
//   runtime as its first steps, and signal.start() again when the request fails: the session keeps its desired
//   room through a stop(), so that start rejoins it;
// - a REST call answered 503 server_shutdown tells the banner that the outage is a restart (05 §7.1).
import type { AppServices } from '../app/context';
import { createLogger } from '../lib/log';
import { CodeServerShutdown } from '../protocol/api.gen';
import { applyInvalidate } from '../protocol/invalidate';
import { queryKeys } from '../protocol/queryKeys';
import { ApiError } from '../protocol/rest';
import type { SignalClient } from '../protocol/signal-client';
import { MessageTypeInvalidate } from '../protocol/types.gen';
import { createConnection, createConnectionStore, type Stores } from './connection';
import { RoomSession, type SessionMedia } from './RoomSession';
import { createRoomStore } from './roomStore';

export interface RoomRuntime {
  readonly signal: SignalClient;
  readonly session: RoomSession;
  readonly stores: Stores;
  /** Connects, unless the client already runs: the first room page calls it, and so does one after a new login. */
  start(): void;
  /** Signing out: stops the client (close 1000) and leaves the room locally. A later start() connects anew. */
  stop(): void;
  /** Stops, and removes every listener. The runtime can't be used afterwards (tests). */
  dispose(): void;
}

export interface RoomRuntimeOptions {
  /** The media folders' parts of the session (viewer/'s sub PC). */
  media?: SessionMedia;
  /** For the SignalClient's stale-build check; tests set it. */
  buildVersion?: string;
}

/**
 * The media seams of this build. viewer/ plugs in here once it is on the branch:
 * `createSubscriber: (deps) => new SubscriberPC({...deps, registry})` (05 §10.1). The local share needs no entry: it
 * comes from platform.sharing (05 §8).
 */
function defaultMedia(): SessionMedia {
  return {};
}

export function createRoomRuntime(services: AppServices, opts: RoomRuntimeOptions = {}): RoomRuntime {
  const { platform, queryClient, ui, prefs } = services;
  const log = createLogger('room');
  const stores: Stores = { connection: createConnectionStore(), room: createRoomStore(), ui, prefs };
  const signal = createConnection(platform, stores, {
    queryClient,
    log: log.child('signal'),
    ...(opts.buildVersion !== undefined ? { buildVersion: opts.buildVersion } : {}),
  });
  const session = new RoomSession({ platform, signal, stores, log, media: opts.media ?? defaultMedia() });
  stores.session = session;

  const stop = (): void => {
    signal.stop();
    // Offline now, so this only cleans up: the share, the sub PC, the stores.
    void session.leave();
  };

  const offInvalidate = signal.on(MessageTypeInvalidate, (msg) => {
    applyInvalidate(queryClient, msg).catch((err: unknown) => {
      log.warn('invalidate failed', { error: err });
    });
  });

  /** A REST call failed (each attempt of a query, a mutation): 503 server_shutdown says the server is restarting. */
  const restFailed = (error: unknown): void => {
    if (error instanceof ApiError && error.code === CodeServerShutdown) stores.connection.getState().serverRestarting();
  };

  const offQueries = queryClient.getQueryCache().subscribe((event) => {
    if (event.type !== 'updated') return;
    if (event.action.type === 'failed' || event.action.type === 'error') {
      restFailed(event.action.error);
      return;
    }
    if (event.action.type !== 'success') return;
    // The cache's queries are typed any; these two fields are all that is read.
    const { queryKey, state } = event.query as { queryKey: readonly unknown[]; state: { data: unknown } };
    if (queryKey.length !== 1 || queryKey[0] !== queryKeys.me[0] || state.data !== null) return;
    if (signal.state === 'stopped' && session.roomId === null) return;
    log.info('signed out: stopping the connection');
    stop();
  });

  const offMutations = queryClient.getMutationCache().subscribe((event) => {
    if (event.type === 'updated' && (event.action.type === 'failed' || event.action.type === 'error')) {
      restFailed(event.action.error);
    }
  });

  return {
    signal,
    session,
    stores,
    start() {
      signal.start();
    },
    stop,
    dispose() {
      offInvalidate();
      offQueries();
      offMutations();
      signal.stop();
      session.dispose();
    },
  };
}

const runtimes = new WeakMap<AppServices, RoomRuntime>();

/**
 * The app's runtime, made on first use (opts count only then). One per AppServices, so each test's services get
 * their own.
 */
export function getRoomRuntime(services: AppServices, opts?: RoomRuntimeOptions): RoomRuntime {
  let runtime = runtimes.get(services);
  if (runtime === undefined) {
    runtime = createRoomRuntime(services, opts);
    runtimes.set(services, runtime);
  }
  return runtime;
}
