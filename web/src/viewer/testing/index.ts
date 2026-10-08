// Wire fixtures for the viewer's tests: shares and participants as room.state has them (01 §8.5). Only tests import
// this folder.
import type { ParticipantInfo, ShareInfo, SubscriptionStatus } from '../../protocol/types.gen';
import type { RoomSnapshot, ViewerSelf } from '../viewerStore';

/** This page in the fixtures: Alex, on connection c_me. */
export const SELF: ViewerSelf = { userId: 'u_alex', connectionId: 'c_me' };

/** A share that started `minute` minutes into the hour; live, a window, nobody watching, unless overridden. */
export function shareInfo(id: string, userId: string, minute: number, overrides: Partial<ShareInfo> = {}): ShareInfo {
  return {
    id,
    userId,
    connectionId: `c_${userId}`,
    kind: 'window',
    preset: 'auto',
    audio: true,
    status: 'live',
    layers: ['high', 'low'],
    codec: 'h264/6400',
    startedAt: `2026-10-12T19:${String(minute).padStart(2, '0')}:00.000Z`,
    watchers: [],
    ...overrides,
  };
}

export function participant(userId: string, name: string): ParticipantInfo {
  return { userId, name, status: 'present', joinedAt: '2026-10-12T19:00:00.000Z', connections: [] };
}

/** A room.state with these shares; Alex (this page), Bea and Cy are in the room. */
export function room(...shares: ShareInfo[]): RoomSnapshot {
  return {
    shares,
    participants: [participant('u_alex', 'Alex'), participant('u_bea', 'Bea'), participant('u_cy', 'Cy')],
  };
}

export function status(shareId: string, overrides: Partial<SubscriptionStatus> = {}): SubscriptionStatus {
  return { shareId, video: 'high', audio: 'on', requestedVideo: 'high', ...overrides };
}
