// Screen capture for the in-page sharer (05 §13.2, §13.3): getDisplayMedia with the plan's options, its fallbacks,
// the classification of what was picked, and the loopback-only fake-display seam (§19.3).
//
// BrowserPlatform calls createBrowserSharing() once, only on a device that may share (05 §8). The provider it gets
// is this file's: pick() is the picker below, and start() hands over to share/'s BrowserSharing (S46), which this
// file gives what only platform/ may touch: the picker and the RTCPeerConnection factory.
//
// The publisher is not in the initial bundle (05 §5, §17.3): BrowserSharing, with its PublisherPC and BrowserShare,
// is a chunk of its own that most visits never need, since most friends watch. It is fetched with the first pick,
// while the browser's picker is open, so start() usually finds it there.
//
// Why these options: isshoni's point is that friends in a voice call don't hear themselves echoed back (PLAN).
// A browser can't leave single apps out of system audio, so the sharer steers towards captures that can't echo:
// the picker opens on Window, a window brings only its own sound (`windowAudio: 'window'`), this tab's playback
// (the friends' shares) stays out of the capture (`restrictOwnAudio`), and the isshoni tab itself isn't offered.
// Discord Web's own screen share passes none of these (PLAN, checked in its code on 2026-09-29), which is what the
// whole-screen warning of §13.3 is about. Echo cancellation, noise suppression and AGC are off: this is programme
// sound, not a microphone.
import { LocalError } from '../../lib/errors';
import { createLogger } from '../../lib/log';
import type { Preset } from '../../protocol/types.gen';
import { applyVideoContentHint } from '../../share/presets';
import type { ActiveShare, KeyValueStore, PickedSource, ShareContext, SharingProvider } from '../types';
import { classify } from './classify';
import { createFakeDisplay, fakeDisplayRequest } from './fakeDisplay';
import { createWebStorage } from './storage';

const log = createLogger('capture');

/** What a pick reads from the browser; tests pass their own. */
export interface CaptureEnv {
  /** navigator.mediaDevices (missing outside a secure context). */
  readonly mediaDevices: Pick<MediaDevices, 'getDisplayMedia' | 'getSupportedConstraints'> | undefined;
  /** location.hostname and sessionStorage: the fake-display seam (05 §19.3). */
  readonly hostname: string;
  readonly session: Pick<KeyValueStore, 'get'>;
}

/** The page's own environment, read at the time of the pick. */
export function browserCaptureEnv(): CaptureEnv {
  return {
    // lib.dom types it as always there; it is undefined on plain http (and in jsdom), hence the wider field type.
    mediaDevices: globalThis.navigator.mediaDevices,
    hostname: globalThis.location.hostname,
    session: createWebStorage('session'),
  };
}

/** Which of §13.2's fallbacks an attempt already applied. */
export interface DisplayMediaAttempt {
  /** After a TypeError: only `video`, `audio` and `systemAudio`. */
  readonly minimal?: boolean;
  /** After an OverconstrainedError: no `frameRate`. */
  readonly noFrameRate?: boolean;
}

/**
 * The getDisplayMedia options of 05 §13.2 (plan values). `supported` is getSupportedConstraints(): the two audio
 * constraints that only some browsers know are sent only where they are listed.
 *
 * - `displaySurface: 'window'` makes the picker open on its Window tab (plan: "the picker defaults to a window").
 * - No width or height: the capture keeps its native size, and the encodings' maxPixels budget scales it (01 §8.7),
 *   so ultrawide screens keep their aspect ratio.
 */
export function displayMediaOptions(
  supported: MediaTrackSupportedConstraints,
  attempt: DisplayMediaAttempt = {},
): DisplayMediaStreamOptions {
  const video: MediaTrackConstraints = {
    ...(attempt.noFrameRate ? {} : { frameRate: { ideal: 60, max: 60 } }),
    displaySurface: 'window',
  };
  const audio: MediaTrackConstraints = {
    echoCancellation: false,
    noiseSuppression: false,
    autoGainControl: false,
    // false: the shared tab keeps playing for the sharer too.
    ...(supported.suppressLocalAudioPlayback ? { suppressLocalAudioPlayback: false } : {}),
    // Keeps this tab's own playback (the friends' shares) out of a tab or system capture.
    ...(supported.restrictOwnAudio ? { restrictOwnAudio: true } : {}),
  };
  if (attempt.minimal) return { video, audio, systemAudio: 'include' };
  return {
    video,
    audio,
    systemAudio: 'include',
    // Per-app audio on Windows 11 and macOS 14.2+ with a current Chrome or Edge.
    windowAudio: 'window',
    // Never the isshoni tab itself (a mirror, and an audio loop).
    selfBrowserSurface: 'exclude',
    surfaceSwitching: 'include',
    monitorTypeSurfaces: 'include',
    preferCurrentTab: false,
  };
}

/** The name of a thrown value. OverconstrainedError was not an Error in older browsers, so no instanceof. */
function errorName(err: unknown): string {
  if (typeof err !== 'object' || err === null) return '';
  const name = (err as { name?: unknown }).name;
  return typeof name === 'string' ? name : '';
}

/**
 * Opens the picker, with the fallbacks of 05 §13.2, each applied at most once:
 * - TypeError (an option value this browser doesn't know) → again with only `video`, `audio` and `systemAudio`;
 * - OverconstrainedError → again without `frameRate`;
 * - NotAllowedError → null: a cancelled picker and a denied permission can't be told apart, so no message;
 * - anything else (NotReadableError, NotFoundError, AbortError, …) → LocalError `capture_failed`.
 *
 * The first getDisplayMedia call happens before this function's first await (the click's transient activation).
 */
async function openPicker(md: NonNullable<CaptureEnv['mediaDevices']>): Promise<MediaStream | null> {
  const supported = typeof md.getSupportedConstraints === 'function' ? md.getSupportedConstraints() : {};
  let minimal = false;
  let noFrameRate = false;
  for (;;) {
    try {
      return await md.getDisplayMedia(displayMediaOptions(supported, { minimal, noFrameRate }));
    } catch (err) {
      const name = errorName(err);
      if (name === 'TypeError' && !minimal) {
        log.info('getDisplayMedia: unknown option, retrying with the basic ones');
        minimal = true;
      } else if (name === 'OverconstrainedError' && !noFrameRate) {
        log.info('getDisplayMedia: overconstrained, retrying without frameRate');
        noFrameRate = true;
      } else if (name === 'NotAllowedError') {
        return null;
      } else {
        log.warn('getDisplayMedia failed', { name });
        throw new LocalError('capture_failed', { cause: err });
      }
    }
  }
}

function stopTracks(stream: MediaStream): void {
  for (const t of stream.getTracks()) t.stop();
}

/**
 * Turns a captured stream into a PickedSource: content hints (audio `music`, plan; video from the preset, §13.5)
 * and the classification of §13.3. `displaySurface` overrides the video track's setting (the fake display).
 */
function toPickedSource(stream: MediaStream, preset: Preset, displaySurface?: string): PickedSource {
  const [video] = stream.getVideoTracks();
  if (!video) {
    stopTracks(stream);
    log.warn('capture has no video track');
    throw new LocalError('capture_failed');
  }
  const [audio] = stream.getAudioTracks();
  if (audio) audio.contentHint = 'music';
  applyVideoContentHint(video, preset);
  const picked = classify(displaySurface ?? video.getSettings().displaySurface, audio !== undefined);
  // Kind and scope only: a track label is a window title, which never leaves the page or enters the log.
  log.info('picked', { kind: picked.kind, audioScope: picked.audioScope });
  return {
    ...picked,
    preview: stream,
    release: () => {
      stopTracks(stream);
    },
  };
}

/**
 * SharingProvider.pick() for the browser (05 §13.1–§13.3): opens the picker (or, on a loopback page that asks for
 * it, the fake display) and resolves with the classified source; null when the user cancels; rejects with
 * LocalError `capture_failed` when the capture fails.
 *
 * Call it synchronously from the click handler: nothing is awaited before getDisplayMedia.
 */
export async function pickDisplayMedia(
  opts: { preset: Preset },
  env: CaptureEnv = browserCaptureEnv(),
): Promise<PickedSource | null> {
  const fake = fakeDisplayRequest(env);
  if (fake) {
    log.info('using the fake display', { displaySurface: fake.displaySurface, audio: fake.audio });
    const display = createFakeDisplay(fake);
    return toPickedSource(display.stream, opts.preset, display.displaySurface);
  }
  const md = env.mediaDevices;
  if (typeof md?.getDisplayMedia !== 'function') throw new LocalError('capture_failed');
  const stream = await openPicker(md);
  return stream ? toPickedSource(stream, opts.preset) : null;
}

/** What start() needs from share/'s BrowserSharing. */
export type Publisher = Pick<SharingProvider, 'start'>;

export interface BrowserSharingOptions {
  /** Opens the picker and classifies the pick. Default: pickDisplayMedia on this page. */
  capture?: (opts: { preset: Preset }) => Promise<PickedSource | null>;
  /**
   * Fetches the publisher's code and makes the page's one publisher. Default: share/'s BrowserSharing, a chunk of
   * its own, over this page's picker and peer connections. Tests pass their own.
   */
  load?: () => Promise<Publisher>;
}

/**
 * The in-page SharingProvider of BrowserPlatform (05 §8): this page's picker, and share/'s BrowserSharing behind
 * start().
 *
 * BrowserSharing is loaded when it is first needed and made once: it keeps the page's pub PC, one per signaling
 * client, for every later share (05 §13.6).
 * - pick() opens the picker at once (the click's transient activation), then asks for the publisher's code without
 *   waiting for it: it arrives while the user chooses what to share.
 * - start() waits for the publisher and hands over. When its code can't be fetched (the server is out of reach),
 *   start() rejects with LocalError `offline` before anything is sent, and the next pick or start fetches again: a
 *   chunk that didn't arrive once may arrive the next time. The source stays the caller's to release, as on every
 *   rejection of start().
 */
export function createBrowserSharing(opts: BrowserSharingOptions = {}): SharingProvider {
  const capture = opts.capture ?? ((o: { preset: Preset }) => pickDisplayMedia(o));
  const load =
    opts.load ??
    (async (): Promise<Publisher> => {
      const { BrowserSharing } = await import('../../share/BrowserSharing');
      return new BrowserSharing({
        capture,
        platform: { createPeerConnection: (config) => new RTCPeerConnection(config) },
      });
    });

  let publisher: Promise<Publisher> | null = null;
  const loaded = (): Promise<Publisher> => {
    // load() runs at once, inside an async function: whatever it throws becomes a rejection, never pick()'s throw.
    publisher ??= (async () => load())().catch((err: unknown) => {
      publisher = null;
      throw err;
    });
    return publisher;
  };

  return {
    mode: 'in-page',
    pick(o: { preset: Preset }): Promise<PickedSource | null> {
      // First, with nothing before it: getDisplayMedia needs the click's transient activation.
      const picked = capture(o);
      // A load that fails here is start()'s to report.
      loaded().catch(() => undefined);
      return picked;
    },
    async start(src: PickedSource, o: { preset: Preset; withAudio: boolean }, ctx: ShareContext): Promise<ActiveShare> {
      let ready: Publisher;
      try {
        ready = await loaded();
      } catch (err) {
        log.warn("the publisher's code could not be loaded", { err });
        throw new LocalError('offline', { cause: err });
      }
      return ready.start(src, o, ctx);
    },
  };
}
