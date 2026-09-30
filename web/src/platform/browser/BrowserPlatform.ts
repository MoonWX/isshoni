// BrowserPlatform (05 §8): the only Platform in M1. Everything here is feature detection; the UA string only
// chooses diagnostics (hello.client) and help text, plus the phone-or-tablet rule, which has no feature to detect.
import { browserName, clientOS, inAppBrowser } from '../../lib/ua';
import { detectCaps } from '../../protocol/codecs';
import { ClientKindWeb, RoleFull, RoleViewer, type Caps, type ClientInfo } from '../../protocol/types.gen';
import type {
  KeyValueStore,
  NotificationsProvider,
  Platform,
  PlatformCapabilities,
  PwaProvider,
  SharingProvider,
  VersionActions,
  WakeLockHandle,
} from '../types';
import { deviceEnv, fullscreenSupport, isHandheld, pipSupport, wakeLockSupport, type DeviceEnv } from './device';
import { createBrowserSharing } from './displayMedia';
import { createBrowserNotifications } from './push';
import { createPwaProvider } from './pwa';
import { createWebStorage } from './storage';
import { requestScreenWakeLock } from './wakeLock';

export interface BrowserPlatformOptions {
  /** Device facts; default: the real navigator and matchMedia. */
  env?: DeviceEnv;
  /** The server's origin; default: location.origin (the SPA is always served by its server). */
  origin?: string;
}

/**
 * Whether this browser may share (05 §8, §13.8): getDisplayMedia exists and the sender encodes H.264 (01's
 * detectCaps reports both as displayCapture), and the device is not a phone or tablet. Not a browser allowlist:
 * desktop Firefox and Safari pass too.
 */
export function canShareHere(caps: Caps, env: DeviceEnv): boolean {
  return caps.displayCapture === true && !isHandheld(env);
}

export class BrowserPlatform implements Platform {
  readonly kind = 'browser' as const;
  readonly client: ClientInfo;
  readonly role: 'full' | 'viewer';
  readonly serverOrigin: string;
  readonly sharing: SharingProvider | null;
  readonly notifications: NotificationsProvider;
  readonly pwa: PwaProvider | null;
  readonly storage: { local: KeyValueStore; session: KeyValueStore };
  readonly #env: DeviceEnv;

  constructor(opts: BrowserPlatformOptions = {}) {
    const env = opts.env ?? deviceEnv();
    this.#env = env;
    this.serverOrigin = opts.origin ?? globalThis.location.origin;
    this.client = {
      kind: ClientKindWeb,
      version: __ISSHONI_VERSION__,
      os: clientOS(env.ua, env.uaData, env.platform, env.maxTouchPoints),
      browser: browserName(env.ua, env.uaData),
    };
    this.sharing = canShareHere(detectCaps(), env) ? createBrowserSharing() : null;
    // 01 §6.3: a viewer connection can't publish; the server enforces it.
    this.role = this.sharing ? RoleFull : RoleViewer;
    this.notifications = createBrowserNotifications();
    this.pwa = createPwaProvider();
    this.storage = { local: createWebStorage('local'), session: createWebStorage('session') };
  }

  capsNow(): Caps {
    return detectCaps();
  }

  async capabilities(): Promise<PlatformCapabilities> {
    return {
      caps: this.capsNow(),
      canShare: this.sharing !== null,
      fullscreen: fullscreenSupport(),
      pip: pipSupport(this.#env),
      wakeLock: wakeLockSupport(),
      push: await this.notifications.support(),
      install: this.pwa?.installState() ?? 'none',
    };
  }

  apiFetch(path: `/api/${string}`, init?: RequestInit): Promise<Response> {
    // The session cookie is same-origin (03 §7.4); globalThis.fetch is looked up per call so test interceptors apply.
    return globalThis.fetch(new URL(path, this.serverOrigin), { credentials: 'same-origin', ...init });
  }

  signaling(): { url: string } {
    const url = new URL('/ws', this.serverOrigin);
    url.protocol = url.protocol === 'https:' ? 'wss:' : 'ws:';
    return { url: url.href };
  }

  createPeerConnection(config: RTCConfiguration): RTCPeerConnection {
    return new RTCPeerConnection(config);
  }

  requestWakeLock(): Promise<WakeLockHandle | null> {
    return requestScreenWakeLock();
  }

  inAppBrowser(): string | null {
    return inAppBrowser(this.#env.ua);
  }

  openExternal(url: string): void {
    globalThis.open(url, '_blank', 'noopener,noreferrer');
  }

  versionActions(): VersionActions {
    return {
      reload: () => {
        globalThis.location.reload();
      },
    };
  }
}
