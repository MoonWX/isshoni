// A desktop-app Platform for the app's tests (docs/m2/03 §7.5, §22.3): test/platform.ts's test platform (REST through
// the global fetch, so MSW answers; memory storage) with the desktop's kind, client and role, and `desktop` features
// over platform/desktop's fake NativeBridge. openExternal records its URLs instead of opening anything.
import { signalStopped } from '../../platform/desktop/actions';
import type { AuthNotice, AuthState } from '../../platform/desktop/bridge';
import { authState, createFakeBridge, type FakeBridge } from '../../platform/desktop/testing/fakeBridge';
import type { DesktopFeatures, Platform } from '../../platform/types';
import { createTestPlatform, testOrigin } from '../../test/platform';

export interface DesktopTestPlatform {
  readonly platform: Platform;
  readonly fake: FakeBridge;
  /** The URLs given to openExternal, oldest first. */
  readonly opened: string[];
  /** Sets what Auth.State() answers for this page's server. */
  setAuth(status: AuthState['status'], notice?: AuthNotice): AuthState;
}

export interface DesktopTestPlatformOptions {
  /** bridge.js's server; null: before the first server is added (serverOrigin ''). Default: the test origin. */
  server?: { id: string; origin: string; name: string } | null;
  /** Members of the Platform to replace. */
  overrides?: Partial<Platform>;
}

/** A derived local id (docs/m2/03 §8.1): 8 characters of lowercase Crockford base32. */
export const TEST_SERVER_ID = 'k3m9p2qx';

export function createDesktopTestPlatform(opts: DesktopTestPlatformOptions = {}): DesktopTestPlatform {
  const server =
    opts.server === undefined ? { id: TEST_SERVER_ID, origin: testOrigin(), name: 'Test server' } : opts.server;
  const origin = server?.origin ?? '';
  const fake = createFakeBridge({ active: origin, linked: true });
  const opened: string[] = [];
  const features: DesktopFeatures = {
    app: { version: '0.2.0', os: 'windows', osBuild: 26100 },
    server,
    bridge: fake.bridge,
    wizardSteps: () => ['link', 'sound', 'voice'],
    sessionEnded: (code) => fake.bridge.auth.sessionEnded(origin, code),
    helloRefused: (e) => fake.bridge.auth.helloRefused(origin, e),
    signalStopped: (err) => signalStopped(features, err),
  };
  const platform = createTestPlatform({
    kind: 'desktop',
    client: { kind: 'desktop', version: '0.2.0', os: 'windows', browser: 'edge' },
    role: 'viewer',
    serverOrigin: origin,
    desktop: features,
    openExternal: (url) => {
      opened.push(url);
    },
    // No reload in the desktop app (docs/m2/03 §16.3).
    versionActions: () => ({}),
    ...opts.overrides,
  });
  return {
    platform,
    fake,
    opened,
    setAuth(status, notice = '') {
      const state = { ...authState(origin, status), notice };
      fake.answers['auth.state'] = state;
      return state;
    },
  };
}
