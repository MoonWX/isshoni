// Test support for the sharer: jsdom has no screen capture, no canvas rendering and no WebAudio, so the tests
// build picks from the fake media of test/fakeMedia.ts and stub the few browser pieces the fake display touches.
import { vi, type Mock } from 'vitest';

import { classify } from '../../platform/browser/classify';
import type { PickedSource, SharingProvider } from '../../platform/types';
import { FakeMediaStream, FakeMediaStreamTrack } from '../../test/fakeMedia';

export type Surface = 'window' | 'browser' | 'monitor';

/** A stream like getDisplayMedia's: one video track that reports displaySurface, and an audio track if asked. */
export function captureStream(displaySurface: Surface | undefined, audio: boolean): FakeMediaStream {
  const settings: MediaTrackSettings = displaySurface ? { displaySurface, width: 1920, height: 1080 } : {};
  // The labels are what a browser puts there: window titles, which must never leave the page.
  const tracks = [new FakeMediaStreamTrack('video', settings, 'Secret Window Title')];
  if (audio) tracks.push(new FakeMediaStreamTrack('audio', {}, 'System Audio'));
  return new FakeMediaStream(tracks);
}

export interface FakePick extends PickedSource {
  readonly stream: FakeMediaStream;
  readonly release: Mock<() => void>;
}

/** A PickedSource as displayMedia.ts builds it, classified by the real classify(). */
export function fakePick(displaySurface: Surface, audio: boolean): FakePick {
  const stream = captureStream(displaySurface, audio);
  return {
    ...classify(displaySurface, audio),
    stream,
    preview: stream as unknown as MediaStream,
    release: vi.fn(() => {
      for (const t of stream.getTracks()) t.stop();
    }),
  };
}

export interface FakeSharing extends SharingProvider {
  readonly pick: Mock<SharingProvider['pick']>;
  readonly start: Mock<SharingProvider['start']>;
}

/** A SharingProvider whose pick() resolves with the given picks, one per call (null = cancelled), then cancels. */
export function fakeSharing(...picks: (PickedSource | null | Error)[]): FakeSharing {
  const pick = vi.fn<SharingProvider['pick']>(() => Promise.resolve(null));
  for (const p of picks) {
    if (p instanceof Error) pick.mockRejectedValueOnce(p);
    else pick.mockResolvedValueOnce(p);
  }
  return { mode: 'in-page', pick, start: vi.fn<SharingProvider['start']>() };
}

/** A promise with its resolve and reject, for picks that settle when the test says so. */
export function deferred<T>(): { promise: Promise<T>; resolve: (v: T) => void; reject: (e: unknown) => void } {
  let resolve!: (v: T) => void;
  let reject!: (e: unknown) => void;
  const promise = new Promise<T>((res, rej) => {
    resolve = res;
    reject = rej;
  });
  return { promise, resolve, reject };
}

/** The part of AudioContext the fake display uses. */
export class FakeAudioContext {
  static instances: FakeAudioContext[] = [];
  state: AudioContextState = 'suspended';
  readonly track = new FakeMediaStreamTrack('audio', {}, 'tone');
  readonly oscillator = { frequency: { value: 440 }, connect: <T>(node: T): T => node, start: vi.fn() };
  readonly gain = { gain: { value: 1 }, connect: <T>(node: T): T => node };

  constructor() {
    FakeAudioContext.instances.push(this);
  }

  createOscillator(): FakeAudioContext['oscillator'] {
    return this.oscillator;
  }

  createGain(): FakeAudioContext['gain'] {
    return this.gain;
  }

  createMediaStreamDestination(): { stream: FakeMediaStream } {
    return { stream: new FakeMediaStream([this.track]) };
  }

  resume(): Promise<void> {
    this.state = 'running';
    return Promise.resolve();
  }

  close(): Promise<void> {
    this.state = 'closed';
    return Promise.resolve();
  }
}

export interface FakeCanvasControl {
  /** Every stream canvas.captureStream() returned, in order. */
  readonly streams: FakeMediaStream[];
  /** The 2D context's fillText: one call per line of text painted. */
  readonly fillText: Mock<(text: string, x: number, y: number) => void>;
  /** Puts HTMLCanvasElement and the globals back. */
  restore(): void;
}

/**
 * Makes the fake display buildable in jsdom: a 2D context that records its drawing, canvas.captureStream()
 * returning a fake video stream, and FakeAudioContext as AudioContext.
 */
export function installFakeCanvas(): FakeCanvasControl {
  const proto = HTMLCanvasElement.prototype;
  const streams: FakeMediaStream[] = [];
  const fillText = vi.fn<(text: string, x: number, y: number) => void>();
  const context = { fillStyle: '', font: '', fillRect: vi.fn(), fillText };
  const getContext = vi.spyOn(proto, 'getContext').mockReturnValue(context as unknown as CanvasRenderingContext2D);
  Object.defineProperty(proto, 'captureStream', {
    configurable: true,
    writable: true,
    value(this: HTMLCanvasElement): FakeMediaStream {
      const stream = new FakeMediaStream([
        new FakeMediaStreamTrack('video', { width: this.width, height: this.height }, 'canvas'),
      ]);
      streams.push(stream);
      return stream;
    },
  });
  FakeAudioContext.instances = [];
  vi.stubGlobal('AudioContext', FakeAudioContext);
  return {
    streams,
    fillText,
    restore() {
      getContext.mockRestore();
      Reflect.deleteProperty(proto, 'captureStream');
      vi.unstubAllGlobals();
    },
  };
}
