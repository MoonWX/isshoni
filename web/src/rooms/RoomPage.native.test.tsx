// The room page in the desktop app (docs/m2/03 §7.6, §10.2–§10.3; README WA65), on the real runtime against the fake
// signaling server, with platform.sharing a real DesktopSharing on a fake NativeBridge: the Share gates read
// capabilities().canShare, the desktop's own ShareSheet and panel are share/native/'s, the app's own share (C2) is
// the page's and not "elsewhere", and the other device's share is stopped through C2 (Share.StopOther). Also, in
// any platform, ?conntest=1 opens the connection test and leaves the URL.
import { act, fireEvent, screen, within } from '@testing-library/react';
import { afterEach, beforeAll, beforeEach, describe, expect, it, vi } from 'vitest';

import { DesktopSharing } from '../platform/desktop/DesktopSharing';
import { createFakeBridge, shareCaps, type FakeBridge } from '../platform/desktop/testing/fakeBridge';
import { shareView } from '../platform/desktop/testing/shareView';
import type { DesktopFeatures, PlatformCapabilities } from '../platform/types';
import type { RoomState } from '../protocol/types.gen';
import { shareStore } from '../share/shareStore';
import { createHarness, participant, shareInfo, type Harness, type HarnessOptions } from './testing/harness';
import { advance, PRELOAD_TIMEOUT_MS, preloadLazyChunks, renderRoomApp, stubRest } from './testing/page';

const ME = 'k3m9p2qxw7ht';
const alex = participant(ME, 'alex');
const ORIGIN = 'https://watch.example.com';

let h: Harness;
let fake: FakeBridge;

beforeAll(async () => {
  await preloadLazyChunks();
  await import('../share/native');
}, PRELOAD_TIMEOUT_MS);

beforeEach(() => {
  vi.useFakeTimers();
  vi.spyOn(Math, 'random').mockReturnValue(0.5);
});

afterEach(() => {
  h.close();
  shareStore.setState({ phase: 'idle', withAudio: false, picked: null, params: null, hint: null, error: null });
  vi.useRealTimers();
  vi.restoreAllMocks();
});

/** A desktop platform: DesktopSharing on a fake bridge whose Share.Capabilities says canShare. */
async function setupDesktop(
  opts: { canShare?: boolean; reason?: string; current?: ReturnType<typeof shareView> } = {},
) {
  fake = createFakeBridge({ active: ORIGIN });
  fake.answers['share.current'] = opts.current ?? null;
  fake.answers['app.info'] = { publisherConnectionId: 'c_publisher' };
  fake.answers['share.capabilities'] = {
    ...shareCaps(opts.canShare ?? true),
    ...(opts.reason !== undefined ? { reason: opts.reason } : {}),
  };
  const sharing = new DesktopSharing(fake.bridge);
  await sharing.ready();
  const platform: HarnessOptions['platform'] = {
    kind: 'desktop',
    role: 'viewer',
    sharing,
    desktop: { bridge: fake.bridge } as unknown as DesktopFeatures,
  };
  h = createHarness({ platform });
  // As DesktopPlatform: canShare from Share.Capabilities.
  vi.spyOn(h.platform, 'capabilities').mockImplementation(async (): Promise<PlatformCapabilities> => ({
    caps: h.platform.capsNow(),
    canShare: (await fake.bridge.share.capabilities()).canShare,
    fullscreen: 'none',
    pip: false,
    wakeLock: false,
    push: 'unsupported',
    install: 'none',
  }));
  stubRest(h.platform);
}

async function open(path = '/r/lounge') {
  const app = renderRoomApp(h.services, path);
  await advance();
  await advance();
  return app;
}

const sendState = (overrides: Partial<RoomState>): void => {
  act(() => {
    h.hub.sendState('lounge', overrides);
  });
};

describe('RoomPage in the desktop app (docs/m2/03 §10.2)', () => {
  it('Share opens the desktop ShareSheet: Optimize for and Share sound', async () => {
    await setupDesktop();
    await open();
    const header = screen.getByRole('banner');
    fireEvent.click(within(header).getByRole('button', { name: 'Share' }));
    const sheet = screen.getByRole('dialog', { name: 'Share your screen' });
    expect(within(sheet).getByRole('checkbox', { name: 'Share sound' })).toBeChecked();
    expect(within(sheet).getByText('Optimize for')).toBeInTheDocument();
    // The empty state's button is the desktop's too.
    expect(screen.getByRole('button', { name: 'Share your screen' })).toBeEnabled();
  });

  it('a PC that can’t share gets no Share button, and the empty state says why (capabilities().canShare)', async () => {
    await setupDesktop({ canShare: false, reason: 'unsupported_os' });
    await open();
    await advance();
    expect(screen.queryByRole('button', { name: /^Share/ })).not.toBeInTheDocument();
    expect(
      screen.getByText("Sharing isn't available on this PC. Sharing needs Windows 10 22H2 or later."),
    ).toBeInTheDocument();
  });

  it('the app’s own share is the page’s; a share from another device is stopped through C2', async () => {
    await setupDesktop();
    await open();
    sendState({ participants: [alex], shares: [shareInfo('s_native', ME, 'c_publisher')] });
    fireEvent.click(within(screen.getByRole('banner')).getByRole('button', { name: 'Share' }));
    let sheet = screen.getByRole('dialog', { name: 'Share your screen' });
    expect(within(sheet).queryByRole('button', { name: 'Stop it' })).not.toBeInTheDocument();
    fireEvent.click(within(sheet).getByRole('button', { name: 'Cancel' }));

    sendState({ participants: [alex], shares: [shareInfo('s_phone', ME, 'c_phone')] });
    fireEvent.click(within(screen.getByRole('banner')).getByRole('button', { name: 'Share' }));
    sheet = screen.getByRole('dialog', { name: 'Share your screen' });
    fireEvent.click(within(sheet).getByRole('button', { name: 'Stop it' }));
    await advance();
    expect(fake.callsOf('share.stopOther')).toEqual([['lounge', 's_phone']]);
    expect(h.hub.sent('share.stop')).toEqual([]);
  });

  it('adopts the app’s share after a reload: the native panel is back, live', async () => {
    const view = shareView({ shareId: 's_native', connectionId: 'c_publisher', label: 'Minecraft' });
    await setupDesktop({ current: view });
    await open();
    sendState({ participants: [alex], shares: [shareInfo('s_native', ME, 'c_publisher')] });
    await advance();
    const panel = screen.getByRole('region', { name: 'Your share' });
    expect(within(panel).getByText(/^You're live · Minecraft · Game ·/)).toBeInTheDocument();
    expect(h.runtime.session.share?.shareId).toBe('s_native');
    expect(within(screen.getByRole('banner')).getByRole('button', { name: 'Share' })).toBeDisabled();
  });
});

describe('RoomPage: ?conntest=1 (docs/m2/03 §7.6)', () => {
  it('opens the connection test and drops the parameter', async () => {
    h = createHarness();
    stubRest(h.platform);
    const { router } = await open('/r/lounge?conntest=1&focus=s_x');
    await advance();
    expect(screen.getByRole('dialog', { name: 'Test my connection' })).toBeInTheDocument();
    expect(router.state.location.pathname).toBe('/r/lounge');
    expect(router.state.location.search).toBe('?focus=s_x');
  });
});
