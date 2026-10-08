// BrowserSharing (05 §8, §13): the in-page SharingProvider, the browser's `platform.sharing`. BrowserPlatform gets
// it from platform/browser/displayMedia.ts (createBrowserSharing), which passes in what only platform/ may touch:
// the picker (getDisplayMedia) and the RTCPeerConnection factory.
//
// - pick() opens the picker and classifies the result (S35: displayMedia.ts, classify.ts, the fake-display seam).
// - start() publishes the picked source: share.start, the PublisherPC, the ActiveShare (05 §13.4–§13.6). S46 writes
//   it in this file; until then it rejects with NotImplementedError.
import { NotImplementedError } from '../lib/errors';
import type { PickedSource, Platform, SharingProvider } from '../platform/types';
import type { Preset } from '../protocol/types.gen';

export interface BrowserSharingDeps {
  /**
   * Opens the browser's picker and classifies the pick; null when the user cancels. It must call getDisplayMedia
   * before its first await: pick() runs inside the Share click (transient activation, 05 §13.1).
   */
  capture(opts: { preset: Preset }): Promise<PickedSource | null>;
  /** For the pub PC (05 §13.6): the platform's createPeerConnection. */
  platform: Pick<Platform, 'createPeerConnection'>;
}

export class BrowserSharing implements SharingProvider {
  readonly mode = 'in-page' as const;
  readonly #deps: BrowserSharingDeps;

  constructor(deps: BrowserSharingDeps) {
    this.#deps = deps;
  }

  /** Call synchronously from the click handler. null = cancelled; a failed capture rejects with a LocalError. */
  pick(opts: { preset: Preset }): Promise<PickedSource | null> {
    return this.#deps.capture(opts);
  }

  /**
   * S46 (05 §13.4): start(src, {preset, withAudio}, ctx) publishes the picked source in ctx's room and resolves
   * with its ActiveShare. Until then it rejects; the caller releases the source (shareStore does).
   */
  readonly start: SharingProvider['start'] = () => Promise.reject(new NotImplementedError('sharing.start', 'S46'));
}
