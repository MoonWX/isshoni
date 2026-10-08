// The room list: GET /api/v1/rooms under the ['rooms'] query key (05 §6.2, 03 §8). It names the default room
// (the root redirect), says whether there is a list to show at all (showRoomList: a second room exists) and lists
// the rooms with their live counts (the switcher). 01's `invalidate` topic `rooms` refetches it after a room was
// created, renamed or deleted (protocol/invalidate.ts); the live counts are as old as the last fetch.
import { queryOptions, useQuery, type UseQueryResult } from '@tanstack/react-query';

import type { Room, Rooms } from '../protocol/api.gen';
import { queryKeys } from '../protocol/queryKeys';
import { api } from '../protocol/rest';

export function fetchRooms(signal?: AbortSignal): Promise<Rooms> {
  return api<Rooms>('GET', '/api/v1/rooms', undefined, signal ? { signal } : {});
}

export function roomsQueryOptions() {
  return queryOptions({
    queryKey: queryKeys.rooms,
    queryFn: ({ signal }) => fetchRooms(signal),
  });
}

export function useRooms(): UseQueryResult<Rooms> {
  return useQuery(roomsQueryOptions());
}

/** A room of the list by id. */
export function roomOf(rooms: Rooms | undefined, roomId: string | null | undefined): Room | undefined {
  return rooms?.rooms.find((r) => r.id === roomId);
}

/**
 * Where "/" goes (05 §5): the last room this device joined (isshoni.lastRoomId) if it still exists, else the
 * default room.
 */
export function homeRoomId(rooms: Rooms, lastRoomId: string | null): string {
  return lastRoomId !== null && roomOf(rooms, lastRoomId) !== undefined ? lastRoomId : rooms.defaultRoomId;
}
