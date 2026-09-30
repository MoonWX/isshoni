// A Platform for tests: REST goes through the global fetch (so MSW answers), storage is in memory, peer connections
// are FakeRTCPeerConnection, and sharing, push and the PWA are inert unless a test passes its own.
import { createMemoryStorage } from '../platform/browser/storage';
import type { NotificationsProvider, Platform, PlatformCapabilities } from '../platform/types';
import { FakeRTCPeerConnection } from './FakeRTCPeerConnection';

/** The origin tests run at: jsdom's page, or a fixed one in node test files. */
export function testOrigin(): string {
  return typeof location === 'undefined' ? 'http://localhost:3000' : location.origin;
}

export const inertNotifications: NotificationsProvider = {
  support: () => Promise.resolve('unsupported'),
  status: () => Promise.resolve('off'),
  enable: () => Promise.resolve(),
  disable: () => Promise.resolve(),
  test: () => Promise.resolve(),
};

export function createTestPlatform(overrides: Partial<Platform> = {}): Platform {
  const origin = overrides.serverOrigin ?? testOrigin();
  const caps = {
    decode: ['h264/6400', 'h264/42e0', 'opus'],
    encode: ['h264/6400', 'h264/42e0', 'opus'],
    simulcast: true,
  };
  const platform: Platform = {
    kind: 'browser',
    client: { kind: 'web', version: '0.0.0-test', os: 'other', browser: 'other' },
    role: 'full',
    serverOrigin: origin,
    capsNow: () => ({ ...caps }),
    capabilities: (): Promise<PlatformCapabilities> =>
      Promise.resolve({
        caps: platform.capsNow(),
        canShare: platform.sharing !== null,
        fullscreen: 'element',
        pip: false,
        wakeLock: false,
        push: 'unsupported',
        install: 'none',
      }),
    apiFetch: (path, init) => fetch(new URL(path, origin), { credentials: 'same-origin', ...init }),
    signaling: () => ({ url: new URL('/ws', origin.replace(/^http/, 'ws')).href }),
    createPeerConnection: (config) => new FakeRTCPeerConnection(config) as unknown as RTCPeerConnection,
    sharing: null,
    notifications: inertNotifications,
    pwa: null,
    requestWakeLock: () => Promise.resolve(null),
    inAppBrowser: () => null,
    openExternal: () => undefined,
    storage: { local: createMemoryStorage(), session: createMemoryStorage() },
    versionActions: () => ({ reload: () => undefined }),
    ...overrides,
  };
  return platform;
}
