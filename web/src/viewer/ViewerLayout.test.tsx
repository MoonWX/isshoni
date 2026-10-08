// ViewerLayout (05 §12.1): the stage with the focused share, the others newest first, the own share's preview, no
// tile for `starting` shares; a pick holds the stage (05 §12.2); fullscreen and a phone held sideways show only the
// stage; the banner when the media port can't be reached (05 §9).
import { act, render, screen, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
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
import { PHONE_LANDSCAPE_QUERY, ViewerLayout } from './ViewerLayout';

const watcher = (userId: string) => ({ userId, video: 'high' as const, audio: 'on' as const });

const BEA = shareInfo('s_bea', 'u_bea', 1);
const CY = shareInfo('s_cy', 'u_cy', 2, { kind: 'tab' });
const MINE = shareInfo('s_mine', 'u_alex', 3, { connectionId: 'c_me', kind: 'screen', watchers: [watcher('u_bea')] });

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

const sync = (...shares: Parameters<typeof room>): void => {
  act(() => {
    syncRoom(viewer, room(...shares), SELF);
  });
};

/** The accessible names of the tiles' main buttons, in order. */
function tileNames(): string[] {
  const list = screen.queryByRole('list', { name: 'Shares' });
  if (!list) return [];
  return within(list)
    .getAllByRole('listitem')
    .map((li) => within(li).getAllByRole('button')[0]?.getAttribute('aria-label') ?? '');
}

/** Makes window.matchMedia answer `matches` for the phone-landscape query; returns the function that flips it. */
function stubPhoneLandscape(initial: boolean): (matches: boolean) => void {
  let matches = initial;
  const listeners = new Set<() => void>();
  vi.stubGlobal('matchMedia', (query: string) => ({
    media: query,
    get matches() {
      return query === PHONE_LANDSCAPE_QUERY && matches;
    },
    addEventListener: (_type: string, fn: () => void) => listeners.add(fn),
    removeEventListener: (_type: string, fn: () => void) => listeners.delete(fn),
  }));
  return (next) => {
    matches = next;
    act(() => {
      for (const fn of listeners) fn();
    });
  };
}

describe('ViewerLayout: stage and others', () => {
  it('shows the room’s empty state while nobody shares', () => {
    const { rerender } = render(<ViewerLayout viewer={viewer} />);
    expect(within(screen.getByRole('region', { name: 'Stage' })).getByText('Nobody is sharing yet.')).toBeVisible();
    expect(screen.queryByRole('list', { name: 'Shares' })).not.toBeInTheDocument();

    rerender(<ViewerLayout viewer={viewer} empty={<button type="button">Share your screen</button>} />);
    expect(screen.getByRole('button', { name: 'Share your screen' })).toBeInTheDocument();
    expect(screen.queryByText('Nobody is sharing yet.')).not.toBeInTheDocument();
  });

  it('puts the newest share on the stage and the others in a list, newest first', () => {
    syncRoom(viewer, room(BEA, CY, shareInfo('s_dee', 'u_bea', 0, { kind: 'screen' })), SELF);
    render(<ViewerLayout viewer={viewer} />);
    expect(screen.getByRole('region', { name: "Now watching: Cy's tab" })).toBeInTheDocument();
    expect(tileNames()).toEqual(["Watch Bea's window, 0 watching", "Watch Bea's screen, 0 watching"]);
    // One video per share: the stage's and one per tile.
    expect(document.querySelectorAll('video')).toHaveLength(3);
  });

  it('gives no tile to a share that is still starting, and one as soon as it is live', () => {
    syncRoom(viewer, room(BEA, { ...CY, status: 'starting' }), SELF);
    render(<ViewerLayout viewer={viewer} />);
    expect(screen.getByRole('region', { name: "Now watching: Bea's window" })).toBeInTheDocument();
    expect(tileNames()).toEqual([]);

    sync(BEA, CY);
    expect(screen.getByRole('region', { name: "Now watching: Cy's tab" })).toBeInTheDocument();
    expect(tileNames()).toEqual(["Watch Bea's window, 0 watching"]);
  });

  it('lists the own share as a preview tile and never puts it on the stage by itself', () => {
    const preview = new FakeMediaStream([new FakeMediaStreamTrack('video')]) as unknown as MediaStream;
    syncRoom(viewer, room(BEA, MINE), SELF);
    render(<ViewerLayout viewer={viewer} localPreviews={{ s_mine: preview }} />);
    expect(screen.getByRole('region', { name: "Now watching: Bea's window" })).toBeInTheDocument();
    expect(tileNames()).toEqual(['Show your preview on the stage, 1 watching']);
    const tile = screen.getByRole('listitem');
    expect(tile.querySelector('video')?.srcObject).toBe(preview);
  });

  it('says "You’re live" on the stage while only this page shares, and enlarges the preview on a click', async () => {
    const preview = new FakeMediaStream([new FakeMediaStreamTrack('video')]) as unknown as MediaStream;
    syncRoom(viewer, room(MINE), SELF);
    render(<ViewerLayout viewer={viewer} localPreviews={{ s_mine: preview }} empty={<p>custom empty</p>} />);
    const stage = screen.getByRole('region', { name: 'Stage' });
    expect(within(stage).getByText("You're live · 1 watching")).toBeInTheDocument();
    expect(screen.queryByText('custom empty')).not.toBeInTheDocument();

    await userEvent.click(screen.getByRole('button', { name: /^Show your preview/ }));
    const enlarged = screen.getByRole('region', { name: 'Your preview: Your screen' });
    expect(enlarged.querySelector('video')?.srcObject).toBe(preview);
    expect(tileNames()).toEqual([]);
  });

  it('asks for a pick when the only shares are this user’s from another device', () => {
    syncRoom(viewer, room(shareInfo('s_phone', 'u_alex', 1, { connectionId: 'c_phone' })), SELF);
    render(<ViewerLayout viewer={viewer} />);
    expect(within(screen.getByRole('region', { name: 'Stage' })).getByText('Pick a share to watch.')).toBeVisible();
    expect(tileNames()).toEqual(['Watch your share from another device, 0 watching']);
  });

  it('passes the stage controls and the tiles’ extra controls through, after its own', () => {
    syncRoom(viewer, room(BEA, CY), SELF);
    render(
      <ViewerLayout
        viewer={viewer}
        stageControls={<button type="button">Stats</button>}
        tileActions={(share) => <button type="button">{`Pin ${share.ownerName}`}</button>}
      />,
    );
    const controls = within(screen.getByRole('toolbar')).getAllByRole('button');
    expect(controls.map((b) => b.getAttribute('aria-label') ?? b.textContent)).toEqual([
      'Mute',
      'Keyboard shortcuts',
      'Fullscreen',
      'Stats',
    ]);
    expect(screen.getByRole('button', { name: 'Pin Bea' })).toBeInTheDocument();
    expect(screen.queryByRole('button', { name: 'Pin Cy' })).not.toBeInTheDocument();
  });

  it('brings the sound controls and the watchers along: a speaker button and an eye per tile', () => {
    syncRoom(viewer, room(BEA, CY, MINE), SELF);
    render(<ViewerLayout viewer={viewer} />);
    const list = screen.getByRole('list', { name: 'Shares' });
    // The own preview has no sound to listen to; who watches it is shown all the same.
    expect(within(list).getAllByRole('button', { name: /^Listen to/ })).toEqual([
      screen.getByRole('button', { name: 'Listen to Bea' }),
    ]);
    expect(within(list).getAllByRole('button', { name: /Show who is watching$/ })).toHaveLength(2);
    const stage = screen.getByRole('region', { name: "Now watching: Cy's tab" });
    expect(within(stage).getByRole('button', { name: 'Mute' })).toBeInTheDocument();
    expect(within(stage).getByRole('button', { name: '0 watching. Show who is watching' })).toBeInTheDocument();
  });

  it('marks what it shows, so that nothing else is subscribed with video (05 §12.4)', () => {
    syncRoom(viewer, room(BEA, CY), SELF);
    const { unmount } = render(<ViewerLayout viewer={viewer} />);
    expect(viewer.store.getState().visible).toEqual({ s_cy: true, s_bea: true });
    unmount();
    expect(viewer.store.getState().visible).toEqual({ s_cy: false, s_bea: false });
  });
});

describe('ViewerLayout: picking a tile (05 §12.2, owner decision)', () => {
  it('moves the picked share to the stage and keeps it there when a new share starts', async () => {
    syncRoom(viewer, room(BEA, CY), SELF);
    render(<ViewerLayout viewer={viewer} />);
    await userEvent.click(screen.getByRole('button', { name: /^Watch Bea's window/ }));
    expect(screen.getByRole('region', { name: "Now watching: Bea's window" })).toBeInTheDocument();
    expect(tileNames()).toEqual(["Watch Cy's tab, 0 watching"]);
    expect(viewer.store.getState()).toMatchObject({ focusMode: 'manual', audibleShareId: 's_bea' });

    // A newer share: it gets a tile (the room shows a toast), not the stage or the sound.
    sync(BEA, CY, shareInfo('s_new', 'u_cy', 9, { kind: 'screen' }));
    expect(screen.getByRole('region', { name: "Now watching: Bea's window" })).toBeInTheDocument();
    expect(tileNames()).toEqual(["Watch Cy's screen, 0 watching", "Watch Cy's tab, 0 watching"]);
    expect(viewer.store.getState().audibleShareId).toBe('s_bea');
  });

  it('goes back to the newest share when the picked one ends', async () => {
    syncRoom(viewer, room(BEA, CY), SELF);
    render(<ViewerLayout viewer={viewer} />);
    await userEvent.click(screen.getByRole('button', { name: /^Watch Bea's window/ }));
    const newest = shareInfo('s_new', 'u_cy', 9, { kind: 'screen' });
    sync(BEA, CY, newest);
    sync(CY, newest);
    expect(screen.getByRole('region', { name: "Now watching: Cy's screen" })).toBeInTheDocument();
    expect(tileNames()).toEqual(["Watch Cy's tab, 0 watching"]);
    expect(viewer.store.getState()).toMatchObject({ focusMode: 'auto', audibleShareId: 's_new' });
  });

  it('updates a tile’s viewer count from room.state', () => {
    syncRoom(viewer, room(BEA, CY), SELF);
    render(<ViewerLayout viewer={viewer} />);
    sync({ ...BEA, watchers: [watcher('u_alex'), watcher('u_cy')] }, CY);
    expect(tileNames()).toEqual(["Watch Bea's window, 2 watching"]);
  });
});

describe('ViewerLayout: fullscreen and phones', () => {
  it('renders only the stage in fullscreen', () => {
    syncRoom(viewer, room(BEA, CY), SELF);
    render(<ViewerLayout viewer={viewer} />);
    expect(tileNames()).toHaveLength(1);
    act(() => {
      viewer.store.getState().setFullscreen(true);
    });
    expect(screen.queryByRole('list', { name: 'Shares' })).not.toBeInTheDocument();
    expect(screen.getByRole('region', { name: "Now watching: Cy's tab" })).toBeInTheDocument();
    expect(document.querySelectorAll('video')).toHaveLength(1);
    act(() => {
      viewer.store.getState().setFullscreen(false);
    });
    expect(tileNames()).toHaveLength(1);
  });

  it('hides the others on a phone held sideways behind a "Shares (N)" button that opens a sheet', async () => {
    const setLandscape = stubPhoneLandscape(true);
    syncRoom(viewer, room(BEA, CY, MINE), SELF);
    render(<ViewerLayout viewer={viewer} />);
    expect(screen.queryByRole('list', { name: 'Shares' })).not.toBeInTheDocument();
    expect(document.querySelectorAll('video')).toHaveLength(1);

    await userEvent.click(screen.getByRole('button', { name: 'Shares (2)' }));
    const sheet = screen.getByRole('dialog', { name: 'Shares' });
    expect(sheet).toHaveAttribute('open');
    expect(tileNames()).toEqual(['Show your preview on the stage, 1 watching', "Watch Bea's window, 0 watching"]);

    // A pick focuses the share and closes the sheet.
    await userEvent.click(within(sheet).getByRole('button', { name: /^Watch Bea's window/ }));
    expect(screen.getByRole('region', { name: "Now watching: Bea's window" })).toBeInTheDocument();
    expect(sheet).not.toHaveAttribute('open');
    expect(screen.queryByRole('list', { name: 'Shares' })).not.toBeInTheDocument();

    // Upright again: the strip is back, the button is gone.
    setLandscape(false);
    expect(screen.queryByRole('button', { name: /^Shares \(/ })).not.toBeInTheDocument();
    expect(tileNames()).toEqual(['Show your preview on the stage, 1 watching', "Watch Cy's tab, 0 watching"]);
  });

  it('closes the sheet when the phone is turned upright and keeps it closed when it is turned back', async () => {
    const setLandscape = stubPhoneLandscape(true);
    syncRoom(viewer, room(BEA, CY), SELF);
    render(<ViewerLayout viewer={viewer} />);
    await userEvent.click(screen.getByRole('button', { name: 'Shares (1)' }));
    expect(screen.getByRole('dialog', { name: 'Shares' })).toHaveAttribute('open');
    setLandscape(false);
    setLandscape(true);
    expect(screen.getByRole('button', { name: 'Shares (1)' })).toBeInTheDocument();
    expect(document.querySelector('dialog')).not.toHaveAttribute('open');
    expect(screen.queryByRole('list', { name: 'Shares' })).not.toBeInTheDocument();
  });

  it('shows no "Shares" button without other shares', () => {
    stubPhoneLandscape(true);
    syncRoom(viewer, room(BEA), SELF);
    render(<ViewerLayout viewer={viewer} />);
    expect(screen.queryByRole('button', { name: /^Shares \(/ })).not.toBeInTheDocument();
  });
});

describe('ViewerLayout: the media connection (05 §9)', () => {
  it('shows a banner with "Test my connection" when the media port can’t be reached', async () => {
    const onTest = vi.fn();
    syncRoom(viewer, room(BEA), SELF);
    render(<ViewerLayout viewer={viewer} onTestConnection={onTest} />);
    expect(screen.queryByText(/media port/)).not.toBeInTheDocument();

    for (const state of ['connecting', 'connected', 'reconnecting'] as const) {
      act(() => {
        viewer.store.getState().setMedia(state);
      });
      expect(screen.queryByText(/media port/)).not.toBeInTheDocument();
    }
    act(() => {
      viewer.store.getState().setMedia('unreachable');
    });
    expect(screen.getByText("Can't reach the server's media port. isshoni keeps trying.")).toBeInTheDocument();
    await userEvent.click(screen.getByRole('button', { name: 'Test my connection' }));
    expect(onTest).toHaveBeenCalledOnce();

    act(() => {
      viewer.store.getState().setMedia('connected');
    });
    expect(screen.queryByText(/media port/)).not.toBeInTheDocument();
  });

  it('shows the banner without a button when there is no connection test to open', () => {
    viewer.store.getState().setMedia('unreachable');
    render(<ViewerLayout viewer={viewer} />);
    expect(screen.getByText(/media port/)).toBeInTheDocument();
    expect(screen.queryByRole('button', { name: 'Test my connection' })).not.toBeInTheDocument();
  });
});
