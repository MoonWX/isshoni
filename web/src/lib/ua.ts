// User-agent parsing (05 §8, §16.3). Every result here only chooses diagnostics (hello.client) or help text; none
// gates a feature, which is always detected directly.
import {
  ClientOSAndroid,
  ClientOSChromeOS,
  ClientOSIOS,
  ClientOSLinux,
  ClientOSMacOS,
  ClientOSOther,
  ClientOSWindows,
  type ClientOS,
} from '../protocol/types.gen';

/** ClientInfo.browser values (01 §8.2): diagnostics only. */
export type BrowserName = 'chrome' | 'edge' | 'firefox' | 'safari' | 'other';

/** The parts of navigator.userAgentData that parsing reads (Chromium only). */
export interface UAData {
  readonly brands: readonly { readonly brand: string }[];
  readonly mobile: boolean;
  readonly platform: string;
}

/** Known in-app browser UA tokens, most specific first, with the app's name (05 §8, §16.3). No heuristics. */
const IN_APP_TOKENS: readonly (readonly [RegExp, string])[] = [
  [/\bInstagram\b/, 'Instagram'],
  [/\bFBAN\/|\bFBAV\//, 'Facebook'],
  [/\bLine\//, 'LINE'],
  [/\bMicroMessenger\//, 'WeChat'],
];

/**
 * The chat or social app whose built-in browser this is (Facebook and Messenger, Instagram, LINE, WeChat), from its
 * known UA token, else null. Apps that open links in a view with a plain Safari or Chrome UA (often Discord and
 * Telegram) can't be told apart and return null; 05 §16.3 gives them a one-line tip instead.
 */
export function inAppBrowser(ua: string): string | null {
  for (const [token, name] of IN_APP_TOKENS) {
    if (token.test(ua)) return name;
  }
  return null;
}

/**
 * iOS or iPadOS: an iPhone, iPad or iPod UA, or iPadOS's desktop-class Safari, which reports "MacIntel" with touch
 * points (05 §16.3).
 */
export function isIOS(ua: string, platform: string, maxTouchPoints: number): boolean {
  if (/\b(iPhone|iPad|iPod)\b/.test(ua)) return true;
  return platform === 'MacIntel' && maxTouchPoints > 1;
}

/** A phone or tablet UA string: the fallback when userAgentData is missing (Safari, Firefox). */
export function isMobileUA(ua: string): boolean {
  return /\b(iPhone|iPad|iPod|Android|Mobile|Silk|Kindle|BlackBerry|Opera Mini|IEMobile)\b/i.test(ua);
}

const UA_DATA_OS: Readonly<Record<string, ClientOS>> = {
  windows: ClientOSWindows,
  macos: ClientOSMacOS,
  linux: ClientOSLinux,
  android: ClientOSAndroid,
  'chrome os': ClientOSChromeOS,
  chromeos: ClientOSChromeOS,
  ios: ClientOSIOS,
};

/** hello.client.os (diagnostics only). */
export function clientOS(ua: string, uaData?: UAData, platform = '', maxTouchPoints = 0): ClientOS {
  const fromHints = uaData ? UA_DATA_OS[uaData.platform.toLowerCase()] : undefined;
  if (fromHints) return fromHints;
  if (isIOS(ua, platform, maxTouchPoints)) return ClientOSIOS;
  if (/\bAndroid\b/.test(ua)) return ClientOSAndroid;
  if (/\bCrOS\b/.test(ua)) return ClientOSChromeOS;
  if (/\bWindows\b/.test(ua)) return ClientOSWindows;
  if (/\bMac OS X\b|\bMacintosh\b/.test(ua)) return ClientOSMacOS;
  if (/\bLinux\b|\bX11\b/.test(ua)) return ClientOSLinux;
  return ClientOSOther;
}

/** Chromium brands that are neither Chrome nor Edge. */
const OTHER_CHROMIUM = /\b(Opera|Brave|Vivaldi|Yandex|Samsung|Whale)\b/i;

/** hello.client.browser (diagnostics only). Chromium forks with their own brand are "other". */
export function browserName(ua: string, uaData?: UAData): BrowserName {
  if (uaData && uaData.brands.length > 0) {
    const brands = uaData.brands.map((b) => b.brand);
    if (brands.some((b) => /Microsoft Edge/i.test(b))) return 'edge';
    if (brands.some((b) => /Google Chrome/i.test(b))) return 'chrome';
    if (brands.some((b) => OTHER_CHROMIUM.test(b))) return 'other';
    if (brands.some((b) => /Chromium/i.test(b))) return 'chrome';
  }
  if (/\bEdg(e|A|iOS)?\//.test(ua)) return 'edge';
  if (/\b(OPR|OPiOS|SamsungBrowser|YaBrowser|Vivaldi|Whale)\//.test(ua)) return 'other';
  if (/\b(Firefox|FxiOS)\//.test(ua)) return 'firefox';
  if (/\b(Chrome|CriOS|Chromium)\//.test(ua)) return 'chrome';
  if (/\bVersion\/[\d.]+.*\bSafari\//.test(ua)) return 'safari';
  return 'other';
}
