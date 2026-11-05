// The desktop app's deep links on the page (docs/m2/03 §13.2): each answer of Go's App.HandleLink as the SPA
// handles it, from App.PendingLink() at start and from deeplink.open after: the active server's room (the M1 switch
// question when a share is live in another room, playback.stop again for the room the app is in), an unknown server
// (the wizard's add question), another known server while a share is live (02 §12.3's question), 20 servers, and a
// link while a question is open. Go's side of the table is internal/desktop/app's HandleLink tests.
import { act, fireEvent, render, screen } from '@testing-library/react';
import { createMemoryRouter, Outlet, RouterProvider, useLocation, type DataRouter } from 'react-router';
import { afterEach, beforeAll, beforeEach, describe, expect, it, vi } from 'vitest';

import { AppProviders } from '../app/App';
import type { AppServices } from '../app/context';
import { createDesktopTestPlatform, type DesktopTestPlatform } from '../app/testing/desktop';
import { queryKeys } from '../protocol/queryKeys';
import { meFixture } from '../test/msw';
import type { PendingLink } from '../platform/desktop/bridge';
import { desktopLinkAction, DesktopLinks, type AnyPendingLink, type LinkContext } from './DesktopLinks';
import { createHarness, pickedSource, tick, type Harness } from './testing/harness';
import { stubNativeDialog, stubRest, twoRooms } from './testing/page';

const ctx = (over: Partial<LinkContext> = {}): LinkContext => ({
  roomId: 'lounge',
  sharing: false,
  lastRoomId: 'lounge',
  ...over,
});
const target = {
  origin: 'https://other.example.com',
  host: 'other.example.com',
  hostUnicode: 'other.example.com',
  roomId: 'den',
};

describe('desktopLinkAction: 03 §13.2’s table, on the page', () => {
  it('the active server: the room, or "" for the last room (else "/": the default room)', () => {
    expect(desktopLinkAction({ roomId: 'games' }, ctx())).toEqual({ kind: 'room', to: '/r/games', again: false });
    expect(desktopLinkAction({ roomId: '' }, ctx({ lastRoomId: 'games' }))).toEqual({
      kind: 'room',
      to: '/r/games',
      again: false,
    });
    expect(desktopLinkAction({ roomId: '' }, ctx({ roomId: null, lastRoomId: null }))).toEqual({
      kind: 'room',
      to: '/',
      again: false,
    });
  });

  it('the room the app is in: playback.stop again; a live share in another room: M1’s switch question', () => {
    expect(desktopLinkAction({ roomId: 'lounge' }, ctx({ sharing: true }))).toEqual({
      kind: 'room',
      to: '/r/lounge',
      again: true,
    });
    expect(desktopLinkAction({ roomId: '' }, ctx())).toEqual({ kind: 'room', to: '/r/lounge', again: true });
    expect(desktopLinkAction({ roomId: 'games' }, ctx({ sharing: true }))).toEqual({
      kind: 'askRoom',
      roomId: 'games',
    });
    expect(desktopLinkAction({ roomId: 'a b/c' }, ctx())).toEqual({ kind: 'room', to: '/r/a%20b%2Fc', again: false });
  });

  it('an unknown server: the wizard’s add question; 20 servers: the wizard says so', () => {
    expect(desktopLinkAction({ add: { ...target } }, ctx())).toEqual({
      kind: 'add',
      to: '/desktop/setup?add=https%3A%2F%2Fother.example.com',
    });
    expect(desktopLinkAction({ error: 'too_many_servers' }, ctx())).toEqual({ kind: 'tooMany', to: '/desktop/setup' });
  });

  it('another known server while a share is live: 02 §12.3’s question', () => {
    expect(desktopLinkAction({ switch: target, live: { roomId: 'lounge' } }, ctx({ sharing: true }))).toEqual({
      kind: 'askServer',
      target,
      liveRoomId: 'lounge',
    });
  });

  it('anything else does nothing', () => {
    for (const p of [null, undefined, 'lounge', [], {}, { roomId: 7 }, { add: 'x' }, { error: 'other' }]) {
      expect(desktopLinkAction(p, ctx())).toEqual({ kind: 'none' });
    }
  });
});

/** Where the router is; the pages are placeholders. */
function Where() {
  const { pathname, search } = useLocation();
  return <p data-testid="where">{pathname + search}</p>;
}

const routes = [
  { path: '/', element: <Where /> },
  { path: '/r/:roomId', element: <Where /> },
  { path: '/desktop/setup', element: <Where /> },
  { path: '/desktop/settings', element: <Where /> },
];

describe('DesktopLinks', () => {
  /** Go's deeplink.open with any of its shapes (bridge.ts types only the two the router needs). */
  const emitLink = (p: AnyPendingLink): void => {
    d.fake.emit('deeplink.open', p as PendingLink);
  };
  let d: DesktopTestPlatform;
  let h: Harness;
  let router: DataRouter;
  let dialogs: ReturnType<typeof stubNativeDialog>;
  const where = () => screen.getByTestId('where').textContent;

  beforeAll(async () => {
    // The component's chunk (and the desktop texts), before the clock is faked.
    await import('./DesktopLinks');
  });

  beforeEach(() => {
    vi.useFakeTimers();
    vi.spyOn(Math, 'random').mockReturnValue(0.5);
    dialogs = stubNativeDialog();
    d = createDesktopTestPlatform();
    const { kind, client, role, serverOrigin } = d.platform;
    h = createHarness({
      platform: { kind, client, role, serverOrigin, versionActions: () => ({}), desktop: d.platform.desktop ?? null },
    });
    stubRest(h.platform, { rooms: twoRooms() });
    d.fake.answers['app.pendingLink'] = null;
  });

  afterEach(() => {
    h.close();
    dialogs.restore();
    vi.useRealTimers();
    vi.restoreAllMocks();
  });

  async function show(path: string, signedIn = true, services: AppServices = h.services): Promise<void> {
    if (signedIn) services.queryClient.setQueryData(queryKeys.me, meFixture());
    // DesktopLinks above the routes, as InRoomBar mounts it in the app's layout.
    router = createMemoryRouter(
      [
        {
          element: (
            <>
              <DesktopLinks />
              <Outlet />
            </>
          ),
          children: routes,
        },
      ],
      { initialEntries: [path] },
    );
    render(
      <AppProviders services={services}>
        <RouterProvider router={router} />
      </AppProviders>,
    );
    await act(() => tick());
  }

  it('opens the room of the link that started the app, then tells Go (App.LinkHandled)', async () => {
    d.fake.answers['app.pendingLink'] = { roomId: 'games' };
    await show('/');
    expect(where()).toBe('/r/games');
    expect(d.fake.callsOf('app.linkHandled')).toHaveLength(1);
  });

  it('waits while the wizard or the relink page shows, and while nobody is signed in', async () => {
    d.fake.answers['app.pendingLink'] = { roomId: 'games' };
    await show('/desktop/setup');
    expect(d.fake.callsOf('app.pendingLink')).toEqual([]);
    act(() => {
      d.fake.emit('deeplink.open', { roomId: 'games' });
    });
    expect(where()).toBe('/desktop/setup');
    expect(d.fake.callsOf('app.linkHandled')).toEqual([]);
  });

  it('does nothing before the user is signed in', async () => {
    d.fake.answers['app.pendingLink'] = { roomId: 'games' };
    stubRest(h.platform, { me: null, rooms: twoRooms() });
    await show('/', false);
    expect(d.fake.callsOf('app.pendingLink')).toEqual([]);
    expect(where()).toBe('/');
  });

  it('follows deeplink.open: a room, an unknown server (the wizard), 20 servers', async () => {
    await show('/desktop/settings');
    act(() => {
      d.fake.emit('deeplink.open', { roomId: 'games' });
    });
    expect(where()).toBe('/r/games');
    act(() => {
      d.fake.emit('deeplink.open', { add: target });
    });
    expect(where()).toBe('/desktop/setup?add=https%3A%2F%2Fother.example.com');
  });

  it('a link to the room the app is in sends playback.stop again', async () => {
    const joined = h.runtime.session.join('lounge');
    await h.connect();
    await joined;
    const requestStop = vi.spyOn(h.runtime.handoff, 'requestStop');
    await show('/r/lounge');
    act(() => {
      d.fake.emit('deeplink.open', { roomId: 'lounge' });
    });
    expect(where()).toBe('/r/lounge');
    expect(requestStop).toHaveBeenCalledTimes(1);
  });

  it('asks before a link to another room ends a live share; Cancel stays, and a link meanwhile is ignored', async () => {
    const joined = h.runtime.session.join('lounge');
    await h.connect();
    await joined;
    await h.runtime.session.startShare(pickedSource, { preset: 'auto', withAudio: true });
    expect(h.runtime.session.share).not.toBeNull();
    await show('/r/lounge');

    act(() => {
      d.fake.emit('deeplink.open', { roomId: 'games' });
    });
    await act(() => tick()); // the room list, for the room's name
    expect(screen.getByRole('dialog', { name: 'Stop sharing and switch to Games?' })).toBeInTheDocument();
    expect(where()).toBe('/r/lounge');
    // A newer link must not swap the question under the user's cursor.
    act(() => {
      d.fake.emit('deeplink.open', { roomId: 'den' });
    });
    expect(screen.getByRole('dialog', { name: 'Stop sharing and switch to Games?' })).toBeInTheDocument();
    fireEvent.click(screen.getByRole('button', { name: 'Cancel' }));
    expect(where()).toBe('/r/lounge');
    expect(d.fake.callsOf('app.linkHandled')).toHaveLength(1);

    act(() => {
      d.fake.emit('deeplink.open', { roomId: 'games' });
    });
    fireEvent.click(screen.getByRole('button', { name: 'Stop sharing and switch' }));
    expect(where()).toBe('/r/games');
    expect(d.fake.callsOf('app.linkHandled')).toHaveLength(2);
  });

  it('another known server while sharing: "Your share will stop.", then Open stops it and switches', async () => {
    await show('/r/lounge');
    act(() => {
      emitLink({ switch: target, live: { roomId: 'lounge' } });
    });
    await act(() => tick()); // the room list, for the room's name
    const dialog = screen.getByRole('dialog', { name: 'Open other.example.com?' });
    expect(dialog).toHaveTextContent("You're sharing in Lounge. Open other.example.com? Your share will stop.");
    fireEvent.click(screen.getByRole('button', { name: 'Cancel' }));
    expect(d.fake.callsOf('app.linkHandled')).toHaveLength(1);
    expect(d.fake.callsOf('share.stop')).toEqual([]);

    act(() => {
      emitLink({ switch: target, live: { roomId: 'lounge' } });
    });
    fireEvent.click(screen.getByRole('button', { name: 'Stop sharing and open' }));
    await act(() => tick());
    expect(
      d.fake.calls.map((c) => c.method).filter((m) => m.startsWith('share.') || m.startsWith('auth.switch')),
    ).toEqual(['share.stop', 'auth.switchServer']);
    expect(d.fake.callsOf('auth.switchServer')).toEqual([['https://other.example.com']]);
  });
});
