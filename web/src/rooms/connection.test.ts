// Signaling wiring (05 §7, §7.1, §16.4) against the fake signaling server: what hello carries, connectionStore, the
// banner's states over time, where a stopped connection sends the user, and the one reload for a stale build.
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { createDesktopTestPlatform, type DesktopTestPlatform } from '../app/testing/desktop';
import type { AppScreen } from '../app/uiStore';
import type { DesktopFeatures, SignalStopAction } from '../platform/types';
import { queryKeys } from '../protocol/queryKeys';
import { BackoffMaxMs, BackoffResetMs } from '../protocol/signal-client';
import { makeError } from '../protocol/testing';
import {
  connectionBanner,
  createConnectionStore,
  nextBannerChange,
  RECONNECTING_AFTER_MS,
  RELOADED_FOR_KEY,
  UNREACHABLE_AFTER_MS,
  type ConnectionState,
} from './connection';
import {
  createHarness,
  FakeShare,
  pickedSource,
  refuseConnections,
  tick,
  type Harness,
  type HarnessOptions,
} from './testing/harness';

let h: Harness;

beforeEach(() => {
  vi.useFakeTimers();
  // No jitter: the backoff waits are exactly 0.5, 1, 2, 4, 8, 10 s.
  vi.spyOn(Math, 'random').mockReturnValue(0.5);
  h = createHarness();
});

afterEach(() => {
  h.close();
  vi.useRealTimers();
  vi.restoreAllMocks();
});

const connection = (): ConnectionState => h.runtime.stores.connection.getState();
const screen = (): AppScreen | null => h.services.ui.getState().screen;
const banner = () => connectionBanner(connection(), Date.now());

describe('createConnection (05 §7)', () => {
  it('says hello with the platform’s client, role and caps, and no features', async () => {
    await h.connect();
    expect(h.server.socket.url).toBe(h.platform.signaling().url);
    expect(h.server.hellos[0]).toMatchObject({
      client: h.platform.client,
      role: 'full',
      caps: h.platform.capsNow(),
      features: [],
    });
  });

  it('says hello without auth in the browser, and with the platform’s auth (M2: docs/m2/03 §7.3)', async () => {
    await h.connect();
    expect(h.server.hellos[0]).not.toHaveProperty('auth');
    h.close();
    const calls: boolean[] = [];
    const url = h.platform.signaling().url;
    h = createHarness({
      platform: {
        signaling: () => ({
          url,
          auth: ({ forceRefresh }) => {
            calls.push(forceRefresh);
            return Promise.resolve({ scheme: 'bearer', token: 'isa_t1' });
          },
        }),
      },
    });
    await h.connect();
    expect(h.server.hellos[0]?.auth).toEqual({ scheme: 'bearer', token: 'isa_t1' });
    expect(calls).toEqual([false]);
  });

  it('re-reads the caps on every reconnect', async () => {
    h.close();
    let decode = ['opus'];
    h = createHarness({ platform: { capsNow: () => ({ decode }) } });
    await h.connect();
    decode = ['opus', 'h264/42e0'];
    h.server.drop();
    await tick(BackoffMaxMs);
    expect(h.server.hellos.map((x) => x.caps.decode)).toEqual([['opus'], ['opus', 'h264/42e0']]);
  });

  it('does not start the client by itself', () => {
    expect(h.runtime.signal.state).toBe('stopped');
    expect(connection()).toMatchObject({ state: 'stopped', welcome: null, downSince: null, stopReason: null });
  });

  it('gives every welcome to the session’s resync, with the welcome already in the store', async () => {
    const seen: (string | undefined)[] = [];
    const resync = vi.spyOn(h.runtime.session, 'resync').mockImplementation((w) => {
      seen.push(connection().welcome?.connectionId, w.connectionId);
      return Promise.resolve();
    });
    await h.connect();
    h.server.drop();
    await tick(BackoffMaxMs);
    expect(resync).toHaveBeenCalledTimes(2);
    const id = h.server.welcomes[0]?.connectionId;
    expect(seen).toEqual([id, id, id, id]);
    expect(h.server.welcomes[1]?.resumed).toBe(true);
  });
});

describe('connectionStore (05 §6.1)', () => {
  it('mirrors the client: welcome, resumed, and downSince from the first backoff to the next ready', async () => {
    await h.connect();
    expect(connection()).toMatchObject({ state: 'ready', resumed: false, staleBuild: false, downSince: null });
    expect(connection().welcome).toEqual(h.server.welcomes[0]);

    const restore = refuseConnections(h.server);
    const droppedAt = Date.now();
    h.server.drop();
    expect(connection()).toMatchObject({ state: 'backoff', downSince: droppedAt, retryAt: droppedAt + 500 });
    // The outage spans the failed attempts in between.
    await tick(4_000);
    expect(connection().downSince).toBe(droppedAt);
    expect(connection().welcome).toEqual(h.server.welcomes[0]);

    restore();
    await tick(BackoffMaxMs);
    expect(connection()).toMatchObject({ state: 'ready', resumed: true, downSince: null, retryAt: null });
    expect(connection().welcome).toEqual(h.server.welcomes[1]);
  });

  it('forgets the last connection’s welcome when the client starts again', async () => {
    await h.connect();
    h.runtime.signal.stop();
    expect(connection()).toMatchObject({ state: 'stopped', stopReason: null });
    expect(connection().welcome).not.toBeNull();
    h.server.autoOpen = false;
    h.runtime.start();
    expect(connection()).toMatchObject({ state: 'connecting', welcome: null, resumed: false });
  });

  it('keeps the rate-limit wait: retryAt and rateLimited', async () => {
    await h.connect();
    const at = Date.now();
    h.server.error(makeError('rate_limited', 'connection', { retryAfterMs: 45_000 }));
    expect(connection()).toMatchObject({ state: 'backoff', rateLimited: true, retryAt: at + 45_000 });
  });
});

describe('connectionBanner (05 §7.1)', () => {
  const down = { state: 'backoff', downSince: 1_000, shutdown: null, retryAt: null, rateLimited: false } as const;

  it('shows nothing while ready, stopped or on the first connect', () => {
    const store = createConnectionStore();
    expect(connectionBanner(store.getState(), 5_000)).toBeNull();
    expect(connectionBanner({ ...down, state: 'ready', downSince: null }, 99_000)).toBeNull();
    expect(connectionBanner({ ...down, state: 'stopped', downSince: null }, 99_000)).toBeNull();
    expect(connectionBanner({ ...down, state: 'connecting', downSince: null }, 99_000)).toBeNull();
  });

  it('follows the time the connection has been down: 2 s, then 30 s', () => {
    // The values of 05 §18.
    expect([RECONNECTING_AFTER_MS, UNREACHABLE_AFTER_MS]).toEqual([2_000, 30_000]);
    expect(connectionBanner(down, 1_000 + RECONNECTING_AFTER_MS - 1)).toBeNull();
    expect(connectionBanner(down, 1_000 + RECONNECTING_AFTER_MS)).toEqual({ kind: 'reconnecting', retryInSec: null });
    // An attempt in between is still the same outage.
    expect(connectionBanner({ ...down, state: 'handshaking' }, 10_000)?.kind).toBe('reconnecting');
    expect(connectionBanner(down, 1_000 + UNREACHABLE_AFTER_MS - 1)?.kind).toBe('reconnecting');
    expect(connectionBanner(down, 1_000 + UNREACHABLE_AFTER_MS)).toEqual({ kind: 'unreachable', retryInSec: null });
  });

  it('says "restarting" at once after a server.shutdown, and "unreachable" when it is not back after 30 s', () => {
    const restarting = { ...down, shutdown: { reason: 'restart', reconnectInMs: 1_000 } } as const;
    expect(connectionBanner(restarting, 1_000)?.kind).toBe('restarting');
    expect(connectionBanner(restarting, 20_000)?.kind).toBe('restarting');
    expect(connectionBanner(restarting, 1_000 + UNREACHABLE_AFTER_MS)?.kind).toBe('unreachable');
  });

  it('counts a rate-limit wait down in whole seconds', () => {
    const limited = { ...down, rateLimited: true, retryAt: 40_500 } as const;
    expect(connectionBanner(limited, 31_000)).toEqual({ kind: 'unreachable', retryInSec: 10 });
    expect(connectionBanner(limited, 40_499)).toEqual({ kind: 'unreachable', retryInSec: 1 });
    expect(connectionBanner(limited, 40_500)).toEqual({ kind: 'unreachable', retryInSec: null });
    // Between attempts nothing is waited for.
    expect(connectionBanner({ ...limited, state: 'connecting' }, 31_000)?.retryInSec).toBeNull();
  });

  it('nextBannerChange gives the wait to the next mark', () => {
    expect(nextBannerChange({ ...down, state: 'ready', downSince: null }, 5_000)).toBeNull();
    expect(nextBannerChange(down, 1_500)).toBe(RECONNECTING_AFTER_MS - 500);
    expect(nextBannerChange(down, 4_000)).toBe(UNREACHABLE_AFTER_MS - 3_000);
    expect(nextBannerChange(down, 40_000)).toBeNull();
    // Restarting has no 2 s mark, only the 30 s one.
    expect(nextBannerChange({ ...down, shutdown: { reason: 'restart', reconnectInMs: 0 } }, 1_500)).toBe(
      UNREACHABLE_AFTER_MS - 500,
    );
    // The countdown: every whole second.
    const limited = { ...down, rateLimited: true, retryAt: 40_500 } as const;
    expect(nextBannerChange(limited, 31_200)).toBe(300);
    expect(nextBannerChange(limited, 31_500)).toBe(1_000);
  });

  it('over a real outage: nothing for 2 s, "Reconnecting…", "Can’t reach the server" after 30 s, gone on recovery', async () => {
    await h.connect();
    const restore = refuseConnections(h.server);
    h.server.drop();
    expect(banner()).toBeNull();
    await tick(RECONNECTING_AFTER_MS - 1);
    expect(banner()).toBeNull();
    await tick(1);
    expect(banner()?.kind).toBe('reconnecting');
    await tick(UNREACHABLE_AFTER_MS - RECONNECTING_AFTER_MS);
    expect(banner()?.kind).toBe('unreachable');

    restore();
    h.runtime.signal.retryNow();
    await tick(BackoffMaxMs);
    expect(connection().state).toBe('ready');
    expect(banner()).toBeNull();
    // The banner just disappears, so the recovery is said (05 §16.6).
    expect(h.services.ui.getState().announcements.polite?.text).toBe('Reconnected');
  });

  it('a blip under 2 s shows nothing and announces nothing', async () => {
    await h.connect();
    h.server.drop();
    await tick(600);
    expect(connection().state).toBe('ready');
    expect(h.services.ui.getState().announcements.polite).toBeNull();
  });

  it('a server.shutdown reads "Server restarting…" until ready', async () => {
    await h.connect();
    const restore = refuseConnections(h.server);
    h.server.shutdown(1_000);
    expect(connection().shutdown).toEqual({ reason: 'restart', reconnectInMs: 1_000 });
    expect(banner()?.kind).toBe('restarting');
    await tick(5_000);
    expect(banner()?.kind).toBe('restarting');
    restore();
    await tick(BackoffMaxMs);
    expect(connection()).toMatchObject({ state: 'ready', shutdown: null });
    expect(banner()).toBeNull();
  });
});

describe('a stopped connection (05 §7.1)', () => {
  const me = () => h.services.queryClient.getQueryData(queryKeys.me);

  beforeEach(async () => {
    h.services.queryClient.setQueryData(queryKeys.me, { user: { id: 'k3m9p2qxw7ht' } });
    await h.connect();
  });

  it.each([
    ['unauthenticated', "You're signed out. Log in again."],
    ['session_revoked', 'You were signed out.'],
  ])('%s: the login page, with a notice', (code, notice) => {
    h.server.error(makeError(code, 'session'));
    expect(connection()).toMatchObject({ state: 'stopped' });
    expect(connection().stopReason?.code).toBe(code);
    // ['me'] null is what sends guarded routes to /login?next=… (app/guards.tsx).
    expect(me()).toBeNull();
    expect(h.toasts()).toEqual([notice]);
    expect(screen()).toBeNull();
  });

  it('close 4401 without an error: the login page', () => {
    h.server.close(4401);
    expect(me()).toBeNull();
    expect(screen()).toBeNull();
  });

  it('a session-scope code this build does not know: the login page (01 §12.3)', () => {
    h.server.error(makeError('session_moved', 'session'));
    expect(me()).toBeNull();
    expect(h.toasts()).toEqual(['Something went wrong (session_moved).']);
  });

  it('account_disabled: the Fatal screen "Your account was disabled"', () => {
    h.server.error(makeError('account_disabled', 'session'));
    expect(screen()).toEqual({ kind: 'fatal', reason: 'account_disabled' });
    expect(h.toasts()).toEqual([]);
  });

  it('too_many_connections: the Fatal screen "Close other isshoni tabs"', () => {
    h.server.error(makeError('too_many_connections', 'connection'));
    expect(screen()).toEqual({ kind: 'fatal', reason: 'too_many_connections' });
  });

  it('replaced: nothing (another socket took over)', () => {
    h.server.error(makeError('replaced', 'connection'));
    expect(connection().state).toBe('stopped');
    expect(screen()).toBeNull();
    expect(me()).not.toBeNull();
    expect(h.toasts()).toEqual([]);
  });

  it.each<[string, { error: string } | { close: number }, string]>([
    ['bad_message', { error: 'bad_message' }, 'bad_message'],
    ['hello_required', { error: 'hello_required' }, 'hello_required'],
    ['bad_request (a second hello)', { error: 'bad_request' }, 'bad_request'],
    ['an unknown code', { error: 'quota_exceeded' }, 'quota_exceeded'],
    ['close 1003', { close: 1003 }, 'bad_message'],
    ['close 4400', { close: 4400 }, 'bad_message'],
    ['close 4403', { close: 4403 }, 'forbidden'],
  ])('%s: the Fatal screen "Something went wrong" with Reload', (_name, how, code) => {
    if ('error' in how) h.server.error(makeError(how.error, 'connection'));
    else h.server.close(how.close);
    expect(connection().state).toBe('stopped');
    expect(screen()).toEqual({ kind: 'fatal', reason: 'generic', code });
    expect(me()).not.toBeNull();
  });

  it('stop() itself shows nothing', () => {
    h.runtime.signal.stop();
    expect(connection()).toMatchObject({ state: 'stopped', stopReason: null });
    expect(screen()).toBeNull();
    expect(h.toasts()).toEqual([]);
  });

  describe('protocol_unsupported: VersionMismatch (05 §16.4)', () => {
    it('with the server’s version; reload when the page is the old one', () => {
      h.server.error(
        makeError('protocol_unsupported', 'connection', {
          params: { serverMin: 2, serverMax: 3, serverVersion: '0.9.0' },
        }),
      );
      expect(screen()).toEqual({
        kind: 'versionMismatch',
        serverVersion: '0.9.0',
        serverOlder: false,
        stillStale: false,
      });
    });

    it('"ask your admin" when the server is the old one', () => {
      h.server.error(
        makeError('protocol_unsupported', 'connection', {
          params: { serverMin: 0, serverMax: 0, serverVersion: '0.0.9' },
        }),
      );
      expect(screen()).toMatchObject({ kind: 'versionMismatch', serverVersion: '0.0.9', serverOlder: true });
    });

    it('close 4426 without an error: the screen without versions', () => {
      h.server.close(4426);
      expect(screen()).toEqual({
        kind: 'versionMismatch',
        serverVersion: undefined,
        serverOlder: false,
        stillStale: false,
      });
    });

    it('says so when a reload did not help: the same server version again in this tab', async () => {
      const unsupported = makeError('protocol_unsupported', 'connection', { params: { serverVersion: '0.9.0' } });
      h.server.error(unsupported);
      expect(screen()).toMatchObject({ stillStale: false });
      expect(h.platform.storage.session.get(RELOADED_FOR_KEY)).toBe('0.9.0');

      // The reload: a new page with the same sessionStorage.
      const { platform } = h;
      h.close();
      h = createHarness({ platform: { storage: platform.storage } });
      await h.connect();
      h.server.error(unsupported);
      expect(screen()).toMatchObject({ kind: 'versionMismatch', serverVersion: '0.9.0', stillStale: true });
    });
  });
});

describe('a stale build (05 §16.4)', () => {
  const reload = vi.fn();

  /** A page of build 0.1.0 whose server says it runs serverVersion. */
  function stalePage(serverVersion: string, storage = h.platform.storage): void {
    h.close();
    h = createHarness({
      server: { welcome: { serverVersion } },
      platform: { storage, versionActions: () => ({ reload }) },
      runtime: { buildVersion: '0.1.0' },
    });
  }

  beforeEach(() => {
    reload.mockClear();
  });

  it('reloads exactly once per server version', async () => {
    stalePage('0.2.0');
    await h.connect();
    expect(connection().staleBuild).toBe(true);
    expect(reload).toHaveBeenCalledTimes(1);
    expect(h.platform.storage.session.get(RELOADED_FOR_KEY)).toBe('0.2.0');

    // Still this page (the test's reload is a spy): a reconnect is stale again, and must not reload again.
    h.server.drop();
    await tick(BackoffMaxMs);
    expect(connection()).toMatchObject({ state: 'ready', staleBuild: true });
    expect(reload).toHaveBeenCalledTimes(1);

    // The page after the reload, in the same tab: the reload didn't help. No loop, and the app keeps working.
    stalePage('0.2.0');
    await h.connect();
    expect(connection().staleBuild).toBe(true);
    expect(reload).toHaveBeenCalledTimes(1);
    expect(screen()).toBeNull();
    expect(h.hub.joins).toEqual(['lounge']);

    // A newer server version later is a new reason.
    stalePage('0.3.0');
    await h.connect();
    expect(reload).toHaveBeenCalledTimes(2);
    expect(h.platform.storage.session.get(RELOADED_FOR_KEY)).toBe('0.3.0');
  });

  it('reloads before the session joins a room: no join, leave and join again around the reload', async () => {
    stalePage('0.2.0');
    const wanted = h.runtime.session.join('lounge');
    wanted.catch(() => undefined);
    let sentAtReload: string[] | undefined;
    reload.mockImplementationOnce(() => {
      sentAtReload = h.server.messages().map((m) => m.type);
    });
    await h.connect();
    expect(sentAtReload).toEqual(['hello']);
    // Nothing follows either: the page is going away, and the one after it joins.
    expect(h.hub.joins).toEqual([]);
    expect(h.runtime.stores.room.getState()).toMatchObject({ roomId: 'lounge', joinState: 'joining' });

    // A page that is still here at the next welcome (the reload didn't happen) joins then.
    h.server.drop();
    await tick(BackoffMaxMs);
    await wanted;
    expect(reload).toHaveBeenCalledTimes(1);
    expect(h.hub.joins).toEqual(['lounge']);
  });

  it('a welcome without a server version is not stale, and the session resyncs', async () => {
    stalePage('0.2.0');
    h.server.welcomeDefaults = { serverVersion: undefined };
    await h.connect();
    expect(connection().staleBuild).toBe(false);
    expect(reload).not.toHaveBeenCalled();
    expect(h.hub.joins).toEqual(['lounge']);
  });

  it('a platform without a reload action carries on: the session joins', async () => {
    h.close();
    h = createHarness({
      server: { welcome: { serverVersion: '0.2.0' } },
      platform: { versionActions: () => ({}) },
      runtime: { buildVersion: '0.1.0' },
    });
    await h.connect();
    expect(connection().staleBuild).toBe(true);
    expect(h.platform.storage.session.get(RELOADED_FOR_KEY)).toBeNull();
    expect(h.hub.joins).toEqual(['lounge']);
  });

  it('does nothing when the versions match, or when one is a dev build', async () => {
    stalePage('0.1.0');
    await h.connect();
    stalePage('0.2.0-dev.1a2b3c4');
    await h.connect();
    expect(connection().staleBuild).toBe(false);
    expect(reload).not.toHaveBeenCalled();
  });

  it('while sharing: the UpdatePill instead of a reload', async () => {
    stalePage('0.2.0');
    vi.spyOn(h.runtime.session, 'share', 'get').mockReturnValue(new FakeShare('s1'));
    await h.connect();
    expect(reload).not.toHaveBeenCalled();
    expect(h.services.ui.getState().updateReady).toBe(true);
    expect(h.platform.storage.session.get(RELOADED_FOR_KEY)).toBeNull();
  });

  it('a session really sharing is not reloaded on a reconnect', async () => {
    stalePage('0.1.0');
    await h.connect();
    await h.runtime.session.join('lounge');
    await h.runtime.session.startShare(pickedSource, { preset: 'auto', withAudio: true });
    // The server is updated under the page.
    h.server.welcomeDefaults = { serverVersion: '0.2.0' };
    h.server.drop();
    await tick(BackoffMaxMs);
    expect(connection()).toMatchObject({ state: 'ready', staleBuild: true });
    expect(reload).not.toHaveBeenCalled();
    expect(h.services.ui.getState().updateReady).toBe(true);
  });
});

describe('M2: in the desktop app (docs/m2/03 §7.2, §7.3; 02 §11.8)', () => {
  let d: DesktopTestPlatform;
  const me = () => h.services.queryClient.getQueryData(queryKeys.me);
  const origin = (): string => d.platform.serverOrigin;

  /**
   * This page as the desktop app's: its kind, client, role and desktop features (over the fake bridge, whose
   * signalStopped is DesktopPlatform's own, platform/desktop/actions.ts), signed in.
   */
  function desktopPage(opts: HarnessOptions = {}): void {
    h.close();
    d = createDesktopTestPlatform();
    const { kind, client, role } = d.platform;
    h = createHarness({
      ...opts,
      platform: {
        kind,
        client,
        role,
        versionActions: () => d.platform.versionActions(),
        desktop: d.platform.desktop ?? null,
        ...opts.platform,
      },
    });
    h.services.queryClient.setQueryData(queryKeys.me, { user: { id: 'k3m9p2qxw7ht' } });
  }

  it('says hello as the desktop app: role viewer (C1 never publishes)', async () => {
    desktopPage();
    await h.connect();
    expect(h.server.hellos[0]).toMatchObject({ client: { kind: 'desktop' }, role: 'viewer' });
  });

  it('has no stale-build reload: the build version is "" whatever is given (§7.2)', async () => {
    const reload = vi.fn();
    desktopPage({
      server: { welcome: { serverVersion: '0.2.0' } },
      platform: { versionActions: () => ({ reload }) },
      // A browser page of this build would reload (see "a stale build" above).
      runtime: { buildVersion: '0.1.0' },
    });
    await h.connect();
    expect(connection()).toMatchObject({ state: 'ready', staleBuild: false });
    expect(reload).not.toHaveBeenCalled();
    expect(h.platform.storage.session.get(RELOADED_FOR_KEY)).toBeNull();
    expect(h.hub.joins).toEqual(['lounge']);
  });

  it('session_revoked and Go still linked: Auth.SessionEnded with this page’s origin, then one redial', async () => {
    desktopPage();
    await h.connect();
    h.server.error(makeError('session_revoked', 'session'));
    expect(connection().state).toBe('stopped');
    await tick();
    expect(d.fake.callsOf('auth.sessionEnded')).toEqual([[origin(), 'session_revoked']]);
    expect(h.server.hellos).toHaveLength(2);
    expect(connection().state).toBe('ready');
    // Nothing of M1's: no login page, no toast, no screen.
    expect(me()).not.toBeNull();
    expect(h.toasts()).toEqual([]);
    expect(screen()).toBeNull();
  });

  it('a revocation right after the redialled welcome: M1’s reaction, not another redial (no loop)', async () => {
    desktopPage();
    await h.connect();
    h.server.error(makeError('session_revoked', 'session'));
    await tick();
    expect(h.server.hellos).toHaveLength(2);
    expect(connection().state).toBe('ready');
    // Ready, but not yet for BackoffResetMs: the redial isn't back.
    await tick(BackoffResetMs - 1);
    h.server.error(makeError('session_revoked', 'session'));
    await tick();
    expect(d.fake.callsOf('auth.sessionEnded')).toHaveLength(2);
    expect(h.server.hellos).toHaveLength(2);
    expect(connection().state).toBe('stopped');
    expect(me()).toBeNull();
    expect(h.toasts()).toEqual(['You were signed out.']);
  });

  it('the redial comes back once a connection has stayed ready for BackoffResetMs: a later revocation redials again', async () => {
    desktopPage();
    await h.connect();
    h.server.error(makeError('session_revoked', 'session'));
    await tick();
    expect(connection().state).toBe('ready');
    await tick(BackoffResetMs);
    h.server.error(makeError('session_revoked', 'session'));
    await tick();
    expect(h.server.hellos).toHaveLength(3);
    expect(connection().state).toBe('ready');
    expect(me()).not.toBeNull();
    expect(h.toasts()).toEqual([]);
  });

  it('a ready that ends before BackoffResetMs doesn’t count: its time isn’t added to the next one’s', async () => {
    desktopPage();
    await h.connect();
    h.server.error(makeError('session_revoked', 'session'));
    await tick();
    expect(h.server.hellos).toHaveLength(2);
    await tick(BackoffResetMs / 2);
    // The network drops and the client comes back after its first backoff (0.5 s): a new ready, timed from now.
    h.server.drop();
    await tick(1_000);
    expect(connection().state).toBe('ready');
    expect(h.server.hellos).toHaveLength(3);
    // Ready for 5 s and then for 9.5 s: neither stayed for BackoffResetMs.
    await tick(BackoffResetMs - 1_000);
    h.server.error(makeError('session_revoked', 'session'));
    await tick();
    expect(connection().state).toBe('stopped');
    expect(me()).toBeNull();
  });

  it('one redial only: stopped again before ready, M1’s reaction (["me"] cleared, so the AuthState decides)', async () => {
    desktopPage();
    await h.connect();
    h.server.autoWelcome = false;
    h.server.error(makeError('session_revoked', 'session'));
    await tick();
    expect(h.server.hellos).toHaveLength(2);
    h.server.replyError(h.server.last('hello'), makeError('session_revoked', 'session'));
    await tick();
    expect(d.fake.callsOf('auth.sessionEnded')).toHaveLength(2);
    expect(h.server.hellos).toHaveLength(2);
    expect(connection().state).toBe('stopped');
    expect(me()).toBeNull();
    expect(h.toasts()).toEqual(['You were signed out.']);
  });

  it.each(['session_revoked', 'account_disabled', 'unauthenticated'])(
    '%s and Go unlinked: relink, through ["me"] (no toast, no Fatal screen, no redial)',
    async (code) => {
      desktopPage();
      d.fake.sessionEndStatus = 'unlinked';
      await h.connect();
      h.server.error(makeError(code, 'session'));
      await tick();
      expect(d.fake.callsOf('auth.sessionEnded')).toEqual([[origin(), code]]);
      // RequireAuth follows the AuthState from here (app/guards.tsx): /desktop/relink or the link step.
      expect(me()).toBeNull();
      expect(h.toasts()).toEqual([]);
      expect(screen()).toBeNull();
      expect(h.server.hellos).toHaveLength(1);
      expect(connection().state).toBe('stopped');
    },
  );

  it.each([
    ['client_outdated', { serverVersion: '0.3.0', minClientVersion: '0.2.5' }],
    ['protocol_unsupported', { serverMin: 2, serverMax: 3, serverVersion: '0.9.0' }],
  ])('%s: Auth.HelloRefused with the error, then the version screen', async (code, params) => {
    desktopPage();
    await h.connect();
    h.server.error(makeError(code, 'connection', { params }));
    await tick();
    expect(d.fake.callsOf('auth.helloRefused')).toEqual([[origin(), { code, params }]]);
    expect(screen()).toMatchObject({ kind: 'versionMismatch', serverVersion: params.serverVersion });
    expect(h.server.hellos).toHaveLength(1);
  });

  it('the version screen has no reload marker: never "Reloading didn’t help" in the desktop app', async () => {
    const outdated = makeError('client_outdated', 'connection', { params: { serverVersion: '0.3.0' } });
    desktopPage();
    await h.connect();
    h.server.error(outdated);
    await tick();
    expect(screen()).toMatchObject({ kind: 'versionMismatch', stillStale: false });
    expect(h.platform.storage.session.get(RELOADED_FOR_KEY)).toBeNull();
    // Once more in this window: still not "still stale".
    h.services.ui.getState().showScreen(null);
    h.runtime.start();
    await tick();
    h.server.error(outdated);
    await tick();
    expect(screen()).toMatchObject({ kind: 'versionMismatch', stillStale: false });
  });

  it.each(['session_revoked', 'client_outdated'])(
    '%s while this server is no longer the active one (not_found): stopped, nothing shown, no redial',
    async (code) => {
      desktopPage();
      await h.connect();
      // The user switched servers in another window: the bridge answers not_found, and this window reloads.
      d.fake.active = 'https://other.example';
      h.server.error(makeError(code, code === 'client_outdated' ? 'connection' : 'session'));
      await tick();
      expect(connection().state).toBe('stopped');
      expect(h.server.hellos).toHaveLength(1);
      expect(me()).not.toBeNull();
      expect(screen()).toBeNull();
      expect(h.toasts()).toEqual([]);
    },
  );

  it('codes Go leaves to the page: M1’s reactions', async () => {
    desktopPage();
    await h.connect();
    h.server.error(makeError('too_many_connections', 'connection'));
    await tick();
    expect(screen()).toEqual({ kind: 'fatal', reason: 'too_many_connections' });
    expect(d.fake.calls).toEqual([]);
  });

  it('calls signalStopped with the stop’s code and params, once per stop; stop() calls nothing', async () => {
    desktopPage();
    const signalStopped = vi.fn<DesktopFeatures['signalStopped']>(() => Promise.resolve('wait'));
    const features = h.platform.desktop;
    if (!features) throw new Error('no desktop features');
    Object.assign(features, { signalStopped });
    await h.connect();
    h.runtime.signal.stop();
    expect(signalStopped).not.toHaveBeenCalled();
    h.runtime.start();
    await tick();
    h.server.error(makeError('bad_message', 'connection', { params: { field: 'type' } }));
    await tick();
    expect(signalStopped).toHaveBeenCalledExactlyOnceWith({ code: 'bad_message', params: { field: 'type' } });
    // wait: nothing else happens.
    expect(screen()).toBeNull();
  });

  it('signalStopped failing (its chunk didn’t load): M1’s reaction', async () => {
    desktopPage();
    const features = h.platform.desktop;
    if (!features) throw new Error('no desktop features');
    Object.assign(features, { signalStopped: () => Promise.reject(new Error('chunk 404')) });
    await h.connect();
    h.server.error(makeError('session_revoked', 'session'));
    await tick();
    expect(me()).toBeNull();
    expect(h.toasts()).toEqual(['You were signed out.']);
  });

  it('started again before Go answered: that connection decides, the old stop changes nothing', async () => {
    desktopPage();
    let answer: (a: SignalStopAction) => void = () => undefined;
    const features = h.platform.desktop;
    if (!features) throw new Error('no desktop features');
    Object.assign(features, {
      signalStopped: () =>
        new Promise<SignalStopAction>((resolve) => {
          answer = resolve;
        }),
    });
    await h.connect();
    h.server.error(makeError('session_revoked', 'session'));
    h.runtime.start();
    await tick();
    expect(connection().state).toBe('ready');
    answer('relink');
    await tick();
    expect(me()).not.toBeNull();
    expect(h.server.hellos).toHaveLength(2);
  });
});
