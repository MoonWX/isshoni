// The checks of doctor (04 §13.2), as the Doctor page names them. A module of its own, without imports, so that the
// catalog test (doctorCatalog.node.test.ts, which runs in Node) can compare it with doctor's own list.

/** The check ids of 04 §13.2, in its order (doctor.CheckIDs()). The page names each from admin.doctor.check.<id>. */
export const CHECK_IDS = [
  'config',
  'data_dir',
  'secrets',
  'schema',
  'public_ip',
  'dns',
  'tls',
  'clock',
  'udp_buffers',
  'ports',
  'firewall_hint',
  'container',
  'lan_privacy',
  'transfer',
  'release',
  'nofile',
  'bandwidth',
] as const;

export type CheckId = (typeof CHECK_IDS)[number];

/** Whether a check id is one this build has a name for (a newer server may have more). */
export function isCheckId(id: string): id is CheckId {
  return (CHECK_IDS as readonly string[]).includes(id);
}
