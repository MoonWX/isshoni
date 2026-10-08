// The viewer's keyboard in the layout (05 §12.7): the tile list as one tab stop that the arrow keys move, and each
// shortcut doing its thing. The map itself is keyboard.test.ts's.
import { act, fireEvent, render, screen, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import {
  FakeMediaStream,
  FakeMediaStreamTrack,
  installFakeMedia,
  installFakeMediaElement,
  type FakeMediaElementControl,
} from '../test/fakeMedia';
import { createTestServices, renderWithApp } from '../test/render';
import { createViewer, syncRoom, type ViewerServices } from './services';
import { room, SELF, shareInfo } from './testing';
import { PHONE_LANDSCAPE_QUERY, ViewerLayout } from './ViewerLayout';

// Tile order is newest first: Cy's screen on the stage, then Cy's tab, Bea's window, and the page's own preview
// last (the oldest). The fixtures' room has Alex (this page), Bea and Cy.
const MINE = shareInfo('s_mine', 'u_alex', 0, { connectionId: 'c_me', kind: 'screen' });
const BEA = shareInfo('s_bea', 'u_bea', 1);
const CY = shareInfo('s_cy', 'u_cy', 2, { kind: 'tab' });
const DEE = shareInfo('s_dee', 'u_cy', 3, { kind: 'screen', audio: false });

const track = (kind: 'audio' | 'video') => new FakeMediaStreamTrack(kind) as unknown as MediaStreamTrack;

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

const state = () => viewer.store.getState();
const watch = (...shares: Parameters<typeof room>): void => {
  act(() => {
    syncRoom(viewer, room(...shares), SELF);
  });
};
const layout = () => (
  <ViewerLayout
    viewer={viewer}
    localPreviews={{ s_mine: new FakeMediaStream([new FakeMediaStreamTrack('video')]) as unknown as MediaStream }}
  />
);

/** The tile of a share: its main button. */
const tile = (name: RegExp): HTMLElement => screen.getByRole('button', { name });
const CY_TILE = /^Watch Cy's tab/;
const BEA_TILE = /^Watch Bea's window/;
const DEE_TILE = /^Watch Cy's screen/;
const MY_TILE = /^Show your preview/;
/** The main buttons of the tiles, in order, as "name:tabindex". */
const tabStops = (): string[] =>
  within(screen.getByRole('list', { name: 'Shares' }))
    .getAllByRole('listitem')
    .map((li) => {
      const main = within(li).getAllByRole('button')[0];
      return `${main?.getAttribute('aria-label')?.split(',')[0] ?? ''}:${String(main?.tabIndex)}`;
    });
const stageName = (): string =>
  screen.getByRole('region', { name: /^Now watching|^Your preview|^Stage$/ }).getAttribute('aria-label') ?? '';

describe('the tile list is a roving-tabindex group', () => {
  it('has one tab stop: the first tile, with its eye and its speaker button', () => {
    syncRoom(viewer, room(BEA, CY, DEE), SELF);
    render(layout());
    expect(tabStops()).toEqual(["Watch Cy's tab:0", "Watch Bea's window:-1"]);
    const [first, second] = within(screen.getByRole('list', { name: 'Shares' })).getAllByRole('listitem');
    if (!first || !second) throw new Error('two tiles expected');
    expect(
      within(first)
        .getAllByRole('button')
        .map((b) => b.tabIndex),
    ).toEqual([0, 0, 0]);
    expect(
      within(second)
        .getAllByRole('button')
        .map((b) => b.tabIndex),
    ).toEqual([-1, -1, -1]);
  });

  it('is entered and left with one press of Tab', async () => {
    const user = userEvent.setup();
    syncRoom(viewer, room(BEA, CY, DEE), SELF);
    render(
      <>
        {layout()}
        <button type="button">After</button>
      </>,
    );
    // From the stage's last control into the list, through the first tile's three controls, and out.
    screen.getByRole('button', { name: 'Fullscreen' }).focus();
    await user.tab();
    expect(tile(CY_TILE)).toHaveFocus();
    await user.tab();
    await user.tab();
    expect(screen.getByRole('button', { name: 'Listen to Cy' })).toHaveFocus();
    await user.tab();
    expect(screen.getByRole('button', { name: 'After' })).toHaveFocus();
  });

  it('moves between the tiles with the arrow keys, and the tab stop moves along', async () => {
    const user = userEvent.setup();
    syncRoom(viewer, room(MINE, BEA, CY, DEE), SELF);
    render(layout());
    tile(CY_TILE).focus();
    await user.keyboard('{ArrowRight}');
    expect(tile(BEA_TILE)).toHaveFocus();
    expect(tabStops()).toEqual(["Watch Cy's tab:-1", "Watch Bea's window:0", 'Show your preview on the stage:-1']);
    await user.keyboard('{ArrowDown}');
    expect(tile(MY_TILE)).toHaveFocus();
    await user.keyboard('{ArrowDown}'); // the end: it stays
    expect(tile(MY_TILE)).toHaveFocus();
    await user.keyboard('{ArrowLeft}{ArrowUp}');
    expect(tile(CY_TILE)).toHaveFocus();
    await user.keyboard('{End}');
    expect(tile(MY_TILE)).toHaveFocus();
    await user.keyboard('{Home}');
    expect(tile(CY_TILE)).toHaveFocus();
    // Nothing was picked on the way.
    expect(state()).toMatchObject({ focusedShareId: 's_dee', focusMode: 'auto' });
  });

  it('moves from a tile’s eye to the next tile, and enters the list from elsewhere at its tab stop', async () => {
    const user = userEvent.setup();
    syncRoom(viewer, room(BEA, CY, DEE), SELF);
    render(layout());
    within(screen.getAllByRole('listitem')[0] as HTMLElement)
      .getByRole('button', { name: /Show who is watching$/ })
      .focus();
    await user.keyboard('{ArrowRight}');
    expect(tile(BEA_TILE)).toHaveFocus();

    // The tab stop is Bea's tile now: an arrow key from the stage goes there.
    screen.getByRole('button', { name: 'Fullscreen' }).focus();
    await user.keyboard('{ArrowDown}');
    expect(tile(BEA_TILE)).toHaveFocus();
  });

  it('leaves the arrow keys to the volume slider', async () => {
    const user = userEvent.setup();
    syncRoom(viewer, room(BEA, CY), SELF);
    viewer.registry.set('s_cy', 'audio', track('audio'));
    render(layout());
    const slider = screen.getByRole('slider', { name: 'Volume' });
    slider.focus();
    await user.keyboard('{ArrowRight}');
    expect(slider).toHaveFocus();
  });

  it('falls back to the first tile when the tab stop’s share ends', async () => {
    const user = userEvent.setup();
    syncRoom(viewer, room(MINE, BEA, CY, DEE), SELF);
    render(layout());
    tile(CY_TILE).focus();
    await user.keyboard('{ArrowRight}');
    expect(tabStops()[1]).toBe("Watch Bea's window:0");
    watch(MINE, CY, DEE);
    expect(tabStops()).toEqual(["Watch Cy's tab:0", 'Show your preview on the stage:-1']);
  });
});

describe('Enter and Space on a tile', () => {
  it('focus the tile’s share, and the keyboard focus goes to the tile of the share that left the stage', async () => {
    const user = userEvent.setup();
    syncRoom(viewer, room(BEA, CY, DEE), SELF);
    render(layout());
    tile(BEA_TILE).focus();
    await user.keyboard('{Enter}');
    expect(stageName()).toBe("Now watching: Bea's window");
    expect(state().focusMode).toBe('manual');
    // The share that left the stage has a tile now, and the focus is on it: Enter again swaps the two back.
    expect(tile(DEE_TILE)).toHaveFocus();
    expect(tabStops()).toEqual(["Watch Cy's screen:0", "Watch Cy's tab:-1"]);
    await user.keyboard(' ');
    expect(stageName()).toBe("Now watching: Cy's screen");
    expect(tile(BEA_TILE)).toHaveFocus();
  });

  it('bring the keyboard’s focus into view, and leave the list where a pointer scrolled it', async () => {
    const user = userEvent.setup();
    // jsdom has no focus ring: the test says which kind of focus the tile has.
    let keyboard = true;
    const matches = Reflect.get(Element.prototype, 'matches') as (this: Element, selector: string) => boolean;
    vi.spyOn(Element.prototype, 'matches').mockImplementation(function (this: Element, selector: string) {
      return selector === ':focus-visible' ? keyboard : matches.call(this, selector);
    });
    syncRoom(viewer, room(BEA, CY, DEE), SELF);
    render(layout());
    const focus = vi.spyOn(HTMLElement.prototype, 'focus');

    tile(BEA_TILE).focus();
    await user.keyboard('{Enter}');
    expect(tile(DEE_TILE)).toHaveFocus();
    expect(focus).toHaveBeenLastCalledWith({ preventScroll: false });

    // A click focuses the button it lands on, without a ring: the tile it swaps with is focused where it is.
    keyboard = false;
    tile(CY_TILE).focus();
    fireEvent.click(tile(CY_TILE));
    expect(state().focusedShareId).toBe('s_cy');
    expect(tile(BEA_TILE)).toHaveFocus();
    expect(focus).toHaveBeenLastCalledWith({ preventScroll: true });
  });

  it('leave the focus alone when a share is picked from elsewhere', () => {
    syncRoom(viewer, room(BEA, CY, DEE), SELF);
    render(layout());
    const fullscreen = screen.getByRole('button', { name: 'Fullscreen' });
    fullscreen.focus();
    act(() => {
      state().focusShare('s_bea'); // the people panel's "Watch"
    });
    expect(document.activeElement).not.toBe(tile(DEE_TILE));
  });
});

describe('the shortcuts', () => {
  it('1–9 focus the Nth share, newest first, as a pick', async () => {
    const user = userEvent.setup();
    syncRoom(viewer, room(MINE, BEA, CY, DEE), SELF);
    render(layout());
    expect(state().shares.map((s) => s.id)).toEqual(['s_dee', 's_cy', 's_bea', 's_mine']);
    await user.keyboard('3');
    expect(state()).toMatchObject({ focusedShareId: 's_bea', focusMode: 'manual', audibleShareId: 's_bea' });
    await user.keyboard('1');
    expect(state().focusedShareId).toBe('s_dee');
    await user.keyboard('4');
    expect(stageName()).toBe('Your preview: Your screen');
    await user.keyboard('5'); // no fifth share
    expect(state().focusedShareId).toBe('s_mine');
  });

  it('a number key on the focused tile’s own share keeps the keyboard in the list', async () => {
    const user = userEvent.setup();
    syncRoom(viewer, room(BEA, CY, DEE), SELF);
    render(layout());
    tile(CY_TILE).focus();
    await user.keyboard('2');
    expect(state().focusedShareId).toBe('s_cy');
    expect(tile(DEE_TILE)).toHaveFocus();
    // Another share's number leaves the focus where it is.
    await user.keyboard('3');
    expect(state().focusedShareId).toBe('s_bea');
    expect(tile(DEE_TILE)).toHaveFocus();
  });

  it('F toggles fullscreen, and Esc leaves the pseudo-fullscreen', async () => {
    const user = userEvent.setup();
    syncRoom(viewer, room(BEA, CY), SELF);
    render(layout());
    await user.keyboard('f');
    expect(state().fullscreen).toBe(true);
    await user.keyboard('f');
    expect(state().fullscreen).toBe(false);
    await user.keyboard('F'); // Caps Lock
    expect(state().fullscreen).toBe(true);
    await user.keyboard('{Escape}');
    expect(state().fullscreen).toBe(false);
  });

  it('F does nothing on an empty stage', async () => {
    const user = userEvent.setup();
    render(layout());
    await user.keyboard('f');
    expect(state().fullscreen).toBe(false);
  });

  it('M mutes and unmutes what is heard', async () => {
    const user = userEvent.setup();
    syncRoom(viewer, room(BEA, CY), SELF);
    viewer.registry.set('s_cy', 'audio', track('audio'));
    render(layout());
    await act(() => Promise.resolve());
    expect(state().audio).toBe('playing');
    await user.keyboard('m');
    expect(state().audio).toBe('muted');
    expect(viewer.audio.element?.muted).toBe(true);
    await user.keyboard('m');
    expect(state().audio).toBe('playing');
    expect(viewer.audio.element?.muted).toBe(false);
  });

  it('M is the tap that starts the sound when the browser wants one', async () => {
    const user = userEvent.setup();
    media.policy = 'block';
    syncRoom(viewer, room(BEA, CY), SELF);
    viewer.registry.set('s_cy', 'audio', track('audio'));
    render(layout());
    await act(() => Promise.resolve());
    expect(state().audio).toBe('blocked');
    media.policy = 'allow';
    await user.keyboard('m');
    await act(() => Promise.resolve());
    expect(state().audio).toBe('playing');
  });

  it('M does nothing while nothing is heard', async () => {
    const user = userEvent.setup();
    syncRoom(viewer, room(MINE), SELF);
    render(layout());
    await user.keyboard('m');
    expect(state().audio).toBe('locked');
  });

  it('L listens to the keyboard-focused tile without moving the stage, and back', async () => {
    const user = userEvent.setup();
    syncRoom(viewer, room(BEA, CY, DEE), SELF);
    render(layout());
    tile(BEA_TILE).focus();
    await user.keyboard('l');
    expect(state()).toMatchObject({ focusedShareId: 's_dee', focusMode: 'auto', audibleShareId: 's_bea' });
    expect(screen.getByRole('button', { name: 'Listen to Bea' })).toHaveAttribute('aria-pressed', 'true');
    // Pressed again, like the speaker button: the sound goes back to the stage's share.
    await user.keyboard('l');
    expect(state().audibleShareId).toBe('s_dee');
    expect(screen.getByRole('button', { name: 'Listen to Bea' })).toHaveAttribute('aria-pressed', 'false');
  });

  it('L does nothing without a focused tile, and on a tile without sound', async () => {
    const user = userEvent.setup();
    syncRoom(viewer, room(MINE, BEA, CY), SELF);
    render(layout());
    await user.keyboard('l');
    screen.getByRole('button', { name: 'Fullscreen' }).focus();
    await user.keyboard('l');
    tile(MY_TILE).focus();
    await user.keyboard('l');
    expect(state().audibleShareId).toBe('s_cy');
  });

  it('? opens the shortcuts dialog, and so does the stage’s keyboard button', async () => {
    const user = userEvent.setup();
    syncRoom(viewer, room(BEA), SELF);
    render(layout());
    expect(screen.queryByRole('dialog', { name: 'Keyboard shortcuts' })).not.toBeInTheDocument();
    await user.keyboard('?');
    const dialog = screen.getByRole('dialog', { name: 'Keyboard shortcuts' });
    expect(within(dialog).getByText('Fullscreen on or off')).toBeInTheDocument();
    // While it is open the shortcuts rest: the dialog has the keyboard.
    await user.keyboard('f');
    expect(state().fullscreen).toBe(false);

    await user.click(within(dialog).getByRole('button', { name: 'Close' }));
    expect(screen.queryByRole('dialog', { name: 'Keyboard shortcuts' })).not.toBeInTheDocument();
    await user.click(screen.getByRole('button', { name: 'Keyboard shortcuts' }));
    expect(screen.getByRole('dialog', { name: 'Keyboard shortcuts' })).toBeInTheDocument();
  });

  it('Shift+D switches the debug overlay’s preference, inside the app', async () => {
    const user = userEvent.setup();
    syncRoom(viewer, room(BEA), SELF);
    const services = createTestServices();
    renderWithApp(layout(), { services });
    expect(services.prefs.getState().debug).toBe(false);
    await user.keyboard('{Shift>}D{/Shift}');
    expect(services.prefs.getState().debug).toBe(true);
    await user.keyboard('{Shift>}D{/Shift}');
    expect(services.prefs.getState().debug).toBe(false);
    await user.keyboard('d');
    expect(services.prefs.getState().debug).toBe(false);
    // The dialog lists it there, and not where there is no overlay to switch.
    await user.keyboard('?');
    expect(screen.getByText('Debug overlay on or off')).toBeInTheDocument();
  });

  it('lists no debug overlay outside the app', async () => {
    const user = userEvent.setup();
    syncRoom(viewer, room(BEA), SELF);
    render(layout());
    await user.keyboard('{Shift>}D{/Shift}?');
    expect(screen.getByRole('dialog', { name: 'Keyboard shortcuts' })).toBeInTheDocument();
    expect(screen.queryByText('Debug overlay on or off')).not.toBeInTheDocument();
  });
});

describe('where the shortcuts rest', () => {
  it('in a text field', async () => {
    const user = userEvent.setup();
    syncRoom(viewer, room(BEA, CY), SELF);
    render(
      <>
        {layout()}
        <input aria-label="Chat" />
      </>,
    );
    await user.click(screen.getByRole('textbox', { name: 'Chat' }));
    await user.keyboard('fm1l?');
    expect(screen.getByRole('textbox', { name: 'Chat' })).toHaveValue('fm1l?');
    expect(state()).toMatchObject({ fullscreen: false, focusedShareId: 's_cy', focusMode: 'auto' });
    expect(screen.queryByRole('dialog', { name: 'Keyboard shortcuts' })).not.toBeInTheDocument();
  });

  it('with Ctrl or Cmd held: those are the browser’s', async () => {
    const user = userEvent.setup();
    syncRoom(viewer, room(BEA, CY), SELF);
    render(layout());
    await user.keyboard('{Control>}f{/Control}{Meta>}l{/Meta}{Control>}1{/Control}');
    expect(state()).toMatchObject({ fullscreen: false, focusedShareId: 's_cy', focusMode: 'auto' });
  });

  it('in the watchers popover: its Escape closes it and nothing else', async () => {
    const user = userEvent.setup();
    syncRoom(viewer, room(BEA, CY), SELF);
    render(layout());
    await user.keyboard('f');
    expect(state().fullscreen).toBe(true);
    await user.click(screen.getByRole('button', { name: /Show who is watching$/ }));
    expect(screen.getByRole('dialog', { name: 'Watching now' })).toBeVisible();
    await user.keyboard('{Escape}');
    expect(screen.queryByRole('dialog', { name: 'Watching now' })).not.toBeInTheDocument();
    expect(state().fullscreen).toBe(true);
    await user.keyboard('{Escape}');
    expect(state().fullscreen).toBe(false);
  });

  it('when the layout is gone', async () => {
    const user = userEvent.setup();
    syncRoom(viewer, room(BEA, CY), SELF);
    const { unmount } = render(layout());
    unmount();
    await user.keyboard('2f');
    expect(state()).toMatchObject({ fullscreen: false, focusedShareId: 's_cy', focusMode: 'auto' });
  });
});

describe('the phone’s "Shares" sheet', () => {
  it('keeps the arrow keys among its tiles, and gives the focus back to its button after a pick', async () => {
    const user = userEvent.setup();
    vi.stubGlobal('matchMedia', (query: string) => ({
      media: query,
      matches: query === PHONE_LANDSCAPE_QUERY,
      addEventListener: () => undefined,
      removeEventListener: () => undefined,
    }));
    syncRoom(viewer, room(BEA, CY, DEE), SELF);
    render(layout());
    await user.click(screen.getByRole('button', { name: 'Shares (2)' }));
    tile(CY_TILE).focus();
    fireEvent.keyDown(tile(CY_TILE), { key: 'ArrowRight' });
    expect(tile(BEA_TILE)).toHaveFocus();
    // The sheet has the keyboard: no fullscreen behind it.
    fireEvent.keyDown(tile(BEA_TILE), { key: 'f' });
    expect(state().fullscreen).toBe(false);

    await user.keyboard('{Enter}');
    expect(state().focusedShareId).toBe('s_bea');
    expect(screen.queryByRole('list', { name: 'Shares' })).not.toBeInTheDocument();
  });
});
