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

  it('stays inside the viewport’s start edge on a phone: the first tile of the strip', async () => {
    render(inViewer(<WatchersPopover share={watched('u_cy')} />));
    vi.spyOn(document.documentElement, 'clientWidth', 'get').mockReturnValue(375);
    vi.spyOn(document.documentElement, 'clientHeight', 'get').mockReturnValue(667);
    const rect = (right: number) =>
      ({ top: 560, bottom: 604, left: right - 44, right, width: 44, height: 44, x: right - 44, y: 560 }) as DOMRect;

    // The eye ends 129px in: the panel's 10rem don't fit before it, so it starts at the viewport's edge.
    const spy = vi.spyOn(eye(), 'getBoundingClientRect').mockReturnValue(rect(129));
    await userEvent.click(eye());
    expect(panel()).toHaveStyle({ left: '8px', bottom: '115px' });
    expect(panel()?.style.right).toBe('');
    expect(panel()?.style.maxWidth).toBe(''); // the stylesheet's: min(20rem, the viewport less the gaps)
    await userEvent.click(eye());

    // 200px in they fit: end-aligned again, and no wider than the room up to the start edge.
    spy.mockReturnValue(rect(200));
    await userEvent.click(eye());
    expect(panel()).toHaveStyle({ right: '175px', maxWidth: '192px' });
    expect(panel()?.style.left).toBe('');
    await userEvent.click(eye());

    // Exactly 10rem of room is enough.
    spy.mockReturnValue(rect(168));
    await userEvent.click(eye());
    expect(panel()).toHaveStyle({ right: '207px', maxWidth: '160px' });
    await userEvent.click(eye());
    spy.mockReturnValue(rect(167));
    await userEvent.click(eye());
    expect(panel()).toHaveStyle({ left: '8px' });
    expect(panel()?.style.right).toBe('');
  });

  it('is at most 20rem wide where there is room, and follows the page’s font size', async () => {
    render(inViewer(<WatchersPopover share={watched('u_cy')} />));
    vi.spyOn(document.documentElement, 'clientWidth', 'get').mockReturnValue(1000);
    vi.spyOn(document.documentElement, 'clientHeight', 'get').mockReturnValue(800);
    const rect = (right: number) =>
      ({ top: 700, bottom: 744, left: right - 44, right, width: 44, height: 44, x: right - 44, y: 700 }) as DOMRect;
    const spy = vi.spyOn(eye(), 'getBoundingClientRect').mockReturnValue(rect(900));
    await userEvent.click(eye());
    expect(panel()).toHaveStyle({ right: '100px', maxWidth: '320px' });
    await userEvent.click(eye());

    // A reader with a 20px root font: 10rem are 200px, so 192px of room are not enough anymore.
    document.documentElement.style.fontSize = '20px';
    try {
      spy.mockReturnValue(rect(200));
      await userEvent.click(eye());
      expect(panel()).toHaveStyle({ left: '8px' });
      expect(panel()?.style.right).toBe('');
      await userEvent.click(eye());

      spy.mockReturnValue(rect(900));
      await userEvent.click(eye());
      expect(panel()).toHaveStyle({ right: '100px', maxWidth: '400px' });
    } finally {
      document.documentElement.style.fontSize = '';
    }
  });

  it('keeps its Escape from a modal dialog around it: the "Shares" sheet stays open', async () => {
    render(
      inViewer(
        <dialog open aria-label="Shares">
          <WatchersPopover share={watched('u_cy')} />
        </dialog>,
      ),
    );
    const sheet = screen.getByRole('dialog', { name: 'Shares' });
    const onSheetKeyDown = vi.fn();
    sheet.addEventListener('keydown', onSheetKeyDown);
    await userEvent.click(eye());
    const dialog = panel();
    if (!dialog) throw new Error('no popover');

    // A browser closes a modal <dialog> on Escape unless the keydown is cancelled (jsdom has no such close request).
    const escape = new KeyboardEvent('keydown', { key: 'Escape', bubbles: true, cancelable: true });
    act(() => {
      dialog.dispatchEvent(escape);
    });
    expect(escape.defaultPrevented).toBe(true);
    expect(onSheetKeyDown).not.toHaveBeenCalled();
    expect(panel()).not.toBeInTheDocument();
    expect(sheet).toContainElement(eye());
    expect(eye()).toHaveFocus();

    // Any other key is left alone.
    await userEvent.click(eye());
    const other = new KeyboardEvent('keydown', { key: 'a', bubbles: true, cancelable: true });
    act(() => {
      panel()?.dispatchEvent(other);
    });
    expect(other.defaultPrevented).toBe(false);
    expect(panel()).toBeInTheDocument();
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
