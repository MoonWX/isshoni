// Tests of platform/browser/fakeDisplay.ts, the e2e capture seam (05 §19.3). They live in share/ because S35
// touches only the three source files under platform/browser/ (docs/m1/README.md §5).
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import {
  createFakeDisplay,
  FAKE_DISPLAY_KEY,
  fakeDisplayRequest,
  isLoopbackHost,
  parseFakeDisplay,
  type FakeDisplaySpec,
} from '../platform/browser/fakeDisplay';
import { createMemoryStorage } from '../platform/browser/storage';
import { FakeAudioContext, installFakeCanvas, type FakeCanvasControl } from './testing/fakeCapture';

const session = (value?: string) => createMemoryStorage(value === undefined ? {} : { [FAKE_DISPLAY_KEY]: value });

describe('parseFakeDisplay', () => {
  it.each<[string, FakeDisplaySpec]>([
    ['browser', { displaySurface: 'browser', audio: false }],
    ['window', { displaySurface: 'window', audio: false }],
    ['monitor', { displaySurface: 'monitor', audio: false }],
    ['monitor+audio', { displaySurface: 'monitor', audio: true }],
    ['window+audio', { displaySurface: 'window', audio: true }],
    ['browser+audio', { displaySurface: 'browser', audio: true }],
    ['1', { displaySurface: 'browser', audio: true }],
  ])('%s', (value, want) => {
    expect(parseFakeDisplay(value)).toEqual(want);
  });

  it.each(['', '0', 'true', 'screen', 'tab', 'Monitor', 'monitor+', '+audio', 'monitor+audio+audio', 'monitor +audio'])(
    'does not know %j',
    (value) => {
      expect(parseFakeDisplay(value)).toBeNull();
    },
  );
});

describe('the fake-display seam works only on a loopback page (05 §19.3, §20)', () => {
  it.each(['localhost', '127.0.0.1'])('is on at %s', (hostname) => {
    expect(isLoopbackHost(hostname)).toBe(true);
    expect(fakeDisplayRequest({ hostname, session: session('monitor+audio') })).toEqual({
      displaySurface: 'monitor',
      audio: true,
    });
  });

  it.each([
    'watch.example.org',
    'localhost.example.org',
    'app.localhost',
    'LOCALHOST',
    '127.0.0.2',
    '127.0.0.1.example.org',
    '192.168.1.10',
    '10.0.0.5',
    '0.0.0.0',
    '[::1]',
    '',
  ])('is off at %j, whatever sessionStorage says', (hostname) => {
    expect(isLoopbackHost(hostname)).toBe(false);
    expect(fakeDisplayRequest({ hostname, session: session('monitor+audio') })).toBeNull();
    expect(fakeDisplayRequest({ hostname, session: session('1') })).toBeNull();
  });

  it('does not even read the key on another host', () => {
    const get = vi.fn(() => 'not-a-value');
    expect(fakeDisplayRequest({ hostname: 'watch.example.org', session: { get } })).toBeNull();
    expect(get).not.toHaveBeenCalled();
  });

  it('is off while the key is unset or empty', () => {
    expect(fakeDisplayRequest({ hostname: 'localhost', session: session() })).toBeNull();
    expect(fakeDisplayRequest({ hostname: 'localhost', session: session('') })).toBeNull();
  });

  it('rejects an unknown value loudly instead of opening the real picker', () => {
    expect(() => fakeDisplayRequest({ hostname: 'localhost', session: session('screen') })).toThrow(
      /isshoni\.e2e\.fakeDisplay: unknown value "screen"/,
    );
  });
});

describe('createFakeDisplay', () => {
  let canvas: FakeCanvasControl;

  beforeEach(() => {
    vi.useFakeTimers();
    canvas = installFakeCanvas();
  });

  afterEach(() => {
    canvas.restore();
    vi.useRealTimers();
  });

  it('gives a canvas video track and reports the surface the value names', () => {
    const display = createFakeDisplay({ displaySurface: 'window', audio: false });
    expect(display.displaySurface).toBe('window');
    expect(display.stream).toBe(canvas.streams[0]);
    expect(display.stream.getVideoTracks()).toHaveLength(1);
    expect(display.stream.getVideoTracks()[0]?.getSettings()).toMatchObject({ width: 1280, height: 720 });
    expect(display.stream.getAudioTracks()).toHaveLength(0);
    expect(FakeAudioContext.instances).toHaveLength(0);
  });

  it('adds a 1 kHz tone as the audio track when the value asks for sound', () => {
    const display = createFakeDisplay({ displaySurface: 'monitor', audio: true });
    const [ctx] = FakeAudioContext.instances;
    expect(display.stream.getAudioTracks()).toEqual([ctx?.track]);
    expect(ctx?.oscillator.frequency.value).toBe(1000);
    expect(ctx?.oscillator.start).toHaveBeenCalledOnce();
    expect(ctx?.gain.gain.value).toBeGreaterThan(0.05);
    expect(ctx?.state).toBe('running');
  });

  it('keeps the picture changing: a new frame counter on every tick', () => {
    createFakeDisplay({ displaySurface: 'browser', audio: false });
    const frames = (): string[] =>
      canvas.fillText.mock.calls.map(([text]) => text).filter((t) => t.startsWith('frame'));
    expect(frames()).toEqual(['frame 0']);
    vi.advanceTimersByTime(100);
    expect(frames()).toEqual(['frame 0', 'frame 1', 'frame 2', 'frame 3']);
    expect(canvas.fillText.mock.calls[0]?.[0]).toBe('isshoni fake browser');
  });

  it('stops drawing and closes its AudioContext once the video track is stopped', () => {
    const display = createFakeDisplay({ displaySurface: 'monitor', audio: true });
    const [ctx] = FakeAudioContext.instances;
    vi.advanceTimersByTime(100);
    for (const t of display.stream.getTracks()) t.stop();
    vi.advanceTimersByTime(100);
    const painted = canvas.fillText.mock.calls.length;
    expect(ctx?.state).toBe('closed');
    expect(vi.getTimerCount()).toBe(0);
    vi.advanceTimersByTime(1000);
    expect(canvas.fillText.mock.calls).toHaveLength(painted);
  });

  it('closes the AudioContext when only the sound is dropped ("Share without sound"), and keeps drawing', () => {
    const display = createFakeDisplay({ displaySurface: 'monitor', audio: true });
    const [ctx] = FakeAudioContext.instances;
    for (const t of display.stream.getAudioTracks()) t.stop();
    vi.advanceTimersByTime(100);
    expect(ctx?.state).toBe('closed');
    const painted = canvas.fillText.mock.calls.length;
    vi.advanceTimersByTime(100);
    expect(canvas.fillText.mock.calls.length).toBeGreaterThan(painted);
    for (const t of display.stream.getTracks()) t.stop();
    vi.advanceTimersByTime(100);
    expect(vi.getTimerCount()).toBe(0);
  });

  it('fails clearly without a 2D canvas', () => {
    canvas.restore();
    vi.spyOn(HTMLCanvasElement.prototype, 'getContext').mockReturnValue(null);
    expect(() => createFakeDisplay({ displaySurface: 'browser', audio: false })).toThrow(/no 2D canvas/);
    vi.restoreAllMocks();
    canvas = installFakeCanvas();
  });
});
