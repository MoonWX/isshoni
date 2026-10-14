// Test support for Web Push in the UI (05 §16.3): a NotificationsProvider and a PwaProvider whose answers a test
// sets, with every call recorded. Only tests import this folder.
import { vi, type Mock } from 'vitest';

import type { InstallState, NotificationsProvider, PushSupport, PwaProvider } from '../../platform/types';

/** A NotificationsProvider as a browser would answer; enable() turns it on unless the test says otherwise. */
export interface FakePush extends NotificationsProvider {
  readonly support: Mock<() => Promise<PushSupport>>;
  readonly status: Mock<() => Promise<'on' | 'off'>>;
  readonly enable: Mock<() => Promise<void>>;
  readonly disable: Mock<() => Promise<void>>;
  readonly test: Mock<() => Promise<void>>;
  /** What support() and status() answer now. */
  readonly state: { support: PushSupport; status: 'on' | 'off' };
}

export function fakePush(support: PushSupport, status: 'on' | 'off' = 'off'): FakePush {
  const state = { support, status };
  return {
    state,
    support: vi.fn(() => Promise.resolve(state.support)),
    status: vi.fn(() => Promise.resolve(state.status)),
    enable: vi.fn(() => {
      state.status = 'on';
      return Promise.resolve();
    }),
    disable: vi.fn(() => {
      state.status = 'off';
      return Promise.resolve();
    }),
    test: vi.fn(() => Promise.resolve()),
  };
}

/** A PwaProvider whose promptInstall is a mock. */
export type FakePwa = Omit<PwaProvider, 'promptInstall'> & {
  readonly promptInstall: Mock<() => Promise<'accepted' | 'dismissed' | 'unavailable'>>;
};

/** A PwaProvider with an install state of the test's choice; promptInstall() resolves `accepted`. */
export function fakePwa(install: InstallState): FakePwa {
  let state = install;
  return {
    installState: () => state,
    promptInstall: vi.fn(() => {
      state = 'installed';
      return Promise.resolve('accepted' as const);
    }),
    onUpdateReady: () => () => undefined,
    applyUpdate: () => undefined,
  };
}

/** Real-world UA strings, for platform.client.os and platform.inAppBrowser() as BrowserPlatform reads them. */
export const UA = {
  safariIPhone:
    'Mozilla/5.0 (iPhone; CPU iPhone OS 26_0 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/26.0 Mobile/15E148 Safari/604.1',
  instagramIPhone:
    'Mozilla/5.0 (iPhone; CPU iPhone OS 26_0 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Mobile/15E148 Instagram 400.0.0.0 (iPhone17,1; iOS 26_0; en_US)',
  chromeAndroid:
    'Mozilla/5.0 (Linux; Android 16; Pixel 9) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/140.0.0.0 Mobile Safari/537.36',
  chromeMac:
    'Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/140.0.0.0 Safari/537.36',
} as const;
