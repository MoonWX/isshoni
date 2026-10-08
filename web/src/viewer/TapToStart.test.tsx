// TapToStart (05 §10.3, §19.2): it appears when play() rejects and disappears after a click; its one handler
// calls audio.play() synchronously and retries every tile video that was refused; and the tap is not a pick.
import { act, fireEvent, render, screen } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import {
  FakeMediaStreamTrack,
  installFakeMedia,
  installFakeMediaElement,
  type FakeMediaElementControl,
} from '../test/fakeMedia';
import { createViewer, syncRoom, type ViewerServices } from './services';
import { TapToStart, TapToStartPill } from './TapToStart';
import { room, SELF, shareInfo } from './testing';
import { ViewerLayout } from './ViewerLayout';

const track = (kind: 'audio' | 'video') => new FakeMediaStreamTrack(kind) as unknown as MediaStreamTrack;
/** Lets the promises of play() settle, and React render what they changed. */
const settle = () => act(() => Promise.resolve().then(() => Promise.resolve()));

const BEA = shareInfo('s_bea', 'u_bea', 1);
const CY = shareInfo('s_cy', 'u_cy', 2);

let media: FakeMediaElementControl;
let viewer: ViewerServices;

beforeEach(() => {
  installFakeMedia();
  media = installFakeMediaElement();
  viewer = createViewer();
});

afterEach(() => {
  viewer.dispose();
  media.restore();
  vi.unstubAllGlobals();
});

const state = () => viewer.store.getState();
const sync = (...shares: Parameters<typeof room>): void => {
  act(() => {
    syncRoom(viewer, room(...shares), SELF);
  });
};
/** Gives a share its tracks, as the sub PC does. */
const receive = (shareId: string, ...kinds: ('audio' | 'video')[]): void => {
  act(() => {
    for (const kind of kinds) viewer.registry.set(shareId, kind, track(kind));
  });
};
const tap = () => screen.queryByRole('button', { name: /^Tap to/ });

describe('TapToStart', () => {
  it('appears when the audio’s play() rejects and disappears after a click', async () => {
    render(<ViewerLayout viewer={viewer} />);
    sync(BEA);
    receive('s_bea', 'video');
    await settle();
    expect(tap()).not.toBeInTheDocument();

    // iOS Safari: sound needs a gesture.
    media.policy = 'block';
    receive('s_bea', 'audio');
    await settle();
    expect(state().audio).toBe('blocked');
    const button = screen.getByRole('button', { name: 'Tap to unmute' });
    // Over the stage, where the video is.
    expect(screen.getByRole('region', { name: "Now watching: Bea's window" })).toContainElement(button);

    media.policy = 'allow'; // a click is a user gesture
    fireEvent.click(button);
    await settle();
    expect(state().audio).toBe('playing');
    expect(tap()).not.toBeInTheDocument();
    expect(viewer.audio.element?.paused).toBe(false);
  });

  it('calls audio.play() and play() on every refused video synchronously, inside the gesture', async () => {
    media.policy = 'block'; // iOS Low Power Mode: even muted video is refused
    render(<ViewerLayout viewer={viewer} />);
    sync(BEA, CY);
    receive('s_bea', 'video', 'audio');
    receive('s_cy', 'video', 'audio');
    await settle();
    expect(state()).toMatchObject({ audio: 'blocked', videoBlocked: true });
    const videos = [...document.querySelectorAll('video')];
    expect(videos).toHaveLength(2);
    expect(videos.every((v) => v.paused)).toBe(true);

    media.policy = 'allow';
    const before = media.played.length;
    fireEvent.click(screen.getByRole('button', { name: 'Tap to unmute' }));
    // No await: everything was started before the click handler returned.
    const started = media.played.slice(before);
    expect(started[0]).toBe(viewer.audio.element);
    expect(new Set(started)).toEqual(new Set([viewer.audio.element, ...videos]));
    await settle();
    expect(state()).toMatchObject({ audio: 'playing', videoBlocked: false });
    expect(videos.every((v) => !v.paused)).toBe(true);
    expect(tap()).not.toBeInTheDocument();
  });

  it('says "Tap to start video" when only the video is refused', async () => {
    media.policy = 'block';
    render(<ViewerLayout viewer={viewer} />);
    sync(BEA);
    receive('s_bea', 'video'); // a share without sound
    await settle();
    expect(state()).toMatchObject({ audio: 'locked', videoBlocked: true });
    const button = screen.getByRole('button', { name: 'Tap to start video' });

    media.policy = 'allow';
    fireEvent.click(button);
    await settle();
    expect(state().videoBlocked).toBe(false);
    expect(tap()).not.toBeInTheDocument();
  });

  it('stays while the browser still refuses', async () => {
    media.policy = 'block';
    render(<ViewerLayout viewer={viewer} />);
    sync(BEA);
    receive('s_bea', 'video', 'audio');
    await settle();
    fireEvent.click(screen.getByRole('button', { name: 'Tap to unmute' }));
    await settle();
    expect(screen.getByRole('button', { name: 'Tap to unmute' })).toBeInTheDocument();
    expect(state()).toMatchObject({ audio: 'blocked', videoBlocked: true });
  });

  it('is not a pick: auto-focus goes on after the unmute tap (05 §12.2)', async () => {
    media.policy = 'block';
    render(<ViewerLayout viewer={viewer} />);
    sync(BEA);
    receive('s_bea', 'video', 'audio');
    await settle();
    media.policy = 'allow';
    fireEvent.click(screen.getByRole('button', { name: 'Tap to unmute' }));
    await settle();
    expect(state()).toMatchObject({ focusMode: 'auto', focusedShareId: 's_bea' });

    sync(BEA, CY);
    expect(state()).toMatchObject({ focusMode: 'auto', focusedShareId: 's_cy', audibleShareId: 's_cy' });
  });

  it('goes away when the user mutes instead, and when nothing is left to unmute', async () => {
    media.policy = 'block';
    render(<ViewerLayout viewer={viewer} />);
    sync(BEA);
    media.policy = 'allow';
    receive('s_bea', 'video');
    media.policy = 'block';
    receive('s_bea', 'audio');
    await settle();
    expect(tap()).toBeInTheDocument();

    fireEvent.click(screen.getByRole('button', { name: 'Mute' }));
    expect(state().audio).toBe('muted');
    expect(tap()).not.toBeInTheDocument();

    // Unmuting is a gesture too, but this browser refuses it: the tap is asked for again.
    fireEvent.click(screen.getByRole('button', { name: 'Unmute' }));
    await settle();
    expect(tap()).toBeInTheDocument();

    sync(); // the share ended
    await settle();
    expect(state().audio).toBe('locked');
    expect(tap()).not.toBeInTheDocument();
  });

  it('covers an empty stage too: a share can be heard through its speaker button', async () => {
    // Only this user's share from another device: nothing is focused by itself.
    const other = shareInfo('s_other', 'u_alex', 1, { connectionId: 'c_laptop' });
    render(<ViewerLayout viewer={viewer} />);
    sync(other);
    receive('s_other', 'video');
    media.policy = 'block';
    receive('s_other', 'audio');
    fireEvent.click(screen.getByRole('button', { name: 'Listen to your share' }));
    await settle();
    expect(screen.getByRole('region', { name: 'Stage' })).toContainElement(
      screen.getByRole('button', { name: 'Tap to unmute' }),
    );
  });

  it('renders nothing outside a viewer', () => {
    const { container } = render(<TapToStart />);
    expect(container).toBeEmptyDOMElement();
  });
});

describe('TapToStartPill (the room header)', () => {
  it('is the same tap as a pill, outside the layout', async () => {
    render(
      <>
        <header>
          <TapToStartPill viewer={viewer} />
        </header>
        <ViewerLayout viewer={viewer} />
      </>,
    );
    sync(BEA);
    receive('s_bea', 'video');
    await settle();
    expect(screen.getByRole('banner')).toBeEmptyDOMElement();

    media.policy = 'block';
    receive('s_bea', 'audio');
    await settle();
    const buttons = screen.getAllByRole('button', { name: 'Tap to unmute' });
    expect(buttons).toHaveLength(2);
    const pill = buttons.find((b) => screen.getByRole('banner').contains(b));
    if (!pill) throw new Error('no pill in the header');

    media.policy = 'allow';
    fireEvent.click(pill);
    await settle();
    expect(state().audio).toBe('playing');
    expect(tap()).not.toBeInTheDocument();
    expect(screen.getByRole('banner')).toBeEmptyDOMElement();
  });

  it('says "Tap to start video" for refused video', async () => {
    media.policy = 'block';
    render(
      <>
        <TapToStartPill viewer={viewer} />
        <ViewerLayout viewer={viewer} />
      </>,
    );
    sync(BEA);
    receive('s_bea', 'video');
    await settle();
    expect(screen.getAllByRole('button', { name: 'Tap to start video' })).toHaveLength(2);
  });
});
