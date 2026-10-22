// Compile-time checks between the Platform interface and the modules it describes structurally (npm run typecheck).
import { describe, expect, expectTypeOf, it } from 'vitest';

import type { SignalClient, SignalClientOptions } from '../protocol';
import { BrowserPlatform } from './browser/BrowserPlatform';
import type { DesktopPlatform } from './desktop/DesktopPlatform';
import type { HelloAuthProvider, Platform, SignalClientLike } from './types';

describe('Platform types', () => {
  it("S20's SignalClient fits ShareContext.signal (SignalClientLike)", () => {
    expectTypeOf<SignalClient>().toExtend<SignalClientLike>();
  });

  it('signaling().auth is what SignalClientOptions.auth takes (docs/m2/03 §7.1)', () => {
    expectTypeOf<ReturnType<Platform['signaling']>['auth']>().toEqualTypeOf<SignalClientOptions['auth']>();
    expectTypeOf<HelloAuthProvider>().toEqualTypeOf<NonNullable<SignalClientOptions['auth']>>();
  });

  it('both platforms implement Platform; only the desktop has desktop features', () => {
    expectTypeOf<DesktopPlatform>().toExtend<Platform>();
    expectTypeOf<BrowserPlatform>().toExtend<Platform>();
    expect(new BrowserPlatform().desktop).toBeNull();
    expect(new BrowserPlatform().signaling()).not.toHaveProperty('auth');
  });
});
