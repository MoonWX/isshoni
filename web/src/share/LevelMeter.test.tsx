import { act, render, screen } from '@testing-library/react';
import { I18nextProvider } from 'react-i18next';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { i18n } from '../i18n';
import { FakeMediaStreamTrack, installFakeMedia } from '../test/fakeMedia';
import { openLevelTap, primeLevelAudio, resetLevelAudio } from './levelAudio';
import { LEVEL_INTERVAL_MS, LevelMeter, SILENCE_AFTER_MS } from './LevelMeter';

/** The part of WebAudio the meter uses: an analyser that reports the level the test sets. */
class FakeAudioContext {
  static instances: FakeAudioContext[] = [];
  /** What every analyser's samples peak at. */
  static level = 0;
  state: AudioContextState = 'suspended';
  readonly sources: { connect: ReturnType<typeof vi.fn>; disconnect: ReturnType<typeof vi.fn>; stream: unknown }[] = [];
  readonly resume = vi.fn(() => {
    this.state = 'running';
    return Promise.resolve();
  });
  readonly suspend = vi.fn(() => {
    this.state = 'suspended';
    return Promise.resolve();
  });
  readonly close = vi.fn(() => {
    this.state = 'closed';
    return Promise.resolve();
  });
  readonly destination = {};

  constructor() {
    FakeAudioContext.instances.push(this);
  }

  createMediaStreamSource(stream: unknown) {
    const source = { connect: vi.fn(), disconnect: vi.fn(), stream };
    this.sources.push(source);
    return source;
  }

  createAnalyser() {
    return {
      fftSize: 2048,
      getFloatTimeDomainData(samples: Float32Array) {
        samples.fill(0);
        samples[3] = -FakeAudioContext.level;
      },
    };
  }
}

const track = () => new FakeMediaStreamTrack('audio') as unknown as MediaStreamTrack;
const renderMeter = (props: { track: MediaStreamTrack | null; active: boolean }) =>
  render(
    <I18nextProvider i18n={i18n}>
      <LevelMeter {...props} />
    </I18nextProvider>,
  );
const tick = (ms: number): void => {
  act(() => {
    vi.advanceTimersByTime(ms);
  });
};

beforeEach(() => {
  vi.useFakeTimers();
  FakeAudioContext.instances = [];
  FakeAudioContext.level = 0;
  installFakeMedia();
  vi.stubGlobal('AudioContext', FakeAudioContext);
});

afterEach(() => {
  resetLevelAudio();
  vi.unstubAllGlobals();
  vi.useRealTimers();
});

describe('LevelMeter (05 §13.7)', () => {
  it('shows the level of the captured track', () => {
    renderMeter({ track: track(), active: true });
    const meter = screen.getByRole('meter', { name: 'Sound level' });
    expect(meter).toHaveValue(0);
    FakeAudioContext.level = 0.25;
    tick(LEVEL_INTERVAL_MS);
    expect(meter).toHaveValue(0.5);
    FakeAudioContext.level = 4; // clipped
    tick(LEVEL_INTERVAL_MS);
    expect(meter).toHaveValue(1);
  });

  it('measures the track without playing it: the analyser is never connected to the output', () => {
    renderMeter({ track: track(), active: true });
    const [ctx] = FakeAudioContext.instances;
    expect(ctx?.sources).toHaveLength(1);
    expect(ctx?.sources[0]?.connect).toHaveBeenCalledOnce();
    expect(ctx?.sources[0]?.connect).not.toHaveBeenCalledWith(ctx?.destination);
  });

  it('"No sound captured" after 10 s of digital silence, gone when sound comes', () => {
    renderMeter({ track: track(), active: true });
    tick(SILENCE_AFTER_MS);
    expect(screen.queryByText('No sound captured')).not.toBeInTheDocument();
    tick(LEVEL_INTERVAL_MS);
    expect(screen.getByText('No sound captured')).toBeInTheDocument();

    FakeAudioContext.level = 0.01;
    tick(LEVEL_INTERVAL_MS);
    expect(screen.queryByText('No sound captured')).not.toBeInTheDocument();
    // Silence has to last 10 s again.
    FakeAudioContext.level = 0;
    tick(SILENCE_AFTER_MS - LEVEL_INTERVAL_MS);
    expect(screen.queryByText('No sound captured')).not.toBeInTheDocument();
  });

  it('with the sound switched off nothing is measured: silence is what was asked for', () => {
    const t = track();
    const { rerender } = renderMeter({ track: t, active: true });
    FakeAudioContext.level = 0.25;
    tick(LEVEL_INTERVAL_MS);
    rerender(
      <I18nextProvider i18n={i18n}>
        <LevelMeter track={t} active={false} />
      </I18nextProvider>,
    );
    const [ctx] = FakeAudioContext.instances;
    expect(ctx?.sources[0]?.disconnect).toHaveBeenCalledOnce();
    expect(screen.getByRole('meter')).toHaveValue(0);
    FakeAudioContext.level = 0;
    tick(SILENCE_AFTER_MS * 2);
    expect(screen.queryByText('No sound captured')).not.toBeInTheDocument();
  });

  it('unmounting disconnects the analyser and suspends the context until the next meter', () => {
    const { unmount } = renderMeter({ track: track(), active: true });
    const [ctx] = FakeAudioContext.instances;
    expect(ctx?.state).toBe('running');
    unmount();
    expect(ctx?.sources[0]?.disconnect).toHaveBeenCalledOnce();
    expect(ctx?.suspend).toHaveBeenCalledOnce();

    renderMeter({ track: track(), active: true });
    expect(FakeAudioContext.instances).toHaveLength(1);
    expect(ctx?.state).toBe('running');
  });

  it('renders nothing for a share without an audio track', () => {
    const { container } = renderMeter({ track: null, active: true });
    expect(container).toBeEmptyDOMElement();
    expect(FakeAudioContext.instances).toHaveLength(0);
  });

  it('without WebAudio the meter stays at 0 and never claims silence', () => {
    vi.stubGlobal('AudioContext', undefined);
    renderMeter({ track: track(), active: true });
    tick(SILENCE_AFTER_MS * 2);
    expect(screen.getByRole('meter')).toHaveValue(0);
    expect(screen.queryByText('No sound captured')).not.toBeInTheDocument();
  });
});

describe('levelAudio', () => {
  it('primeLevelAudio (the Share click) makes one context for the page and resumes it', () => {
    primeLevelAudio();
    primeLevelAudio();
    expect(FakeAudioContext.instances).toHaveLength(1);
    expect(FakeAudioContext.instances[0]?.resume).toHaveBeenCalledOnce();
    expect(FakeAudioContext.instances[0]?.state).toBe('running');
  });

  it('a tap reads the peak, and 0 after it was closed; closing twice is harmless', () => {
    const tap = openLevelTap(track());
    FakeAudioContext.level = 0.5;
    expect(tap?.read()).toBe(0.5);
    tap?.close();
    tap?.close();
    expect(tap?.read()).toBe(0);
    expect(FakeAudioContext.instances[0]?.suspend).toHaveBeenCalledOnce();
  });

  it('no tap where the browser refuses the track', () => {
    primeLevelAudio();
    const [ctx] = FakeAudioContext.instances;
    if (!ctx) throw new Error('no context');
    ctx.createMediaStreamSource = () => {
      throw new DOMException('no audio in this stream', 'InvalidStateError');
    };
    expect(openLevelTap(track())).toBeNull();
  });
});
