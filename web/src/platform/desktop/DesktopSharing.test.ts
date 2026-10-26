// DesktopSharing and DesktopShare (docs/m2/03 §10.2, §22.3) with a fake NativeBridge: the PickedSource of a native
// pick, Share.Start's arguments, the state mapping from share.state (ShareView), the shareId change on `replaces`,
// the recovery methods as no-ops, stopOther(roomId, shareId), localConnectionIds, the hint from ShareView.hint (no
// HintDetector), the end reasons, the page's share store, and the order of events and answers.
import { afterEach, describe, expect, it, vi } from 'vitest';

import { createShareStore, type ShareStore } from '../../share/shareStore';
import type { ActiveShare, ShareContext, ShareHint, SignalClientLike } from '../types';
import type { Picked, ShareView } from './bridge';
import { DesktopShare, isDesktopShare, localEndOf, localStateOf } from './DesktopShare';
import { DesktopSharing } from './DesktopSharing';
import { BridgeError } from './errors';
import { createFakeBridge, type FakeBridge } from './testing/fakeBridge';
import { shareView, startingView } from './testing/shareView';

const CTX: ShareContext = { signal: {} as SignalClientLike, roomId: 'lounge' };

const PICKED: Picked = {
  pickId: '0123456789abcdef',
  kind: 'window',
  label: 'Minecraft',
  border: true,
  confirm: ['first-share'],
};

interface Setup {
  fake: FakeBridge;
  sharing: DesktopSharing;
  store: ShareStore;
}

async function setup(
  init: { current?: ShareView | null; publisherConnectionId?: string; store?: ShareStore | null } = {},
): Promise<Setup> {
  const fake = createFakeBridge({ active: 'https://watch.example.com' });
  fake.answers['share.current'] = init.current ?? null;
  fake.answers['app.info'] = { publisherConnectionId: init.publisherConnectionId ?? '' };
  fake.answers['share.pick'] = PICKED;
  const store = createShareStore();
  const sharing = new DesktopSharing(fake.bridge, { store: init.store === undefined ? store : init.store });
  await sharing.ready();
  return { fake, sharing, store };
}

/** Lets the bridge's resolved promises run. */
const flush = async (): Promise<void> => {
  for (let i = 0; i < 5; i++) await Promise.resolve();
};

afterEach(() => {
  vi.useRealTimers();
});

describe('DesktopSharing: pick (docs/m2/03 §10.2)', () => {
  it('is a native provider; a pick has no stream, the sheet’s sound choice, the picker’s name and the sheets due', async () => {
    const { sharing, fake } = await setup();
    expect(sharing.mode).toBe('native');
    const src = await sharing.pick({ preset: 'auto' });
    expect(src).toMatchObject({
      kind: 'window',
      audioScope: 'system',
      warning: null,
      preview: null,
      label: 'Minecraft',
      confirm: ['first-share'],
    });
    expect(fake.callsOf('share.pick')).toEqual([[]]);
    const silent = await sharing.pick({ preset: 'auto', withAudio: false });
    expect(silent?.audioScope).toBe('none');
  });

  it('release() gives the pick back once', async () => {
    const { sharing, fake } = await setup();
    const src = await sharing.pick({ preset: 'auto' });
    src?.release();
    src?.release();
    expect(fake.callsOf('share.release')).toEqual([[PICKED.pickId]]);
  });

  it('a closed picker is null; a refused pick rejects with the bridge error', async () => {
    const { sharing, fake } = await setup();
    fake.answers['share.pick'] = () => Promise.resolve(null);
    expect(await sharing.pick({ preset: 'auto' })).toBeNull();
    fake.answers['share.pick'] = () => Promise.reject(new BridgeError('engine.invalid', { reason: 'self_window' }));
    await expect(sharing.pick({ preset: 'auto' })).rejects.toMatchObject({
      code: 'engine.invalid',
      params: { reason: 'self_window' },
    });
  });
});

describe('DesktopSharing: start (docs/m2/03 §10.2)', () => {
  it('calls Share.Start with the pick, the room, the preset and the sound, and resolves with the DesktopShare', async () => {
    const { sharing, fake, store } = await setup();
    fake.answers['share.start'] = startingView({ shareId: 's_new0000000000000' });
    const src = await sharing.pick({ preset: 'game' });
    if (src === null) throw new Error('no pick');
    const share = await sharing.start(src, { preset: 'game', withAudio: false }, CTX);
    expect(fake.callsOf('share.start')).toEqual([
      [{ pickId: PICKED.pickId, roomId: 'lounge', preset: 'game', withAudio: false }],
    ]);
    expect(isDesktopShare(share)).toBe(true);
    expect(share).toMatchObject({ kind: 'window', preview: null, params: null, state: 'starting' });
    expect(share.shareId).toBe('s_new0000000000000');
    expect(sharing.current()).toBe(share);
    // The page's store follows it.
    expect(store.getState()).toMatchObject({ phase: 'starting', withAudio: true });
  });

  it('refuses a source it didn’t pick', async () => {
    const { sharing } = await setup();
    const foreign = {
      kind: 'window',
      audioScope: 'none',
      warning: null,
      preview: null,
      release: () => undefined,
    } as const;
    await expect(sharing.start(foreign, { preset: 'auto', withAudio: false }, CTX)).rejects.toMatchObject({
      code: 'capture_failed',
    });
  });

  it('passes the bridge error on (publisher_offline, wire codes, engine codes)', async () => {
    const { sharing, fake } = await setup();
    fake.answers['share.start'] = () => Promise.reject(new BridgeError('publisher_offline'));
    const src = await sharing.pick({ preset: 'auto' });
    if (src === null) throw new Error('no pick');
    await expect(sharing.start(src, { preset: 'auto', withAudio: true }, CTX)).rejects.toMatchObject({
      code: 'publisher_offline',
    });
    expect(sharing.current()).toBeNull();
  });

  it('returns the share the events made, without rolling it back to the answer’s older view', async () => {
    const { sharing, fake } = await setup();
    let answer: (v: ShareView) => void = () => undefined;
    fake.answers['share.start'] = () =>
      new Promise<ShareView>((resolve) => {
        answer = resolve;
      });
    const src = await sharing.pick({ preset: 'auto' });
    if (src === null) throw new Error('no pick');
    const started = sharing.start(src, { preset: 'auto', withAudio: true }, CTX);
    await flush();
    fake.emit('share.state', startingView());
    fake.emit('share.state', shareView());
    answer(startingView({ shareId: shareView().shareId }));
    const share = await started;
    expect(share.state).toBe('live');
    expect(share).toBe(sharing.current());
  });

  it('a stale event from before the start (the picker’s settled null) doesn’t lose the answer', async () => {
    const { sharing, fake } = await setup();
    let answer: (v: ShareView) => void = () => undefined;
    fake.answers['share.start'] = () =>
      new Promise<ShareView>((resolve) => {
        answer = resolve;
      });
    const src = await sharing.pick({ preset: 'auto' });
    if (src === null) throw new Error('no pick');
    const started = sharing.start(src, { preset: 'auto', withAudio: true }, CTX);
    await flush();
    fake.emit('share.state', null);
    answer(startingView({ shareId: 's_x' }));
    const share = await started;
    expect(share.shareId).toBe('s_x');
    // A late null while the share runs only makes it ask Go again.
    fake.answers['share.current'] = shareView({ shareId: 's_x' });
    fake.emit('share.state', null);
    await flush();
    expect(sharing.current()).toBe(share);
    expect(share.state).toBe('live');
  });

  it('a share that ended before the answer was read fails the start with its error', async () => {
    const { sharing, fake } = await setup();
    let answer: (v: ShareView) => void = () => undefined;
    fake.answers['share.start'] = () =>
      new Promise<ShareView>((resolve) => {
        answer = resolve;
      });
    const src = await sharing.pick({ preset: 'auto' });
    if (src === null) throw new Error('no pick');
    const started = sharing.start(src, { preset: 'auto', withAudio: true }, CTX);
    await flush();
    fake.emit(
      'share.state',
      shareView({ state: 'failed', endReason: 'error', error: { code: 'engine.encoder_lost', params: {} } }),
    );
    answer(startingView());
    await expect(started).rejects.toMatchObject({ code: 'engine.encoder_lost' });
  });
});

describe('DesktopShare: the view (docs/m2/03 §6.3, §10.1)', () => {
  it('maps ShareView.state to ActiveShare.state', () => {
    expect(localStateOf('starting', 'starting')).toBe('starting');
    expect(localStateOf('live', 'starting')).toBe('live');
    expect(localStateOf('paused', 'live')).toBe('paused');
    expect(localStateOf('reconnecting', 'live')).toBe('reconnecting');
    expect(localStateOf('unreachable', 'live')).toBe('reconnecting');
    expect(localStateOf('stopping', 'paused')).toBe('paused');
    expect(localStateOf('ended', 'live')).toBe('ended');
    expect(localStateOf('failed', 'live')).toBe('ended');
  });

  it('maps the end reasons', () => {
    const end = (endReason: ShareView['endReason'], state: ShareView['state'] = 'ended') =>
      localEndOf({ state, endReason });
    expect(end('user')).toBe('user');
    expect(end('hotkey')).toBe('user');
    expect(end('tray')).toBe('user');
    expect(end('source-gone')).toBe('source-gone');
    expect(end('elsewhere')).toBe('server');
    expect(end('server')).toBe('server');
    expect(end('server-unreachable')).toBe('server-unreachable');
    expect(end('unlinked')).toBe('unlinked');
    expect(end('system')).toBe('system');
    expect(end('error', 'failed')).toBe('error');
    expect(localEndOf(null)).toBe('user');
  });

  it('follows share.state: a share that outlived the page is current() from the first pull', async () => {
    const { sharing } = await setup({ current: shareView() });
    const share = sharing.current();
    expect(share?.state).toBe('live');
    expect(share?.shareId).toBe(shareView().shareId);
  });

  it('no share for the picker’s view, a final view or none', async () => {
    for (const current of [null, shareView({ state: 'picking' }), shareView({ state: 'ended', endReason: 'user' })]) {
      const { sharing } = await setup({ current });
      expect(sharing.current()).toBeNull();
    }
  });

  it('emits state on every change, and when C2 publishes again with a new id (replaces)', async () => {
    const { sharing, fake } = await setup({ current: startingView() });
    const share = sharing.current() as ActiveShare;
    const states: string[] = [];
    share.on('state', (s) => states.push(`${s}:${share.shareId}`));
    fake.emit('share.state', shareView({ shareId: 's_a' }));
    fake.emit('share.state', shareView({ shareId: 's_a', watching: 4 }));
    fake.emit('share.state', shareView({ state: 'reconnecting', shareId: 's_a' }));
    fake.emit('share.state', shareView({ shareId: 's_b' }));
    expect(states).toEqual(['live:s_a', 'reconnecting:s_a', 'live:s_b']);
    expect(sharing.current()).toBe(share);
  });

  it('takes its hint from ShareView.hint, emitted on change only', async () => {
    const { sharing, fake } = await setup({ current: shareView() });
    const share = sharing.current() as ActiveShare;
    const hints: (ShareHint | null)[] = [];
    share.on('hint', (h) => hints.push(h));
    fake.emit('share.state', shareView({ hint: { kind: 'upload-limited', approxHeight: 720 } }));
    fake.emit('share.state', shareView({ hint: { kind: 'upload-limited', approxHeight: 720 } }));
    fake.emit('share.state', shareView({ hint: { kind: 'upload-limited', approxHeight: 540 } }));
    fake.emit('share.state', shareView({ hint: { kind: 'cpu-limited' } }));
    fake.emit('share.state', shareView({ hint: null }));
    expect(hints).toEqual([
      { kind: 'upload-limited', approxHeight: 720 },
      { kind: 'upload-limited', approxHeight: 540 },
      { kind: 'cpu-limited' },
      null,
    ]);
  });

  it('ends once, with the reason, on the final view; a new share is a new object', async () => {
    const { sharing, fake } = await setup({ current: shareView() });
    const share = sharing.current() as ActiveShare;
    const ended = vi.fn();
    share.on('ended', ended);
    fake.emit('share.state', shareView({ state: 'ended', endReason: 'source-gone' }));
    fake.emit('share.state', shareView({ state: 'ended', endReason: 'source-gone' }));
    expect(ended.mock.calls).toEqual([['source-gone']]);
    expect(share.state).toBe('ended');
    expect(sharing.current()).toBeNull();
    fake.emit('share.state', startingView());
    expect(sharing.current()).not.toBe(share);
  });

  it('relays share.notice to the running share', async () => {
    const { sharing, fake } = await setup({ current: shareView() });
    const share = sharing.current() as DesktopShare;
    const notices = vi.fn();
    share.on('notice', notices);
    fake.emit('share.notice', { code: 'encoder_fallback', params: { encoder: 'mf' } });
    expect(notices.mock.calls).toEqual([[{ code: 'encoder_fallback', params: { encoder: 'mf' } }]]);
  });
});

describe('DesktopShare: its methods (docs/m2/03 §6.2, §10.2)', () => {
  it('asks Go for each change; stop() never rejects and pulls the final view', async () => {
    const { sharing, fake } = await setup({ current: shareView() });
    const share = sharing.current() as DesktopShare;
    await share.setPreset('text');
    await share.setPaused(true);
    await share.setAudioEnabled(false);
    share.watchServerStats(true);
    fake.answers['share.stats'] = { at: 1, layers: [], audioKbps: 32 };
    expect(await share.stats()).toEqual({ at: 1, layers: [], audioKbps: 32 });
    expect(fake.callsOf('share.setPreset')).toEqual([['text']]);
    expect(fake.callsOf('share.setPaused')).toEqual([[true]]);
    expect(fake.callsOf('share.setAudioEnabled')).toEqual([[false]]);
    expect(fake.callsOf('share.watchServerStats')).toEqual([[true]]);

    fake.answers['share.stop'] = () => Promise.reject(new BridgeError('internal'));
    fake.answers['share.current'] = shareView({ state: 'ended', endReason: 'user' });
    await expect(share.stop()).resolves.toBeUndefined();
    await flush();
    expect(share.state).toBe('ended');
    expect(sharing.current()).toBeNull();
  });

  it('Share sound is a no-op for a share started without sound', async () => {
    const { sharing, fake } = await setup({ current: shareView({ withAudio: false, audioOn: false }) });
    await (sharing.current() as DesktopShare).setAudioEnabled(true);
    expect(fake.callsOf('share.setAudioEnabled')).toEqual([]);
  });

  it('the recovery methods resolve at once; serverEnded only asks Go for the view again', async () => {
    const { sharing, fake } = await setup({ current: shareView() });
    const share = sharing.current() as DesktopShare;
    const pulls = fake.callsOf('share.current').length;
    await expect(share.resync()).resolves.toBeUndefined();
    await expect(share.republish()).resolves.toBeUndefined();
    share.serverEnded();
    await flush();
    expect(fake.callsOf('share.current')).toHaveLength(pulls + 1);
    expect(share.state).toBe('live');
  });

  it('stopOther asks C2 to stop another connection’s share of this user', async () => {
    const { sharing, fake } = await setup();
    await sharing.stopOther('lounge', 's_phone');
    expect(fake.callsOf('share.stopOther')).toEqual([['lounge', 's_phone']]);
  });
});

describe('DesktopSharing: localConnectionIds (docs/m2/03 §10.4)', () => {
  it('names C2: App.Info’s id, then the share’s', async () => {
    const { sharing, fake } = await setup({ publisherConnectionId: 'c_pub' });
    await flush();
    expect(sharing.localConnectionIds()).toEqual(['c_pub']);
    fake.emit('share.state', shareView({ connectionId: 'c_new' }));
    expect(sharing.localConnectionIds()).toEqual(['c_new']);
  });

  it('is empty while C2 isn’t known', async () => {
    const { sharing } = await setup();
    await flush();
    expect(sharing.localConnectionIds()).toEqual([]);
  });
});

describe('DesktopSharing and the page’s share store (05 §13.1)', () => {
  it('a share that outlived the page is followed: live, the sound, the hint, the end', async () => {
    const { sharing, fake, store } = await setup({ current: shareView({ preset: 'movie', audioOn: false }) });
    expect(sharing.current()).not.toBeNull();
    expect(store.getState()).toMatchObject({
      phase: 'live',
      preset: 'movie',
      withAudio: true,
      soundOn: false,
      picked: { kind: 'window', audioScope: 'system', warning: null, label: 'Minecraft' },
    });
    fake.emit('share.state', shareView({ state: 'unreachable' }));
    expect(store.getState()).toMatchObject({ phase: 'reconnecting', unreachable: true });
    fake.emit('share.state', shareView({ hint: { kind: 'cpu-limited' } }));
    expect(store.getState()).toMatchObject({ phase: 'live', unreachable: false, hint: { kind: 'cpu-limited' } });
    fake.emit('share.state', shareView({ state: 'stopping' }));
    expect(store.getState().phase).toBe('stopping');
    fake.emit('share.state', shareView({ state: 'ended', endReason: 'user' }));
    expect(store.getState()).toMatchObject({ phase: 'idle', error: null, notice: null });
  });

  it('a share stopped from another device is a notice; a failed one is the store’s error', async () => {
    const { fake, store } = await setup({ current: shareView() });
    fake.emit('share.state', shareView({ state: 'ended', endReason: 'elsewhere' }));
    expect(store.getState()).toMatchObject({ phase: 'idle', notice: { kind: 'elsewhere' } });
    fake.emit('share.state', shareView());
    fake.emit(
      'share.state',
      shareView({
        state: 'failed',
        endReason: 'error',
        error: { code: 'engine.encoder_lost', params: { reason: 'device_removed' } },
      }),
    );
    const { phase, error } = store.getState();
    expect(phase).toBe('failed');
    expect(error).toBeInstanceOf(BridgeError);
    expect(error).toMatchObject({ code: 'engine.encoder_lost', params: { reason: 'device_removed' } });
  });

  it('a closed window and an unreachable server end the share with what the panel says', async () => {
    const { fake, store } = await setup({ current: shareView() });
    fake.emit('share.state', shareView({ state: 'ended', endReason: 'source-gone' }));
    expect(store.getState()).toMatchObject({ phase: 'failed', error: { code: 'engine.source_gone' } });
    store.getState().dismiss();
    fake.emit('share.state', shareView());
    fake.emit('share.state', shareView({ state: 'ended', endReason: 'server-unreachable' }));
    expect(store.getState()).toMatchObject({ phase: 'failed', error: { code: 'server_unreachable' } });
    store.getState().dismiss();
    for (const endReason of ['server', 'unlinked', 'system', 'hotkey'] as const) {
      fake.emit('share.state', shareView());
      fake.emit('share.state', shareView({ state: 'ended', endReason }));
      expect(store.getState()).toMatchObject({ phase: 'idle', error: null });
    }
  });

  it('without a store nothing is written', async () => {
    const other = createShareStore();
    const { sharing } = await setup({ current: shareView(), store: null });
    expect(sharing.current()?.state).toBe('live');
    expect(other.getState().phase).toBe('idle');
  });
});
