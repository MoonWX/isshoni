// The sharer's presets (05 §13.5). The preset travels in share.start / share.update, and the server turns it into
// ShareParams (bitrates, frame rates, pixel budgets: 02 owns the numbers). This table is the part only the browser
// can set: the video track's contentHint and the sender's degradationPreference.
//
// | Preset | contentHint (video)       | degradationPreference |
// |--------|---------------------------|-----------------------|
// | auto   | ''                        | balanced              |
// | game   | motion                    | maintain-framerate    |
// | movie  | motion                    | maintain-framerate    |
// | text   | text (fallback: detail)   | maintain-resolution   |
import { PresetAuto, PresetGame, PresetMovie, PresetText, type Preset } from '../protocol/types.gen';

/** The presets in the order the ShareSheet lists them. */
export const PRESETS: readonly Preset[] = [PresetAuto, PresetGame, PresetMovie, PresetText];

export interface PresetHints {
  /**
   * MediaStreamTrack.contentHint values for the video track, in order of preference: a browser that doesn't know
   * one leaves the attribute unchanged, and the next is tried.
   */
  readonly contentHint: readonly string[];
  /** RTCRtpSendParameters.degradationPreference, set once before the offer (05 §13.4 step 5). */
  readonly degradationPreference: RTCDegradationPreference;
}

export const PRESET_HINTS: Readonly<Record<Preset, PresetHints>> = {
  [PresetAuto]: { contentHint: [''], degradationPreference: 'balanced' },
  [PresetGame]: { contentHint: ['motion'], degradationPreference: 'maintain-framerate' },
  [PresetMovie]: { contentHint: ['motion'], degradationPreference: 'maintain-framerate' },
  [PresetText]: { contentHint: ['text', 'detail'], degradationPreference: 'maintain-resolution' },
};

/** The browser-side settings of a preset. An unknown value (a newer server's) is treated as auto, as 01 says. */
export function presetHints(preset: string): PresetHints {
  return Object.hasOwn(PRESET_HINTS, preset) ? PRESET_HINTS[preset as Preset] : PRESET_HINTS[PresetAuto];
}

/**
 * Sets the video track's contentHint from the preset and returns the value now in effect. Used when the source is
 * picked (05 §13.2) and again whenever the preset changes while live (§13.5).
 */
export function applyVideoContentHint(track: Pick<MediaStreamTrack, 'contentHint'>, preset: string): string {
  for (const hint of presetHints(preset).contentHint) {
    track.contentHint = hint;
    if (track.contentHint === hint) break;
  }
  return track.contentHint;
}
