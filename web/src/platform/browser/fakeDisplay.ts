// The fake-display seam (05 §19.3): a capture source that needs no picker, for end-to-end tests. Headless Chrome
// can't accept getDisplayMedia's picker (S4 finding 7), and no flag picks a whole screen with system audio, so local
// headless runs and the whole-screen cases (warning.spec) ask for a fake display instead:
//
//   sessionStorage['isshoni.e2e.fakeDisplay'] = 'browser' | 'window' | 'monitor', plus '+audio' for sound
//                                               ('monitor+audio'); '1' is short for 'browser+audio'
//
// displayMedia.ts then skips getDisplayMedia and gets a canvas + WebAudio stream from here, with the displaySurface
// the value names, which it hands to classify.ts like a real pick's.
//
// The seam exists only on a loopback page (05 §20): the host must be exactly `localhost` or `127.0.0.1`. On any
// other host the key is not even read, so a deployed server can never be made to publish a fake screen.
import type { KeyValueStore } from '../types';

export const FAKE_DISPLAY_KEY = 'isshoni.e2e.fakeDisplay';

/** MediaTrackSettings.displaySurface values the seam can report. */
export type FakeDisplaySurface = 'browser' | 'window' | 'monitor';

export interface FakeDisplaySpec {
  readonly displaySurface: FakeDisplaySurface;
  /** Whether the stream has an audio track (a 1 kHz tone). */
  readonly audio: boolean;
}

/** Where the seam reads from; displayMedia.ts passes the page's host and sessionStorage, tests their own. */
export interface FakeDisplayEnv {
  /** location.hostname */
  readonly hostname: string;
  readonly session: Pick<KeyValueStore, 'get'>;
}

export interface FakeDisplay {
  readonly stream: MediaStream;
  /** What a real pick's video track would report in getSettings().displaySurface. */
  readonly displaySurface: FakeDisplaySurface;
}

const SURFACES: readonly FakeDisplaySurface[] = ['browser', 'window', 'monitor'];
const AUDIO_SUFFIX = '+audio';

/** Whether the seam may work on this host: exactly `localhost` or `127.0.0.1` (05 §19.3). */
export function isLoopbackHost(hostname: string): boolean {
  return hostname === 'localhost' || hostname === '127.0.0.1';
}

/** Reads a value of the key: `<surface>` or `<surface>+audio`, or `1` for `browser+audio`. null: not a known value. */
export function parseFakeDisplay(value: string): FakeDisplaySpec | null {
  if (value === '1') return { displaySurface: 'browser', audio: true };
  const audio = value.endsWith(AUDIO_SUFFIX);
  const name = audio ? value.slice(0, -AUDIO_SUFFIX.length) : value;
  const displaySurface = SURFACES.find((s) => s === name);
  return displaySurface ? { displaySurface, audio } : null;
}

/**
 * The fake display this page asks for, or null: the page is not on a loopback host (the key is not read then), or
 * the key is unset. A value that isn't one of the known ones throws, so a typo in a test fails at the first pick
 * instead of opening the real picker.
 */
export function fakeDisplayRequest(env: FakeDisplayEnv): FakeDisplaySpec | null {
  if (!isLoopbackHost(env.hostname)) return null;
  const value = env.session.get(FAKE_DISPLAY_KEY);
  if (value === null || value === '') return null;
  const spec = parseFakeDisplay(value);
  if (!spec) {
    throw new Error(
      `${FAKE_DISPLAY_KEY}: unknown value ${JSON.stringify(value)} (use browser, window or monitor, with "+audio" for sound, or 1)`,
    );
  }
  return spec;
}

const WIDTH = 1280;
const HEIGHT = 720;
const FPS = 30;
/** Like e2e/tone.html: a tone that stats can tell from silence (audioLevel well above 0.05). */
const TONE_HZ = 1000;
const TONE_GAIN = 0.25;

/** One frame of the test picture: a bar that crosses it every two seconds, and a frame counter. */
function paint(g: CanvasRenderingContext2D, frame: number, spec: FakeDisplaySpec): void {
  g.fillStyle = '#101014';
  g.fillRect(0, 0, WIDTH, HEIGHT);
  g.fillStyle = '#8e88ff';
  g.fillRect(((frame % (2 * FPS)) / (2 * FPS)) * WIDTH, 0, 80, HEIGHT);
  g.fillStyle = '#ffffff';
  g.font = '48px monospace';
  g.fillText(`isshoni fake ${spec.displaySurface}${spec.audio ? AUDIO_SUFFIX : ''}`, 48, 96);
  g.fillText(`frame ${String(frame)}`, 48, 168);
}

/**
 * Builds the fake capture: an animated canvas (1280×720, 30 fps) as the video track and, with spec.audio, a 1 kHz
 * WebAudio tone as the audio track. It cleans up after itself: once the video track is stopped (release(), or the
 * share ending) the drawing timer ends and the AudioContext closes; the AudioContext also closes when every audio
 * track was stopped ("Share without sound").
 */
export function createFakeDisplay(spec: FakeDisplaySpec): FakeDisplay {
  const canvas = globalThis.document.createElement('canvas');
  canvas.width = WIDTH;
  canvas.height = HEIGHT;
  const g = canvas.getContext('2d');
  if (!g) throw new Error('fake display: this browser has no 2D canvas');
  let frame = 0;
  paint(g, frame, spec);
  const stream = canvas.captureStream(FPS);

  let audio: { ctx: AudioContext; tracks: MediaStreamTrack[] } | null = null;
  if (spec.audio) {
    const ctx = new AudioContext();
    const tone = ctx.createOscillator();
    tone.frequency.value = TONE_HZ;
    const gain = ctx.createGain();
    gain.gain.value = TONE_GAIN;
    const out = ctx.createMediaStreamDestination();
    tone.connect(gain).connect(out);
    tone.start();
    // A context made outside a user gesture starts suspended; a pick always comes from a click.
    void ctx.resume();
    const tracks = out.stream.getAudioTracks();
    for (const t of tracks) stream.addTrack(t);
    audio = { ctx, tracks };
  }
  const closeAudio = (): void => {
    if (!audio) return;
    void audio.ctx.close();
    audio = null;
  };

  const videoTracks = stream.getVideoTracks();
  const timer = setInterval(() => {
    if (videoTracks.every((t) => t.readyState === 'ended')) {
      clearInterval(timer);
      closeAudio();
      return;
    }
    if (audio?.tracks.every((t) => t.readyState === 'ended')) closeAudio();
    paint(g, ++frame, spec);
  }, 1000 / FPS);

  return { stream, displaySurface: spec.displaySurface };
}
