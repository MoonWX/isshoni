// The admin pages' REST calls (05 §6.2, §15.3; 03 §12.3 #19–#42): one query per list, under the shared query keys
// so 01's `invalidate` topics reach them (protocol/invalidate.ts), and one function per change.
//
// Lists refetch when the window gets focus again (05 §6.2: "refetchOnWindowFocus only for admin lists"): an admin
// who comes back to the tab sees what the others did meanwhile. The audit log doesn't: it is paged, and a refetch
// would ask for every loaded page again.
//
// refresh() is what a page calls after its own change went through. The server also sends an `invalidate` message
// for it, but only over the room's WebSocket, which an admin page opened by its address doesn't have.
import { infiniteQueryOptions, queryOptions, type QueryClient } from '@tanstack/react-query';

import type {
  AdminUserResponse,
  AdminUsersResponse,
  ApprovalsResponse,
  AuditPage,
  CreateInviteRequest,
  CreateInviteResponse,
  CreateRoomRequest,
  InvitesResponse,
  PasswordResetRequest,
  PatchRoomRequest,
  PatchUserRequest,
  RejectAllResponse,
  RejectRequest,
  ResetLink,
  RoomResponse,
  Rooms,
  Settings,
  SettingsResponse,
  SignOutResponse,
} from '../protocol/api.gen';
import { applyInvalidate } from '../protocol/invalidate';
import { queryKeys } from '../protocol/queryKeys';
import { api, type ApiPath } from '../protocol/rest';
import type { Topic } from '../protocol/types.gen';

/** One path segment: IDs are opaque strings from the API (05 §5). */
function seg(id: string): string {
  return encodeURIComponent(id);
}

/** GET /api/v1/admin/users (#29): every account, pending ones included. No IPs (03 §12.4.8). */
export function usersQueryOptions() {
  return queryOptions({
    queryKey: queryKeys.adminUsers,
    queryFn: ({ signal }) => api<AdminUsersResponse>('GET', '/api/v1/admin/users', undefined, { signal }),
    refetchOnWindowFocus: true,
  });
}

/** GET /api/v1/admin/approvals (#34): the pending sign-ups. */
export function approvalsQueryOptions() {
  return queryOptions({
    queryKey: queryKeys.adminApprovals,
    queryFn: ({ signal }) => api<ApprovalsResponse>('GET', '/api/v1/admin/approvals', undefined, { signal }),
    refetchOnWindowFocus: true,
  });
}

/**
 * GET /api/v1/invites?state=all (#21): admins get every invite, members their own (the server filters). Inactive
 * invites stay listed for 30 days; the page hides them by default, client-side (05 §15.3).
 */
export function invitesQueryOptions() {
  return queryOptions({
    queryKey: [...queryKeys.invites, { state: 'all' }] as const,
    queryFn: ({ signal }) => api<InvitesResponse>('GET', '/api/v1/invites?state=all', undefined, { signal }),
    refetchOnWindowFocus: true,
  });
}

/** GET /api/v1/rooms (#19). */
export function roomsQueryOptions() {
  return queryOptions({
    queryKey: queryKeys.rooms,
    queryFn: ({ signal }) => api<Rooms>('GET', '/api/v1/rooms', undefined, { signal }),
    refetchOnWindowFocus: true,
  });
}

/** GET /api/v1/admin/settings (#40): the settings, their defaults and the names config pins. */
export function settingsQueryOptions() {
  return queryOptions({
    queryKey: queryKeys.adminSettings,
    queryFn: ({ signal }) => api<SettingsResponse>('GET', '/api/v1/admin/settings', undefined, { signal }),
  });
}

/** How many rows one page of the audit log asks for (the server's default; it allows up to 200, 03 §6). */
export const AUDIT_PAGE_SIZE = 50;

/** The audit log's filters (03 §12.4.8). Empty strings mean "no filter". */
export interface AuditFilters {
  /** A prefix of the action, e.g. "user." or "auth.login_failed". */
  readonly action: string;
  /** The acting user's ID. */
  readonly actor: string;
  /** The target's ID: a user, session, invite or room. */
  readonly target: string;
}

export const NO_AUDIT_FILTERS: AuditFilters = { action: '', actor: '', target: '' };

/** The path of one audit page: `before` is the cursor (the previous page's nextBefore), absent for the newest. */
export function auditPath(filters: AuditFilters, before: number | null): ApiPath {
  const query = new URLSearchParams({ limit: String(AUDIT_PAGE_SIZE) });
  if (before !== null) query.set('before', String(before));
  if (filters.action !== '') query.set('action', filters.action);
  if (filters.actor !== '') query.set('actor', filters.actor);
  if (filters.target !== '') query.set('target', filters.target);
  return `/api/v1/admin/audit?${query.toString()}`;
}

/**
 * GET /api/v1/admin/audit (#42), newest first, paged by the id cursor: each page's nextBefore asks for the next
 * one, and null ends the log.
 */
export function auditQueryOptions(filters: AuditFilters) {
  return infiniteQueryOptions({
    queryKey: [...queryKeys.adminAudit, filters] as const,
    queryFn: ({ pageParam, signal }) => api<AuditPage>('GET', auditPath(filters, pageParam), undefined, { signal }),
    initialPageParam: null as number | null,
    getNextPageParam: (last) => last.nextBefore ?? undefined,
  });
}

/** A partial Settings: only the fields that change (03 §9; never a locked field, never null). */
export type SettingsPatch = Partial<Omit<Settings, 'setupWizardDone'>>;

/** The changes the admin pages make (03 §12.3 #22–#41). */
export const adminApi = {
  /** #30: rename, change the role (to admin: with currentPassword), disable or enable. */
  patchUser: (id: string, body: PatchUserRequest) =>
    api<AdminUserResponse>('PATCH', `/api/v1/admin/users/${seg(id)}`, body),
  /** #31 */
  deleteUser: (id: string) => api<undefined>('DELETE', `/api/v1/admin/users/${seg(id)}`),
  /** #32: a one-time reset link; currentPassword only when the target is an admin (03 §7.10). */
  resetPassword: (id: string, body: PasswordResetRequest) =>
    api<ResetLink>('POST', `/api/v1/admin/users/${seg(id)}/password-reset`, body),
  /** #33: ends every session and device of the user. */
  signOutUser: (id: string) => api<SignOutResponse>('POST', `/api/v1/admin/users/${seg(id)}/sign-out`),
  /** #35 */
  approve: (id: string) => api<AdminUserResponse>('POST', `/api/v1/admin/approvals/${seg(id)}/approve`),
  /** #36 */
  reject: (id: string) => api<undefined>('POST', `/api/v1/admin/approvals/${seg(id)}/reject`),
  /**
   * #36 in its "all" form (03 §7.9): `all` in place of the ID and {"all": true} in the body. Both are required,
   * so a stray request can't empty the queue.
   */
  rejectAll: () => {
    const body: RejectRequest = { all: true };
    return api<RejectAllResponse>('POST', '/api/v1/admin/approvals/all/reject', body);
  },
  /** #22: the reply's url is the only time the link is ever shown. */
  createInvite: (body: CreateInviteRequest) => api<CreateInviteResponse>('POST', '/api/v1/invites', body),
  /** #23 */
  revokeInvite: (id: string) => api<undefined>('DELETE', `/api/v1/invites/${seg(id)}`),
  /** #37 */
  createRoom: (body: CreateRoomRequest) => api<RoomResponse>('POST', '/api/v1/admin/rooms', body),
  /** #38 */
  patchRoom: (id: string, body: PatchRoomRequest) => api<RoomResponse>('PATCH', `/api/v1/admin/rooms/${seg(id)}`, body),
  /** #39 */
  deleteRoom: (id: string) => api<undefined>('DELETE', `/api/v1/admin/rooms/${seg(id)}`),
  /** #41 */
  patchSettings: (patch: SettingsPatch) => api<SettingsResponse>('PATCH', '/api/v1/admin/settings', patch),
} as const;

/**
 * After a change of this page went through: marks the REST data of these topics stale, exactly as the server's
 * `invalidate` message would (05 §6.2), and the audit log with it, since every admin change adds a row there.
 * Lists on screen refetch now; the others when next shown.
 */
export function refresh(queryClient: QueryClient, ...topics: Topic[]): void {
  void applyInvalidate(queryClient, { topics });
  void queryClient.invalidateQueries({ queryKey: queryKeys.adminAudit });
}
