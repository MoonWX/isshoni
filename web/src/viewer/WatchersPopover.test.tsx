// WatchersPopover (05 §12.6, §19.2): the tile shows the viewer count and opens the watchers popover with the
// names; nobody watches unseen.
import { act, render, screen, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import type { ReactNode } from 'react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { installFakeMedia, installFakeMediaElement, type FakeMediaElementControl } from '../test/fakeMedia';
import { ViewerContext } from './context';
import { createViewer, syncRoom, type ViewerServices } from './services';
import { Stage } from './Stage';
import { participant, room, SELF, shareInfo } from './testing';
import { Tile } from './Tile';
import type { ViewerShare } from './viewerStore';
import { WatchersPopover } from './WatchersPopover';

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
  vi.restoreAllMocks();
});

/** The store's entry of share s_a watched by these users. */
function watched(...userIds: string[]): ViewerShare {
  const snapshot = room(shareInfo('s_a', 'u_bea', 1, { watchers: userIds.map(watcher) }));
  syncRoom(viewer, { ...snapshot, participants: [...snapshot.participants, participant('u_dee', 'Dee')] }, SELF);
  const share = viewer.store.getState().shares.find((s) => s.id === 's_a');
  if (!share) throw new Error('no tile for s_a');
  return share;
}

const inViewer = (ui: ReactNode) => <ViewerContext.Provider value={viewer}>{ui}</ViewerContext.Provider>;
const eye = () => screen.getByRole('button', { name: /Show who is watching$/ });
const panel = () => screen.queryByRole('dialog', { name: 'Watching now' });

describe('WatchersPopover', () => {
  it('shows the viewer count on the tile and opens the names: the others, then "you"', async () => {
    const share = watched('u_alex', 'u_cy', 'u_dee');
    render(
      inViewer(
        <ul>
          <Tile share={share} onPick={vi.fn()} />
        </ul>,
      ),
    );
    const button = eye();
    expect(button).toHaveAccessibleName('3 watching. Show who is watching');
    expect(button).toHaveTextContent('3');
    expect(button).toHaveAttribute('aria-expanded', 'false');
    expect(button).toHaveAttribute('aria-haspopup', 'dialog');
    expect(panel()).not.toBeInTheDocument();

    await userEvent.click(button);
    const dialog = panel();
    if (!dialog) throw new Error('no popover');
    expect(button).toHaveAttribute('aria-expanded', 'true');
    expect(button).toHaveAttribute('aria-controls', dialog.id);
    expect(dialog).toHaveFocus();
    expect(within(dialog).getByText('Watching')).toBeInTheDocument();
    expect(
      within(dialog)
        .getAllByRole('listitem')
        .map((li) => li.textContent),
    ).toEqual(['Cy', 'Dee', 'you']);
  });

  it('opening it is not a pick of the tile', async () => {
    const onPick = vi.fn();
    render(
      inViewer(
        <ul>
          <Tile share={watched('u_cy')} onPick={onPick} />
        </ul>,
      ),
    );
    await userEvent.click(eye());
    expect(panel()).toBeInTheDocument();
    expect(onPick).not.toHaveBeenCalled();
    expect(screen.getByRole('button', { name: /^Watch Bea/ })).not.toContainElement(eye());
  });

  it('closes on Escape, with the focus back on the eye, and on a second press', async () => {
    render(inViewer(<WatchersPopover share={watched('u_cy')} />));
    await userEvent.click(eye());
    expect(panel()).toBeInTheDocument();
    await userEvent.keyboard('{Escape}');
    expect(panel()).not.toBeInTheDocument();
    expect(eye()).toHaveFocus();

    await userEvent.click(eye());
    expect(panel()).toBeInTheDocument();
    await userEvent.click(eye());
    expect(panel()).not.toBeInTheDocument();
  });

  it('closes on a press outside, when focus moves on, and when the page scrolls or resizes under it', async () => {
    render(
      inViewer(
        <>
          <WatchersPopover share={watched('u_cy')} />
          <button type="button">elsewhere</button>
        </>,
      ),
    );
    const elsewhere = screen.getByRole('button', { name: 'elsewhere' });

    await userEvent.click(eye());
    await userEvent.click(elsewhere);
    expect(panel()).not.toBeInTheDocument();

    await userEvent.click(eye());
    await userEvent.tab(); // from the panel on to the next control
    expect(elsewhere).toHaveFocus();
    expect(panel()).not.toBeInTheDocument();

    await userEvent.click(eye());
    act(() => {
      document.dispatchEvent(new Event('scroll'));
    });
    expect(panel()).not.toBeInTheDocument();

    await userEvent.click(eye());
    act(() => {
      window.dispatchEvent(new Event('resize'));
    });
    expect(panel()).not.toBeInTheDocument();
  });

  it('stays open while its own list scrolls', async () => {
    render(inViewer(<WatchersPopover share={watched('u_cy', 'u_dee')} />));
    await userEvent.click(eye());
    act(() => {
      panel()?.dispatchEvent(new Event('scroll'));
    });
    expect(panel()).toBeInTheDocument();
  });

  it('says so when nobody watches, and names someone the room no longer lists', async () => {
    const { rerender } = render(inViewer(<WatchersPopover share={watched()} />));
    expect(eye()).toHaveAccessibleName('0 watching. Show who is watching');
    await userEvent.click(eye());
    expect(within(panel() ?? document.body).getByText('Nobody is watching yet.')).toBeInTheDocument();

    rerender(inViewer(<WatchersPopover share={watched('u_gone')} />));
    expect(
      within(panel() ?? document.body)
        .getAllByRole('listitem')
        .map((li) => li.textContent),
    ).toEqual(['Someone']);
  });

  it('follows room.state while it is open', async () => {
    const view = () => {
      const share = viewer.store.getState().shares[0];
      if (!share) throw new Error('no share');
      return inViewer(<WatchersPopover share={share} />);
    };
    watched('u_cy');
    const { rerender } = render(view());
    await userEvent.click(eye());
    watched('u_cy', 'u_alex');
    rerender(view());
    expect(eye()).toHaveTextContent('2');
    expect(
      within(panel() ?? document.body)
        .getAllByRole('listitem')
        .map((li) => li.textContent),
    ).toEqual(['Cy', 'you']);
  });

  it('opens above the eye when there is more room there, else below; never past the viewport’s edge', async () => {
    render(inViewer(<WatchersPopover share={watched('u_cy')} />));
    vi.spyOn(document.documentElement, 'clientWidth', 'get').mockReturnValue(1000);
    vi.spyOn(document.documentElement, 'clientHeight', 'get').mockReturnValue(800);
    const rect = (top: number, right: number) =>
      ({ top, bottom: top + 44, left: right - 60, right, width: 60, height: 44, x: right - 60, y: top }) as DOMRect;

    // A tile in the strip at the bottom of the page.
    const spy = vi.spyOn(eye(), 'getBoundingClientRect').mockReturnValue(rect(700, 900));
    await userEvent.click(eye());
    expect(panel()).toHaveStyle({ right: '100px', bottom: '108px', maxHeight: '684px' });
    expect(panel()?.style.top).toBe('');
    await userEvent.click(eye());

    // A tile at the top of the right column, at the page's edge.
    spy.mockReturnValue(rect(60, 998));
    await userEvent.click(eye());
    expect(panel()).toHaveStyle({ right: '8px', top: '112px', maxHeight: '680px' });
    expect(panel()?.style.bottom).toBe('');
  });

  it('is the stage’s "3 watching", too', async () => {
    const share = watched('u_alex', 'u_cy', 'u_dee');
    render(inViewer(<Stage share={share} />));
    const button = eye();
    expect(button).toHaveTextContent('3 watching');
    expect(screen.getByRole('region', { name: "Now watching: Bea's window" })).toContainElement(button);
    await userEvent.click(button);
    expect(within(panel() ?? document.body).getAllByRole('listitem')).toHaveLength(3);
  });
});
