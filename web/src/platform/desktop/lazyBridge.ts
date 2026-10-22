// The bridge DesktopPlatform hands out before its transport has loaded (docs/m2/03 §7.2): the transport module
// (WailsBridge or HttpBridge) comes through a dynamic import() started in the constructor, so that the entry chunk
// carries neither the Wails runtime nor the generated bindings. A call made earlier waits for the load; a load that
// fails rejects every call with its error. One small Proxy instead of a wrapper per method keeps this file tiny.
import { VoidMethods, type NativeBridge } from './bridge';

type Fn = (...args: unknown[]) => unknown;
type Group = Record<string, Fn>;

/** NativeBridge's method groups; any other property of the bridge is undefined. */
const GROUPS: ReadonlySet<string> = new Set(['app', 'auth', 'share', 'audio', 'settings', 'window', 'updates']);
/** Never methods: a group must not look like a promise (await) or serialize through a method. */
const NOT_METHODS: ReadonlySet<string> = new Set(['then', 'toJSON']);

/** Wraps a bridge that is still loading. report gets the failures of fire-and-forget calls (VoidMethods). */
export function lazyBridge(load: Promise<NativeBridge>, report: (what: string, err: unknown) => void): NativeBridge {
  let ready: NativeBridge | null = null;
  load.then(
    (b) => {
      ready = b;
    },
    (err: unknown) => {
      report('desktop: the bridge failed to load', err);
    },
  );
  const groups = new Map<string, Group>();
  const target = (b: NativeBridge, group: string): Group => (b as unknown as Record<string, Group>)[group] ?? {};

  const group = (name: string): Group =>
    new Proxy<Group>(
      {},
      {
        get: (_t, method) => {
          if (typeof method !== 'string' || NOT_METHODS.has(method)) return undefined;
          const key = `${name}.${method}`;
          return (...args: unknown[]): unknown => {
            const call = (b: NativeBridge): unknown => {
              const fn = target(b, name)[method];
              if (typeof fn !== 'function') throw new TypeError(`desktop: no bridge method ${key}`);
              return fn(...args);
            };
            if (VoidMethods.has(key)) {
              const run = ready ? Promise.resolve(ready).then(call) : load.then(call);
              run.catch((err: unknown) => {
                report(`desktop: ${key} failed`, err);
              });
              return undefined;
            }
            return ready ? Promise.resolve(ready).then(call) : load.then(call);
          };
        },
      },
    );

  const on: NativeBridge['on'] = (e, fn) => {
    if (ready) return ready.on(e, fn);
    let off: (() => void) | null = null;
    let cancelled = false;
    load.then(
      (b) => {
        if (!cancelled) off = b.on(e, fn);
      },
      () => undefined,
    );
    return () => {
      cancelled = true;
      off?.();
    };
  };

  return new Proxy({} as NativeBridge, {
    get: (_t, prop) => {
      if (prop === 'on') return on;
      if (typeof prop !== 'string' || !GROUPS.has(prop)) return undefined;
      let g = groups.get(prop);
      if (!g) {
        g = group(prop);
        groups.set(prop, g);
      }
      return g;
    },
  });
}
