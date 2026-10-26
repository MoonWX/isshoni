// platform.sharing of the desktop (docs/m2/03 §7.3, §10.2) as the entry chunk holds it: DesktopSharing, which writes
// share/'s store, loads with a dynamic import() started at once, and every call waits for it. current() and
// localConnectionIds() answer null and [] until it is there, as they do before its first view: the room runtime asks
// again with every room.state. Kept tiny for DesktopPlatform's budget (size.node.test.ts).
import type { SharingProvider } from '../types';
import type { NativeBridge } from './bridge';

export function lazySharing(bridge: NativeBridge, report: (err: unknown) => void): SharingProvider {
  let ready: SharingProvider | null = null;
  const load = import('./DesktopSharing').then((m) => (ready = new m.DesktopSharing(bridge)));
  load.catch(report);
  return {
    mode: 'native',
    pick: (o) => load.then((s) => s.pick(o)),
    start: (src, o, ctx) => load.then((s) => s.start(src, o, ctx)),
    current: () => ready?.current?.() ?? null,
    localConnectionIds: () => ready?.localConnectionIds?.() ?? [],
    stopOther: (roomId, shareId) => load.then((s) => s.stopOther(roomId, shareId)),
  };
}
