// TanStack Query keys for the REST resources (05 §6.2). Every query uses one of these, so 01's `invalidate` topics
// (invalidate.ts) reach them. A key with parameters extends its base key (['invites', {state: 'all'}]), and
// invalidating the base key also invalidates every parameterized form (TanStack matches key prefixes). ['me'] is the
// exception: ['me','sessions'] and ['me','devices'] are resources of their own, so the `me` topic invalidates ['me']
// exactly.

export const queryKeys = {
  /** GET /api/v1/info: seeded at boot (05 §4). */
  info: ['info'],
  /** GET /api/v1/me: Me, or null when signed out (app/me.ts). */
  me: ['me'],
  /** GET /api/v1/me/sessions */
  meSessions: ['me', 'sessions'],
  /** GET /api/v1/me/devices (empty in M1) */
  meDevices: ['me', 'devices'],
  /** GET /api/v1/rooms */
  rooms: ['rooms'],
  /** GET /api/v1/invites?state=…: extend with the query, e.g. [...queryKeys.invites, {state: 'all'}] */
  invites: ['invites'],
  /** GET /api/v1/push/preferences */
  pushPreferences: ['push', 'preferences'],
  /** GET /api/v1/admin/users */
  adminUsers: ['admin', 'users'],
  /** GET /api/v1/admin/approvals */
  adminApprovals: ['admin', 'approvals'],
  /** GET /api/v1/admin/settings */
  adminSettings: ['admin', 'settings'],
  /** GET /api/v1/admin/audit: extend with the page cursor */
  adminAudit: ['admin', 'audit'],
  /** (04) GET /api/v1/admin/dashboard */
  adminDashboard: ['admin', 'dashboard'],
  /** (04) GET /api/v1/admin/doctor */
  adminDoctor: ['admin', 'doctor'],
  /** (04) GET /api/v1/admin/bandwidth: extend with the input */
  adminBandwidth: ['admin', 'bandwidth'],
} as const satisfies Record<string, readonly string[]>;

export type QueryKeyName = keyof typeof queryKeys;
export type AppQueryKey = (typeof queryKeys)[QueryKeyName];
