// WailsBridge over fakes of the generated bindings and of @wailsio/runtime's events (docs/m2/03 §7.4): each method
// calls the binding of the same Go method with the page's arguments, a rejection becomes a BridgeError from the
// message the pinned Wails rejects with (W12: the Go error's JSON verbatim), fire-and-forget calls never throw, and
// events arrive by their names without the isshoni: prefix.
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { BridgeError } from '../errors';
import { createWailsBridge } from './WailsBridge';

type Fn = ReturnType<typeof vi.fn>;

const fakes = vi.hoisted(() => {
  const group = (...names: string[]): Record<string, Fn> =>
    Object.fromEntries(names.map((n) => [n, vi.fn(() => Promise.resolve(null))]));
  return {
    App: group(
      'Info',
      'Heartbeat',
      'OpenExternal',
      'OpenOSSettings',
      'Quit',
      'Log',
      'OpenLogsFolder',
      'OpenNotices',
      'PendingLink',
      'LinkHandled',
      'OwnConnections',
    ),
    Auth: group(
      'State',
      'Servers',
      'Probe',
      'AddServer',
      'SwitchServer',
      'RemoveServer',
      'Link',
      'CancelLink',
      'LinkWithPassword',
      'Unlink',
      'Recheck',
      'AccessToken',
      'SessionEnded',
      'HelloRefused',
    ),
    Share: group(
      'Capabilities',
      'Pick',
      'Release',
      'Start',
      'Current',
      'Stop',
      'StopOther',
      'SetPaused',
      'SetPreset',
      'SetAudioEnabled',
      'Stats',
      'WatchServerStats',
    ),
    Audio: group(
      'FriendsHear',
      'Watch',
      'WatchLevels',
      'SetAppDecision',
      'SetKeepOutMicUsers',
      'Decide',
      'DismissPrompt',
      'DismissTip',
      'AppRules',
      'SoundCheck',
    ),
    Settings: group('Get', 'Update', 'Hotkeys', 'TestHotkey'),
    Window: group('SetFullscreen'),
    Updates: group('Status', 'Check', 'Install'),
  };
});

vi.mock('../bindings/github.com/MoonWX/isshoni/internal/desktop/bridge', () => fakes);
vi.mock('@wailsio/runtime', () => ({ Events: { On: vi.fn(() => () => undefined) } }));

/** The runtime's RuntimeError: what a bound call rejects with when the Go method returned an error. */
class RuntimeError extends Error {
  override name = 'RuntimeError';
}

beforeEach(() => {
  for (const group of Object.values(fakes)) {
    for (const fn of Object.values(group)) {
      fn.mockReset();
      fn.mockResolvedValue(null);
    }
  }
});

afterEach(() => {
  vi.restoreAllMocks();
});

describe('WailsBridge', () => {
  it('calls the binding of the same Go method with the page’s arguments', async () => {
    const b = createWailsBridge({ on: () => () => undefined });
    fakes.Auth['AccessToken']?.mockResolvedValue({ token: 'isa_x', expiresAt: 1 });
    await expect(b.auth.accessToken('https://a.example.com', true)).resolves.toEqual({ token: 'isa_x', expiresAt: 1 });
    expect(fakes.Auth['AccessToken']).toHaveBeenCalledWith('https://a.example.com', true);

    await b.auth.sessionEnded('https://a.example.com', 'session_revoked');
    expect(fakes.Auth['SessionEnded']).toHaveBeenCalledWith('https://a.example.com', 'session_revoked');
    await b.auth.helloRefused('https://a.example.com', { code: 'client_outdated' });
    expect(fakes.Auth['HelloRefused']).toHaveBeenCalledWith('https://a.example.com', {
      code: 'client_outdated',
      params: {},
    });
    await b.auth.linkWithPassword('alex', 'pw');
    expect(fakes.Auth['LinkWithPassword']).toHaveBeenCalledWith('alex', 'pw');
    await b.app.openOSSettings('sound-communications');
    expect(fakes.App['OpenOSSettings']).toHaveBeenCalledWith('sound-communications');
    await b.share.start({ pickId: 'p', roomId: 'lounge', preset: 'game', withAudio: true });
    expect(fakes.Share['Start']).toHaveBeenCalledWith({
      pickId: 'p',
      roomId: 'lounge',
      preset: 'game',
      withAudio: true,
    });
    await b.audio.decide('p1', 'keep-out');
    expect(fakes.Audio['Decide']).toHaveBeenCalledWith('p1', 'keep-out');
    await b.settings.update({ closeToTray: true });
    expect(fakes.Settings['Update']).toHaveBeenCalledWith({ closeToTray: true });
    await b.window.setFullscreen(true);
    expect(fakes.Window['SetFullscreen']).toHaveBeenCalledWith(true);
    await b.updates.check();
    expect(fakes.Updates['Check']).toHaveBeenCalledWith();
  });

  it('turns the Go error’s JSON in the rejection’s message into a BridgeError (W12)', async () => {
    const b = createWailsBridge({ on: () => () => undefined });
    fakes.Auth['SessionEnded']?.mockRejectedValue(new RuntimeError('{"code":"not_found","params":{}}'));
    const err = await b.auth.sessionEnded('https://b.example.com', 'unauthenticated').catch((e: unknown) => e);
    expect(err).toBeInstanceOf(BridgeError);
    expect(err).toMatchObject({ code: 'not_found', params: {} });

    fakes.Share['Pick']?.mockRejectedValue(
      new RuntimeError('{"code":"engine.invalid","params":{"reason":"self_window"}}'),
    );
    await expect(b.share.pick()).rejects.toMatchObject({ code: 'engine.invalid', params: { reason: 'self_window' } });

    // A binding out of step with the Go side: Wails' TypeError is internal.
    fakes.App['Info']?.mockRejectedValue(new TypeError('…App.Info expects 0 arguments, got 1'));
    await expect(b.app.info()).rejects.toMatchObject({ code: 'internal' });
  });

  it('fire-and-forget methods return nothing and never throw', async () => {
    const warn = vi.spyOn(console, 'warn').mockImplementation(() => undefined);
    const b = createWailsBridge({ on: () => () => undefined });
    fakes.Audio['Watch']?.mockRejectedValue(new RuntimeError('{"code":"not_implemented","params":{}}'));
    // Typed void: a rejection of theirs is logged, never thrown or left unhandled.
    b.audio.watch(true);
    b.app.log('warn', 'a line');
    expect(fakes.App['Log']).toHaveBeenCalledWith('warn', 'a line', {});
    b.app.log('error', 'x', { k: 'v' });
    expect(fakes.App['Log']).toHaveBeenLastCalledWith('error', 'x', { k: 'v' });
    b.app.ownConnections(['c_1']);
    b.share.release('p_1');
    b.share.watchServerStats(true);
    b.audio.watchLevels(false);
    expect(fakes.App['OwnConnections']).toHaveBeenCalledWith(['c_1']);
    expect(fakes.Share['Release']).toHaveBeenCalledWith('p_1');
    expect(fakes.Share['WatchServerStats']).toHaveBeenCalledWith(true);
    expect(fakes.Audio['WatchLevels']).toHaveBeenCalledWith(false);
    await new Promise((r) => setTimeout(r, 0)); // the rejected Watch is handled, not unhandled
    warn.mockRestore();
  });

  it('share.stats gets its receipt time; null stays null', async () => {
    const b = createWailsBridge({ on: () => () => undefined });
    fakes.Share['Stats']?.mockResolvedValue({ layers: [], audioKbps: 64 });
    const s = await b.share.stats();
    expect(s).toMatchObject({ layers: [], audioKbps: 64 });
    expect(typeof s?.at).toBe('number');
    fakes.Share['Stats']?.mockResolvedValue(null);
    await expect(b.share.stats()).resolves.toBeNull();
  });

  it('listens to isshoni:<name> and hands over the event’s data', () => {
    const listeners = new Map<string, (ev: { data: unknown }) => void>();
    const off = vi.fn();
    const b = createWailsBridge({
      on: (name, fn) => {
        listeners.set(name, fn);
        return off;
      },
    });
    const got: unknown[] = [];
    const stop = b.on('auth.state', (s) => got.push(s));
    b.on('share.stats', (s) => got.push(s));
    b.on('share.notice', () => {
      throw new Error('a listener that fails');
    });
    expect([...listeners.keys()]).toEqual(['isshoni:auth.state', 'isshoni:share.stats', 'isshoni:share.notice']);
    listeners.get('isshoni:auth.state')?.({ data: { origin: 'https://a.example.com', status: 'linked' } });
    listeners.get('isshoni:share.stats')?.({ data: { layers: [], audioKbps: 0 } });
    expect(() => listeners.get('isshoni:share.notice')?.({ data: { code: 'encoder_fallback' } })).not.toThrow();
    expect(got[0]).toEqual({ origin: 'https://a.example.com', status: 'linked' });
    expect(typeof (got[1] as { at?: unknown }).at).toBe('number');
    stop();
    expect(off).toHaveBeenCalledOnce();
  });
});
