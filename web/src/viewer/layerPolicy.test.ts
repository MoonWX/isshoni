// @vitest-environment node
// The layer policy (05 §12.4): each rule, in order.
import { describe, expect, it } from 'vitest';

import { desiredSubscriptions, PAGE_HIDDEN_OFF_MS, type LayerInputs } from './layerPolicy';

/** Three remote shares: s_a on the stage and heard, s_b a visible tile, s_c a tile scrolled out of view. */
const inputs = (over: Partial<LayerInputs> = {}): LayerInputs => ({
  remoteShares: ['s_a', 's_b', 's_c'],
  focused: 's_a',
  audible: 's_a',
  visible: { s_a: true, s_b: true, s_c: false },
  pageHiddenForMs: 0,
  fullscreen: false,
  pip: null,
  ...over,
});

/** "video/audio" per share, in the order of remoteShares. */
const wants = (over: Partial<LayerInputs> = {}): string[] =>
  desiredSubscriptions(inputs(over)).map((w) => `${w.shareId} ${w.video}/${w.audio}`);

describe('desiredSubscriptions', () => {
  it('gives one want per remote share, in their order', () => {
    expect(desiredSubscriptions(inputs())).toEqual([
      { shareId: 's_a', video: 'high', audio: 'on' },
      { shareId: 's_b', video: 'low', audio: 'off' },
      { shareId: 's_c', video: 'off', audio: 'off' },
    ]);
    expect(desiredSubscriptions(inputs({ remoteShares: [] }))).toEqual([]);
  });

  it('rule 1: audio is on only for the audible share, whatever the page visibility', () => {
    expect(wants({ audible: 's_b' })).toEqual(['s_a high/off', 's_b low/on', 's_c off/off']);
    expect(wants({ audible: null })).toEqual(['s_a high/off', 's_b low/off', 's_c off/off']);
    // A tile that is not shown can still be heard, and a hidden page keeps its sound.
    expect(wants({ audible: 's_c' })).toEqual(['s_a high/off', 's_b low/off', 's_c off/on']);
    expect(wants({ pageHiddenForMs: 60_000 })).toEqual(['s_a off/on', 's_b off/off', 's_c off/off']);
    // A share that is not there is not subscribed for being audible.
    expect(wants({ audible: 's_gone' })).toEqual(['s_a high/off', 's_b low/off', 's_c off/off']);
  });

  it('rule 2: the PiP share gets the high layer, even when the page is hidden', () => {
    expect(wants({ pip: 's_b' })).toEqual(['s_a high/on', 's_b high/off', 's_c off/off']);
    expect(wants({ pip: 's_b', pageHiddenForMs: 60_000 })).toEqual(['s_a off/on', 's_b high/off', 's_c off/off']);
    expect(wants({ pip: 's_c', fullscreen: true })).toEqual(['s_a high/on', 's_b off/off', 's_c high/off']);
  });

  it('rule 3: a page hidden for 10 s gets no other video', () => {
    expect(PAGE_HIDDEN_OFF_MS).toBe(10_000);
    expect(wants({ pageHiddenForMs: PAGE_HIDDEN_OFF_MS - 1 })).toEqual(['s_a high/on', 's_b low/off', 's_c off/off']);
    expect(wants({ pageHiddenForMs: PAGE_HIDDEN_OFF_MS })).toEqual(['s_a off/on', 's_b off/off', 's_c off/off']);
  });

  it('rule 4: the focused share gets the high layer, shown or not', () => {
    expect(wants({ focused: 's_b' })).toEqual(['s_a low/on', 's_b high/off', 's_c off/off']);
    expect(wants({ focused: 's_c' })).toEqual(['s_a low/on', 's_b low/off', 's_c high/off']);
    expect(wants({ focused: null })).toEqual(['s_a low/on', 's_b low/off', 's_c off/off']);
  });

  it('rule 5: in fullscreen every share but the focused one gets no video', () => {
    expect(wants({ fullscreen: true })).toEqual(['s_a high/on', 's_b off/off', 's_c off/off']);
    expect(wants({ fullscreen: true, audible: 's_b' })).toEqual(['s_a high/off', 's_b off/on', 's_c off/off']);
  });

  it('rule 6: the other shares get the low layer while they are visible, else none', () => {
    expect(wants({ visible: { s_a: true, s_b: false, s_c: true } })).toEqual([
      's_a high/on',
      's_b off/off',
      's_c low/off',
    ]);
    // Absent means not visible: an unmounted tile.
    expect(wants({ visible: {} })).toEqual(['s_a high/on', 's_b off/off', 's_c off/off']);
  });

  it('rule 7 (M2): a local share without a preview of the page gets the low layer at most and never audio', () => {
    // docs/m2/03 §10.4: the desktop app's own share. After the remote shares, in its order.
    const local = (over: Partial<LayerInputs>) =>
      wants({ remoteShares: ['s_a'], localShares: ['s_own'], ...over }).filter((w) => w.startsWith('s_own'));
    expect(local({ visible: { s_a: true, s_own: true } })).toEqual(['s_own low/off']);
    expect(local({ visible: { s_a: true, s_own: false } })).toEqual(['s_own off/off']);
    // Picked onto the stage, in PiP, or "audible": still the preview layer, still silent.
    expect(local({ focused: 's_own', visible: { s_own: true } })).toEqual(['s_own low/off']);
    expect(local({ pip: 's_own' })).toEqual(['s_own low/off']);
    expect(local({ audible: 's_own', visible: { s_own: true } })).toEqual(['s_own low/off']);
    // The page's other rules apply: a page hidden for 10 s, fullscreen on another share.
    expect(local({ pageHiddenForMs: PAGE_HIDDEN_OFF_MS, visible: { s_own: true } })).toEqual(['s_own off/off']);
    expect(local({ fullscreen: true, visible: { s_own: true } })).toEqual(['s_own off/off']);
    expect(desiredSubscriptions(inputs({ localShares: ['s_own'], visible: { s_own: true } })).at(-1)).toEqual({
      shareId: 's_own',
      video: 'low',
      audio: 'off',
    });
  });
});
