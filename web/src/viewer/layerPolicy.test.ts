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
});
