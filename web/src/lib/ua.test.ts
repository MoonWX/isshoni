import { describe, expect, it } from 'vitest';

import { browserName, clientOS, inAppBrowser, isIOS, isMobileUA, type UAData } from './ua';

// Real-world UA strings (versions trimmed where they don't matter).
const UA = {
  chromeMac:
    'Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/140.0.0.0 Safari/537.36',
  chromeWin:
    'Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/140.0.0.0 Safari/537.36',
  edgeWin:
    'Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/140.0.0.0 Safari/537.36 Edg/140.0.0.0',
  firefoxLinux: 'Mozilla/5.0 (X11; Linux x86_64; rv:143.0) Gecko/20100101 Firefox/143.0',
  safariMac:
    'Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/26.0 Safari/605.1.15',
  safariIPhone:
    'Mozilla/5.0 (iPhone; CPU iPhone OS 26_0 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/26.0 Mobile/15E148 Safari/604.1',
  chromeIPhone:
    'Mozilla/5.0 (iPhone; CPU iPhone OS 26_0 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) CriOS/140.0.0.0 Mobile/15E148 Safari/604.1',
  // iPadOS Safari asks for desktop sites: a Mac UA, told apart by MacIntel + touch points.
  safariIPad:
    'Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/26.0 Safari/605.1.15',
  chromeAndroid:
    'Mozilla/5.0 (Linux; Android 16; Pixel 9) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/140.0.0.0 Mobile Safari/537.36',
  chromeOS:
    'Mozilla/5.0 (X11; CrOS x86_64 16000.0.0) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/140.0.0.0 Safari/537.36',
  samsung:
    'Mozilla/5.0 (Linux; Android 16; SM-S928B) AppleWebKit/537.36 (KHTML, like Gecko) SamsungBrowser/28.0 Chrome/130.0.0.0 Mobile Safari/537.36',
  facebookIOS:
    'Mozilla/5.0 (iPhone; CPU iPhone OS 26_0 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Mobile/15E148 [FBAN/FBIOS;FBAV/500.0.0.0;FBBV/1;FBDV/iPhone17,1;FBMD/iPhone;FBSN/iOS;FBSV/26.0]',
  facebookAndroid:
    'Mozilla/5.0 (Linux; Android 16; Pixel 9 Build/X; wv) AppleWebKit/537.36 (KHTML, like Gecko) Version/4.0 Chrome/140.0.0.0 Mobile Safari/537.36 [FB_IAB/FB4A;FBAV/500.0.0.0;]',
  messengerIOS:
    'Mozilla/5.0 (iPhone; CPU iPhone OS 26_0 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Mobile/15E148 [FBAN/MessengerForiOS;FBAV/500.0.0.0]',
  instagram:
    'Mozilla/5.0 (iPhone; CPU iPhone OS 26_0 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Mobile/15E148 Instagram 400.0.0.0 (iPhone17,1; iOS 26_0; en_US)',
  line: 'Mozilla/5.0 (iPhone; CPU iPhone OS 26_0 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Mobile/15E148 Safari Line/15.0.0',
  wechat:
    'Mozilla/5.0 (iPhone; CPU iPhone OS 26_0 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Mobile/15E148 MicroMessenger/8.0.50(0x18003237) NetType/WIFI Language/en',
} as const;

describe('inAppBrowser (05 §8, §19.1)', () => {
  it.each([
    [UA.facebookIOS, 'Facebook'], // FBAN
    [UA.facebookAndroid, 'Facebook'], // FBAV
    [UA.messengerIOS, 'Facebook'],
    [UA.instagram, 'Instagram'],
    [UA.line, 'LINE'],
    [UA.wechat, 'WeChat'],
  ])('%s → %s', (ua, want) => {
    expect(inAppBrowser(ua)).toBe(want);
  });

  it.each([
    ['plain Safari (iPhone)', UA.safariIPhone],
    ['Safari (Mac)', UA.safariMac],
    ['Chrome', UA.chromeMac],
    ['Chrome on Android', UA.chromeAndroid],
    ['iOS Chrome (CriOS)', UA.chromeIPhone],
    ['Firefox', UA.firefoxLinux],
    ['Edge', UA.edgeWin],
  ])('%s → null', (_name, ua) => {
    expect(inAppBrowser(ua)).toBeNull();
  });
});

describe('isIOS and isMobileUA', () => {
  it('knows iPhone and iPad UAs, and iPadOS by MacIntel with touch points', () => {
    expect(isIOS(UA.safariIPhone, 'iPhone', 5)).toBe(true);
    expect(isIOS(UA.safariIPad, 'MacIntel', 5)).toBe(true);
    expect(isIOS(UA.safariMac, 'MacIntel', 0)).toBe(false);
    expect(isIOS(UA.chromeAndroid, 'Linux armv8l', 5)).toBe(false);
  });

  it.each([
    [UA.safariIPhone, true],
    [UA.chromeAndroid, true],
    [UA.samsung, true],
    [UA.chromeMac, false],
    [UA.edgeWin, false],
    [UA.firefoxLinux, false],
    [UA.safariIPad, false], // desktop-class: only isIOS catches it
  ])('%s → %s', (ua, want) => {
    expect(isMobileUA(ua)).toBe(want);
  });
});

describe('clientOS', () => {
  it.each([
    [UA.chromeWin, 'windows'],
    [UA.chromeMac, 'macos'],
    [UA.firefoxLinux, 'linux'],
    [UA.chromeAndroid, 'android'],
    [UA.safariIPhone, 'ios'],
    [UA.chromeOS, 'chromeos'],
    ['curl/8.0', 'other'],
  ])('%s → %s', (ua, want) => {
    expect(clientOS(ua)).toBe(want);
  });

  it('reads iPadOS from MacIntel with touch points', () => {
    expect(clientOS(UA.safariIPad, undefined, 'MacIntel', 5)).toBe('ios');
  });

  it('prefers Client Hints', () => {
    const uaData: UAData = { brands: [], mobile: false, platform: 'Chrome OS' };
    expect(clientOS(UA.chromeWin, uaData)).toBe('chromeos');
    expect(clientOS(UA.chromeWin, { ...uaData, platform: 'Unknown' })).toBe('windows');
  });
});

describe('browserName', () => {
  it.each([
    [UA.chromeMac, 'chrome'],
    [UA.chromeIPhone, 'chrome'],
    [UA.edgeWin, 'edge'],
    [UA.firefoxLinux, 'firefox'],
    [UA.safariMac, 'safari'],
    [UA.safariIPhone, 'safari'],
    [UA.samsung, 'other'],
    [UA.instagram, 'other'],
  ])('%s → %s', (ua, want) => {
    expect(browserName(ua)).toBe(want);
  });

  it('prefers Client Hints brands', () => {
    const hints = (...brands: string[]): UAData => ({
      brands: brands.map((brand) => ({ brand })),
      mobile: false,
      platform: 'Windows',
    });
    expect(browserName('', hints('Not)A;Brand', 'Chromium', 'Microsoft Edge'))).toBe('edge');
    expect(browserName('', hints('Chromium', 'Google Chrome'))).toBe('chrome');
    expect(browserName('', hints('Chromium', 'Opera'))).toBe('other');
    expect(browserName('', hints('Chromium'))).toBe('chrome');
    expect(browserName(UA.firefoxLinux, hints())).toBe('firefox');
  });
});
