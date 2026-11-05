// The endpoint mute (docs/m2/03 §12.1, §22.3 "Viewer"; README WA77): `audio: off` for every subscription while the
// desktop app shares with 05's `endpoint` engine, reverting when the engine leaves it or the share ends.
import { describe, expect, it } from 'vitest';

import type { FriendsHear } from '../platform/desktop/bridge';
import { createFakeBridge } from '../platform/desktop/testing/fakeBridge';
import { AudioStateOff, AudioStateOn, VideoLayerHigh, VideoLayerLow } from '../protocol/types.gen';
import { attachEndpointMute, mutesViewer } from './endpointMute';
import { desiredSubscriptions } from './layerPolicy';
import { createViewerStore } from './viewerStore';

const report = (seq: number, live: boolean, engine: FriendsHear['engine']): FriendsHear =>
  ({ seq, live, engine }) as FriendsHear;

describe('layerPolicy rule 8', () => {
  it('turns every audio off while muted; video unchanged', () => {
    const inputs = {
      remoteShares: ['s_a', 's_b'],
      focused: 's_a',
      audible: 's_a',
      visible: { s_a: true, s_b: true },
      pageHiddenForMs: 0,
      fullscreen: false,
      pip: null,
    };
    expect(desiredSubscriptions(inputs)[0]).toEqual({ shareId: 's_a', video: VideoLayerHigh, audio: AudioStateOn });
    expect(desiredSubscriptions({ ...inputs, audioMuted: true })).toEqual([
      { shareId: 's_a', video: VideoLayerHigh, audio: AudioStateOff },
      { shareId: 's_b', video: VideoLayerLow, audio: AudioStateOff },
    ]);
  });
});

describe('attachEndpointMute', () => {
  it('mutes only while a share is live on the endpoint engine', () => {
    expect(mutesViewer({ live: true, engine: 'endpoint' })).toBe(true);
    expect(mutesViewer({ live: false, engine: 'endpoint' })).toBe(false);
    expect(mutesViewer({ live: true, engine: 'include-set' })).toBe(false);
  });

  it('follows the reports, ignores an older one, and stops muting when detached', async () => {
    const store = createViewerStore();
    const fake = createFakeBridge();
    fake.answers['audio.friendsHear'] = report(1, false, 'endpoint');
    const off = attachEndpointMute(store, fake.bridge);
    await Promise.resolve();
    expect(store.getState().audioMutedForShare).toBe(false);
    fake.emit('audio.report', report(2, true, 'endpoint'));
    expect(store.getState().audioMutedForShare).toBe(true);
    fake.emit('audio.report', report(1, false, 'include-set')); // older: ignored
    expect(store.getState().audioMutedForShare).toBe(true);
    fake.emit('audio.report', report(3, true, 'include-set')); // the engine left endpoint
    expect(store.getState().audioMutedForShare).toBe(false);
    fake.emit('audio.report', report(4, true, 'endpoint'));
    expect(store.getState().audioMutedForShare).toBe(true);
    fake.emit('audio.report', report(5, false, 'endpoint')); // the share ended
    expect(store.getState().audioMutedForShare).toBe(false);
    fake.emit('audio.report', report(6, true, 'endpoint'));
    off();
    expect(store.getState().audioMutedForShare).toBe(false);
    expect(fake.listeners('audio.report')).toBe(0);
  });

  it('a reset of the store keeps the mute: it belongs to the page', () => {
    const store = createViewerStore();
    store.getState().setAudioMutedForShare(true);
    store.getState().reset();
    expect(store.getState().audioMutedForShare).toBe(true);
  });
});
