// Signaling wiring (05 §7, §7.1, §16.4) against the fake signaling server: what hello carries, connectionStore, the
// banner's states over time, where a stopped connection sends the user, and the one reload for a stale build.
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import type { AppScreen } from '../app/uiStore';
import { queryKeys } from '../protocol/queryKeys';
import { BackoffMaxMs } from '../protocol/signal-client';
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
import { createHarness, FakeShare, pickedSource, refuseConnections, tick, type Harness } from './testing/harness';

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

    // A newer server version later is a new reason.
    stalePage('0.3.0');
    await h.connect();
    expect(reload).toHaveBeenCalledTimes(2);
    expect(h.platform.storage.session.get(RELOADED_FOR_KEY)).toBe('0.3.0');
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
