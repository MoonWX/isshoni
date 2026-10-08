import { http, HttpResponse } from 'msw';
import { afterEach, describe, expect, it, vi } from 'vitest';

import type { UAData } from '../../lib/ua';
import { apiPath, server } from '../../test/msw';
import { installFakeRTC } from '../../test/FakeRTCPeerConnection';
import { NotImplementedError } from '../../lib/errors';
import { BrowserPlatform, canShareHere } from './BrowserPlatform';
import type { DeviceEnv } from './device';
import { detectPlatform } from '../detect';

const UA = {
  chromeMac:
    'Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/140.0.0.0 Safari/537.36',
  firefoxWin: 'Mozilla/5.0 (Windows NT 10.0; Win64; x64; rv:143.0) Gecko/20100101 Firefox/143.0',
  safariIPhone:
    'Mozilla/5.0 (iPhone; CPU iPhone OS 26_0 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/26.0 Mobile/15E148 Safari/604.1',
  safariIPad:
    'Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/26.0 Safari/605.1.15',
  chromeAndroidPhone:
    'Mozilla/5.0 (Linux; Android 16; Pixel 9) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/140.0.0.0 Mobile Safari/537.36',
  chromeAndroidTablet:
    'Mozilla/5.0 (Linux; Android 16; Pixel Tablet) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/140.0.0.0 Safari/537.36',
  firefoxAndroid: 'Mozilla/5.0 (Android 16; Mobile; rv:143.0) Gecko/143.0 Firefox/143.0',
  instagram:
    'Mozilla/5.0 (iPhone; CPU iPhone OS 26_0 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Mobile/15E148 Instagram 400.0.0.0',
};

const chromeHints = (mobile: boolean, platform: string): UAData => ({
  brands: [{ brand: 'Chromium' }, { brand: 'Google Chrome' }],
  mobile,
  platform,
});

function env(ua: string, over: Partial<DeviceEnv> = {}): DeviceEnv {
  return { ua, platform: 'MacIntel', maxTouchPoints: 0, matches: () => false, ...over };
}

/** A browser that can capture and encode H.264, so only the device decides the role. */
function sharingCapableBrowser(): () => void {
  return installFakeRTC({ h264: true, displayMedia: true });
}

describe('BrowserPlatform role (05 §8)', () => {
  let uninstall: (() => void) | undefined;
  afterEach(() => {
    uninstall?.();
    uninstall = undefined;
  });

  it('is full on a desktop that can capture and encode H.264', () => {
    uninstall = sharingCapableBrowser();
    const p = new BrowserPlatform({ env: env(UA.chromeMac, { uaData: chromeHints(false, 'macOS') }) });
    expect(p.role).toBe('full');
    expect(p.sharing).not.toBeNull();
    expect(p.sharing?.mode).toBe('in-page');
  });

  it('is full on desktop Firefox too (feature detection, not a browser allowlist)', () => {
    uninstall = sharingCapableBrowser();
    const p = new BrowserPlatform({
      env: env(UA.firefoxWin, { platform: 'Win32', matches: (q) => q === '(any-pointer: fine)' }),
    });
    expect(p.role).toBe('full');
  });

  it.each([
    ['iPhone Safari', env(UA.safariIPhone, { platform: 'iPhone', maxTouchPoints: 5 })],
    ['iPad Safari (desktop-class UA)', env(UA.safariIPad, { platform: 'MacIntel', maxTouchPoints: 5 })],
    [
      'Android Chrome phone (Client Hints mobile)',
      env(UA.chromeAndroidPhone, { uaData: chromeHints(true, 'Android') }),
    ],
    [
      'Android Chrome tablet (Client Hints say not mobile)',
      env(UA.chromeAndroidTablet, { uaData: chromeHints(false, 'Android') }),
    ],
    ['Android Firefox', env(UA.firefoxAndroid, { platform: 'Linux armv8l', maxTouchPoints: 5 })],
    ['an unknown UA with only a touch screen', env('SomeBrowser/1.0', { matches: (q) => q === '(pointer: coarse)' })],
  ])('is viewer with a mobile UA: %s, even where getDisplayMedia and H.264 exist', (_name, e) => {
    uninstall = sharingCapableBrowser();
    const p = new BrowserPlatform({ env: e });
    expect(p.role).toBe('viewer');
    expect(p.sharing).toBeNull();
  });

  it('is viewer on a desktop without getDisplayMedia or without an H.264 encoder', () => {
    uninstall = installFakeRTC({ h264: true, displayMedia: false });
    expect(new BrowserPlatform({ env: env(UA.chromeMac) }).role).toBe('viewer');
    uninstall();
    uninstall = installFakeRTC({ h264: false, displayMedia: true });
    expect(new BrowserPlatform({ env: env(UA.chromeMac) }).role).toBe('viewer');
  });

  it('is viewer in jsdom (no WebRTC at all)', () => {
    expect(new BrowserPlatform({ env: env(UA.chromeMac) }).role).toBe('viewer');
  });

  it('canShareHere combines displayCapture with the device', () => {
    const desktop = env(UA.chromeMac);
    expect(canShareHere({ decode: [], displayCapture: true }, desktop)).toBe(true);
    expect(canShareHere({ decode: [] }, desktop)).toBe(false);
    expect(canShareHere({ decode: [], displayCapture: true }, env(UA.safariIPhone))).toBe(false);
  });

  it('the stub sharing provider rejects with NotImplementedError (S35/S46 fill it in)', async () => {
    uninstall = sharingCapableBrowser();
    const p = new BrowserPlatform({ env: env(UA.chromeMac) });
    await expect(p.sharing?.pick({ preset: 'auto' })).rejects.toBeInstanceOf(NotImplementedError);
  });
});

describe('BrowserPlatform basics', () => {
  it('reports hello.client from the UA, with the build version', () => {
    const p = new BrowserPlatform({ env: env(UA.safariIPhone, { platform: 'iPhone', maxTouchPoints: 5 }) });
    expect(p.kind).toBe('browser');
    expect(p.client).toEqual({ kind: 'web', version: __ISSHONI_VERSION__, os: 'ios', browser: 'safari' });
  });

  it('builds the signaling URL from the server origin: ws on http, wss on https', () => {
    expect(new BrowserPlatform({ origin: 'http://localhost:5173' }).signaling()).toEqual({
      url: 'ws://localhost:5173/ws',
    });
    expect(new BrowserPlatform({ origin: 'https://watch.example.org' }).signaling()).toEqual({
      url: 'wss://watch.example.org/ws',
    });
  });

  it('serves the page origin by default', () => {
    expect(new BrowserPlatform().serverOrigin).toBe(location.origin);
    expect(detectPlatform()).toBeInstanceOf(BrowserPlatform);
  });

  it('apiFetch resolves the path against the server origin, same-origin credentials', async () => {
    const seen: { url: string; credentials: RequestCredentials }[] = [];
    const fetchSpy = vi.spyOn(globalThis, 'fetch');
    server.use(
      http.get(apiPath('/api/v1/info'), ({ request }) => {
        seen.push({ url: request.url, credentials: request.credentials });
        return HttpResponse.json({});
      }),
    );
    const p = new BrowserPlatform();
    const res = await p.apiFetch('/api/v1/info', { method: 'GET' });
    expect(res.status).toBe(200);
    expect(seen[0]?.url).toBe(`${location.origin}/api/v1/info`);
    expect(fetchSpy.mock.calls[0]?.[1]).toMatchObject({ credentials: 'same-origin', method: 'GET' });
    fetchSpy.mockRestore();
  });

  it('matches in-app browsers by UA token', () => {
    expect(new BrowserPlatform({ env: env(UA.instagram) }).inAppBrowser()).toBe('Instagram');
    expect(new BrowserPlatform({ env: env(UA.chromeMac) }).inAppBrowser()).toBeNull();
  });

  it('creates peer connections through RTCPeerConnection', () => {
    const uninstall = installFakeRTC();
    try {
      const pc = new BrowserPlatform().createPeerConnection({ bundlePolicy: 'max-bundle' });
      expect(pc.getConfiguration()).toEqual({ bundlePolicy: 'max-bundle' });
    } finally {
      uninstall();
    }
  });

  it('reports capabilities; push and install come from the stub providers (S77, S38)', async () => {
    const caps = await new BrowserPlatform({ env: env(UA.chromeMac) }).capabilities();
    expect(caps).toEqual({
      caps: { decode: [] },
      canShare: false,
      fullscreen: 'none',
      pip: false,
      wakeLock: false,
      push: 'unsupported',
      install: 'none',
    });
  });

  it('has no wake lock in jsdom, and the PWA provider has nothing to install or update there', async () => {
    const p = new BrowserPlatform();
    await expect(p.requestWakeLock()).resolves.toBeNull();
    expect(p.pwa?.installState()).toBe('none');
    await expect(p.pwa?.promptInstall()).resolves.toBe('unavailable');
    const onUpdateReady = vi.fn();
    const off = p.pwa?.onUpdateReady(onUpdateReady);
    await Promise.resolve();
    expect(onUpdateReady).not.toHaveBeenCalled();
    off?.();
    await expect(p.notifications.enable()).rejects.toBeInstanceOf(NotImplementedError);
  });

  it('offers reload as its version action', () => {
    expect(Object.keys(new BrowserPlatform().versionActions())).toEqual(['reload']);
  });

  it('stores through Web Storage', () => {
    const p = new BrowserPlatform();
    p.storage.local.set('isshoni.test', 'a');
    expect(localStorage.getItem('isshoni.test')).toBe('a');
    p.storage.local.remove('isshoni.test');
    p.storage.session.set('isshoni.test', 'b');
    expect(sessionStorage.getItem('isshoni.test')).toBe('b');
    p.storage.session.remove('isshoni.test');
  });
});
