// Tile (05 §10.2, §12.6, §16.6): one muted video with only that share's video track, a button named after the
// share and its watchers, the viewer count (the eye, which opens the watchers popover), the speaker button, and
// the share's state.
import { act, render, screen, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import type { ReactNode } from 'react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import {
  FakeMediaStream,
  FakeMediaStreamTrack,
  installFakeMedia,
  installFakeMediaElement,
  type FakeMediaElementControl,
} from '../test/fakeMedia';
import { ViewerContext } from './context';
import { createViewer, syncRoom, type ViewerServices } from './services';
import { room, SELF, shareInfo, status } from './testing';
import { Tile } from './Tile';
import type { ViewerShare } from './viewerStore';

const track = (kind: 'audio' | 'video') => new FakeMediaStreamTrack(kind) as unknown as MediaStreamTrack;
const watcher = (userId: string) => ({ userId, video: 'low' as const, audio: 'off' as const });

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

/** The store's entry of a share, after a room.state with the given shares. */
function synced(id: string, ...shares: Parameters<typeof room>): ViewerShare {
  syncRoom(viewer, room(...shares), SELF);
  const share = viewer.store.getState().shares.find((s) => s.id === id);
  if (!share) throw new Error(`no tile for ${id}`);
  return share;
}

function renderTile(ui: ReactNode) {
  return render(
    <ViewerContext.Provider value={viewer}>
      <ul>{ui}</ul>
    </ViewerContext.Provider>,
  );
}

const video = (): HTMLVideoElement => {
  const el = document.querySelector('video');
  if (!el) throw new Error('no <video>');
  return el;
};

/** The tile's main area: the button that picks its share. */
const mainButton = (): HTMLElement => screen.getByRole('button', { name: /^(Watch|Show your preview)/ });

describe('Tile', () => {
  it('is a button named after the share and its watchers, with the sharer, the kind and the viewer count', () => {
    const share = synced(
      's_a',
      shareInfo('s_a', 'u_bea', 1, { watchers: [watcher('u_alex'), watcher('u_cy'), watcher('u_dee')] }),
    );
    renderTile(<Tile share={share} onPick={vi.fn()} />);
    const item = screen.getByRole('listitem');
    const button = within(item).getByRole('button', { name: "Watch Bea's window, 3 watching" });
    expect(within(button).getByText('Bea')).toBeInTheDocument();
    expect(within(button).getByText('Window')).toBeInTheDocument();
    // Next to the main button, never inside it: the eye with the count, then the speaker button.
    const eye = within(item).getByRole('button', { name: '3 watching. Show who is watching' });
    const speaker = within(item).getByRole('button', { name: 'Listen to Bea' });
    expect(within(item).getAllByRole('button')).toEqual([button, eye, speaker]);
    expect(eye).toHaveTextContent('3');
    expect(button).not.toContainElement(eye);
    expect(button).not.toContainElement(speaker);
  });

  it('counts as shown while it is mounted (the layer policy’s `visible`, 05 §12.4)', () => {
    const share = synced('s_a', shareInfo('s_a', 'u_bea', 1));
    expect(viewer.store.getState().visible).toEqual({});
    const { unmount } = renderTile(<Tile share={share} onPick={vi.fn()} />);
    expect(viewer.store.getState().visible).toEqual({ s_a: true });
    unmount();
    expect(viewer.store.getState().visible).toEqual({ s_a: false });
  });

  it('names a labeled share by its label and a single watcher in the singular form', () => {
    const share = synced(
      's_a',
      shareInfo('s_a', 'u_bea', 1, { kind: 'screen', label: 'Movie night', watchers: [watcher('u_alex')] }),
    );
    renderTile(<Tile share={share} onPick={vi.fn()} />);
    expect(screen.getByRole('button', { name: 'Watch Bea: Movie night, 1 watching' })).toBeInTheDocument();
    expect(screen.getByText('Movie night')).toBeInTheDocument();
  });

  it('plays only that share’s video track: muted, inline, autoplaying, never cast', () => {
    const share = synced('s_a', shareInfo('s_a', 'u_bea', 1));
    const v = track('video');
    viewer.registry.set('s_a', 'video', v);
    viewer.registry.set('s_a', 'audio', track('audio'));
    viewer.registry.set('s_other', 'video', track('video'));
    renderTile(<Tile share={share} onPick={vi.fn()} />);

    const el = video();
    expect(el.muted).toBe(true);
    expect(el.autoplay).toBe(true);
    expect(el).toHaveAttribute('playsinline');
    expect(el).toHaveAttribute('disableremoteplayback');
    expect(el).not.toHaveAttribute('controls');
    const stream = el.srcObject as unknown as FakeMediaStream;
    expect(stream).toBeInstanceOf(FakeMediaStream);
    expect(stream.getTracks()).toEqual([v]);
  });

  it('follows the registry: connecting until the track arrives, a new stream when it is replaced', () => {
    const share = synced('s_a', shareInfo('s_a', 'u_bea', 1));
    renderTile(<Tile share={share} onPick={vi.fn()} />);
    expect(video().srcObject).toBeNull();
    expect(screen.getByText('Connecting…')).toBeInTheDocument();
    expect(mainButton()).toHaveAccessibleDescription('Connecting…');

    const first = track('video');
    act(() => {
      viewer.registry.set('s_a', 'video', first);
    });
    expect(screen.queryByText('Connecting…')).not.toBeInTheDocument();
    expect(mainButton()).not.toHaveAttribute('aria-describedby');
    const stream = video().srcObject as unknown as FakeMediaStream;
    expect(stream.getTracks()).toEqual([first]);

    // The SFU reused the transceiver, or the sub PC was rebuilt.
    const second = track('video');
    act(() => {
      viewer.registry.set('s_a', 'video', second);
    });
    const next = video().srcObject as unknown as FakeMediaStream;
    expect(next).not.toBe(stream);
    expect(next.getTracks()).toEqual([second]);

    act(() => {
      viewer.registry.delete('s_a');
    });
    expect(video().srcObject).toBeNull();
    expect(screen.getByText('Connecting…')).toBeInTheDocument();
  });

  it('lets go of the stream when it unmounts', () => {
    const share = synced('s_a', shareInfo('s_a', 'u_bea', 1));
    viewer.registry.set('s_a', 'video', track('video'));
    const { unmount } = renderTile(<Tile share={share} onPick={vi.fn()} />);
    const el = video();
    expect(el.srcObject).not.toBeNull();
    unmount();
    expect(el.srcObject).toBeNull();
  });

  it('picks its share on a click and from the keyboard', async () => {
    const share = synced('s_a', shareInfo('s_a', 'u_bea', 1));
    const onPick = vi.fn();
    renderTile(<Tile share={share} onPick={onPick} />);
    await userEvent.click(mainButton());
    expect(onPick).toHaveBeenLastCalledWith('s_a');
    mainButton().focus();
    await userEvent.keyboard('{Enter}');
    await userEvent.keyboard(' ');
    expect(onPick).toHaveBeenCalledTimes(3);
  });

  it('shows what the server reports for the subscription (05 §10.4)', () => {
    const share = synced('s_a', shareInfo('s_a', 'u_bea', 1));
    viewer.registry.set('s_a', 'video', track('video'));
    renderTile(<Tile share={share} onPick={vi.fn()} />);
    const report = (reason?: string) => {
      act(() => {
        viewer.store.getState().applyStatus([status('s_a', reason ? { video: 'low', reason: reason as never } : {})]);
      });
    };
    expect(screen.queryByText('Connecting…')).not.toBeInTheDocument();

    report('waiting');
    expect(screen.getByText('Connecting…')).toBeInTheDocument();
    report('bandwidth');
    expect(screen.queryByText('Connecting…')).not.toBeInTheDocument();
    expect(screen.getByText('Lower quality (your connection)')).toBeInTheDocument();
    expect(mainButton()).toHaveAccessibleDescription('Lower quality (your connection)');
    report('unavailable');
    expect(screen.getByText("The sharer isn't sending this quality right now")).toBeInTheDocument();
    report('codec');
    expect(screen.getByText(/still getting its video decoder/)).toBeInTheDocument();
    report('something-new');
    expect(screen.getByText('Lower quality')).toBeInTheDocument();
    report();
    expect(screen.queryByText(/quality/)).not.toBeInTheDocument();
  });

  it('says "Connection unstable" over a stalled share', () => {
    const share = synced('s_a', shareInfo('s_a', 'u_bea', 1, { status: 'stalled' }));
    viewer.registry.set('s_a', 'video', track('video'));
    renderTile(<Tile share={share} onPick={vi.fn()} />);
    expect(screen.getByText('Connection unstable')).toBeInTheDocument();
    // The last frame stays: the video keeps its stream.
    expect(video().srcObject).not.toBeNull();
  });

  it('shows the local preview for a share this page publishes', () => {
    const share = synced(
      's_mine',
      shareInfo('s_mine', 'u_alex', 1, { connectionId: 'c_me', watchers: [watcher('u_bea'), watcher('u_cy')] }),
    );
    const preview = new FakeMediaStream([new FakeMediaStreamTrack('video')]) as unknown as MediaStream;
    // A track in the registry under its id (there is none in practice) would not be used.
    viewer.registry.set('s_mine', 'video', track('video'));
    renderTile(<Tile share={share} preview={preview} onPick={vi.fn()} />);
    expect(screen.getByRole('button', { name: 'Show your preview on the stage, 2 watching' })).toBeInTheDocument();
    expect(screen.getByText('You')).toBeInTheDocument();
    expect(video().srcObject).toBe(preview);
    expect(screen.queryByText('Connecting…')).not.toBeInTheDocument();
  });

  it('puts extra controls next to the button, never inside it', () => {
    const share = synced('s_a', shareInfo('s_a', 'u_bea', 1));
    renderTile(<Tile share={share} onPick={vi.fn()} actions={<button type="button">More</button>} />);
    const main = screen.getByRole('button', { name: /^Watch/ });
    const extra = screen.getByRole('button', { name: 'More' });
    expect(main).not.toContainElement(extra);
    expect(screen.getByRole('listitem')).toContainElement(extra);
    // After the tile's own controls.
    expect(within(screen.getByRole('listitem')).getAllByRole('button').at(-1)).toBe(extra);
  });
});
