// Tests of platform/browser/displayMedia.ts: the getDisplayMedia options and pick()'s fallbacks (05 §13.2), and
// the fake-display seam as pick() sees it (§19.3). They live in share/ because S35 touches only the three source
// files under platform/browser/ (docs/m1/README.md §5).
import { afterEach, beforeEach, describe, expect, it, vi, type Mock } from 'vitest';

import { LocalError } from '../lib/errors';
import { clearLog, logLines } from '../lib/log';
import {
  browserCaptureEnv,
  createBrowserSharing,
  displayMediaOptions,
  pickDisplayMedia,
  type CaptureEnv,
} from '../platform/browser/displayMedia';
import { FAKE_DISPLAY_KEY } from '../platform/browser/fakeDisplay';
import { createMemoryStorage } from '../platform/browser/storage';
import type { PickedSource, ShareContext } from '../platform/types';
import { FakeRTCPeerConnection, installFakeRTC } from '../test/FakeRTCPeerConnection';
import { FakeMediaStream } from '../test/fakeMedia';
import { shareStore } from './shareStore';
import { captureStream, fakePick, installFakeCanvas, type FakeCanvasControl } from './testing/fakeCapture';
import { shareParams } from './testing/publish';

type GetDisplayMedia = (options?: DisplayMediaStreamOptions) => Promise<MediaStream>;

const ALL_SUPPORTED: MediaTrackSupportedConstraints = { restrictOwnAudio: true, suppressLocalAudioPlayback: true };

/** mediaDevices whose getDisplayMedia settles with the given results, one per call. */
function mediaDevices(results: unknown[], supported: MediaTrackSupportedConstraints = ALL_SUPPORTED) {
  const getDisplayMedia = vi.fn<GetDisplayMedia>();
  for (const r of results) {
    if (r instanceof FakeMediaStream) getDisplayMedia.mockResolvedValueOnce(r as unknown as MediaStream);
    else getDisplayMedia.mockRejectedValueOnce(r);
  }
  return { getDisplayMedia, getSupportedConstraints: () => supported };
}

/** A deployed page: not a loopback host, nothing in sessionStorage. */
function env(md: CaptureEnv['mediaDevices'], over: Partial<CaptureEnv> = {}): CaptureEnv {
  return { mediaDevices: md, hostname: 'watch.example.org', session: createMemoryStorage(), ...over };
}

const domError = (name: string): DOMException => new DOMException(`fake ${name}`, name);

/** The full options of 05 §13.2. */
const PLAN_OPTIONS: DisplayMediaStreamOptions = {
  video: { frameRate: { ideal: 60, max: 60 }, displaySurface: 'window' },
  audio: {
    echoCancellation: false,
    noiseSuppression: false,
    autoGainControl: false,
    suppressLocalAudioPlayback: false,
    restrictOwnAudio: true,
  },
  systemAudio: 'include',
  windowAudio: 'window',
  selfBrowserSurface: 'exclude',
  surfaceSwitching: 'include',
  monitorTypeSurfaces: 'include',
  preferCurrentTab: false,
};

/** After a TypeError: only video, audio and systemAudio. */
const BASIC_OPTIONS: DisplayMediaStreamOptions = {
  video: PLAN_OPTIONS.video,
  audio: PLAN_OPTIONS.audio,
  systemAudio: 'include',
};

beforeEach(() => {
  clearLog();
});

describe('displayMediaOptions (05 §13.2)', () => {
  it('are the plan values: 60 fps, the picker on Window, per-window audio, own playback and own tab kept out', () => {
    expect(displayMediaOptions(ALL_SUPPORTED)).toEqual(PLAN_OPTIONS);
  });

  it('turn echo cancellation, noise suppression and AGC off: this is programme sound, not a microphone', () => {
    expect(displayMediaOptions({}).audio).toEqual({
      echoCancellation: false,
      noiseSuppression: false,
      autoGainControl: false,
    });
  });

  it('send restrictOwnAudio and suppressLocalAudioPlayback only where getSupportedConstraints() lists them', () => {
    expect(displayMediaOptions({ restrictOwnAudio: true }).audio).toEqual({
      echoCancellation: false,
      noiseSuppression: false,
      autoGainControl: false,
      restrictOwnAudio: true,
    });
    expect(displayMediaOptions({ suppressLocalAudioPlayback: true }).audio).toEqual({
      echoCancellation: false,
      noiseSuppression: false,
      autoGainControl: false,
      suppressLocalAudioPlayback: false,
    });
  });

  it('carry no width or height: the capture keeps its native size', () => {
    expect(Object.keys(displayMediaOptions(ALL_SUPPORTED).video as object).sort()).toEqual([
      'displaySurface',
      'frameRate',
    ]);
  });

  it('shrink for the fallbacks', () => {
    expect(displayMediaOptions(ALL_SUPPORTED, { minimal: true })).toEqual(BASIC_OPTIONS);
    expect(displayMediaOptions(ALL_SUPPORTED, { noFrameRate: true })).toEqual({
      ...PLAN_OPTIONS,
      video: { displaySurface: 'window' },
    });
  });
});

describe('pick() (05 §13.1–§13.3)', () => {
  it('calls getDisplayMedia before it returns: nothing is awaited first (transient activation)', async () => {
    const md = mediaDevices([captureStream('window', true)]);
    const picking = pickDisplayMedia({ preset: 'auto' }, env(md));
    expect(md.getDisplayMedia).toHaveBeenCalledExactlyOnceWith(PLAN_OPTIONS);
    await picking;
  });

  it('resolves with the classified source and its stream', async () => {
    const stream = captureStream('window', true);
    const src = await pickDisplayMedia({ preset: 'auto' }, env(mediaDevices([stream])));
    expect(src).toMatchObject({ kind: 'window', audioScope: 'window', warning: null });
    expect(src?.preview).toBe(stream);
  });

  it.each([
    ['window', false, { kind: 'window', audioScope: 'none', warning: 'no-audio' }],
    ['browser', true, { kind: 'tab', audioScope: 'tab', warning: null }],
    ['monitor', true, { kind: 'screen', audioScope: 'system', warning: 'screen-with-system-audio' }],
    ['monitor', false, { kind: 'screen', audioScope: 'none', warning: 'no-audio' }],
  ] as const)('classifies %s (audio %s) from the track settings', async (surface, audio, want) => {
    const src = await pickDisplayMedia({ preset: 'auto' }, env(mediaDevices([captureStream(surface, audio)])));
    expect(src).toMatchObject(want);
  });

  it('warns about sound from a browser that does not say what was picked', async () => {
    const src = await pickDisplayMedia({ preset: 'auto' }, env(mediaDevices([captureStream(undefined, true)])));
    expect(src).toMatchObject({ kind: 'screen', warning: 'screen-with-system-audio' });
  });

  it('hints the audio track as music and the video track by the preset', async () => {
    const hints = async (preset: 'auto' | 'game' | 'movie' | 'text'): Promise<(string | undefined)[]> => {
      const stream = captureStream('browser', true);
      await pickDisplayMedia({ preset }, env(mediaDevices([stream])));
      return [stream.getVideoTracks()[0]?.contentHint, stream.getAudioTracks()[0]?.contentHint];
    };
    expect(await hints('auto')).toEqual(['', 'music']);
    expect(await hints('game')).toEqual(['motion', 'music']);
    expect(await hints('movie')).toEqual(['motion', 'music']);
    expect(await hints('text')).toEqual(['text', 'music']);
  });

  it('release() stops every track', async () => {
    const stream = captureStream('monitor', true);
    const src = await pickDisplayMedia({ preset: 'auto' }, env(mediaDevices([stream])));
    expect(stream.active).toBe(true);
    src?.release();
    expect(stream.getTracks().map((t) => t.readyState)).toEqual(['ended', 'ended']);
  });

  it('never carries or logs a window title', async () => {
    const src = await pickDisplayMedia({ preset: 'auto' }, env(mediaDevices([captureStream('window', true)])));
    expect(Object.keys(src ?? {}).sort()).toEqual(['audioScope', 'kind', 'preview', 'release', 'warning']);
    expect(JSON.stringify(logLines())).not.toContain('Secret Window Title');
    expect(logLines().at(-1)).toMatchObject({
      component: 'capture',
      msg: 'picked',
      attrs: { kind: 'window', audioScope: 'window' },
    });
  });

  describe('fallbacks (05 §13.2)', () => {
    it('TypeError (an option value this browser does not know) → once more with only video, audio, systemAudio', async () => {
      const md = mediaDevices([new TypeError('unknown enum value'), captureStream('window', false)]);
      const src = await pickDisplayMedia({ preset: 'auto' }, env(md));
      expect(src?.kind).toBe('window');
      expect(md.getDisplayMedia.mock.calls).toEqual([[PLAN_OPTIONS], [BASIC_OPTIONS]]);
    });

    it('a second TypeError is a failed capture: the basic options are tried once', async () => {
      const md = mediaDevices([new TypeError('first'), new TypeError('second')]);
      await expect(pickDisplayMedia({ preset: 'auto' }, env(md))).rejects.toMatchObject({
        name: 'LocalError',
        code: 'capture_failed',
      });
      expect(md.getDisplayMedia).toHaveBeenCalledTimes(2);
    });

    it.each([
      ['a DOMException', domError('OverconstrainedError')],
      ['the old non-Error object', { name: 'OverconstrainedError', constraint: 'frameRate', message: '' }],
    ])('OverconstrainedError (%s) → once more without frameRate', async (_form, error) => {
      const md = mediaDevices([error, captureStream('browser', true)]);
      const src = await pickDisplayMedia({ preset: 'auto' }, env(md));
      expect(src?.kind).toBe('tab');
      expect(md.getDisplayMedia.mock.calls).toEqual([
        [PLAN_OPTIONS],
        [{ ...PLAN_OPTIONS, video: { displaySurface: 'window' } }],
      ]);
    });

    it('a second OverconstrainedError is a failed capture', async () => {
      const md = mediaDevices([domError('OverconstrainedError'), domError('OverconstrainedError')]);
      await expect(pickDisplayMedia({ preset: 'auto' }, env(md))).rejects.toBeInstanceOf(LocalError);
      expect(md.getDisplayMedia).toHaveBeenCalledTimes(2);
    });

    it('both fallbacks add up: TypeError, then OverconstrainedError', async () => {
      const md = mediaDevices([
        new TypeError('unknown enum value'),
        domError('OverconstrainedError'),
        captureStream('monitor', false),
      ]);
      const src = await pickDisplayMedia({ preset: 'auto' }, env(md));
      expect(src?.kind).toBe('screen');
      expect(md.getDisplayMedia.mock.calls[2]).toEqual([{ ...BASIC_OPTIONS, video: { displaySurface: 'window' } }]);
    });

    it('NotAllowedError → null: cancelling the picker and denying the permission look the same, no message', async () => {
      const md = mediaDevices([domError('NotAllowedError')]);
      await expect(pickDisplayMedia({ preset: 'auto' }, env(md))).resolves.toBeNull();
      expect(md.getDisplayMedia).toHaveBeenCalledOnce();
      expect(logLines().filter((l) => l.level === 'warn' || l.level === 'error')).toEqual([]);
    });

    it('NotAllowedError after a fallback is still a cancel', async () => {
      const md = mediaDevices([new TypeError('unknown enum value'), domError('NotAllowedError')]);
      await expect(pickDisplayMedia({ preset: 'auto' }, env(md))).resolves.toBeNull();
    });

    it.each(['NotReadableError', 'NotFoundError', 'AbortError', 'InvalidStateError', 'SecurityError'])(
      '%s → errors.local.capture_failed, with the cause',
      async (name) => {
        const cause = domError(name);
        const md = mediaDevices([cause]);
        const err: unknown = await pickDisplayMedia({ preset: 'auto' }, env(md)).catch((e: unknown) => e);
        expect(err).toBeInstanceOf(LocalError);
        expect(err).toMatchObject({ code: 'capture_failed', cause });
        expect(md.getDisplayMedia).toHaveBeenCalledOnce();
        expect(logLines().at(-1)).toMatchObject({ level: 'warn', attrs: { name } });
      },
    );

    it('a getDisplayMedia that throws instead of rejecting is handled the same way', async () => {
      const stream = captureStream('window', true);
      const getDisplayMedia = vi
        .fn<GetDisplayMedia>()
        .mockImplementationOnce(() => {
          throw new TypeError('thrown, not rejected');
        })
        .mockResolvedValueOnce(stream as unknown as MediaStream);
      const src = await pickDisplayMedia(
        { preset: 'auto' },
        env({ getDisplayMedia, getSupportedConstraints: () => ({}) }),
      );
      expect(src?.preview).toBe(stream);
      expect(getDisplayMedia).toHaveBeenCalledTimes(2);
    });

    it('no getDisplayMedia at all (plain http) → capture_failed', async () => {
      await expect(pickDisplayMedia({ preset: 'auto' }, env(undefined))).rejects.toMatchObject({
        code: 'capture_failed',
      });
    });

    it('a stream without a video track is stopped and fails', async () => {
      const stream = captureStream('window', true);
      const [video] = stream.getVideoTracks();
      if (video) stream.removeTrack(video);
      await expect(pickDisplayMedia({ preset: 'auto' }, env(mediaDevices([stream])))).rejects.toMatchObject({
        code: 'capture_failed',
      });
      expect(stream.getTracks().map((t) => t.readyState)).toEqual(['ended']);
    });

    it('works without getSupportedConstraints', async () => {
      const md = mediaDevices([captureStream('window', true)]);
      const src = await pickDisplayMedia(
        { preset: 'auto' },
        env({ getDisplayMedia: md.getDisplayMedia } as unknown as CaptureEnv['mediaDevices']),
      );
      expect(src?.kind).toBe('window');
      expect(md.getDisplayMedia.mock.calls[0]?.[0]?.audio).toEqual({
        echoCancellation: false,
        noiseSuppression: false,
        autoGainControl: false,
      });
    });
  });
});

describe('the fake-display seam in pick() (05 §19.3)', () => {
  let canvas: FakeCanvasControl;

  beforeEach(() => {
    vi.useFakeTimers();
    canvas = installFakeCanvas();
  });

  afterEach(() => {
    canvas.restore();
    vi.useRealTimers();
  });

  const withKey = (value: string) => createMemoryStorage({ [FAKE_DISPLAY_KEY]: value });

  it.each([
    ['monitor+audio', { kind: 'screen', audioScope: 'system', warning: 'screen-with-system-audio' }],
    ['monitor', { kind: 'screen', audioScope: 'none', warning: 'no-audio' }],
    ['window', { kind: 'window', audioScope: 'none', warning: 'no-audio' }],
    ['browser', { kind: 'tab', audioScope: 'none', warning: 'no-audio' }],
    ['1', { kind: 'tab', audioScope: 'tab', warning: null }],
  ] as const)('on localhost, %j skips getDisplayMedia and reports its surface to classify', async (value, want) => {
    const md = mediaDevices([]);
    const src = await pickDisplayMedia({ preset: 'game' }, env(md, { hostname: 'localhost', session: withKey(value) }));
    expect(md.getDisplayMedia).not.toHaveBeenCalled();
    expect(src).toMatchObject(want);
    expect(src?.preview.getVideoTracks()).toHaveLength(1);
    expect(src?.preview.getVideoTracks()[0]?.contentHint).toBe('motion');
    expect(src?.preview.getAudioTracks()).toHaveLength(want.audioScope === 'none' ? 0 : 1);
    src?.release();
  });

  it('works on 127.0.0.1 too, and needs no mediaDevices', async () => {
    const src = await pickDisplayMedia(
      { preset: 'auto' },
      env(undefined, { hostname: '127.0.0.1', session: withKey('1') }),
    );
    expect(src?.kind).toBe('tab');
    src?.release();
  });

  it.each(['watch.example.org', 'localhost.example.org', '192.168.1.10', '[::1]'])(
    'is ignored on %s: the real picker opens',
    async (hostname) => {
      const stream = captureStream('window', true);
      const md = mediaDevices([stream]);
      const src = await pickDisplayMedia({ preset: 'auto' }, env(md, { hostname, session: withKey('monitor+audio') }));
      expect(md.getDisplayMedia).toHaveBeenCalledOnce();
      expect(src?.preview).toBe(stream);
      expect(src?.kind).toBe('window');
    },
  );

  it('an unknown value on localhost rejects instead of opening the real picker', async () => {
    const md = mediaDevices([]);
    await expect(
      pickDisplayMedia({ preset: 'auto' }, env(md, { hostname: 'localhost', session: withKey('screen') })),
    ).rejects.toThrow(/unknown value "screen"/);
    expect(md.getDisplayMedia).not.toHaveBeenCalled();
  });

  it("reads this page's own host and sessionStorage by default", async () => {
    // jsdom's page is http://localhost:3000, a loopback page.
    expect(browserCaptureEnv().hostname).toBe('localhost');
    sessionStorage.setItem(FAKE_DISPLAY_KEY, 'monitor+audio');
    try {
      const src = await createBrowserSharing().pick({ preset: 'auto' });
      expect(src).toMatchObject({ kind: 'screen', warning: 'screen-with-system-audio' });
      src?.release();
    } finally {
      sessionStorage.removeItem(FAKE_DISPLAY_KEY);
    }
  });
});

describe('createBrowserSharing', () => {
  let uninstall: (() => void) | undefined;

  afterEach(() => {
    uninstall?.();
    uninstall = undefined;
  });

  it("is the in-page provider over this page's getDisplayMedia", async () => {
    uninstall = installFakeRTC({ displayMedia: true });
    const stream = captureStream('browser', true);
    // installFakeRTC puts a vi.fn() there.
    const { getDisplayMedia } = navigator.mediaDevices as unknown as { getDisplayMedia: Mock<GetDisplayMedia> };
    getDisplayMedia.mockResolvedValueOnce(stream as unknown as MediaStream);
    const sharing = createBrowserSharing();
    expect(sharing.mode).toBe('in-page');
    const picking = sharing.pick({ preset: 'movie' });
    expect(getDisplayMedia).toHaveBeenCalledOnce();
    const src = await picking;
    expect(src).toMatchObject({ kind: 'tab', audioScope: 'tab', warning: null });
    expect(stream.getVideoTracks()[0]?.contentHint).toBe('motion');
  });

  it("start() publishes on this page's RTCPeerConnection, with the browser's own codec capabilities", async () => {
    uninstall = installFakeRTC({ displayMedia: true });
    const request = vi.fn((type: string) => Promise.resolve(type === 'share.start' ? shareParams('s_1') : {}));
    const notify = vi.fn(() => true);
    const ctx = {
      signal: { request, notify, on: () => () => undefined, probe: vi.fn() },
      roomId: 'lounge',
    } as unknown as ShareContext;
    const src: PickedSource = fakePick('window', true);

    const share = await createBrowserSharing().start(src, { preset: 'auto', withAudio: true }, ctx);
    try {
      expect(request).toHaveBeenCalledWith('share.start', expect.objectContaining({ kind: 'window', audio: true }));
      expect(FakeRTCPeerConnection.instances).toHaveLength(1);
      // RTCRtpSender.getCapabilities('video'): H.264 with packetization-mode 1 and RTX, the server's profile first.
      expect(FakeRTCPeerConnection.last?.transceivers[0]?.codecPreferences.map((c) => c.mimeType)).toEqual([
        'video/H264',
        'video/H264',
        'video/rtx',
      ]);
      expect(notify).toHaveBeenCalledWith('pc.offer', expect.objectContaining({ pc: 'pub', gen: 1, neg: 1 }));
      // The page's share state machine follows (the default store).
      expect(shareStore.getState().phase).toBe('starting');
    } finally {
      await share.stop();
    }
    expect(shareStore.getState().phase).toBe('idle');
    expect(notify).toHaveBeenLastCalledWith('pc.close', { pc: 'pub', gen: 1 });
  });

  it('start() without an H.264 encoder says so before anything is sent', async () => {
    uninstall = installFakeRTC({ h264: false, displayMedia: true });
    const request = vi.fn();
    const ctx = { signal: { request }, roomId: 'lounge' } as unknown as ShareContext;
    await expect(
      createBrowserSharing().start(fakePick('window', true), { preset: 'auto', withAudio: true }, ctx),
    ).rejects.toMatchObject({ name: 'LocalError', code: 'h264_unavailable' });
    expect(request).not.toHaveBeenCalled();
  });
});
