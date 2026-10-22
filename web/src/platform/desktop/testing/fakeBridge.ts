// A NativeBridge for tests (docs/m2/03 §22.3: "DesktopPlatform with a fake NativeBridge"). It behaves like Go's
// bridge where the SPA's safety depends on it, and records every call:
// - auth.accessToken(origin, force) answers null unless origin is the active server's and it is linked (03 §5.8); a
//   forced call issues a new token, a plain one repeats the current token;
// - auth.sessionEnded and auth.helloRefused reject with not_found for any origin but the active one;
// - app.openExternal refuses anything but an absolute https URL (03 §5.5);
// - on() keeps listeners and emit() delivers a Go event.
// Every other method records the call and resolves with the value set in `answers` (undefined by default).
import type {
  AccessToken,
  AuthState,
  BridgeEventName,
  BridgeEvents,
  NativeBridge,
  ShareCaps,
  UpdateStatus,
} from '../bridge';
import { BridgeError } from '../errors';

export interface BridgeCall {
  /** group.method, e.g. "auth.accessToken". */
  method: string;
  args: unknown[];
}

export interface FakeBridge {
  readonly bridge: NativeBridge;
  /** Every call, oldest first. */
  readonly calls: BridgeCall[];
  /** The active server's origin; '' = none. */
  active: string;
  linked: boolean;
  /** The lifetime of a token the fake issues, in ms from now(). */
  tokenTtlMs: number;
  now: () => number;
  /** What sessionEnded answers for the active origin. */
  sessionEndStatus: AuthState['status'];
  /** Answers of the other methods, by group.method (a function is called with the arguments). */
  answers: Record<string, unknown>;
  /** The tokens issued so far, oldest first. */
  readonly issued: string[];
  /** Delivers a Go event to the listeners. */
  emit<E extends BridgeEventName>(e: E, payload: BridgeEvents[E]): void;
  listeners(e: BridgeEventName): number;
  /** The calls of one method. */
  callsOf(method: string): unknown[][];
}

export function authState(origin: string, status: AuthState['status'] = 'linked'): AuthState {
  return {
    origin,
    serverName: 'Test server',
    status,
    link: null,
    user: status === 'linked' ? { id: 'u1', username: 'alex', role: 'user' } : null,
    device: status === 'linked' ? { id: 'd1', name: 'Test PC' } : null,
    notice: '',
    compat: { verdict: 'ok', reason: '', serverVersion: '0.2.0', minClientVersion: '' },
  };
}

export const shareCaps = (canShare: boolean): ShareCaps => ({
  canShare,
  encoders: ['fake'],
  borderless: true,
  osBuild: 26100,
  audioBackend: 'process-loopback',
});

export function updateStatus(state: UpdateStatus['state'], over: Partial<UpdateStatus> = {}): UpdateStatus {
  return {
    state,
    current: '0.2.0',
    available: '',
    required: false,
    notesUrl: '',
    progress: 0,
    checkedAt: '0001-01-01T00:00:00Z',
    errorCode: '',
    policyOff: false,
    ...over,
  };
}

const GROUPS = ['app', 'auth', 'share', 'audio', 'settings', 'window', 'updates'];

export function createFakeBridge(init: { active?: string; linked?: boolean } = {}): FakeBridge {
  const listeners = new Map<string, Set<(p: unknown) => void>>();
  let current: AccessToken | null = null;

  const fake: FakeBridge = {
    bridge: undefined as unknown as NativeBridge,
    calls: [],
    active: init.active ?? '',
    linked: init.linked ?? true,
    tokenTtlMs: 15 * 60_000,
    now: () => Date.now(),
    sessionEndStatus: 'linked',
    answers: {},
    issued: [],
    emit(e, payload) {
      for (const fn of [...(listeners.get(e) ?? [])]) fn(payload);
    },
    listeners: (e) => listeners.get(e)?.size ?? 0,
    callsOf: (method) => fake.calls.filter((c) => c.method === method).map((c) => c.args),
  };

  const special: Record<string, (...args: unknown[]) => Promise<unknown>> = {
    'auth.accessToken': (origin, force) => {
      if (origin !== fake.active || !fake.linked || fake.active === '') return Promise.resolve(null);
      if (force === true || current === null) {
        const token = `isa_fake${String(fake.issued.length + 1).padStart(4, '0')}`;
        fake.issued.push(token);
        current = { token, expiresAt: fake.now() + fake.tokenTtlMs };
      }
      return Promise.resolve({ ...current });
    },
    'auth.sessionEnded': (origin) =>
      origin === fake.active && fake.active !== ''
        ? Promise.resolve(authState(fake.active, fake.sessionEndStatus))
        : Promise.reject(new BridgeError('not_found')),
    'auth.helloRefused': (origin) =>
      origin === fake.active && fake.active !== ''
        ? Promise.resolve(authState(fake.active, 'incompatible'))
        : Promise.reject(new BridgeError('not_found')),
    'app.openExternal': (url) => {
      let ok = false;
      try {
        ok = new URL(String(url)).protocol === 'https:';
      } catch {
        ok = false;
      }
      return ok ? Promise.resolve(undefined) : Promise.reject(new BridgeError('invalid_argument', { field: 'url' }));
    },
  };

  const group = (name: string): Record<string, (...args: unknown[]) => unknown> =>
    new Proxy<Record<string, (...args: unknown[]) => unknown>>(
      {},
      {
        get: (_t, method) => {
          if (typeof method !== 'string' || method === 'then') return undefined;
          const key = `${name}.${method}`;
          return (...args: unknown[]): unknown => {
            fake.calls.push({ method: key, args });
            const answer = fake.answers[key];
            if (typeof answer === 'function') return (answer as (...a: unknown[]) => unknown)(...args);
            if (answer !== undefined) return Promise.resolve(answer);
            const fn = special[key];
            return fn ? fn(...args) : Promise.resolve(undefined);
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
    const l = fn as (p: unknown) => void;
    set.add(l);
    return () => {
      listeners.get(e)?.delete(l);
    };
  };
  const groups = Object.fromEntries(GROUPS.map((g) => [g, group(g)]));
  (fake as { bridge: NativeBridge }).bridge = { ...groups, on } as unknown as NativeBridge;
  return fake;
}
