// The room header (05 §11.2): the room's name, the people count, the switcher (only when GET /api/v1/rooms says
// showRoomList, and "Create room" only for admins then), and the account menu.
import { act, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { http, HttpResponse } from 'msw';
import { describe, expect, it, vi } from 'vitest';
import { useStore } from 'zustand';
import { createStore } from 'zustand/vanilla';

import type { PwaProvider } from '../platform/types';
import type { CreateInviteResponse, Info } from '../protocol/api.gen';
import { queryKeys } from '../protocol/queryKeys';
import { apiPath, infoFixture, meFixture, roomsFixture, server } from '../test/msw';
import { createTestPlatform } from '../test/platform';
import { createTestServices, renderRoute } from '../test/render';
import { RoomHeader, type RoomHeaderProps } from './RoomHeader';
import { stubNativeDialog, twoRooms } from './testing/page';

function renderHeader(props: Partial<RoomHeaderProps> = {}, pwa: PwaProvider | null = null, info = infoFixture()) {
  const onOpenPeople = vi.fn();
  const onOpenRooms = vi.fn();
  const services = createTestServices({ platform: createTestPlatform({ pwa }), info });
  // The header's props are in a store, so that a test can render it again with other ones (setProps), as the page
  // does on every room.state.
  const current = createStore<RoomHeaderProps>(() => ({
    roomId: 'lounge',
    name: 'Lounge',
    peopleCount: 5,
    onOpenPeople,
    rooms: roomsFixture(),
    me: meFixture(),
    sharing: false,
    onOpenRooms,
    ...props,
  }));
  function Header() {
    return <RoomHeader {...useStore(current)} />;
  }
  const result = renderRoute({
    path: '/r/lounge',
    services,
    routes: [
      { path: '/r/:roomId', Component: Header },
      { path: '*', element: <h1>Another page</h1> },
    ],
  });
  const setProps = (next: Partial<RoomHeaderProps>): void => {
    act(() => {
      current.setState(next);
    });
  };
  return { ...result, onOpenPeople, onOpenRooms, setProps };
}

const INVITE_LINK = 'http://localhost:3000/invite#EXAMPLEinviteTOKEN0123456789abcd';

function inviteFixture(): CreateInviteResponse {
  return {
    url: INVITE_LINK,
    invite: {
      id: 'h6j8k0m2n4p6',
      note: '',
      createdBy: { id: 'k3m9p2qxw7ht', username: 'alex' },
      createdAt: '2026-10-01T12:00:00.000Z',
      expiresAt: '2026-10-08T12:00:00.000Z',
      maxUses: 10,
      uses: 0,
      state: 'active',
      redeemedBy: [],
    },
  };
}

const openSwitcher = () => userEvent.click(screen.getByRole('button', { name: 'Switch room' }));
const openMenu = () => userEvent.click(screen.getByRole('button', { name: /^Account menu/ }));

describe('RoomHeader', () => {
  it('names the room in the page’s <h1>', () => {
    renderHeader();
    expect(screen.getByRole('heading', { level: 1, name: 'Lounge' })).toBeInTheDocument();
  });

  it('has a placeholder name until the room’s is known', () => {
    renderHeader({ name: undefined });
    expect(screen.getByRole('heading', { level: 1, name: 'Room' })).toBeInTheDocument();
  });

  it('counts the people, and the count opens the people panel', async () => {
    const { onOpenPeople } = renderHeader({ peopleCount: 5 });
    const people = screen.getByRole('button', { name: '5 here. Show who' });
    expect(people).toHaveTextContent('5 here');
    expect(people).toHaveAttribute('aria-haspopup', 'dialog');
    await userEvent.click(people);
    expect(onOpenPeople).toHaveBeenCalledOnce();
  });

  it('shows no count before the room said who is here', () => {
    renderHeader({ peopleCount: undefined });
    expect(screen.queryByRole('button', { name: /here/ })).not.toBeInTheDocument();
  });

  it('renders the share controls it is given', () => {
    renderHeader({ children: <button type="button">Share</button> });
    expect(screen.getByRole('button', { name: 'Share' })).toBeInTheDocument();
  });

  it('has no account menu until the user is known', () => {
    renderHeader({ me: undefined });
    expect(screen.queryByRole('button', { name: /^Account menu/ })).not.toBeInTheDocument();
  });
});

describe('the room switcher (05 §11.2: only when showRoomList)', () => {
  it('is not there while only one room exists, for members and for admins', () => {
    renderHeader({ rooms: roomsFixture({ showRoomList: false }), me: meFixture({ admin: true }) });
    expect(screen.queryByRole('button', { name: 'Switch room' })).not.toBeInTheDocument();
    // Admins create the second room under Admin → Rooms: no "Create room" here before that.
    expect(screen.queryByRole('link', { name: 'Create room' })).not.toBeInTheDocument();
  });

  it('is not there while the room list hasn’t loaded', () => {
    renderHeader({ rooms: undefined });
    expect(screen.queryByRole('button', { name: 'Switch room' })).not.toBeInTheDocument();
  });

  it('lists the rooms with their live counts once a second room exists', async () => {
    const { onOpenRooms } = renderHeader({ rooms: twoRooms() });
    await openSwitcher();
    // The page refetches the list when it opens: the counts are as old as the last fetch.
    expect(onOpenRooms).toHaveBeenCalledOnce();
    const list = screen.getByRole('dialog', { name: 'Rooms' });
    const rooms = within(list).getAllByRole('listitem');
    expect(rooms.map((li) => li.textContent)).toEqual(['Lounge3 here1 live', 'Games1 here']);
    // The room the page is in is marked, and is not a link to itself.
    expect(within(list).getByText('Lounge').closest('[aria-current]')).toHaveAttribute('aria-current', 'true');
    expect(within(list).queryByRole('link', { name: /Lounge/ })).not.toBeInTheDocument();
    expect(within(list).getByRole('link', { name: /Games/ })).toHaveAttribute('href', '/r/games');
  });

  it('switches by navigating to the other room’s page', async () => {
    const { router } = renderHeader({ rooms: twoRooms() });
    await openSwitcher();
    await userEvent.click(screen.getByRole('link', { name: /Games/ }));
    expect(router.state.location.pathname).toBe('/r/games');
    expect(screen.queryByRole('dialog', { name: 'Rooms' })).not.toBeInTheDocument();
  });

  it('asks before a switch that would end this page’s share, and stays on Cancel', async () => {
    const { router } = renderHeader({ rooms: twoRooms(), sharing: true });
    await openSwitcher();
    await userEvent.click(screen.getByRole('link', { name: /Games/ }));
    expect(router.state.location.pathname).toBe('/r/lounge');
    const confirm = screen.getByRole('dialog', { name: 'Stop sharing and switch to Games?' });
    expect(within(confirm).getByText('Your share ends when you leave this room.')).toBeInTheDocument();

    await userEvent.click(within(confirm).getByRole('button', { name: 'Cancel' }));
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument();
    expect(router.state.location.pathname).toBe('/r/lounge');
  });

  it('Cancel closes the question in place, with the focus on the switcher’s button to return to (05 §16.6)', async () => {
    // A native <dialog> gives the focus back to where it was when it opened, if it is closed while in the page.
    const dialogs = stubNativeDialog();
    try {
      renderHeader({ rooms: twoRooms(), sharing: true });
      await openSwitcher();
      await userEvent.click(screen.getByRole('link', { name: /Games/ }));
      const confirm = screen.getByRole('dialog', { name: 'Stop sharing and switch to Games?' });
      // The link went with the list: the focus is on something that stays when the question opens.
      expect(screen.getByRole('button', { name: 'Switch room' })).toHaveFocus();

      await userEvent.click(within(confirm).getByRole('button', { name: 'Cancel' }));
      expect(screen.queryByRole('dialog')).not.toBeInTheDocument();
      expect(dialogs.closes).toEqual([{ title: 'Stop sharing and switch to Games?', inPage: true }]);
    } finally {
      dialogs.restore();
    }
  });

  it('switches once the user confirmed', async () => {
    const { router } = renderHeader({ rooms: twoRooms(), sharing: true });
    await openSwitcher();
    await userEvent.click(screen.getByRole('link', { name: /Games/ }));
    await userEvent.click(screen.getByRole('button', { name: 'Stop sharing and switch' }));
    expect(router.state.location.pathname).toBe('/r/games');
  });

  it('gives admins "Create room", a link to Admin → Rooms', async () => {
    const { router } = renderHeader({ rooms: twoRooms(), me: meFixture({ admin: true }) });
    await openSwitcher();
    const create = screen.getByRole('link', { name: 'Create room' });
    expect(create).toHaveAttribute('href', '/admin/rooms');
    await userEvent.click(create);
    expect(router.state.location.pathname).toBe('/admin/rooms');
  });

  it('gives members no "Create room"', async () => {
    renderHeader({ rooms: twoRooms(), me: meFixture() });
    await openSwitcher();
    expect(screen.getByRole('dialog', { name: 'Rooms' })).toBeInTheDocument();
    expect(screen.queryByRole('link', { name: 'Create room' })).not.toBeInTheDocument();
  });

  it('leaves the keyboard focus on a room’s link when the header renders again', async () => {
    const { setProps, onOpenRooms } = renderHeader({ rooms: twoRooms() });
    await openSwitcher();
    const games = screen.getByRole('link', { name: /Games/ });
    games.focus();
    expect(games).toHaveFocus();
    // What the page does on every room.state, and when the list it refetched on opening arrives.
    setProps({ peopleCount: 6, rooms: twoRooms() });
    expect(screen.getByRole('button', { name: /^6 here/ })).toBeInTheDocument();
    expect(screen.getByRole('link', { name: /Games/ })).toHaveFocus();
    expect(screen.getByRole('dialog', { name: 'Rooms' })).toBeInTheDocument();
    expect(onOpenRooms).toHaveBeenCalledOnce();
  });

  it('tells the page’s newest onOpenRooms that the list opened', async () => {
    const { setProps, onOpenRooms } = renderHeader({ rooms: twoRooms() });
    const newest = vi.fn();
    setProps({ onOpenRooms: newest });
    await openSwitcher();
    expect(newest).toHaveBeenCalledOnce();
    expect(onOpenRooms).not.toHaveBeenCalled();
  });
});

describe('the account menu', () => {
  const links = (): Record<string, string | null> =>
    Object.fromEntries(
      within(screen.getByRole('dialog', { name: 'Account menu' }))
        .getAllByRole('link')
        .map((a) => [a.textContent, a.getAttribute('href')]),
    );

  it('says who is signed in, and leads a member to their account pages', async () => {
    renderHeader({ me: meFixture() });
    await userEvent.click(screen.getByRole('button', { name: 'Account menu, alex' }));
    expect(screen.getByText('Signed in as alex')).toBeInTheDocument();
    // No Admin (others would get NotFound there) and no Invite friends without the permission.
    expect(links()).toEqual({
      Account: '/account',
      Notifications: '/account/notifications',
      'About isshoni': '/about',
    });
    expect(screen.getByRole('button', { name: 'Sign out' })).toBeEnabled();
  });

  it('gives members who may invite "Invite friends", and "My invites", a link to their invites', async () => {
    renderHeader({ me: meFixture({ createInvites: true }) });
    await openMenu();
    expect(screen.getByRole('button', { name: 'Invite friends' })).toHaveAttribute('aria-haspopup', 'dialog');
    expect(links()).toMatchObject({ 'My invites': '/admin/invites' });
    expect(links()).not.toHaveProperty('Admin');
    expect(links()).not.toHaveProperty('Invite friends');
  });

  it('gives admins "Admin" and "Invite friends"; their invites are under Admin', async () => {
    renderHeader({ me: meFixture({ admin: true }) });
    await openMenu();
    expect(screen.getByRole('button', { name: 'Invite friends' })).toBeInTheDocument();
    expect(links()).toMatchObject({ Admin: '/admin' });
    expect(links()).not.toHaveProperty('My invites');
  });

  // 03 §7.9: while registration is closed, creating an invite is refused to everyone, admins too.
  it('offers no "Invite friends" to an admin while registration is closed', async () => {
    renderHeader({ me: meFixture({ admin: true }) }, null, infoFixture({ registration: 'closed' }));
    await openMenu();
    expect(screen.queryByRole('button', { name: 'Invite friends' })).not.toBeInTheDocument();
    expect(links()).toMatchObject({ Admin: '/admin' });
  });

  it('offers no "Invite friends" to a member while registration is closed, but still "My invites"', async () => {
    renderHeader({ me: meFixture({ createInvites: true }) }, null, infoFixture({ registration: 'closed' }));
    await openMenu();
    expect(screen.queryByRole('button', { name: 'Invite friends' })).not.toBeInTheDocument();
    // They still see and revoke their invites there.
    expect(links()).toMatchObject({ 'My invites': '/admin/invites' });
  });

  // A member gets no admin.settings invalidation (01 §8.12), and the room page stays open for hours: the menu reads
  // /info again when it opens, once the one it has is older than staleTime.
  it("reads the registration mode again when a member's menu opens, once it is 30 s old", async () => {
    let registration: Info['registration'] = 'invite';
    let reads = 0;
    server.use(
      http.get(apiPath('/api/v1/info'), () => {
        reads += 1;
        return HttpResponse.json(infoFixture({ registration }));
      }),
    );
    const later = (ms: number): void => {
      vi.setSystemTime(Date.now() + ms);
    };
    vi.useFakeTimers({ toFake: ['Date'] });
    try {
      renderHeader({ me: meFixture({ createInvites: true }) });
      await openMenu();
      expect(screen.getByRole('button', { name: 'Invite friends' })).toBeInTheDocument();
      await userEvent.keyboard('{Escape}');

      // An admin closes registration. Within staleTime the menu keeps what boot read.
      registration = 'closed';
      later(29_000);
      await openMenu();
      expect(screen.getByRole('button', { name: 'Invite friends' })).toBeInTheDocument();
      await userEvent.keyboard('{Escape}');
      expect(reads).toBe(0);

      later(2_000);
      await openMenu();
      await waitFor(() => {
        expect(screen.queryByRole('button', { name: 'Invite friends' })).not.toBeInTheDocument();
      });
      expect(reads).toBe(1);
      expect(links()).toMatchObject({ 'My invites': '/admin/invites' });
      await userEvent.keyboard('{Escape}');

      // And opens it again: the entry comes back without a reload.
      registration = 'invite';
      later(31_000);
      await openMenu();
      expect(await screen.findByRole('button', { name: 'Invite friends' })).toBeInTheDocument();
      expect(reads).toBe(2);
    } finally {
      vi.useRealTimers();
    }
  });

  it('"Invite friends" closes the menu and opens the invite card with a new invite; closing it returns the focus to the menu’s button', async () => {
    const bodies: unknown[] = [];
    // The server answers once the test looked at the focus: the link takes it when it arrives.
    let answer: () => void = () => undefined;
    const answered = new Promise<void>((resolve) => {
      answer = resolve;
    });
    server.use(
      http.post(apiPath('/api/v1/invites'), async ({ request }) => {
        bodies.push(await request.json());
        await answered;
        return HttpResponse.json(inviteFixture(), { status: 201 });
      }),
    );
    const dialogs = stubNativeDialog();
    try {
      renderHeader({ me: meFixture({ createInvites: true }) });
      await openMenu();
      await userEvent.click(screen.getByRole('button', { name: 'Invite friends' }));
      expect(screen.queryByRole('dialog', { name: 'Account menu' })).not.toBeInTheDocument();
      // The entry went with the menu: the dialog gives the focus back to the menu's button, which stays.
      expect(screen.getByRole('button', { name: /^Account menu/ })).toHaveFocus();

      const invite = screen.getByRole('dialog', { name: 'Invite friends' });
      expect(within(invite).getByText('Making an invite link…')).toBeInTheDocument();
      answer();
      expect(await within(invite).findByRole('textbox', { name: 'Invite link' })).toHaveValue(INVITE_LINK);
      expect(bodies).toEqual([{}]);
      // The menu tells the card who reads it: a member finds the invite under My invites, as their menu has it.
      expect(within(invite).getByRole('link', { name: 'My invites' })).toHaveAttribute('href', '/admin/invites');

      await userEvent.click(within(invite).getByRole('button', { name: 'Done' }));
      expect(screen.queryByRole('dialog')).not.toBeInTheDocument();
      expect(dialogs.closes).toEqual([{ title: 'Invite friends', inPage: true }]);
    } finally {
      dialogs.restore();
    }
  });

  it('keeps a link it made on screen when the permission goes meanwhile', async () => {
    server.use(http.post(apiPath('/api/v1/invites'), () => HttpResponse.json(inviteFixture(), { status: 201 })));
    const dialogs = stubNativeDialog();
    try {
      const { setProps } = renderHeader({ me: meFixture({ createInvites: true }) });
      await openMenu();
      await userEvent.click(screen.getByRole('button', { name: 'Invite friends' }));
      const invite = screen.getByRole('dialog', { name: 'Invite friends' });
      expect(await within(invite).findByRole('textbox', { name: 'Invite link' })).toHaveValue(INVITE_LINK);

      // An admin turned membersCanInvite off: ['me'] says so now (03 §10).
      setProps({ me: meFixture() });
      expect(within(invite).getByRole('textbox', { name: 'Invite link' })).toHaveValue(INVITE_LINK);
      await userEvent.click(within(invite).getByRole('button', { name: 'Done' }));
      expect(dialogs.closes).toEqual([{ title: 'Invite friends', inPage: true }]);
      await openMenu();
      expect(screen.queryByRole('button', { name: 'Invite friends' })).not.toBeInTheDocument();
    } finally {
      dialogs.restore();
    }
  });

  it('closes when a link is followed', async () => {
    const { router } = renderHeader();
    await openMenu();
    await userEvent.click(screen.getByRole('link', { name: 'Account' }));
    expect(router.state.location.pathname).toBe('/account');
  });

  it('offers "Install app" only while the browser has an install prompt', async () => {
    const promptInstall = vi.fn(() => Promise.resolve('accepted' as const));
    let state: ReturnType<PwaProvider['installState']> = 'none';
    const pwa: PwaProvider = {
      installState: () => state,
      promptInstall,
      onUpdateReady: () => () => undefined,
      applyUpdate: () => undefined,
    };
    renderHeader({}, pwa);
    await openMenu();
    expect(screen.queryByRole('button', { name: 'Install app' })).not.toBeInTheDocument();
    await userEvent.keyboard('{Escape}');

    // Read again each time the menu opens.
    state = 'prompt';
    await openMenu();
    await userEvent.click(screen.getByRole('button', { name: 'Install app' }));
    expect(promptInstall).toHaveBeenCalledOnce();
  });

  it('has no "Test my connection" unless the page offers the test', async () => {
    renderHeader();
    await openMenu();
    expect(screen.queryByRole('button', { name: 'Test my connection' })).not.toBeInTheDocument();
  });

  it('"Test my connection" (05 §14.2) closes the menu, asks the page for the test, and leaves the focus on the menu’s button', async () => {
    const onTestConnection = vi.fn();
    renderHeader({ onTestConnection });
    await openMenu();
    await userEvent.click(screen.getByRole('button', { name: 'Test my connection' }));
    expect(onTestConnection).toHaveBeenCalledOnce();
    expect(screen.queryByRole('dialog', { name: 'Account menu' })).not.toBeInTheDocument();
    // The entry went with the menu. The test's dialog returns the focus to where it was when it opened, so that
    // must be something that stays: the menu's button.
    expect(screen.getByRole('button', { name: /^Account menu/ })).toHaveFocus();
  });

  it('Sign out runs the logout flow: the session ends on the server and the user is forgotten here', async () => {
    const requests: string[] = [];
    server.use(
      http.post(apiPath('/api/v1/auth/logout'), () => {
        requests.push('logout');
        return new HttpResponse(null, { status: 204 });
      }),
    );
    const { services } = renderHeader();
    services.queryClient.setQueryData(queryKeys.me, meFixture());
    await openMenu();
    await userEvent.click(screen.getByRole('button', { name: 'Sign out' }));
    await waitFor(() => {
      expect(services.queryClient.getQueryData(queryKeys.me)).toBeNull();
    });
    expect(requests).toEqual(['logout']);
  });
});
