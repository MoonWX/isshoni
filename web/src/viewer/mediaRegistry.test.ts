import { describe, expect, it, vi } from 'vitest';

import { FakeMediaStreamTrack } from '../test/fakeMedia';
import { createMediaRegistry, NO_MEDIA } from './mediaRegistry';

const track = (kind: 'audio' | 'video') => new FakeMediaStreamTrack(kind) as unknown as MediaStreamTrack;

describe('media registry', () => {
  it('keeps a video and an audio track per share', () => {
    const r = createMediaRegistry();
    const v = track('video');
    const a = track('audio');
    expect(r.get('s_a')).toBe(NO_MEDIA);
    r.set('s_a', 'video', v);
    r.set('s_a', 'audio', a);
    expect(r.get('s_a')).toEqual({ video: v, audio: a });
    expect(r.get('s_b')).toBe(NO_MEDIA);
    expect(r.shareIds()).toEqual(['s_a']);
  });

  it('returns the same snapshot until the share’s tracks change', () => {
    const r = createMediaRegistry();
    const v = track('video');
    r.set('s_a', 'video', v);
    const snap = r.get('s_a');
    expect(r.get('s_a')).toBe(snap);
    r.set('s_a', 'video', v); // the same track: nothing changes
    r.set('s_b', 'video', track('video')); // another share
    expect(r.get('s_a')).toBe(snap);
    r.set('s_a', 'audio', track('audio'));
    expect(r.get('s_a')).not.toBe(snap);
    expect(Object.isFrozen(r.get('s_a'))).toBe(true);
  });

  it('replaces a track: a reused transceiver carries another share', () => {
    const r = createMediaRegistry();
    const first = track('video');
    const second = track('video');
    r.set('s_a', 'video', first);
    r.set('s_a', 'video', second);
    expect(r.get('s_a')).toEqual({ video: second });
  });

  it('tells a share’s subscribers about its changes only', () => {
    const r = createMediaRegistry();
    const onA = vi.fn();
    const onB = vi.fn();
    const off = r.subscribe('s_a', onA);
    r.subscribe('s_b', onB);
    const v = track('video');
    r.set('s_a', 'video', v);
    r.set('s_a', 'video', v);
    expect(onA).toHaveBeenCalledTimes(1);
    expect(onB).not.toHaveBeenCalled();
    r.delete('s_a', 'video');
    expect(onA).toHaveBeenCalledTimes(2);
    r.delete('s_a', 'video'); // already gone
    expect(onA).toHaveBeenCalledTimes(2);
    off();
    r.set('s_a', 'video', v);
    expect(onA).toHaveBeenCalledTimes(2);
  });

  it('deletes one kind or the whole share', () => {
    const r = createMediaRegistry();
    const v = track('video');
    const a = track('audio');
    r.set('s_a', 'video', v);
    r.set('s_a', 'audio', a);
    r.delete('s_a', 'video');
    expect(r.get('s_a')).toEqual({ audio: a });
    r.set('s_a', 'video', v);
    r.delete('s_a', 'audio');
    expect(r.get('s_a')).toEqual({ video: v });
    r.delete('s_a');
    expect(r.get('s_a')).toBe(NO_MEDIA);
    expect(r.shareIds()).toEqual([]);
    r.delete('never-there');
  });

  it('retain() drops the shares that ended, clear() everything', () => {
    const r = createMediaRegistry();
    const gone = vi.fn();
    const kept = vi.fn();
    r.set('s_a', 'video', track('video'));
    r.set('s_b', 'video', track('video'));
    r.set('s_c', 'audio', track('audio'));
    r.subscribe('s_a', gone);
    r.subscribe('s_b', kept);
    r.retain(['s_b', 's_c', 's_not_subscribed']);
    expect(r.shareIds()).toEqual(['s_b', 's_c']);
    expect(gone).toHaveBeenCalledTimes(1);
    expect(kept).not.toHaveBeenCalled();
    r.clear();
    expect(r.shareIds()).toEqual([]);
    expect(kept).toHaveBeenCalledTimes(1);
  });
});
