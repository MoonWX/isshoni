// The room runtime: the tab's one signaling connection and its room session, with their stores (05 §7 "One
// connection per tab", §11.1 "The session is app-level"). It is made once per app (getRoomRuntime), on the first
// room page, and kept until logout or tab close; pages reach it through the hooks in rooms/hooks.ts.
//
// Besides creating the parts it wires what belongs to none of them:
// - the viewer (05 §10, §12): viewer/'s store, media registry and <audio> element are made here, once per page,
//   and follow the session from here on (connectViewer.ts): every room.state, subscribe.status, the "bo started
//   sharing [Watch]" toast, and the subscriptions the viewer wants (session.subscriptions). The session's sub PC is
//   viewer/'s SubscriberPC, made by the viewer on that registry and store (defaultMedia); a volume the user sets
//   is remembered on the device (prefsStore);
// - the stats (05 §10.7): the collector that samples the PCs while the session is in a room, and the debug handle
//   of the e2e tests (connectStats.ts);
// - 01's `invalidate` messages refetch REST data (protocol/invalidate.ts, 05 §6.2);
// - signing out stops the connection: when ['me'] becomes null (a logout here or in another tab, a 401, a session
//   the server revoked: app/me.ts), the client stops, which closes with 1000 so the server skips the grace period
//   (05 §7 "Intentional leave"), and the session lets go of its room. This is the net under every way of being
//   signed out, not the logout flow: a logout started in this tab makes ['me'] null only after its request, and by
//   then the server has closed the socket itself (session_revoked). 05 §15.1's order (stop the share, stop the
//   client, then the request) is the logout flow's (auth/logout.ts): the runtime registers its two steps there
//   with addLogoutStep, session.stopShare() and signal.stop(), and signal.start() as the undo when the request
//   fails: the session keeps its desired room through a stop(), so that start rejoins it;
// - a REST call answered 503 server_shutdown tells the banner that the outage is a restart (05 §7.1).
import type { AppServices } from '../app/context';
import type { UiStore } from '../app/uiStore';
import { addLogoutStep } from '../auth/logout';
import { createLogger } from '../lib/log';
import { CodeServerShutdown } from '../protocol/api.gen';
import { applyInvalidate } from '../protocol/invalidate';
import { queryKeys } from '../protocol/queryKeys';
import { ApiError } from '../protocol/rest';
import type { SignalClient } from '../protocol/signal-client';
import { MessageTypeInvalidate } from '../protocol/types.gen';
import { createViewer, type ViewerServices } from '../viewer/services';
import { createConnection, createConnectionStore, type Stores } from './connection';
import { connectStats } from './connectStats';
import { connectViewer } from './connectViewer';
import { lazySubscriber } from './lazySubscriber';
import { loadRoomMedia } from './loadMedia';
import { RoomSession, type SessionMedia } from './RoomSession';
import { createRoomStore } from './roomStore';

export interface RoomRuntime {
  readonly signal: SignalClient;
  readonly session: RoomSession;
  readonly stores: Stores;
  /**
   * The viewer's store, media registry and audio (viewer/'s createViewer), made once per page: what the room page
   * gives its ViewerLayout, and what the session's sub PC writes.
   */
  readonly viewer: ViewerServices;
  /** Connects, unless the client already runs: the first room page calls it, and so does one after a new login. */
  start(): void;
  /** Signing out: stops the client (close 1000) and leaves the room locally. A later start() connects anew. */
  stop(): void;
  /** Stops, and removes every listener. The runtime can't be used afterwards (tests). */
  dispose(): void;
}

export interface RoomRuntimeOptions {
  /** The media folders' parts of the session (viewer/'s sub PC). Default: defaultMedia(). Tests pass fakes. */
  media?: SessionMedia;
  /** For the SignalClient's stale-build check; tests set it. */
  buildVersion?: string;
}

/**
 * The media seams of this build: the session's sub PC is viewer/'s SubscriberPC (05 §10.1), writing the page's
 * media registry and viewerStore.media, and showing the Fatal screen "Can't connect media" through uiStore. The
 * viewer makes it (viewer.createSubscriber), so it knows the PC for the stats. Its code is in the room's media
 * chunk, so it is loaded with the first sub offer (lazySubscriber.ts); the room page has usually fetched that chunk
 * by then. The local share needs no entry: it comes from platform.sharing (05 §8).
 */
function defaultMedia(viewer: ViewerServices, ui: UiStore): SessionMedia {
  return {
    createSubscriber: (deps) =>
      lazySubscriber(() =>
        loadRoomMedia().then(
          ({ SubscriberPC }) =>
            () =>
              viewer.createSubscriber({ ...deps, ui }, SubscriberPC),
        ),
      ),
  };
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
  const viewer = createViewer({
    volume: prefs.getState().volume,
    // The stage's volume slider: the device remembers it (05 §10.3).
    onVolume: (volume) => {
      prefs.getState().setVolume(volume);
    },
    log: log.child('viewer'),
  });
  const session = new RoomSession({ platform, signal, stores, log, media: opts.media ?? defaultMedia(viewer, ui) });
  stores.session = session;
  const offViewer = connectViewer({ viewer, signal, session, room: stores.room, ui });
  const offStats = connectStats({
    viewer,
    room: stores.room,
    connection: stores.connection,
    storage: platform.storage.session,
    log: log.child('stats'),
  });

  const stop = (): void => {
    signal.stop();
    // Offline now, so this only cleans up: the share, the sub PC, the stores.
    void session.leave();
  };

  // 05 §15.1 steps 1 and 2 of a logout started in this tab: the share stops while signaling can still say
  // share.stop, then the client closes with 1000. A client that wasn't running stays stopped when the logout fails.
  const offLogoutShares = addLogoutStep('shares', () => session.stopShare().then(() => undefined));
  const offLogoutSignal = addLogoutStep('signal', () => {
    if (signal.state === 'stopped') return undefined;
    signal.stop();
    return () => {
      signal.start();
    };
  });

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
    viewer,
    start() {
      signal.start();
    },
    stop,
    dispose() {
      offStats();
      offViewer();
      offLogoutShares();
      offLogoutSignal();
      offInvalidate();
      offQueries();
      offMutations();
      signal.stop();
      session.dispose();
      // The page's <audio> element goes with the runtime.
      viewer.dispose();
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

/**
 * The app's runtime if one was made, without making one: for what shows the session on other pages (InRoomBar) and
 * must not start a connection of its own.
 */
export function peekRoomRuntime(services: AppServices): RoomRuntime | undefined {
  return runtimes.get(services);
}
