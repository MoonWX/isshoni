// /admin/rooms (05 §15.3, 03 §8, §12.4.4) against MSW: the list, create, rename, delete, and the default room that
// can't be deleted.
import { screen, waitFor, within } from '@testing-library/react';
import { HttpResponse } from 'msw';
import { describe, expect, it } from 'vitest';

import type { Room } from '../protocol/api.gen';
import { apiError } from '../test/msw';
import { checkRoomName } from './RoomsPage';
import { dialog, LOUNGE, noContent, on, renderAdmin, room, rowOf, serve, toasts, typeInto } from './testing/harness';

async function openRooms(initial: Room[] = [LOUNGE, room()]) {
  const state = { rooms: initial };
  const gets = serve('/api/v1/rooms', () => ({
    defaultRoomId: 'lounge',
    showRoomList: state.rooms.length > 1,
    rooms: state.rooms,
  }));
  const page = renderAdmin({ path: '/admin/rooms' });
  await screen.findByRole('table', { name: 'Rooms' });
  return { state, gets, ...page };
}

describe('RoomsPage', () => {
  it('lists the rooms with who is in them, and links to each', async () => {
    await openRooms([
      { ...LOUNGE, live: { participants: 3, shares: 1 } },
      room({ live: { participants: 1, shares: 2 } }),
      room({ id: 'g1g1g1g1g1g1', name: 'Games' }),
    ]);
    expect(screen.getAllByRole('columnheader').map((th) => th.textContent)).toEqual(['Room', 'In it now', 'Created']);
    const lounge = within(rowOf(/^Lounge/));
    expect(lounge.getByRole('link', { name: 'Lounge' })).toHaveAttribute('href', '/r/lounge');
    expect(lounge.getByText('Default')).toBeInTheDocument();
    expect(lounge.getByText('3 people, 1 share')).toBeInTheDocument();
    expect(within(rowOf(/^Movie night/)).getByText('1 person, 2 shares')).toBeInTheDocument();
    expect(within(rowOf(/^Games/)).getByText('Nobody')).toBeInTheDocument();
  });

  it('offers Rename on every room and Delete on all but the default room', async () => {
    await openRooms();
    const lounge = within(rowOf(/^Lounge/));
    expect(lounge.getByRole('button', { name: 'Rename Lounge' })).toBeInTheDocument();
    expect(lounge.queryByRole('button', { name: /Delete/ })).not.toBeInTheDocument();
    const movies = within(rowOf(/^Movie night/));
    expect(movies.getByRole('button', { name: 'Rename Movie night' })).toBeInTheDocument();
    expect(movies.getByRole('button', { name: 'Delete Movie night' })).toBeInTheDocument();
  });

  describe('create', () => {
    it('creates a room and clears the field', async () => {
      const { user, state, services } = await openRooms([LOUNGE]);
      const posted = on('post', '/api/v1/admin/rooms', ({ body }) => {
        const created = room({ id: 'n1n1n1n1n1n1', name: (body as { name: string }).name });
        state.rooms = [...state.rooms, created];
        return HttpResponse.json({ room: created }, { status: 201 });
      });
      const name = screen.getByLabelText('Room name');
      await user.type(name, '  🎬 Movie night ');
      await user.click(screen.getByRole('button', { name: 'Create room' }));
      expect(await screen.findByRole('row', { name: /^🎬 Movie night/ })).toBeInTheDocument();
      expect(posted.map((p) => p.body)).toEqual([{ name: '🎬 Movie night' }]);
      expect(name).toHaveValue('');
      expect(toasts(services)).toEqual(['🎬 Movie night was created.']);
    });

    it('asks for a name before it sends anything', async () => {
      const { user } = await openRooms();
      const posted = on('post', '/api/v1/admin/rooms', () => apiError(500, { code: 'internal' }));
      await user.click(screen.getByRole('button', { name: 'Create room' }));
      const name = screen.getByLabelText('Room name');
      expect(name).toHaveAccessibleDescription(expect.stringContaining("This can't be empty."));
      expect(name).toBeInvalid();
      expect(name).toHaveFocus();
      expect(posted).toEqual([]);
    });

    it.each([
      ['room_name_taken', 409, { code: 'room_name_taken' }, 'A room with that name already exists.'],
      ['too_long', 422, { code: 'validation_failed', fields: { name: 'too_long' } }, "That's too long."],
      ['too_short', 422, { code: 'validation_failed', fields: { name: 'too_short' } }, "That's too short."],
      ['invalid', 422, { code: 'validation_failed', fields: { name: 'invalid' } }, "That value can't be used."],
    ] as const)('shows %s under the name', async (_name, status, error, shown) => {
      const { user } = await openRooms();
      on('post', '/api/v1/admin/rooms', () => apiError(status, error));
      const name = screen.getByLabelText('Room name');
      await user.type(name, 'Lounge');
      await user.click(screen.getByRole('button', { name: 'Create room' }));
      await waitFor(() => {
        expect(name).toHaveAccessibleDescription(expect.stringContaining(shown));
      });
      expect(name).toBeInvalid();
      // What was typed stays, to be fixed.
      expect(name).toHaveValue('Lounge');
    });

    it('shows the room limit above the form', async () => {
      const { user } = await openRooms();
      on('post', '/api/v1/admin/rooms', () => apiError(409, { code: 'limit_reached', params: { limit: 'rooms' } }));
      await user.type(screen.getByLabelText('Room name'), 'One more');
      await user.click(screen.getByRole('button', { name: 'Create room' }));
      expect(await screen.findByRole('alert')).toHaveTextContent(
        'This server has as many rooms as it can hold. Delete one first.',
      );
    });
  });

  describe('rename', () => {
    it('renames a room, the default one too', async () => {
      const { user, state, services } = await openRooms();
      const patched = on('patch', '/api/v1/admin/rooms/:id', ({ body, params }) => {
        state.rooms = state.rooms.map((r) => (r.id === params['id'] ? { ...r, ...(body as { name: string }) } : r));
        return HttpResponse.json({ room: state.rooms.find((r) => r.id === params['id']) });
      });
      await user.click(within(rowOf(/^Lounge/)).getByRole('button', { name: 'Rename Lounge' }));
      const box = within(dialog('Rename Lounge'));
      const name = box.getByLabelText('Room name');
      expect(name).toHaveValue('Lounge');
      await typeInto(user, name, 'Living room');
      await user.click(box.getByRole('button', { name: 'Rename' }));
      expect(await screen.findByRole('row', { name: /^Living room/ })).toBeInTheDocument();
      expect(patched).toEqual([
        expect.objectContaining({ url: '/api/v1/admin/rooms/lounge', body: { name: 'Living room' } }),
      ]);
      expect(toasts(services)).toEqual(['Lounge is now Living room.']);
      expect(screen.queryByRole('dialog')).not.toBeInTheDocument();
    });

    it('sends nothing for the same name', async () => {
      const { user } = await openRooms();
      const patched = on('patch', '/api/v1/admin/rooms/:id', () => apiError(500, { code: 'internal' }));
      await user.click(screen.getByRole('button', { name: 'Rename Movie night' }));
      await user.click(within(dialog('Rename Movie night')).getByRole('button', { name: 'Rename' }));
      expect(screen.queryByRole('dialog')).not.toBeInTheDocument();
      expect(patched).toEqual([]);
    });

    it.each([
      ['room_name_taken', 409, { code: 'room_name_taken' }, 'A room with that name already exists.'],
      ['a field code', 422, { code: 'validation_failed', fields: { name: 'invalid' } }, "That value can't be used."],
    ] as const)('shows %s under the name', async (_name, status, error, shown) => {
      const { user } = await openRooms();
      on('patch', '/api/v1/admin/rooms/:id', () => apiError(status, error));
      await user.click(screen.getByRole('button', { name: 'Rename Movie night' }));
      const box = within(dialog('Rename Movie night'));
      const name = box.getByLabelText('Room name');
      await typeInto(user, name, 'Lounge');
      await user.click(box.getByRole('button', { name: 'Rename' }));
      await waitFor(() => {
        expect(name).toHaveAccessibleDescription(expect.stringContaining(shown));
      });
    });

    it('says so when the room is gone', async () => {
      const { user } = await openRooms();
      on('patch', '/api/v1/admin/rooms/:id', () => apiError(404, { code: 'room_not_found' }));
      await user.click(screen.getByRole('button', { name: 'Rename Movie night' }));
      const box = within(dialog('Rename Movie night'));
      await typeInto(user, box.getByLabelText('Room name'), 'Films');
      await user.click(box.getByRole('button', { name: 'Rename' }));
      expect(await box.findByRole('alert')).toHaveTextContent("That room doesn't exist anymore.");
    });
  });

  describe('delete', () => {
    it('deletes a room after a question that says who is in it', async () => {
      const { user, state, services } = await openRooms([LOUNGE, room({ live: { participants: 2, shares: 1 } })]);
      const deleted = on('delete', '/api/v1/admin/rooms/:id', ({ params }) => {
        state.rooms = state.rooms.filter((r) => r.id !== params['id']);
        return noContent();
      });
      await user.click(screen.getByRole('button', { name: 'Delete Movie night' }));
      const box = within(dialog('Delete Movie night?'));
      expect(box.getByText(/2 people are in it right now\. They move to the default room/)).toBeInTheDocument();
      await user.click(box.getByRole('button', { name: 'Delete room' }));
      await waitFor(() => {
        expect(screen.queryByRole('row', { name: /^Movie night/ })).not.toBeInTheDocument();
      });
      expect(deleted.map((d) => d.url)).toEqual(['/api/v1/admin/rooms/p4t7w2m9k1qs']);
      expect(toasts(services)).toEqual(['Movie night was deleted.']);
      // The button that opened the dialog is gone with the row: focus is on the page's heading, not nowhere.
      expect(screen.getByRole('heading', { level: 1, name: 'Rooms' })).toHaveFocus();
    });

    it('does not mention people for an empty room', async () => {
      const { user } = await openRooms();
      await user.click(screen.getByRole('button', { name: 'Delete Movie night' }));
      const box = within(dialog('Delete Movie night?'));
      expect(box.getByText(/removed for everyone/)).toBeInTheDocument();
      expect(box.queryByText(/right now/)).not.toBeInTheDocument();
    });

    it('explains room_is_default when the server refuses', async () => {
      // The list was behind: the room became the default one meanwhile.
      const { user, gets } = await openRooms();
      on('delete', '/api/v1/admin/rooms/:id', () => apiError(409, { code: 'room_is_default' }));
      await user.click(screen.getByRole('button', { name: 'Delete Movie night' }));
      const before = gets.length;
      const box = within(dialog('Delete Movie night?'));
      await user.click(box.getByRole('button', { name: 'Delete room' }));
      expect(await box.findByRole('alert')).toHaveTextContent("The default room can't be deleted.");
      await waitFor(() => {
        expect(gets.length).toBeGreaterThan(before);
      });
    });
  });

  it('checkRoomName: required, too_long by characters, else fine', () => {
    expect(checkRoomName('')).toBe('required');
    expect(checkRoomName('   ')).toBe('required');
    expect(checkRoomName('a')).toBeNull();
    expect(checkRoomName('x'.repeat(40))).toBeNull();
    expect(checkRoomName('x'.repeat(41))).toBe('too_long');
    // 40 emoji are 40 characters for the server, though 80 UTF-16 units.
    expect(checkRoomName('🎬'.repeat(40))).toBeNull();
  });
});
