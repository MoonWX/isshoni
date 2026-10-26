// Test helpers for share/native/'s components: a desktop-like platform whose sharing provider is a real DesktopSharing
// on a fake NativeBridge (platform/desktop/testing/fakeBridge.ts), with the page's share store of the test.
import type { Picked, ShareView } from '../../platform/desktop/bridge';
import { DesktopSharing } from '../../platform/desktop/DesktopSharing';
import { createFakeBridge, type FakeBridge } from '../../platform/desktop/testing/fakeBridge';
import type { DesktopFeatures, Platform } from '../../platform/types';
import { createTestPlatform } from '../../test/platform';
import { createShareStore, type ShareStore } from '../shareStore';

export const ORIGIN = 'https://watch.example.com';

export const PICKED: Picked = {
  pickId: '0123456789abcdef',
  kind: 'window',
  label: 'Minecraft',
  border: true,
  confirm: [],
};

export interface NativeSetup {
  readonly fake: FakeBridge;
  readonly sharing: DesktopSharing;
  readonly store: ShareStore;
  readonly platform: Platform;
  /** platform.openExternal's calls. */
  readonly opened: string[];
}

export async function nativeSetup(init: { current?: ShareView | null; picked?: Picked } = {}): Promise<NativeSetup> {
  const fake = createFakeBridge({ active: ORIGIN });
  fake.answers['share.current'] = init.current ?? null;
  fake.answers['app.info'] = { publisherConnectionId: 'c_pub' };
  fake.answers['share.pick'] = init.picked ?? PICKED;
  const store = createShareStore();
  const sharing = new DesktopSharing(fake.bridge, { store });
  await sharing.ready();
  const opened: string[] = [];
  const platform = createTestPlatform({
    kind: 'desktop',
    serverOrigin: ORIGIN,
    sharing,
    desktop: { bridge: fake.bridge } as unknown as DesktopFeatures,
    openExternal: (url) => {
      opened.push(url);
    },
  });
  return { fake, sharing, store, platform, opened };
}
