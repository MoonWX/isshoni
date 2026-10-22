// DesktopPlatform (docs/m2/03 §7.3): the Platform of the SPA inside the desktop app's webview. bridge.js names the
// app and the active server; this page talks to that server only (serverOrigin), with a bearer token from Go that it
// asks for with serverOrigin in every call (docs/m2/02 §11.8, 03 §5.8).
//
// It is in the entry chunk (detect.ts imports it) and stays under 4 KB gzip (size.node.test.ts): it imports neither
// the Wails runtime nor the generated bindings. The bridge's transport (wails/WailsBridge.ts, or http/HttpBridge.ts
// for transport "http") loads with a dynamic import() started in the constructor, and calls made before it arrives
// wait for it (lazyBridge.ts); the rarely used actions (actions.ts) load when first used. For the same reason its
// private members are TypeScript's, not #private ones, which the build target lowers into helpers.
import { createLogger } from '../../lib/log';
import { CodeInvalidToken, CodeUnauthenticated } from '../../protocol/api.gen';
import { detectCaps } from '../../protocol/codecs';
import { apiErrorFromResponse } from '../../protocol/rest';
import { ClientKindDesktop, RoleViewer, type Caps, type ClientInfo } from '../../protocol/types.gen';
import { fullscreenSupport, wakeLockSupport } from '../browser/device';
import { createWebStorage, listWebStorageKeys } from '../browser/storage';
import { requestScreenWakeLock } from '../browser/wakeLock';
import type {
  DesktopFeatures,
  HelloAuthProvider,
  KeyValueStore,
  NotificationsProvider,
  Platform,
  PlatformCapabilities,
  SharingProvider,
  VersionActions,
  WakeLockHandle,
} from '../types';
import type { NativeBridge } from './bridge';
import type { DesktopConfig } from './config';
import { bridgeErrorCode } from './errors';
import { lazyBridge } from './lazyBridge';
import { installLinkInterception } from './links';
import { createPrefixedStorage, serverStoragePrefix, staleServerKeys } from './storage';
import { createTokens, type Tokens } from './tokens';

export interface DesktopPlatformOptions {
  /** The bridge itself (tests); default: the transport bridge.js names, loaded with a dynamic import(). */
  bridge?: NativeBridge;
  /** Makes platform.sharing (WA65's DesktopSharing goes here); default: none, sharing is null. */
  sharing?: (bridge: NativeBridge) => SharingProvider | null;
  /** Installs the link interception of docs/m2/03 §5.5 (default true). */
  interceptLinks?: boolean;
}

const noPushError = (): Promise<never> => Promise.reject(new Error('desktop: no push notifications'));

/** No Web Push in WebView2: the push card never shows (docs/m2/03 §7.3). */
const noPush: NotificationsProvider = {
  support: () => Promise.resolve('unsupported'),
  status: () => Promise.resolve('off'),
  enable: noPushError,
  disable: () => Promise.resolve(),
  test: noPushError,
};

function loadTransport(transport: DesktopConfig['transport']): Promise<NativeBridge> {
  return transport === 'http'
    ? import('./http/HttpBridge').then((m) => m.createHttpBridge())
    : import('./wails/WailsBridge').then((m) => m.createWailsBridge());
}

export class DesktopPlatform implements Platform {
  readonly kind = 'desktop' as const;
  readonly client: ClientInfo;
  /** C1 never publishes; the app's own connection (C2) does (docs/m2/03 §7.3). */
  readonly role = RoleViewer;
  readonly serverOrigin: string;
  readonly sharing: SharingProvider | null;
  readonly notifications = noPush;
  readonly pwa = null;
  readonly storage: { local: KeyValueStore; session: KeyValueStore };
  readonly desktop: DesktopFeatures;
  private readonly tokens: Tokens;
  private readonly log = createLogger('desktop');
  private readonly disposeLinks: () => void;
  /** The URL openExternal opened in this task (see there). */
  private lastExternal: string | null = null;

  constructor(cfg: DesktopConfig, opts: DesktopPlatformOptions = {}) {
    const { app, server } = cfg;
    const origin = server?.origin ?? '';
    this.serverOrigin = origin;
    this.client = { kind: ClientKindDesktop, version: app.version, os: app.os, browser: 'edge' };
    const bridge =
      opts.bridge ??
      lazyBridge(loadTransport(cfg.transport), (what, err) => {
        this.log.error(what, { err });
      });
    this.tokens = createTokens({ origin, bridge });

    const id = server?.id ?? null;
    const local = createWebStorage('local');
    // Keys of servers that servers.json no longer lists (docs/m2/03 §14.3).
    for (const k of staleServerKeys(listWebStorageKeys('local'), cfg.serverIds, id)) local.remove(k);
    this.storage = {
      local: createPrefixedStorage(local, serverStoragePrefix(id)),
      session: createWebStorage('session'),
    };

    const desktop: DesktopFeatures = {
      app: { version: app.version, os: app.os, osBuild: app.osBuild },
      server,
      bridge,
      wizardSteps: () => ['link', 'sound', 'voice'],
      sessionEnded: (code) => bridge.auth.sessionEnded(origin, code),
      helloRefused: (e) => bridge.auth.helloRefused(origin, e),
      signalStopped: (err) => import('./actions').then((m) => m.signalStopped(desktop, err)),
    };
    this.desktop = desktop;
    this.sharing = opts.sharing?.(bridge) ?? null;
    this.disposeLinks =
      opts.interceptLinks === false
        ? () => undefined
        : installLinkInterception({
            openExternal: (url) => {
              this.openExternal(url);
            },
          });
  }

  /** Removes the link interception and the auth.state listener (tests; the page never disposes its platform). */
  dispose(): void {
    this.disposeLinks();
    this.tokens.dispose();
  }

  capsNow(): Caps {
    // Decode capabilities matter (C1 subscribes); C1 sends nothing.
    return detectCaps();
  }

  async capabilities(): Promise<PlatformCapabilities> {
    let canShare = false;
    if (this.sharing) {
      try {
        canShare = (await this.desktop.bridge.share.capabilities()).canShare;
      } catch {
        // the engine's probe failed: no Share button
      }
    }
    return {
      caps: this.capsNow(),
      canShare,
      fullscreen: fullscreenSupport() === 'element' ? 'element' : 'none',
      // lib.dom says boolean, but a webview without PiP leaves it undefined.
      pip: (globalThis.document.pictureInPictureEnabled as boolean | undefined) === true,
      wakeLock: wakeLockSupport(),
      push: 'unsupported',
      install: 'none',
    };
  }

  /**
   * docs/m2/02 §11.8: a bearer from Auth.AccessToken(serverOrigin, false), credentials omitted; without a token (not
   * linked, or this page's server isn't active any more) the request goes without Authorization, so Public routes
   * (GET /info at boot) work and User routes get the server's own 401. A 401 invalid_token or unauthenticated to a
   * request that carried a token gets one retry with a forced token; a second 401 goes to the caller, and so does the
   * first one when the forced call fails (offline): the SPA's 401 rule runs either way.
   */
  async apiFetch(path: `/api/${string}`, init: RequestInit = {}): Promise<Response> {
    if (this.serverOrigin === '') throw new Error('desktop: no active server');
    const url = new URL(path, this.serverOrigin);
    const send = (token: string | null): Promise<Response> => {
      const headers = new Headers(init.headers);
      if (token === null) headers.delete('Authorization');
      else headers.set('Authorization', `Bearer ${token}`);
      // globalThis.fetch is looked up per call so that test interceptors apply.
      return globalThis.fetch(url, {
        ...init,
        headers,
        credentials: 'omit',
        mode: 'cors',
        referrerPolicy: 'no-referrer',
      });
    };
    const token = await this.tokens.get();
    const res = await send(token);
    if (token === null || res.status !== 401) return res;
    const { code } = await apiErrorFromResponse(res.clone());
    if (code !== CodeInvalidToken && code !== CodeUnauthenticated) return res;
    this.tokens.rejected(token);
    let fresh: string | null;
    try {
      fresh = await this.tokens.get(true);
    } catch {
      return res;
    }
    return fresh === null ? res : send(fresh);
  }

  signaling(): { url: string; auth: HelloAuthProvider } {
    let url = '';
    if (this.serverOrigin !== '') {
      const u = new URL('/ws', this.serverOrigin);
      u.protocol = u.protocol === 'https:' ? 'wss:' : 'ws:';
      url = u.href;
    }
    return {
      url,
      // undefined for null: a hello to a server that isn't active carries no token.
      auth: async ({ forceRefresh }) => {
        const token = await this.tokens.get(forceRefresh);
        return token === null ? undefined : { scheme: 'bearer', token };
      },
    };
  }

  createPeerConnection(config: RTCConfiguration): RTCPeerConnection {
    return new RTCPeerConnection(config);
  }

  /** The Screen Wake Lock API when present; Go keeps the display awake while sharing (docs/m2/03 §11.6). */
  requestWakeLock(): Promise<WakeLockHandle | null> {
    return requestScreenWakeLock();
  }

  inAppBrowser(): string | null {
    return null;
  }

  /**
   * App.OpenExternal: Go accepts https URLs only (docs/m2/03 §5.5); a refused URL is logged (its scheme) and dropped.
   * One click can get here twice, from the link interception and from the link's own onClick, so a second call with
   * the same URL in the same task is dropped.
   */
  openExternal(url: string): void {
    let key = url;
    try {
      key = new URL(url).href;
    } catch {
      // a relative URL: compared as it is
    }
    if (key === this.lastExternal) return;
    this.lastExternal = key;
    setTimeout(() => {
      this.lastExternal = null;
    }, 0);
    this.desktop.bridge.app.openExternal(url).catch((err: unknown) => {
      const scheme = /^([a-z][a-z\d+.-]*):/i.exec(url)?.[1] ?? '';
      this.log.warn('openExternal refused', { code: bridgeErrorCode(err) ?? 'unknown', scheme });
    });
  }

  /** docs/m2/03 §16.3. No reload: the SPA is the app's own build (no stale-build reload). */
  versionActions(): VersionActions {
    return {
      openInBrowser: () => {
        if (this.serverOrigin === '') return;
        const room = /^\/r\/([^/]+)/.exec(globalThis.location.pathname)?.[1];
        this.openExternal(this.serverOrigin + (room !== undefined ? `/r/${room}` : '/'));
      },
      updateApp: () =>
        import('./actions').then((m) =>
          m.updateApp(this.desktop.bridge, (url) => {
            this.openExternal(url);
          }),
        ),
    };
  }
}
