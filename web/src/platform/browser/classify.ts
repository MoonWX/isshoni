// What a getDisplayMedia pick is (05 §13.3). Pure: it reads the video track's displaySurface setting and whether an
// audio track came back, nothing else. Never the track label: that is a window title (01 §8.5).
//
// | displaySurface | audio | kind   | audioScope | warning                    | UX                                    |
// |----------------|-------|--------|------------|----------------------------|---------------------------------------|
// | window         | yes   | window | window     | –                          | recommended: straight to live         |
// | window         | no    | window | none       | no-audio                   | live, note "this window's sound isn't shared" |
// | browser        | yes   | tab    | tab        | –                          | live                                  |
// | browser        | no    | tab    | none       | no-audio                   | live, note "tick 'Also share tab audio'" |
// | monitor        | yes   | screen | system     | screen-with-system-audio   | the ScreenAudioWarning dialog         |
// | monitor        | no    | screen | none       | no-audio                   | live, note "no sound is shared"       |
import { ShareKindScreen, ShareKindTab, ShareKindWindow } from '../../protocol/types.gen';
import type { PickedSource } from '../types';

/** The classification of a pick: what PickedSource says about it, without the stream. */
export type PickClassification = Pick<PickedSource, 'kind' | 'audioScope' | 'warning'>;

/**
 * Classifies a pick from `videoTrack.getSettings().displaySurface` and whether the stream has an audio track.
 *
 * A browser that doesn't report displaySurface (or reports a value this build doesn't know) is treated as a whole
 * screen, like 01's rule for an unknown ShareKind: it is the widest capture, so sound that came with it gets the
 * whole-screen warning instead of slipping through as "just a window".
 */
export function classify(displaySurface: string | undefined, hasAudio: boolean): PickClassification {
  switch (displaySurface) {
    case 'window':
      return hasAudio
        ? { kind: ShareKindWindow, audioScope: 'window', warning: null }
        : { kind: ShareKindWindow, audioScope: 'none', warning: 'no-audio' };
    case 'browser':
      return hasAudio
        ? { kind: ShareKindTab, audioScope: 'tab', warning: null }
        : { kind: ShareKindTab, audioScope: 'none', warning: 'no-audio' };
    default:
      // 'monitor', and anything unknown.
      return hasAudio
        ? { kind: ShareKindScreen, audioScope: 'system', warning: 'screen-with-system-audio' }
        : { kind: ShareKindScreen, audioScope: 'none', warning: 'no-audio' };
  }
}
