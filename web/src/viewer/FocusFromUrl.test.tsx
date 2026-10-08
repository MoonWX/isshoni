// `?focus=<shareId>` on the room page (05 §5, §12.2): the layout, inside a router, focuses that share once it is
// live, waits up to 5 s for it, and then drops the parameter from the URL.
import { act, render } from '@testing-library/react';
import { createMemoryRouter, RouterProvider, type DataRouter } from 'react-router';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { installFakeMedia, installFakeMediaElement, type FakeMediaElementControl } from '../test/fakeMedia';
import { FOCUS_PARAM_WAIT_MS } from './focusParam';
import { createViewer, syncRoom, type ViewerServices } from './services';
import { room, SELF, shareInfo } from './testing';
import { ViewerLayout } from './ViewerLayout';

const BEA = shareInfo('s_bea', 'u_bea', 1);
const CY = shareInfo('s_cy', 'u_cy', 2);

let media: FakeMediaElementControl;
let viewer: ViewerServices;
let router: DataRouter;

beforeEach(() => {
  vi.useFakeTimers();
  installFakeMedia();
  media = installFakeMediaElement();
  viewer = createViewer();
});

afterEach(() => {
  viewer.dispose();
  media.restore();
  vi.unstubAllGlobals();
  vi.useRealTimers();
});

/** The room page's route with the layout on it, at `path`. */
function open(path: string): void {
  router = createMemoryRouter([{ path: '/r/:roomId', element: <ViewerLayout viewer={viewer} /> }], {
    initialEntries: ['/r/other', path],
  });
  render(<RouterProvider router={router} />);
}

const state = () => viewer.store.getState();
const url = (): string => router.state.location.pathname + router.state.location.search;
const flush = (ms = 0) => act(() => vi.advanceTimersByTimeAsync(ms));

describe('?focus=<shareId>', () => {
  it('focuses a share that is live, as a pick, and drops the parameter with a replace', async () => {
    syncRoom(viewer, room(BEA, CY), SELF);
    open('/r/lounge?focus=s_bea');
    await flush();
    expect(state()).toMatchObject({ focusedShareId: 's_bea', focusMode: 'manual', audibleShareId: 's_bea' });
    expect(url()).toBe('/r/lounge');
    // Replaced, not pushed: Back leaves the room page instead of bringing the link back.
    await act(() => router.navigate(-1));
    expect(url()).toBe('/r/other');
  });

  it('waits for a share that is not there yet (a notification is ahead of room.state)', async () => {
    syncRoom(viewer, room(CY), SELF);
    open('/r/lounge?focus=s_bea');
    await flush(FOCUS_PARAM_WAIT_MS - 100);
    expect(state().focusedShareId).toBe('s_cy');
    expect(url()).toBe('/r/lounge?focus=s_bea');

    act(() => {
      syncRoom(viewer, room(BEA, CY), SELF);
    });
    await flush();
    expect(state()).toMatchObject({ focusedShareId: 's_bea', focusMode: 'manual' });
    expect(url()).toBe('/r/lounge');
  });

  it('gives up after 5 s and drops the parameter all the same', async () => {
    syncRoom(viewer, room(CY), SELF);
    open('/r/lounge?focus=s_gone');
    await flush(FOCUS_PARAM_WAIT_MS - 1);
    expect(url()).toBe('/r/lounge?focus=s_gone');
    await flush(1);
    expect(url()).toBe('/r/lounge');
    expect(state()).toMatchObject({ focusedShareId: 's_cy', focusMode: 'auto', pendingFocusParam: null });
  });

  it('keeps the other parameters of the URL', async () => {
    syncRoom(viewer, room(BEA, CY), SELF);
    open('/r/lounge?debug=1&focus=s_bea&x=y');
    await flush();
    expect(url()).toBe('/r/lounge?debug=1&x=y');
  });

  it('drops an empty parameter at once and picks nothing', async () => {
    syncRoom(viewer, room(BEA, CY), SELF);
    open('/r/lounge?focus=');
    await flush();
    expect(url()).toBe('/r/lounge');
    expect(state()).toMatchObject({ focusedShareId: 's_cy', focusMode: 'auto' });
  });

  it('follows a link that arrives while the page is open', async () => {
    syncRoom(viewer, room(BEA, CY), SELF);
    open('/r/lounge');
    await flush();
    expect(state().focusedShareId).toBe('s_cy');
    await act(() => router.navigate('/r/lounge?focus=s_bea'));
    await flush();
    expect(state()).toMatchObject({ focusedShareId: 's_bea', focusMode: 'manual' });
    expect(url()).toBe('/r/lounge');
  });

  it('stops waiting when the page is left: the share is not picked behind the user’s back', async () => {
    syncRoom(viewer, room(CY), SELF);
    router = createMemoryRouter(
      [
        { path: '/r/:roomId', element: <ViewerLayout viewer={viewer} /> },
        { path: '/account', element: <p>Account</p> },
      ],
      { initialEntries: ['/r/lounge?focus=s_bea'] },
    );
    render(<RouterProvider router={router} />);
    await flush();
    expect(state().pendingFocusParam).toBe('s_bea');
    await act(() => router.navigate('/account'));
    expect(state().pendingFocusParam).toBeNull();
    act(() => {
      syncRoom(viewer, room(BEA, CY), SELF);
    });
    expect(state().focusedShareId).toBe('s_cy');
    await flush(FOCUS_PARAM_WAIT_MS);
    expect(url()).toBe('/account');
  });

  it('is not read outside a router: the layout works without one', () => {
    syncRoom(viewer, room(BEA, CY), SELF);
    render(<ViewerLayout viewer={viewer} />);
    expect(state()).toMatchObject({ focusedShareId: 's_cy', focusMode: 'auto' });
  });
});
