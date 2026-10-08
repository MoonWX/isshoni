// audioOut (05 §10.2–§10.3): all audio plays through ONE <audio> element, created once per page load and appended
// to document.body, so the unlock survives room switches. Its srcObject is a MediaStream with the audible share's
// audio track; audio-follows-focus is a srcObject swap. Tile videos are always muted, so they autoplay everywhere,
// and iOS needs only one gesture: the one that starts this element.
//
// The unlock state machine (viewerStore.audio):
//
//   locked ──assign track, play() ok──► playing ◄──tap (user gesture): audio.play() + video.play()── blocked
//      │                                  │  ▲                                                          ▲
//      └──── play() NotAllowedError ──────┼──┼──────────────────────────────────────────────────────────┘
//                                         │  └── unmute ── muted ◄── user mutes (M key / button)
//                                         └── later play() rejects (iOS interruption) → blocked
//
// The interface is declared here, with the viewer's first slice ("interfaces first", docs/m1/README.md §4); the
// element and the state machine arrive with S47 (05 W7: audioOut and TapToStart). Until then createAudioOut throws
// NotImplementedError, and nothing in this slice plays sound.
import { NotImplementedError } from '../lib/errors';
import type { ViewerStore } from './viewerStore';

export interface AudioOut {
  /** The audible share's audio track, or null for silence. Swapping tracks keeps playing (05 §10.2). */
  setTrack(track: MediaStreamTrack | null): void;
  /**
   * The tap of "Tap to unmute": calls audio.play() synchronously, inside the user gesture (05 §10.3). Resolves once
   * it plays; rejects when the browser still refuses.
   */
  unlock(): Promise<void>;
  /** The user's mute (M key or button): playing ⇄ muted. */
  setMuted(muted: boolean): void;
  /** Sets the volume (0–1) and returns what the element reports back: iOS ignores it (05 §10.3). */
  setVolume(volume: number): number;
  /** Removes the element. Tests only: the page keeps its element for its lifetime. */
  dispose(): void;
}

export interface AudioOutDeps {
  /** viewerStore.audio gets the unlock state. */
  store: ViewerStore;
  /** Where the element is appended; default document.body. */
  parent?: HTMLElement;
}

export const createAudioOut: (deps: AudioOutDeps) => AudioOut = () => {
  throw new NotImplementedError('viewer.createAudioOut', 'S47');
};
