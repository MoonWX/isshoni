import { QueryClient } from '@tanstack/react-query';
import { http, HttpResponse } from 'msw';
import { afterEach, describe, expect, it, vi } from 'vitest';

import { createMemoryStorage } from '../platform/browser/storage';
import { queryKeys } from '../protocol/queryKeys';
import { ApiError, api, NetworkError } from '../protocol/rest';
import { apiError, apiPath, meFixture, server, signedIn } from '../test/msw';
import { clearMe, fetchMe } from './me';
import { createPrefsStore, DEFAULT_PREFS, LAST_ROOM_KEY, loadPrefs, PREFS_KEY } from './prefs';
import { createQueryClient, retryDelayMs, shouldRetry, STALE_TIME_MS } from './queryClient';
import { broadcastLogout, listenForLogout } from './session';
import { createUiStore, MAX_TOASTS } from './uiStore';

describe('uiStore', () => {
  it('queues toasts with default durations and drops the oldest past the limit', () => {
    const ui = createUiStore();
    const id = ui.getState().toast({ kind: 'info', message: 'a' });
    ui.getState().toast({ kind: 'error', message: 'b' });
    ui.getState().toast({ kind: 'success', message: 'c', durationMs: null });
    expect(ui.getState().toasts.map((t) => [t.message, t.durationMs])).toEqual([
      ['a', 5000],
      ['b', 8000],
      ['c', null],
    ]);
    ui.getState().dismissToast(id);
    expect(ui.getState().toasts.map((t) => t.message)).toEqual(['b', 'c']);
    for (let i = 0; i < MAX_TOASTS + 2; i++) ui.getState().toast({ kind: 'info', message: `n${String(i)}` });
    expect(ui.getState().toasts).toHaveLength(MAX_TOASTS);
    expect(ui.getState().toasts.at(-1)?.message).toBe(`n${String(MAX_TOASTS + 1)}`);
  });

  it('announces each toast: errors assertively, others politely', () => {
    const ui = createUiStore();
    ui.getState().toast({ kind: 'success', message: 'Invite copied' });
    expect(ui.getState().announcements).toMatchObject({ polite: { text: 'Invite copied' }, assertive: null });
    ui.getState().toast({ kind: 'error', message: 'Room is full' });
    expect(ui.getState().announcements.assertive?.text).toBe('Room is full');
    const first = ui.getState().announcements.polite;
    ui.getState().toast({ kind: 'success', message: 'Invite copied' });
    expect(ui.getState().announcements.polite?.id).not.toBe(first?.id);
  });

  it('announces to the polite region by default, with a new id each time', () => {
    const ui = createUiStore();
    ui.getState().announce('alex started sharing');
    const first = ui.getState().announcements.polite;
    ui.getState().announce('alex started sharing');
    expect(ui.getState().announcements.polite?.id).not.toBe(first?.id);
    ui.getState().announce("You're live", 'assertive');
    expect(ui.getState().announcements.assertive?.text).toBe("You're live");
  });

  it('holds the app-level screen, install and update flags', () => {
    const ui = createUiStore();
    ui.getState().showScreen({ kind: 'fatal', reason: 'account_disabled' });
    ui.getState().setInstallAvailable(true);
    ui.getState().setUpdateReady(true);
    expect(ui.getState()).toMatchObject({
      screen: { kind: 'fatal', reason: 'account_disabled' },
      installAvailable: true,
      updateReady: true,
    });
    ui.getState().showScreen(null);
    expect(ui.getState().screen).toBeNull();
  });
});

describe('prefsStore (05 §6.1)', () => {
  it('starts from the defaults on an empty store', () => {
    expect(loadPrefs(createMemoryStorage())).toEqual(DEFAULT_PREFS);
  });

  it('persists lastRoomId under isshoni.lastRoomId (01 §10.5) and the rest as JSON', () => {
    const storage = createMemoryStorage();
    const prefs = createPrefsStore(storage);
    prefs.getState().setLastRoomId('k3m9p2qxw7ht');
    prefs.getState().setVolume(0.4);
    prefs.getState().setPreset('movie');
    prefs.getState().dismiss('push-card', 1000);
    prefs.getState().setDebug(true);
    expect(storage.get(LAST_ROOM_KEY)).toBe('k3m9p2qxw7ht');
    expect(JSON.parse(storage.get(PREFS_KEY) ?? '')).toEqual({
      volume: 0.4,
      preset: 'movie',
      dismissed: { 'push-card': 1000 },
      debug: true,
    });
    expect(loadPrefs(storage)).toEqual({
      volume: 0.4,
      lastRoomId: 'k3m9p2qxw7ht',
      preset: 'movie',
      dismissed: { 'push-card': 1000 },
      debug: true,
    });
    prefs.getState().setLastRoomId(null);
    expect(storage.get(LAST_ROOM_KEY)).toBeNull();
  });

  it("writes only what changed, so one tab doesn't undo another tab's room or dismissals", () => {
    const storage = createMemoryStorage({ [LAST_ROOM_KEY]: 'lounge' });
    const tabA = createPrefsStore(storage);
    const tabB = createPrefsStore(storage);
    tabA.getState().setLastRoomId('k3m9p2qxw7ht');
    tabA.getState().dismiss('push-card', 1000);
    tabA.getState().setPreset('text');
    // Tab B still holds what it loaded (lounge, no dismissals, preset auto).
    tabB.getState().setVolume(0.25);
    tabB.getState().dismiss('install-card', 2000);
    tabA.getState().dismiss('install-card', 3000);
    tabB.getState().dismiss('push-card', 500);
    expect(storage.get(LAST_ROOM_KEY)).toBe('k3m9p2qxw7ht');
    expect(loadPrefs(storage)).toEqual({
      volume: 0.25,
      lastRoomId: 'k3m9p2qxw7ht',
      preset: 'text',
      // The later time of each id wins.
      dismissed: { 'push-card': 1000, 'install-card': 3000 },
      debug: false,
    });
    // Leaving the room in tab B clears it; tab A's next change doesn't bring it back.
    tabB.getState().setLastRoomId(null);
    tabA.getState().setDebug(true);
    expect(storage.get(LAST_ROOM_KEY)).toBeNull();
    expect(loadPrefs(storage)).toMatchObject({ volume: 0.25, preset: 'text', debug: true });
  });

  it('writes nothing when a setter changes nothing', () => {
    const storage = createMemoryStorage();
    const set = vi.spyOn(storage, 'set');
    const remove = vi.spyOn(storage, 'remove');
    const prefs = createPrefsStore(storage);
    prefs.getState().setLastRoomId(null);
    prefs.getState().setDebug(false);
    expect(set).not.toHaveBeenCalled();
    expect(remove).not.toHaveBeenCalled();
  });

  it('keeps valid fields of a damaged value and defaults the rest', () => {
    const storage = createMemoryStorage({
      [PREFS_KEY]: JSON.stringify({ volume: 7, preset: 'loud', dismissed: { a: 5, b: 'x' }, debug: 'yes' }),
    });
    expect(loadPrefs(storage)).toEqual({ ...DEFAULT_PREFS, volume: 1, dismissed: { a: 5 } });
    expect(loadPrefs(createMemoryStorage({ [PREFS_KEY]: '{not json' }))).toEqual(DEFAULT_PREFS);
    expect(loadPrefs(createMemoryStorage({ [PREFS_KEY]: '[1,2]' }))).toEqual(DEFAULT_PREFS);
  });

  it('clamps the volume and ages dismissals', () => {
    const prefs = createPrefsStore(createMemoryStorage());
    prefs.getState().setVolume(-2);
    expect(prefs.getState().volume).toBe(0);
    const day = 24 * 3600 * 1000;
    prefs.getState().dismiss('push-card', 0);
    expect(prefs.getState().isDismissed('push-card')).toBe(true);
    expect(prefs.getState().isDismissed('push-card', 30 * day, 29 * day)).toBe(true);
    expect(prefs.getState().isDismissed('push-card', 30 * day, 30 * day)).toBe(false);
    expect(prefs.getState().isDismissed('other')).toBe(false);
  });
});

describe('query client (05 §6.2)', () => {
  it('uses a 30 s staleTime and no refetch on window focus', () => {
    const q = createQueryClient().getDefaultOptions().queries;
    expect(q?.staleTime).toBe(STALE_TIME_MS);
    expect(q?.refetchOnWindowFocus).toBe(false);
    expect(createQueryClient().getDefaultOptions().mutations?.retry).toBe(false);
  });

  it('retries network errors, 5xx, server_busy and rate_limited up to 3 times, nothing else', () => {
    const e = (status: number, code: string) => new ApiError({ status, code });
    expect(shouldRetry(0, new NetworkError())).toBe(true);
    expect(shouldRetry(2, e(503, 'server_busy'))).toBe(true);
    expect(shouldRetry(3, e(503, 'server_busy'))).toBe(false);
    expect(shouldRetry(0, e(429, 'rate_limited'))).toBe(true);
    expect(shouldRetry(0, e(500, 'internal'))).toBe(true);
    expect(shouldRetry(0, e(404, 'not_found'))).toBe(false);
    expect(shouldRetry(0, e(401, 'unauthenticated'))).toBe(false);
    expect(shouldRetry(0, new Error('bug'))).toBe(false);
  });

  it('waits retryAfter when the server gives one, else backs off 1, 2, 4 … 8 s', () => {
    expect(retryDelayMs(0, new ApiError({ status: 429, code: 'rate_limited', retryAfterSec: 12 }))).toBe(12_000);
    expect([0, 1, 2, 3, 4].map((a) => retryDelayMs(a, new NetworkError()))).toEqual([1000, 2000, 4000, 8000, 8000]);
  });

  it('clears ["me"] on a 401 unauthenticated from any query or mutation, not on other 401s', async () => {
    const qc = createQueryClient();
    qc.setQueryData(queryKeys.me, meFixture());
    server.use(
      http.post(apiPath('/api/v1/auth/login'), () => apiError(401, { code: 'invalid_credentials' })),
      http.get(apiPath('/api/v1/rooms'), () => apiError(401, { code: 'unauthenticated' })),
    );
    await qc
      .getMutationCache()
      .build(qc, { mutationFn: () => api('POST', '/api/v1/auth/login', {}) })
      .execute(undefined)
      .catch(() => undefined);
    expect(qc.getQueryData(queryKeys.me)).toEqual(meFixture());
    await qc
      .query({ queryKey: queryKeys.rooms, queryFn: () => api('GET', '/api/v1/rooms'), retry: false })
      .catch(() => undefined);
    expect(qc.getQueryData(queryKeys.me)).toBeNull();
  });
});

describe('me', () => {
  it('is Me when signed in and null on 401 unauthenticated', async () => {
    server.use(signedIn(meFixture({ admin: true })));
    await expect(fetchMe()).resolves.toMatchObject({ user: { role: 'admin' } });
    server.use(signedIn(null));
    await expect(fetchMe()).resolves.toBeNull();
  });

  it('throws other failures', async () => {
    server.use(http.get(apiPath('/api/v1/me'), () => HttpResponse.error()));
    await expect(fetchMe()).rejects.toBeInstanceOf(NetworkError);
    server.use(http.get(apiPath('/api/v1/me'), () => apiError(500, { code: 'internal', requestId: 'r' })));
    await expect(fetchMe()).rejects.toMatchObject({ code: 'internal' });
  });

  it('clearMe sets ["me"] to null', () => {
    const qc = new QueryClient();
    qc.setQueryData(queryKeys.me, meFixture());
    clearMe(qc);
    expect(qc.getQueryData(queryKeys.me)).toBeNull();
  });
});

describe('cross-tab logout (05 §6.2)', () => {
  afterEach(() => {
    vi.unstubAllGlobals();
  });

  it("delivers {type: 'logout'} to other listeners, and ignores other messages", async () => {
    const onLogout = vi.fn();
    const stop = listenForLogout(onLogout);
    const other = new BroadcastChannel('isshoni');
    other.postMessage({ type: 'hello' });
    broadcastLogout();
    await vi.waitFor(() => {
      expect(onLogout).toHaveBeenCalledOnce();
    });
    stop();
    other.close();
  });

  it('is a no-op without BroadcastChannel', () => {
    vi.stubGlobal('BroadcastChannel', undefined);
    expect(() => {
      broadcastLogout();
    }).not.toThrow();
    const stop = listenForLogout(vi.fn());
    expect(() => {
      stop();
    }).not.toThrow();
  });
});
