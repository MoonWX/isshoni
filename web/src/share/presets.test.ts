import { describe, expect, it } from 'vitest';

import { PresetAuto, PresetGame, PresetMovie, PresetText } from '../protocol/types.gen';
import { FakeMediaStreamTrack } from '../test/fakeMedia';
import { applyVideoContentHint, PRESET_HINTS, presetHints, PRESETS } from './presets';

describe('presets (05 §13.5)', () => {
  it('are listed as the ShareSheet shows them: Auto, Game, Movie, Text', () => {
    expect(PRESETS).toEqual([PresetAuto, PresetGame, PresetMovie, PresetText]);
    expect(Object.keys(PRESET_HINTS).sort()).toEqual([...PRESETS].sort());
  });

  it.each([
    [PresetAuto, [''], 'balanced'],
    [PresetGame, ['motion'], 'maintain-framerate'],
    [PresetMovie, ['motion'], 'maintain-framerate'],
    [PresetText, ['text', 'detail'], 'maintain-resolution'],
  ] as const)('%s → contentHint %j, degradationPreference %s', (preset, contentHint, degradationPreference) => {
    expect(presetHints(preset)).toEqual({ contentHint, degradationPreference });
  });

  it.each(['cinema', '', 'toString', '__proto__'])('treats an unknown preset (%j) as auto', (preset) => {
    expect(presetHints(preset)).toBe(PRESET_HINTS[PresetAuto]);
  });
});

describe('applyVideoContentHint', () => {
  it('sets the hint of the preset, and clears it for auto', () => {
    const track = new FakeMediaStreamTrack('video');
    expect(applyVideoContentHint(track, PresetGame)).toBe('motion');
    expect(track.contentHint).toBe('motion');
    expect(applyVideoContentHint(track, PresetText)).toBe('text');
    expect(applyVideoContentHint(track, PresetAuto)).toBe('');
    expect(track.contentHint).toBe('');
  });

  it('falls back to detail where the browser does not know text', () => {
    // Like a browser: assigning a value it doesn't know leaves contentHint unchanged.
    let hint = '';
    const track = {
      get contentHint(): string {
        return hint;
      },
      set contentHint(value: string) {
        if (['', 'motion', 'detail'].includes(value)) hint = value;
      },
    };
    expect(applyVideoContentHint(track, PresetText)).toBe('detail');
    expect(applyVideoContentHint(track, PresetMovie)).toBe('motion');
  });

  it('leaves the hint alone where the browser knows none of the values', () => {
    const seen: string[] = [];
    const track = {
      get contentHint(): string {
        return '';
      },
      set contentHint(value: string) {
        seen.push(value);
      },
    };
    expect(applyVideoContentHint(track, PresetText)).toBe('');
    expect(seen).toEqual(['text', 'detail']);
  });
});
