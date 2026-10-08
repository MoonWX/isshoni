// The in-app browser banner (05 §16.3, §19.2): on the login and invite pages in a chat app's built-in browser, not
// in Safari or Chrome; Copy link, with the invite token put back; the read-only field when the clipboard refuses;
// dismissal for the tab. The detector itself (lib/ua.ts inAppBrowser(), 05 §19.1) runs here on real UA strings.
import { act, fireEvent, render, screen, waitFor, within } from '@testing-library/react';
import { HttpResponse } from 'msw';
import { afterEach, describe, expect, it, vi } from 'vitest';

import { AppProviders } from '../app/App';
import { clientOS, inAppBrowser } from '../lib/ua';
import type { Platform } from '../platform/types';
import { createMemoryStorage } from '../platform/browser/storage';
import type { InviteInfo } from '../protocol/api.gen';
import { ClientOSAndroid, ClientOSIOS, ClientOSMacOS, ClientOSWindows } from '../protocol/types.gen';
import { apiError } from '../test/msw';
import { createTestPlatform } from '../test/platform';
import { createTestServices } from '../test/render';
import {
  dismissInAppBanner,
  externalBrowser,
  IN_APP_BANNER_DISMISSED_KEY,
  inAppBannerDismissed,
  inviteLink,
  useInAppBanner,
} from './inAppBanner';
import { InAppBrowserBanner } from './InAppBrowserBanner';
// The page folder, loaded with this file rather than by the first test's navigation (the router imports it lazily).
import './index';
import { mockSession, onPost, renderPage } from './testing/harness';

// Real-world UA strings (versions trimmed where they don't matter).
const IPHONE = 'Mozilla/5.0 (iPhone; CPU iPhone OS 26_0 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko)';
const ANDROID = 'Mozilla/5.0 (Linux; Android 16; Pixel 9 Build/X; wv) AppleWebKit/537.36 (KHTML, like Gecko)';
const UA = {
  instagramIPhone: `${IPHONE} Mobile/15E148 Instagram 400.0.0.0 (iPhone17,1; iOS 26_0; en_US)`,
  facebookIPhone: `${IPHONE} Mobile/15E148 [FBAN/FBIOS;FBAV/500.0.0.0;FBBV/1;FBDV/iPhone17,1;FBMD/iPhone]`,
  messengerIPhone: `${IPHONE} Mobile/15E148 [FBAN/MessengerForiOS;FBAV/500.0.0.0]`,
  lineIPhone: `${IPHONE} Mobile/15E148 Safari Line/15.0.0`,
  wechatIPhone: `${IPHONE} Mobile/15E148 MicroMessenger/8.0.50(0x18003237) NetType/WIFI Language/en`,
  facebookAndroid: `${ANDROID} Version/4.0 Chrome/140.0.0.0 Mobile Safari/537.36 [FB_IAB/FB4A;FBAV/500.0.0.0;]`,
  instagramAndroid: `${ANDROID} Version/4.0 Chrome/140.0.0.0 Mobile Safari/537.36 Instagram 400.0.0.0 Android`,
  wechatWindows:
    'Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/130.0.0.0 Safari/537.36 MicroMessenger/7.0.20.1781(0x6700143B) WindowsWechat',
  safariIPhone: `${IPHONE} Version/26.0 Mobile/15E148 Safari/604.1`,
  chromeIPhone: `${IPHONE} CriOS/140.0.0.0 Mobile/15E148 Safari/604.1`,
  chromeAndroid:
    'Mozilla/5.0 (Linux; Android 16; Pixel 9) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/140.0.0.0 Mobile Safari/537.36',
  chromeMac:
    'Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/140.0.0.0 Safari/537.36',
  safariMac:
    'Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/26.0 Safari/605.1.15',
  firefoxAndroid: 'Mozilla/5.0 (Android 16; Mobile; rv:143.0) Gecko/143.0 Firefox/143.0',
} as const;

const SAFARI = 'For notifications and the Home Screen app, open this link in Safari.';
const CHROME = 'For notifications and the Home Screen app, open this link in Chrome.';
const OTHER = 'For notifications and the Home Screen app, open this link in your browser.';
const BANNER = 'Open in your browser';

/** A platform as BrowserPlatform would be for this UA: the real detector and the real OS parsing (lib/ua.ts). */
function platformFor(ua: string): Platform {
  const base = createTestPlatform();
  return createTestPlatform({
    client: { ...base.client, os: clientOS(ua) },
    inAppBrowser: () => inAppBrowser(ua),
  });
}

const TOKEN = 'EXAMPLEinviteTOKEN0123456789abcd';
const INVITE: InviteInfo = {
  serverName: "Alex's server",
  invitedBy: 'Alex',
  expiresAt: '2026-10-15T12:00:00.000Z',
  usesLeft: 9,
};
const INVITE_HEADING = "Alex invited you to Alex's server";
const LOGIN_HEADING = 'Log in to Test server';

function renderLogin(ua: string) {
  mockSession();
  return renderPage({ path: '/login', platform: platformFor(ua) });
}

function renderInvite(ua: string) {
  mockSession();
  onPost('/api/v1/auth/invite/check', () => HttpResponse.json(INVITE));
  return renderPage({ path: '/invite', token: ['invite', TOKEN], platform: platformFor(ua) });
}

function stubClipboard(writeText: (text: string) => Promise<void>) {
  const spy = vi.fn(writeText);
  Object.defineProperty(navigator, 'clipboard', { configurable: true, value: { writeText: spy } });
  return spy;
}

afterEach(() => {
  Reflect.deleteProperty(navigator, 'clipboard');
  vi.useRealTimers();
});

describe('inAppBanner.ts', () => {
  it('names Safari on iOS, Chrome on Android and "your browser" elsewhere', () => {
    expect(externalBrowser(ClientOSIOS)).toBe('safari');
    expect(externalBrowser(ClientOSAndroid)).toBe('chrome');
    expect(externalBrowser(ClientOSWindows)).toBe('other');
    expect(externalBrowser(ClientOSMacOS)).toBe('other');
    // iPadOS asks for desktop sites with a Mac UA; lib/ua.ts tells it apart by its touch points.
    expect(externalBrowser(clientOS(UA.safariMac, undefined, 'MacIntel', 5))).toBe('safari');
  });

  it('rebuilds the invite link with the token in the fragment', () => {
    expect(inviteLink('https://watch.example.com', TOKEN)).toBe(`https://watch.example.com/invite#${TOKEN}`);
    expect(inviteLink('https://watch.example.com:8443/', TOKEN)).toBe(`https://watch.example.com:8443/invite#${TOKEN}`);
    // Boot stored the fragment percent-decoded (app/boot.tsx): it goes back encoded, and boot reads the same token.
    const odd = 'a b#c%d';
    const link = inviteLink('https://watch.example.com', odd);
    expect(link).toBe('https://watch.example.com/invite#a%20b%23c%25d');
    expect(decodeURIComponent(new URL(link).hash.slice(1))).toBe(odd);
  });

  it('keeps the dismissal in the tab storage, under isshoni.inAppBannerDismissed', () => {
    const session = createMemoryStorage();
    expect(IN_APP_BANNER_DISMISSED_KEY).toBe('isshoni.inAppBannerDismissed');
    expect(inAppBannerDismissed(session)).toBe(false);
    dismissInAppBanner(session);
    expect(inAppBannerDismissed(session)).toBe(true);
    expect(session.get('isshoni.inAppBannerDismissed')).not.toBeNull();
  });
});

describe('the in-app browser banner on /login', () => {
  it.each([
    ['Instagram on iPhone', UA.instagramIPhone, SAFARI],
    ['Facebook on iPhone (FBAN)', UA.facebookIPhone, SAFARI],
    ['Messenger on iPhone', UA.messengerIPhone, SAFARI],
    ['LINE on iPhone', UA.lineIPhone, SAFARI],
    ['WeChat on iPhone', UA.wechatIPhone, SAFARI],
    ['Facebook on Android (FBAV)', UA.facebookAndroid, CHROME],
    ['Instagram on Android', UA.instagramAndroid, CHROME],
    ['WeChat on Windows', UA.wechatWindows, OTHER],
  ])('shows in %s', async (_name, ua, text) => {
    renderLogin(ua);
    await screen.findByRole('heading', { name: LOGIN_HEADING });
    const banner = screen.getByRole('region', { name: BANNER });
    expect(within(banner).getByText(text)).toBeInTheDocument();
    expect(within(banner).getByRole('button', { name: 'Copy link' })).toBeInTheDocument();
    expect(within(banner).getByRole('button', { name: 'Dismiss' })).toBeInTheDocument();
    // Above the form, inside the page's main landmark.
    const form = screen.getByRole('button', { name: 'Log in' });
    expect(banner.compareDocumentPosition(form) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
    expect(screen.getByRole('main')).toContainElement(banner);
  });

  it.each([
    ['Safari on iPhone', UA.safariIPhone],
    ['Chrome on iPhone (CriOS)', UA.chromeIPhone],
    ['Chrome on Android', UA.chromeAndroid],
    ['Chrome on a Mac', UA.chromeMac],
    ['Safari on a Mac', UA.safariMac],
    ['Firefox on Android', UA.firefoxAndroid],
  ])('does not show in %s', async (_name, ua) => {
    renderLogin(ua);
    await screen.findByRole('heading', { name: LOGIN_HEADING });
    expect(screen.queryByRole('region', { name: BANNER })).not.toBeInTheDocument();
    expect(screen.queryByRole('button', { name: 'Copy link' })).not.toBeInTheDocument();
  });

  it('copies the address of the page', async () => {
    const writeText = stubClipboard(() => Promise.resolve());
    const { services } = renderLogin(UA.instagramIPhone);
    await screen.findByRole('heading', { name: LOGIN_HEADING });
    fireEvent.click(screen.getByRole('button', { name: 'Copy link' }));
    expect(await screen.findByRole('button', { name: 'Copied' })).toBeInTheDocument();
    expect(writeText.mock.calls).toEqual([[location.href]]);
    // Screen readers hear it: the button's new label alone isn't announced.
    expect(services.ui.getState().announcements.polite?.text).toBe('Link copied.');
    expect(screen.queryByRole('textbox', { name: /Couldn't copy the link/ })).not.toBeInTheDocument();
  });

  it('says "Copied" for two seconds, then offers Copy link again', async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    stubClipboard(() => Promise.resolve());
    renderLogin(UA.instagramIPhone);
    await screen.findByRole('heading', { name: LOGIN_HEADING });
    fireEvent.click(screen.getByRole('button', { name: 'Copy link' }));
    expect(await screen.findByRole('button', { name: 'Copied' })).toBeInTheDocument();
    await act(() => vi.advanceTimersByTimeAsync(1500));
    expect(screen.getByRole('button', { name: 'Copied' })).toBeInTheDocument();
    await act(() => vi.advanceTimersByTimeAsync(600));
    expect(screen.getByRole('button', { name: 'Copy link' })).toBeInTheDocument();
  });

  it('is dismissed for the tab: gone at once, still gone on the next page, back in a new tab', async () => {
    const first = renderLogin(UA.instagramIPhone);
    await screen.findByRole('heading', { name: LOGIN_HEADING });
    fireEvent.click(within(screen.getByRole('region', { name: BANNER })).getByRole('button', { name: 'Dismiss' }));
    expect(screen.queryByRole('region', { name: BANNER })).not.toBeInTheDocument();
    expect(first.platform.storage.session.get('isshoni.inAppBannerDismissed')).not.toBeNull();
    // Nothing else of the page changed.
    expect(screen.getByRole('heading', { name: LOGIN_HEADING })).toBeInTheDocument();
    first.unmount();

    // The same tab (its session storage), another page load.
    mockSession();
    renderPage({ path: '/login', platform: first.platform });
    await screen.findByRole('heading', { name: LOGIN_HEADING });
    expect(screen.queryByRole('region', { name: BANNER })).not.toBeInTheDocument();
  });

  it('shows again in another tab of the same in-app browser', async () => {
    const first = renderLogin(UA.instagramIPhone);
    await screen.findByRole('heading', { name: LOGIN_HEADING });
    fireEvent.click(screen.getByRole('button', { name: 'Dismiss' }));
    first.unmount();

    renderLogin(UA.instagramIPhone);
    await screen.findByRole('heading', { name: LOGIN_HEADING });
    expect(screen.getByRole('region', { name: BANNER })).toBeInTheDocument();
  });
});

describe('the in-app browser banner on /invite', () => {
  it('shows with an Instagram UA, and Copy link includes #<token>', async () => {
    const writeText = stubClipboard(() => Promise.resolve());
    const { platform } = renderInvite(UA.instagramIPhone);
    await screen.findByRole('heading', { name: INVITE_HEADING });
    const banner = screen.getByRole('region', { name: BANNER });
    expect(within(banner).getByText(SAFARI)).toBeInTheDocument();
    expect(screen.getByRole('main')).toContainElement(banner);

    fireEvent.click(within(banner).getByRole('button', { name: 'Copy link' }));
    expect(await within(banner).findByRole('button', { name: 'Copied' })).toBeInTheDocument();
    // Boot took the fragment out of the address bar; the link is rebuilt from the token in memory.
    expect(location.hash).toBe('');
    expect(writeText.mock.calls).toEqual([[`${platform.serverOrigin}/invite#${TOKEN}`]]);
  });

  it('names Chrome on Android', async () => {
    renderInvite(UA.facebookAndroid);
    await screen.findByRole('heading', { name: INVITE_HEADING });
    expect(within(screen.getByRole('region', { name: BANNER })).getByText(CHROME)).toBeInTheDocument();
  });

  it.each([
    ['Safari on iPhone', UA.safariIPhone],
    ['Chrome on Android', UA.chromeAndroid],
    ['Chrome on a Mac', UA.chromeMac],
  ])('does not show in %s', async (_name, ua) => {
    renderInvite(ua);
    await screen.findByRole('heading', { name: INVITE_HEADING });
    expect(screen.queryByRole('region', { name: BANNER })).not.toBeInTheDocument();
  });

  it.each([
    ['refuses', () => stubClipboard(() => Promise.reject(new Error('denied')))],
    ['has no clipboard', () => Reflect.deleteProperty(navigator, 'clipboard')],
  ])('shows the link in a selected read-only field when the browser %s', async (_name, arrange) => {
    arrange();
    const { platform } = renderInvite(UA.instagramIPhone);
    await screen.findByRole('heading', { name: INVITE_HEADING });
    fireEvent.click(screen.getByRole('button', { name: 'Copy link' }));

    const field = await screen.findByRole<HTMLInputElement>('textbox', {
      name: "Couldn't copy the link. Select it here and copy it:",
    });
    const link = `${platform.serverOrigin}/invite#${TOKEN}`;
    expect(field).toHaveAttribute('readonly');
    expect(field).toHaveValue(link);
    await waitFor(() => {
      expect(field).toHaveFocus();
    });
    expect([field.selectionStart, field.selectionEnd]).toEqual([0, link.length]);
    // The button didn't claim success, and a second try is still possible.
    expect(screen.queryByRole('button', { name: 'Copied' })).not.toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Copy link' })).toBeInTheDocument();
  });

  it('is not shown for a link that does not work: there is nothing to open elsewhere', async () => {
    mockSession();
    onPost('/api/v1/auth/invite/check', () => apiError(410, { code: 'invite_expired' }));
    renderPage({ path: '/invite', token: ['invite', TOKEN], platform: platformFor(UA.instagramIPhone) });
    await screen.findByRole('heading', { name: "This invite link doesn't work" });
    expect(screen.queryByRole('region', { name: BANNER })).not.toBeInTheDocument();
  });
});

describe('useInAppBanner()', () => {
  /** What the Home Screen sheet and card do (05 §16.3): stay hidden while the banner shows. */
  function NeedsInstall() {
    const banner = useInAppBanner();
    return banner.showing ? null : <p>Add isshoni to your Home Screen</p>;
  }

  function renderBoth(platform: Platform) {
    return render(
      <AppProviders services={createTestServices({ platform })}>
        <InAppBrowserBanner />
        <NeedsInstall />
      </AppProviders>,
    );
  }

  it('lets other parts wait for the banner: hidden while it shows, there once it is dismissed', () => {
    renderBoth(platformFor(UA.instagramIPhone));
    expect(screen.getByRole('region', { name: BANNER })).toBeInTheDocument();
    expect(screen.queryByText('Add isshoni to your Home Screen')).not.toBeInTheDocument();

    fireEvent.click(screen.getByRole('button', { name: 'Dismiss' }));
    expect(screen.queryByRole('region', { name: BANNER })).not.toBeInTheDocument();
    expect(screen.getByText('Add isshoni to your Home Screen')).toBeInTheDocument();
  });

  it('never holds them back in a real browser', () => {
    renderBoth(platformFor(UA.safariIPhone));
    expect(screen.queryByRole('region', { name: BANNER })).not.toBeInTheDocument();
    expect(screen.getByText('Add isshoni to your Home Screen')).toBeInTheDocument();
  });

  it('reports the app the detector found', () => {
    let seen: string | null | undefined;
    function Probe() {
      seen = useInAppBanner().app;
      return null;
    }
    const { unmount } = render(
      <AppProviders services={createTestServices({ platform: platformFor(UA.wechatIPhone) })}>
        <Probe />
      </AppProviders>,
    );
    expect(seen).toBe('WeChat');
    unmount();
    render(
      <AppProviders services={createTestServices({ platform: platformFor(UA.chromeAndroid) })}>
        <Probe />
      </AppProviders>,
    );
    expect(seen).toBeNull();
  });
});
