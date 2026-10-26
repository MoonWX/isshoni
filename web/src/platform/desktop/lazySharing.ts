// platform.sharing of the desktop (docs/m2/03 §7.3, §10.2) as the entry chunk holds it: DesktopSharing, which writes
// share/'s store, loads with a dynamic import() started at once, and every call waits for it. current() and
// localConnectionIds() answer null and [] until it is there, as they do before its first view; ready() resolves once
// they say what the app knows, and onChange's listeners hear every change from the load on (the room runtime and the
// viewer check again then). Kept tiny for DesktopPlatform's budget (size.node.test.ts).
import type { SharingProvider } from '../types';
import type { NativeBridge } from './bridge';

export function lazySharing(bridge: NativeBridge, report: (err: unknown) => void): SharingProvider {
  let ready: SharingProvider | null = null;
  const listeners = new Set<() => void>();
  const load = import('./DesktopSharing').then((m) => {
    const s = new m.DesktopSharing(bridge);
    s.onChange(() => {
      for (const fn of [...listeners]) fn();
    });
    return (ready = s);
  });
  load.catch(report);
  return {
    mode: 'native',
    pick: (o) => load.then((s) => s.pick(o)),
    start: (src, o, ctx) => load.then((s) => s.start(src, o, ctx)),
    current: () => ready?.current?.() ?? null,
    localConnectionIds: () => ready?.localConnectionIds?.() ?? [],
    stopOther: (roomId, shareId) => load.then((s) => s.stopOther(roomId, shareId)),
    ready: () => load.then((s) => s.ready()),
    onChange: (fn) => {
      listeners.add(fn);
      return () => {
        listeners.delete(fn);
      };
    },
  };
}
