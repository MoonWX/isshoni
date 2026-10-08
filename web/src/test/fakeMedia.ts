// Fake media for jsdom, which has no MediaStream, no MediaStreamTrack and no real playback (05 §19.1: the audio
// unlock tests use "a fake media element"). Nothing here touches real devices.
//
// - FakeMediaStreamTrack / FakeMediaStream: enough of the API for the controllers (kind, id, enabled, readyState,
//   contentHint, getSettings, stop, the `ended` event; getTracks and friends).
// - installFakeMedia(): puts them on globalThis as MediaStream and MediaStreamTrack.
// - installFakeMediaElement(): HTMLMediaElement.play() that resolves or rejects (NotAllowedError) by policy, and a
//   working srcObject; the returned controller changes the policy and restores the prototype.
import { vi } from 'vitest';

let nextId = 1;

export class FakeMediaStreamTrack extends EventTarget {
  readonly kind: 'audio' | 'video';
  readonly id: string;
  label: string;
  enabled = true;
  muted = false;
  contentHint = '';
  readyState: MediaStreamTrackState = 'live';
  onended: ((this: FakeMediaStreamTrack, ev: Event) => unknown) | null = null;
  /** What getSettings() returns (displaySurface, width, height …). */
  settings: MediaTrackSettings;

  constructor(kind: 'audio' | 'video', settings: MediaTrackSettings = {}, label = `${kind} track`) {
    super();
    this.kind = kind;
    this.id = `track-${String(nextId++)}`;
    this.label = label;
    this.settings = { ...settings };
  }

  getSettings(): MediaTrackSettings {
    return { ...this.settings };
  }

  getConstraints(): MediaTrackConstraints {
    return {};
  }

  applyConstraints(): Promise<void> {
    return Promise.resolve();
  }

  clone(): FakeMediaStreamTrack {
    return new FakeMediaStreamTrack(this.kind, this.settings, this.label);
  }

  /** Like the real stop(): no `ended` event (that fires only when the source ends). */
  stop(): void {
    this.readyState = 'ended';
  }

  /** Test control: the source ended (the browser's "Stop sharing" button): readyState ended, `ended` fires. */
  end(): void {
    if (this.readyState === 'ended') return;
    this.readyState = 'ended';
    const ev = new Event('ended');
    this.onended?.call(this, ev);
    this.dispatchEvent(ev);
  }
}

export class FakeMediaStream extends EventTarget {
  readonly id: string;
  readonly #tracks: FakeMediaStreamTrack[] = [];

  constructor(tracks: readonly FakeMediaStreamTrack[] = [], id = `stream-${String(nextId++)}`) {
    super();
    this.id = id;
    this.#tracks.push(...tracks);
  }

  get active(): boolean {
    return this.#tracks.some((t) => t.readyState === 'live');
  }

  getTracks(): FakeMediaStreamTrack[] {
    return [...this.#tracks];
  }

  getAudioTracks(): FakeMediaStreamTrack[] {
    return this.#tracks.filter((t) => t.kind === 'audio');
  }

  getVideoTracks(): FakeMediaStreamTrack[] {
    return this.#tracks.filter((t) => t.kind === 'video');
  }

  getTrackById(id: string): FakeMediaStreamTrack | null {
    return this.#tracks.find((t) => t.id === id) ?? null;
  }

  addTrack(track: FakeMediaStreamTrack): void {
    if (!this.#tracks.includes(track)) this.#tracks.push(track);
  }

  removeTrack(track: FakeMediaStreamTrack): void {
    const i = this.#tracks.indexOf(track);
    if (i >= 0) this.#tracks.splice(i, 1);
  }
}

/** Installs FakeMediaStream and FakeMediaStreamTrack as the globals; vi.unstubAllGlobals() removes them. */
export function installFakeMedia(): void {
  vi.stubGlobal('MediaStream', FakeMediaStream);
  vi.stubGlobal('MediaStreamTrack', FakeMediaStreamTrack);
}

export type PlayPolicy = 'allow' | 'block';

export interface FakeMediaElementControl {
  /** What the next play() calls do: 'allow' resolves, 'block' rejects with NotAllowedError (autoplay policy). */
  policy: PlayPolicy;
  /** Every element play() was called on, in order. */
  readonly played: HTMLMediaElement[];
  /** Puts the original prototype members back. */
  restore(): void;
}

/**
 * Makes HTMLMediaElement playable in jsdom: play() follows control.policy and sets `paused`; pause() sets it back;
 * srcObject stores its value. Call restore() (or use it in a beforeEach/afterEach pair).
 */
export function installFakeMediaElement(initial: PlayPolicy = 'allow'): FakeMediaElementControl {
  const proto = HTMLMediaElement.prototype;
  const originals = {
    play: Object.getOwnPropertyDescriptor(proto, 'play'),
    pause: Object.getOwnPropertyDescriptor(proto, 'pause'),
    paused: Object.getOwnPropertyDescriptor(proto, 'paused'),
    srcObject: Object.getOwnPropertyDescriptor(proto, 'srcObject'),
  };
  const state = new WeakMap<HTMLMediaElement, { paused: boolean; srcObject: MediaProvider | null }>();
  const stateOf = (el: HTMLMediaElement) => {
    let s = state.get(el);
    if (!s) {
      s = { paused: true, srcObject: null };
      state.set(el, s);
    }
    return s;
  };
  const control: FakeMediaElementControl = {
    policy: initial,
    played: [],
    restore() {
      for (const [name, desc] of Object.entries(originals)) {
        if (desc) Object.defineProperty(proto, name, desc);
        else Reflect.deleteProperty(proto, name);
      }
    },
  };
  Object.defineProperty(proto, 'play', {
    configurable: true,
    writable: true,
    value(this: HTMLMediaElement): Promise<void> {
      control.played.push(this);
      if (control.policy === 'block') {
        return Promise.reject(new DOMException('play() needs a user gesture', 'NotAllowedError'));
      }
      stateOf(this).paused = false;
      this.dispatchEvent(new Event('play'));
      this.dispatchEvent(new Event('playing'));
      return Promise.resolve();
    },
  });
  Object.defineProperty(proto, 'pause', {
    configurable: true,
    writable: true,
    value(this: HTMLMediaElement): void {
      stateOf(this).paused = true;
      this.dispatchEvent(new Event('pause'));
    },
  });
  Object.defineProperty(proto, 'paused', {
    configurable: true,
    get(this: HTMLMediaElement): boolean {
      return stateOf(this).paused;
    },
  });
  Object.defineProperty(proto, 'srcObject', {
    configurable: true,
    get(this: HTMLMediaElement): MediaProvider | null {
      return stateOf(this).srcObject;
    },
    set(this: HTMLMediaElement, v: MediaProvider | null) {
      stateOf(this).srcObject = v;
    },
  });
  return control;
}
