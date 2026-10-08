// Tests of platform/browser/classify.ts (05 §19.1 lists them as share/classify). They live in share/ because S35
// touches only the three source files under platform/browser/ (docs/m1/README.md §5).
import { describe, expect, it } from 'vitest';

import { classify, type PickClassification } from '../platform/browser/classify';

describe('classify (05 §13.3)', () => {
  it.each<[string, boolean, PickClassification]>([
    ['window', true, { kind: 'window', audioScope: 'window', warning: null }],
    ['window', false, { kind: 'window', audioScope: 'none', warning: 'no-audio' }],
    ['browser', true, { kind: 'tab', audioScope: 'tab', warning: null }],
    ['browser', false, { kind: 'tab', audioScope: 'none', warning: 'no-audio' }],
    ['monitor', true, { kind: 'screen', audioScope: 'system', warning: 'screen-with-system-audio' }],
    ['monitor', false, { kind: 'screen', audioScope: 'none', warning: 'no-audio' }],
  ])('displaySurface %s, audio %s', (displaySurface, hasAudio, want) => {
    expect(classify(displaySurface, hasAudio)).toEqual(want);
  });

  it('only a whole screen with sound gets the warning dialog', () => {
    const all = (['window', 'browser', 'monitor'] as const).flatMap((s) => [classify(s, true), classify(s, false)]);
    expect(all.filter((c) => c.warning === 'screen-with-system-audio')).toEqual([
      { kind: 'screen', audioScope: 'system', warning: 'screen-with-system-audio' },
    ]);
  });

  it.each([undefined, '', 'application', 'Window'])(
    'treats a missing or unknown displaySurface (%j) as a whole screen, so its sound is never waved through',
    (displaySurface) => {
      expect(classify(displaySurface, true)).toEqual({
        kind: 'screen',
        audioScope: 'system',
        warning: 'screen-with-system-audio',
      });
      expect(classify(displaySurface, false)).toEqual({ kind: 'screen', audioScope: 'none', warning: 'no-audio' });
    },
  );
});
