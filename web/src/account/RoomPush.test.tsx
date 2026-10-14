// Web Push on the room page (05 §11.2, §16.3, §19.2), on the real RoomPage and runtime of rooms/testing: the in-app
// browser banner above the page, the iOS Home Screen sheet that waits for it, the iOS app's first-launch offer, the
// one-time card of the empty state after 3 minutes of watching, the start-up re-post and notification clicks.
import { act, fireEvent, screen, within } from '@testing-library/react';
import { afterEach, beforeAll, beforeEach, describe, expect, it, vi } from 'vitest';

import { clientOS, inAppBrowser } from '../lib/ua';
import { BrowserNotifications } from '../platform/browser/push';
import type { Platform } from '../platform/types';
import { createHarness, participant, shareInfo, type Harness } from '../rooms/testing/harness';
import {
  advance,
  PRELOAD_TIMEOUT_MS,
  preloadLazyChunks,
  renderRoomApp,
  stubNativeDialog,
  stubRest,
} from '../rooms/testing/page';
import { APP_LAUNCH_DISMISSAL, CARD_DISMISSAL, HOME_SCREEN_DISMISSAL } from './pushState';
import { CARD_AFTER_MS } from './RoomPush';
import { fakePush, fakePwa, UA, type FakePush } from './testing/push';

const ME = 'k3m9p2qxw7ht';
const DAY = 24 * 60 * 60_000;

const BANNER = 'Open in your browser';
const SAFARI = 'For notifications and the Home Screen app, open this link in Safari.';
const SHEET = 'Add isshoni to your Home Screen';
const APP_LAUNCH = 'Turn on notifications?';
const CARD_TITLE = 'Get a notification when friends start sharing?';
/** The card is named by its question: about notifications, or (Android, push on) about the install alone. */
const CARD = /^(Get a notification when friends start sharing\?|Install isshoni on this phone\?)$/;

let h: Harness;
let dialogs: ReturnType<typeof stubNativeDialog>;

beforeAll(preloadLazyChunks, PRELOAD_TIMEOUT_MS);

beforeEach(() => {
  vi.useFakeTimers();
  vi.spyOn(Math, 'random').mockReturnValue(0.5); // no backoff jitter
  dialogs = stubNativeDialog();
});

afterEach(() => {
  h.close();
  dialogs.restore();
  vi.useRealTimers();
  vi.restoreAllMocks();
});

/** A platform as BrowserPlatform would be for this UA, with the push and PWA providers of the test. */
function platformFor(ua: string, push: FakePush, extra: Partial<Platform> = {}): Partial<Platform> {
  return {
    client: { kind: 'web', version: '0.0.0-test', os: clientOS(ua), browser: 'other' },
    inAppBrowser: () => inAppBrowser(ua),
    notifications: push,
    ...extra,
  };
}

/** The room page for the lounge, with the user alone in it (the empty state). */
async function openRoom(platform: Partial<Platform>) {
  h = createHarness({ platform });
  stubRest(h.platform);
  const app = renderRoomApp(h.services, '/r/lounge');
  await advance();
  act(() => {
    h.hub.sendState('lounge', { participants: [participant(ME, 'alex')] });
  });
  await advance();
  return app;
}

const sheet = (name: string) => screen.queryByRole('dialog', { name });
const card = () => screen.queryByRole('region', { name: CARD });

describe('the in-app browser banner on the room page (05 §16.3, §19.2)', () => {
  it('shows above the room in a chat app’s browser, naming Safari on an iPhone', async () => {
    await openRoom(platformFor(UA.instagramIPhone, fakePush('needs-install')));
    const banner = screen.getByRole('region', { name: BANNER });
    expect(banner).toHaveTextContent(SAFARI);
    expect(within(banner).getByRole('button', { name: 'Copy link' })).toBeInTheDocument();
    // Above the page: before the room's header.
    const heading = screen.getByRole('heading', { level: 1, name: 'Lounge' });
    expect(banner.compareDocumentPosition(heading) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
  });

  it('shows nothing in a real browser', async () => {
    await openRoom(platformFor(UA.safariIPhone, fakePush('needs-install')));
    expect(screen.queryByRole('region', { name: BANNER })).not.toBeInTheDocument();
  });

  it('keeps the Home Screen sheet hidden until the banner is dismissed', async () => {
    await openRoom(platformFor(UA.instagramIPhone, fakePush('needs-install')));
    expect(screen.getByRole('region', { name: BANNER })).toBeInTheDocument();
    await advance(10 * 60_000);
    expect(sheet(SHEET)).not.toBeInTheDocument();

    fireEvent.click(within(screen.getByRole('region', { name: BANNER })).getByRole('button', { name: 'Dismiss' }));
    await advance();
    expect(screen.queryByRole('region', { name: BANNER })).not.toBeInTheDocument();
    const steps = sheet(SHEET);
    expect(steps).toBeInTheDocument();
    expect(steps).toHaveTextContent('Add isshoni to your Home Screen to get notifications (iOS 16.4 or later).');
    expect(steps).toHaveTextContent('Tap Share in Safari.');
    expect(steps).toHaveTextContent("You'll log in once more inside the app");
    expect(steps).toHaveTextContent('Opened this from a chat app?');
  });
});

describe('the Home Screen sheet (05 §16.3)', () => {
  it('shows at once in a Safari tab on iOS, and comes back after 14 days', async () => {
    const push = fakePush('needs-install');
    const app = await openRoom(platformFor(UA.safariIPhone, push));
    const steps = sheet(SHEET);
    expect(steps).toBeInTheDocument();
    fireEvent.click(within(steps as HTMLElement).getByRole('button', { name: 'Got it' }));
    await advance();
    expect(sheet(SHEET)).not.toBeInTheDocument();
    expect(h.services.prefs.getState().dismissed[HOME_SCREEN_DISMISSAL]).toBe(Date.now());
    // No status() where push isn't supported: nothing is posted from a tab that can't push.
    expect(push.status).not.toHaveBeenCalled();

    // The next visit within 14 days: no sheet.
    app.unmount();
    renderRoomApp(h.services, '/r/lounge');
    await advance();
    expect(sheet(SHEET)).not.toBeInTheDocument();
  });

  it('shows again after 14 days', async () => {
    const push = fakePush('needs-install');
    h = createHarness({ platform: platformFor(UA.safariIPhone, push) });
    stubRest(h.platform);
    h.services.prefs.getState().dismiss(HOME_SCREEN_DISMISSAL, Date.now() - 14 * DAY);
    renderRoomApp(h.services, '/r/lounge');
    await advance();
    expect(sheet(SHEET)).toBeInTheDocument();
  });

  it('is not for a desktop browser', async () => {
    await openRoom(platformFor(UA.chromeMac, fakePush('supported')));
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument();
  });

  it('offers notifications once on the first launch of the iOS Home Screen app', async () => {
    const push = fakePush('supported');
    const app = await openRoom(platformFor(UA.safariIPhone, push, { pwa: fakePwa('installed') }));
    const offer = sheet(APP_LAUNCH);
    expect(offer).toBeInTheDocument();
    expect(push.enable).not.toHaveBeenCalled(); // never an automatic permission prompt

    fireEvent.click(within(offer as HTMLElement).getByRole('button', { name: 'Turn on' }));
    expect(push.enable).toHaveBeenCalledTimes(1);
    await advance();
    expect(sheet(APP_LAUNCH)).not.toBeInTheDocument();
    expect(h.toasts()).toContain('Notifications are on for this device.');
    expect(h.services.prefs.getState().dismissed[APP_LAUNCH_DISMISSAL]).toBeDefined();

    push.state.status = 'off';
    app.unmount();
    renderRoomApp(h.services, '/r/lounge');
    await advance();
    expect(sheet(APP_LAUNCH)).not.toBeInTheDocument();
  });

  it('"Not now" on the first launch also keeps the room’s card away', async () => {
    const push = fakePush('supported');
    await openRoom(platformFor(UA.safariIPhone, push, { pwa: fakePwa('installed') }));
    fireEvent.click(within(sheet(APP_LAUNCH) as HTMLElement).getByRole('button', { name: 'Not now' }));
    await advance(CARD_AFTER_MS);
    expect(push.enable).not.toHaveBeenCalled();
    expect(sheet(APP_LAUNCH)).not.toBeInTheDocument();
    expect(card()).not.toBeInTheDocument();
    expect(h.services.prefs.getState().dismissed[CARD_DISMISSAL]).toBeDefined();
  });
});

describe('the notifications card (05 §11.2, §16.3)', () => {
  it('appears in the empty state after 3 minutes of watching', async () => {
    await openRoom(platformFor(UA.chromeMac, fakePush('supported')));
    expect(screen.getByText('Nobody is sharing yet.')).toBeInTheDocument();
    await advance(CARD_AFTER_MS - 1000);
    expect(card()).not.toBeInTheDocument();
    await advance(1000);
    expect(card()).toHaveTextContent(CARD_TITLE);
  });

  it('counts only the time the page is visible', async () => {
    let state: DocumentVisibilityState = 'visible';
    vi.spyOn(document, 'visibilityState', 'get').mockImplementation(() => state);
    const setVisibility = (next: DocumentVisibilityState): void => {
      state = next;
      act(() => {
        document.dispatchEvent(new Event('visibilitychange'));
      });
    };
    await openRoom(platformFor(UA.chromeMac, fakePush('supported')));
    await advance(60_000);
    setVisibility('hidden');
    await advance(10 * 60_000);
    expect(card()).not.toBeInTheDocument();
    setVisibility('visible');
    await advance(CARD_AFTER_MS - 60_000 - 1000);
    expect(card()).not.toBeInTheDocument();
    await advance(1000);
    expect(card()).toBeInTheDocument();
  });

  it('shows only in the empty state: not while a share is on the stage', async () => {
    await openRoom(platformFor(UA.chromeMac, fakePush('supported')));
    act(() => {
      h.hub.sendState('lounge', {
        participants: [participant(ME, 'alex'), participant('u_bo', 'bo')],
        shares: [shareInfo('s_bo', 'u_bo', 'c_bo')],
      });
    });
    await advance(CARD_AFTER_MS);
    expect(card()).not.toBeInTheDocument();
    // The share ends: the empty state, with the card the watching earned.
    act(() => {
      h.hub.sendState('lounge', { participants: [participant(ME, 'alex'), participant('u_bo', 'bo')] });
    });
    await advance();
    expect(card()).toBeInTheDocument();
  });

  it('Turn on asks the browser from the click, then goes; the page says notifications are on', async () => {
    const push = fakePush('supported');
    await openRoom(platformFor(UA.chromeMac, push));
    await advance(CARD_AFTER_MS);
    fireEvent.click(within(card() as HTMLElement).getByRole('button', { name: 'Turn on' }));
    expect(push.enable).toHaveBeenCalledTimes(1);
    await advance();
    expect(card()).not.toBeInTheDocument();
    expect(h.toasts()).toContain('Notifications are on for this device.');
  });

  it('says so when the browser blocked notifications', async () => {
    const push = fakePush('supported');
    push.enable.mockImplementation(() => {
      push.state.support = 'denied';
      return Promise.resolve();
    });
    await openRoom(platformFor(UA.chromeMac, push));
    await advance(CARD_AFTER_MS);
    fireEvent.click(within(card() as HTMLElement).getByRole('button', { name: 'Turn on' }));
    await advance();
    expect(within(card() as HTMLElement).getByRole('alert')).toHaveTextContent('Notifications are blocked');
    expect(within(card() as HTMLElement).queryByRole('button', { name: 'Turn on' })).not.toBeInTheDocument();
  });

  it('Not now hides it for 30 days', async () => {
    await openRoom(platformFor(UA.chromeMac, fakePush('supported')));
    await advance(CARD_AFTER_MS);
    fireEvent.click(within(card() as HTMLElement).getByRole('button', { name: 'Not now' }));
    await advance();
    expect(card()).not.toBeInTheDocument();
    const at = h.services.prefs.getState().dismissed[CARD_DISMISSAL];
    expect(at).toBe(Date.now());
    expect(h.services.prefs.getState().isDismissed(CARD_DISMISSAL, 30 * DAY, Date.now() + 30 * DAY - 1)).toBe(true);
  });

  it.each([
    ['push is on already', fakePush('supported', 'on')],
    ['the browser blocked it', fakePush('denied')],
    ['the browser can’t', fakePush('unsupported')],
  ])('is not offered where %s', async (_name, push) => {
    await openRoom(platformFor(UA.chromeMac, push));
    await advance(CARD_AFTER_MS);
    expect(card()).not.toBeInTheDocument();
  });

  it('waits for the in-app banner like the sheet does', async () => {
    await openRoom(platformFor(UA.instagramIPhone, fakePush('supported')));
    await advance(CARD_AFTER_MS);
    expect(card()).not.toBeInTheDocument();
    fireEvent.click(within(screen.getByRole('region', { name: BANNER })).getByRole('button', { name: 'Dismiss' }));
    await advance();
    expect(card()).toBeInTheDocument();
  });

  it('offers Install app on Android while the browser has an install prompt, also where push is on', async () => {
    const pwa = fakePwa('prompt');
    await openRoom(platformFor(UA.chromeAndroid, fakePush('supported', 'on'), { pwa }));
    await advance(CARD_AFTER_MS);
    const offer = card() as HTMLElement;
    expect(offer).toHaveTextContent('Install isshoni on this phone?');
    expect(within(offer).queryByRole('button', { name: 'Turn on' })).not.toBeInTheDocument();
    fireEvent.click(within(offer).getByRole('button', { name: 'Install app' }));
    expect(pwa.promptInstall).toHaveBeenCalledTimes(1);
    await advance();
    expect(card()).not.toBeInTheDocument();
  });

  it('offers both on Android where push is off', async () => {
    await openRoom(platformFor(UA.chromeAndroid, fakePush('supported'), { pwa: fakePwa('prompt') }));
    await advance(CARD_AFTER_MS);
    const offer = card() as HTMLElement;
    expect(within(offer).getByRole('button', { name: 'Turn on' })).toBeInTheDocument();
    expect(within(offer).getByRole('button', { name: 'Install app' })).toBeInTheDocument();
  });
});

describe('the push state on the room page', () => {
  it('asks status() once on the page, which is the start-up re-post (05 §16.3)', async () => {
    const push = fakePush('supported', 'on');
    await openRoom(platformFor(UA.chromeMac, push));
    expect(push.support).toHaveBeenCalledTimes(1);
    expect(push.status).toHaveBeenCalledTimes(1);
  });

  it('asks again when the room is mounted again: a sign-in in the same page re-posts (03 §12.4.6)', async () => {
    const push = fakePush('supported', 'on');
    const app = await openRoom(platformFor(UA.chromeMac, push));
    expect(push.status).toHaveBeenCalledTimes(1);
    // Signed out (or the session was revoked) and in again: a navigation away from the room and back, no reload.
    app.unmount();
    renderRoomApp(h.services, '/r/lounge');
    await advance();
    expect(push.status).toHaveBeenCalledTimes(2);
  });

  it('follows a click on a notification inside the app: the room and the share of its link', async () => {
    const container = Object.assign(new EventTarget(), {
      getRegistration: () => Promise.resolve(undefined),
      ready: new Promise(() => undefined),
    });
    const assign = vi.fn();
    const provider = new BrowserNotifications({
      serviceWorker: container as unknown as ServiceWorkerContainer,
      notification: null,
      pushManager: false,
      origin: location.origin,
      assign,
    });
    try {
      const app = await openRoom(platformFor(UA.chromeMac, fakePush('unsupported')));
      act(() => {
        container.dispatchEvent(new MessageEvent('message', { data: { type: 'open', url: '/r/games?focus=s_1' } }));
      });
      await advance();
      expect(app.router.state.location.pathname + app.router.state.location.search).toBe('/r/games?focus=s_1');
      expect(assign).not.toHaveBeenCalled();
    } finally {
      provider.dispose();
    }
  });
});
