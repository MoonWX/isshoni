// The sound controls (05 §10.3, §12.3): a tile's speaker button moves the audio without moving the focus, the
// stage then shows the muted-speaker indicator, exactly one share is audible, and the stage has mute and volume.
import { act, fireEvent, render, screen, within } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import {
  FakeMediaStream,
  FakeMediaStreamTrack,
  installFakeMedia,
  installFakeMediaElement,
  type FakeMediaElementControl,
} from '../test/fakeMedia';
import { createViewer, syncRoom, type ViewerServices } from './services';
import { room, SELF, shareInfo } from './testing';
import { ViewerLayout } from './ViewerLayout';

const track = (kind: 'audio' | 'video') => new FakeMediaStreamTrack(kind) as unknown as MediaStreamTrack;
const settle = () => act(() => Promise.resolve().then(() => Promise.resolve()));

const BEA = shareInfo('s_bea', 'u_bea', 1);
const CY = shareInfo('s_cy', 'u_cy', 2);

let media: FakeMediaElementControl;
let viewer: ViewerServices;
let onVolume: ReturnType<typeof vi.fn<(volume: number) => void>>;
const audio: Record<string, MediaStreamTrack> = {};

beforeEach(() => {
  installFakeMedia();
  media = installFakeMediaElement();
  onVolume = vi.fn();
  viewer = createViewer({ onVolume });
});

afterEach(() => {
  viewer.dispose();
  media.restore();
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
});

const state = () => viewer.store.getState();
/** A room.state with these shares, each with a video and an audio track as the sub PC delivers them. */
const watch = (...shares: Parameters<typeof room>): void => {
  act(() => {
    syncRoom(viewer, room(...shares), SELF);
    for (const s of shares) {
      if (s.connectionId === SELF.connectionId) continue;
      const received = track('audio');
      audio[s.id] = received;
      viewer.registry.set(s.id, 'video', track('video'));
      viewer.registry.set(s.id, 'audio', received);
    }
  });
};
/** The audio tracks the page's one <audio> element plays. */
const heard = () => (viewer.audio.element?.srcObject as unknown as FakeMediaStream | null)?.getTracks() ?? [];
const stage = () => screen.getByRole('region', { name: /^Now watching|^Your preview/ });
const toolbar = () => within(stage()).getByRole('toolbar', { name: 'Stage controls' });

describe('a tile’s speaker button (05 §12.3)', () => {
  it('moves the audio to that tile without moving the video focus; exactly one share is heard', async () => {
    render(<ViewerLayout viewer={viewer} />);
    watch(BEA, CY);
    await settle();
    expect(heard()).toEqual([audio['s_cy']]); // the focused share by default

    const listen = screen.getByRole('button', { name: 'Listen to Bea' });
    expect(listen).toHaveAttribute('aria-pressed', 'false');
    expect(within(stage()).queryByRole('button', { name: /^Listen to/ })).not.toBeInTheDocument();

    fireEvent.click(listen);
    await settle();
    expect(state()).toMatchObject({ audibleShareId: 's_bea', focusedShareId: 's_cy', focusMode: 'auto' });
    expect(stage()).toHaveAccessibleName("Now watching: Cy's window");
    expect(heard()).toEqual([audio['s_bea']]);
    expect(document.querySelectorAll('audio')).toHaveLength(1);
    expect(listen).toHaveAttribute('aria-pressed', 'true');
    expect(state().audio).toBe('playing');
  });

  it('goes back to the stage’s share when pressed again', async () => {
    render(<ViewerLayout viewer={viewer} />);
    watch(BEA, CY);
    const listen = screen.getByRole('button', { name: 'Listen to Bea' });
    fireEvent.click(listen);
    fireEvent.click(listen);
    await settle();
    expect(state().audibleShareId).toBe('s_cy');
    expect(heard()).toEqual([audio['s_cy']]);
    expect(listen).toHaveAttribute('aria-pressed', 'false');
  });

  it('is there for every other tile that has sound, not for the own preview or a silent share', () => {
    const silent = shareInfo('s_dee', 'u_dee', 3, { audio: false });
    const mine = shareInfo('s_mine', 'u_alex', 4, { connectionId: 'c_me' });
    const laptop = shareInfo('s_laptop', 'u_alex', 5, { connectionId: 'c_laptop' });
    render(<ViewerLayout viewer={viewer} />);
    watch(BEA, CY, silent, mine, laptop);
    // Dee's share is the newest of another person: it is on the stage.
    expect(state().focusedShareId).toBe('s_dee');
    const names = screen.getAllByRole('button', { name: /^Listen to/ }).map((b) => b.getAttribute('aria-label'));
    expect(names).toEqual(['Listen to your share', 'Listen to Cy', 'Listen to Bea']);
  });

  it('lets the user hear their own share from another device, which focus never does', async () => {
    const laptop = shareInfo('s_laptop', 'u_alex', 3, { connectionId: 'c_laptop' });
    render(<ViewerLayout viewer={viewer} />);
    watch(BEA, laptop);
    fireEvent.click(screen.getByRole('button', { name: /^Watch your share from another device/ }));
    expect(state()).toMatchObject({ focusedShareId: 's_laptop', audibleShareId: 's_bea' });

    fireEvent.click(screen.getByRole('button', { name: 'Listen to Bea' })); // pressed: off
    expect(state().audibleShareId).toBeNull();
    await settle();
    expect(heard()).toEqual([]);
  });
});

describe('the stage’s sound controls (05 §10.3, §12.3)', () => {
  it('shows the muted-speaker indicator while the sound is on another tile; it brings the sound back', async () => {
    render(<ViewerLayout viewer={viewer} />);
    watch(BEA, CY);
    expect(within(toolbar()).queryByRole('button', { name: /^Listen here/ })).not.toBeInTheDocument();

    fireEvent.click(screen.getByRole('button', { name: 'Listen to Bea' }));
    const here = within(toolbar()).getByRole('button', {
      name: "Listen here. You're listening to Bea now.",
    });
    expect(here).toHaveTextContent('Listen here');

    fireEvent.click(here);
    await settle();
    expect(state()).toMatchObject({ audibleShareId: 's_cy', focusedShareId: 's_cy', focusMode: 'auto' });
    expect(heard()).toEqual([audio['s_cy']]);
    expect(within(toolbar()).queryByRole('button', { name: /^Listen here/ })).not.toBeInTheDocument();
  });

  it('offers the sound of a share that got the stage without it', () => {
    const laptop = shareInfo('s_laptop', 'u_alex', 3, { connectionId: 'c_laptop' });
    render(<ViewerLayout viewer={viewer} />);
    watch(laptop);
    fireEvent.click(screen.getByRole('button', { name: /^Watch your share from another device/ }));
    expect(state()).toMatchObject({ focusedShareId: 's_laptop', audibleShareId: null });
    fireEvent.click(within(toolbar()).getByRole('button', { name: "Listen here. This share's sound is off." }));
    expect(state().audibleShareId).toBe('s_laptop');
  });

  it('mutes and unmutes the one audio element', async () => {
    render(<ViewerLayout viewer={viewer} />);
    watch(BEA);
    await settle();
    const mute = within(toolbar()).getByRole('button', { name: 'Mute' });
    expect(mute).toHaveAttribute('aria-pressed', 'false');

    fireEvent.click(mute);
    expect(state().audio).toBe('muted');
    expect(viewer.audio.element?.muted).toBe(true);
    const unmute = within(toolbar()).getByRole('button', { name: 'Unmute' });
    expect(unmute).toBe(mute);
    expect(unmute).toHaveAttribute('aria-pressed', 'true');

    // Still muted when the focus, and with it the sound, moves on.
    watch(BEA, CY);
    await settle();
    expect(heard()).toEqual([audio['s_cy']]);
    expect(state().audio).toBe('muted');

    fireEvent.click(within(toolbar()).getByRole('button', { name: 'Unmute' }));
    expect(state().audio).toBe('playing');
    expect(viewer.audio.element?.muted).toBe(false);
  });

  it('has a volume slider that sets the element’s volume and reports it for the preferences', async () => {
    render(<ViewerLayout viewer={viewer} />);
    watch(BEA);
    await settle();
    const slider = within(toolbar()).getByRole('slider', { name: 'Volume' });
    expect(slider).toHaveValue('1');
    expect(slider).toHaveAttribute('aria-valuetext', '100%');

    fireEvent.change(slider, { target: { value: '0.5' } });
    expect(state().volume).toBe(0.5);
    expect(viewer.audio.element?.volume).toBe(0.5);
    expect(onVolume).toHaveBeenLastCalledWith(0.5);
    expect(slider).toHaveAttribute('aria-valuetext', '50%');
  });

  it('hides the slider where the browser ignores `volume` (iOS)', async () => {
    const real = Object.getOwnPropertyDescriptor(HTMLMediaElement.prototype, 'volume');
    Object.defineProperty(HTMLMediaElement.prototype, 'volume', {
      configurable: true,
      get: () => 1,
      set: () => undefined,
    });
    try {
      render(<ViewerLayout viewer={viewer} />);
      watch(BEA);
      await settle();
      expect(within(toolbar()).getByRole('button', { name: 'Mute' })).toBeInTheDocument();
      expect(screen.queryByRole('slider')).not.toBeInTheDocument();
    } finally {
      if (real) Object.defineProperty(HTMLMediaElement.prototype, 'volume', real);
    }
  });

  it('has no sound controls for the own preview while nothing is heard', () => {
    const mine = shareInfo('s_mine', 'u_alex', 1, { connectionId: 'c_me' });
    render(<ViewerLayout viewer={viewer} />);
    watch(mine);
    fireEvent.click(screen.getByRole('button', { name: /^Show your preview on the stage/ }));
    expect(stage()).toHaveAccessibleName('Your preview: Your window');
    expect(within(stage()).queryByRole('toolbar')).not.toBeInTheDocument();
  });

  it('keeps mute on the stage while the preview is enlarged and another share is heard', () => {
    const mine = shareInfo('s_mine', 'u_alex', 3, { connectionId: 'c_me' });
    render(<ViewerLayout viewer={viewer} />);
    watch(BEA, mine);
    fireEvent.click(screen.getByRole('button', { name: /^Show your preview on the stage/ }));
    expect(state()).toMatchObject({ focusedShareId: 's_mine', audibleShareId: 's_bea' });
    expect(within(toolbar()).getByRole('button', { name: 'Mute' })).toBeInTheDocument();
    // The preview has no sound of its own to switch to.
    expect(within(toolbar()).queryByRole('button', { name: /^Listen here/ })).not.toBeInTheDocument();
  });
});
