// HttpBridge (docs/m2/03 §7.4, §22.4 A): the NativeBridge over plain HTTP, for the desktop-in-Chrome e2e harness
// (internal/desktop/e2ehost), which serves the SPA with bridge.js `transport: "http"`. Nothing else selects it, and
// DesktopPlatform loads it with a dynamic import(), so it is never in the entry chunk.
//
// The wire, which e2ehost implements on the asset origin:
// - a call: POST /__bridge/call with Content-Type: application/json and {"service", "method", "args"}, where service
//   and method are the bound Go names (group `auth` → service `Auth`, method `accessToken` → `AccessToken`) and args
//   is the argument list. 2xx: the body is the method's JSON result (null, or no body, for none). Any other status:
//   the body is the bridge error JSON {"code", "params"} (docs/m2/03 §6.4).
// - events: an EventSource on /__bridge/events; every message's data is {"name": "isshoni:<event>", "data": <payload>}.
import { createLogger } from '../../../lib/log';
import { BridgeEventPrefix, VoidMethods, type NativeBridge } from '../bridge';
import { BridgeCodeInternal, BridgeError, bridgeErrorFromJSON } from '../errors';

export const BridgeCallPath = '/__bridge/call';
export const BridgeEventsPath = '/__bridge/events';

export interface HttpBridgeOptions {
  /** Default: globalThis.fetch, looked up per call (test interceptors). */
  fetch?: typeof fetch;
  /** Default: globalThis.EventSource. */
  EventSource?: typeof EventSource;
  /** Where the paths resolve; default: location.href (the asset origin). */
  base?: string;
}

const GROUPS: ReadonlySet<string> = new Set(['app', 'auth', 'share', 'audio', 'settings', 'window', 'updates']);

const goName = (s: string): string => s.charAt(0).toUpperCase() + s.slice(1);

type Listener = (payload: unknown) => void;

/** Calls a bound method over HTTP; resolves with its result, rejects with a BridgeError. */
async function call(opts: HttpBridgeOptions, service: string, method: string, args: unknown[]): Promise<unknown> {
  const doFetch = opts.fetch ?? globalThis.fetch;
  // Trailing undefined arguments are left out (an optional parameter), the way a bound call omits them.
  let n = args.length;
  while (n > 0 && args[n - 1] === undefined) n--;
  let res: Response;
  try {
    res = await doFetch(new URL(BridgeCallPath, opts.base ?? globalThis.location.href), {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ service, method, args: args.slice(0, n) }),
      credentials: 'same-origin',
    });
  } catch (err) {
    throw new BridgeError(BridgeCodeInternal, {}, { cause: err });
  }
  const text = await res.text().catch(() => '');
  let body: unknown = null;
  try {
    body = text === '' ? null : JSON.parse(text);
  } catch {
    throw new BridgeError(BridgeCodeInternal, { status: res.status });
  }
  if (!res.ok) throw bridgeErrorFromJSON(body) ?? new BridgeError(BridgeCodeInternal, { status: res.status });
  return body;
}

/** share.stats' sample gets its receipt time here (docs/m2/03 §6.3). */
function stamp(key: string, v: unknown): unknown {
  if (key === 'share.stats' && typeof v === 'object' && v !== null) return { ...v, at: performance.now() };
  return v;
}

export function createHttpBridge(opts: HttpBridgeOptions = {}): NativeBridge {
  const log = createLogger('desktop/http-bridge');
  const listeners = new Map<string, Set<Listener>>();

  const ES = opts.EventSource ?? globalThis.EventSource;
  // Opened at once, so the stream is usually up before the SPA subscribes and pulls (docs/m2/03 §6.3).
  const events = new ES(new URL(BridgeEventsPath, opts.base ?? globalThis.location.href).href);
  events.onmessage = (ev: MessageEvent) => {
    let msg: unknown;
    try {
      msg = JSON.parse(String(ev.data));
    } catch {
      log.warn('dropped a malformed bridge event');
      return;
    }
    if (typeof msg !== 'object' || msg === null) return;
    const { name, data } = msg as { name?: unknown; data?: unknown };
    if (typeof name !== 'string' || !name.startsWith(BridgeEventPrefix)) return;
    const event = name.slice(BridgeEventPrefix.length);
    for (const fn of [...(listeners.get(event) ?? [])]) {
      try {
        fn(stamp(event, data));
      } catch (err) {
        log.error('a bridge event listener failed', { event, err });
      }
    }
  };

  const group = (name: string): Record<string, (...args: unknown[]) => unknown> =>
    new Proxy<Record<string, (...args: unknown[]) => unknown>>(
      {},
      {
        get: (_t, method) => {
          if (typeof method !== 'string' || method === 'then' || method === 'toJSON') return undefined;
          const key = `${name}.${method}`;
          return (...args: unknown[]): unknown => {
            const p = call(opts, goName(name), goName(method), args).then((v) => stamp(key, v));
            if (!VoidMethods.has(key)) return p;
            p.catch((err: unknown) => {
              log.warn('a bridge call failed', { call: key, err });
            });
            return undefined;
          };
        },
      },
    );

  const on: NativeBridge['on'] = (e, fn) => {
    let set = listeners.get(e);
    if (!set) {
      set = new Set();
      listeners.set(e, set);
    }
    const l = fn as Listener;
    set.add(l);
    return () => {
      listeners.get(e)?.delete(l);
    };
  };

  const groups = new Map<string, ReturnType<typeof group>>();
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
