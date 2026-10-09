// The stage's controls in the layout (05 §12.5, §12.8): fullscreen, picture-in-picture and the wake lock are
// feature-detected through the platform's capabilities (05 §8). Each way of going fullscreen; PiP only where the
// platform has it, never on iOS; the wake lock only while a share is watched on a visible page.
import { act, fireEvent, render, screen, within } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { BrowserPlatform } from '../platform/browser/BrowserPlatform';
import type { Platform, PlatformCapabilities, WakeLockHandle } from '../platform/types';
import {
  FakeMediaStream,
  FakeMediaStreamTrack,
  installFakeMedia,
  installFakeMediaElement,
  type FakeMediaElementControl,
} from '../test/fakeMedia';
import { createTestPlatform } from '../test/platform';
import { createTestServices, renderWithApp } from '../test/render';
import { createViewer, syncRoom, type ViewerServices } from './services';
import { installFakeIntersectionObserver, room, SELF, shareInfo } from './testing';
import { ViewerLayout } from './ViewerLayout';

const BEA = shareInfo('s_bea', 'u_bea', 1);
const CY = shareInfo('s_cy', 'u_cy', 2, { kind: 'tab' });
const MINE = shareInfo('s_mine', 'u_alex', 3, { connectionId: 'c_me', kind: 'screen' });

const UA_IPHONE =
  'Mozilla/5.0 (iPhone; CPU iPhone OS 26_0 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/26.0 Mobile/15E148 Safari/604.1';
const UA_MAC =
  'Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/26.0 Safari/605.1.15';

let media: FakeMediaElementControl;
let viewer: ViewerServices;
const cleanups: (() => void)[] = [];

beforeEach(() => {
  installFakeMedia();
  media = installFakeMediaElement();
  viewer = createViewer();
});

afterEach(() => {
  viewer.dispose();
  media.restore();
  for (const cleanup of cleanups.splice(0)) cleanup();
  vi.unstubAllGlobals();
});

/** Defines a property for one test. */
function define(target: object, name: string, descriptor: PropertyDescriptor): void {
  Object.defineProperty(target, name, { configurable: true, ...descriptor });
  cleanups.push(() => Reflect.deleteProperty(target, name));
}

/** A platform that reports these capabilities; the rest is the test platform's. */
function platformWith(caps: Partial<PlatformCapabilities>, over: Partial<Platform> = {}): Platform {
  const base = createTestPlatform(over);
  return {
    ...base,
    capabilities: async () => ({ ...(await base.capabilities()), fullscreen: 'none', ...caps }),
  };
}

/** Renders the layout inside the app's providers and waits for the platform's answer. */
async function renderLayout(platform: Platform, ...shares: Parameters<typeof room>) {
  syncRoom(viewer, room(...shares), SELF);
  const result = renderWithApp(<ViewerLayout viewer={viewer} localPreviews={{ s_mine: preview() }} />, {
    services: createTestServices({ platform }),
  });
  await act(async () => {
    await Promise.resolve();
    await Promise.resolve();
  });
  return result;
}

const preview = () => new FakeMediaStream([new FakeMediaStreamTrack('video')]) as unknown as MediaStream;
const toolbar = () => screen.getByRole('toolbar', { name: 'Stage controls' });
const layout = (): HTMLElement => {
  const el = screen.getByRole('region', { name: /^Now watching|^Your preview|^Stage$/ }).parentElement?.parentElement;
  if (!el) throw new Error('no layout');
  return el;
};
const stageVideo = (): HTMLVideoElement => {
  const el = document.querySelector<HTMLVideoElement>('[data-viewer-stage] video');
  if (!el) throw new Error('no stage video');
  return el;
};
const isFullscreen = () => viewer.store.getState().fullscreen;

describe('fullscreen, by what the platform can do (05 §12.5)', () => {
  it('element fullscreen: asks for the layout, so the stage keeps its bar, and follows the browser', async () => {
    const now: { el: Element | null } = { el: null };
    const request = vi.fn(function (this: Element) {
      now.el = this;
      document.dispatchEvent(new Event('fullscreenchange'));
      return Promise.resolve();
    });
    const exit = vi.fn(() => {
      now.el = null;
      document.dispatchEvent(new Event('fullscreenchange'));
      return Promise.resolve();
    });
    define(document, 'fullscreenElement', { get: () => now.el });
    define(document, 'exitFullscreen', { value: exit });
    define(HTMLElement.prototype, 'requestFullscreen', { value: request });

    await renderLayout(platformWith({ fullscreen: 'element' }), BEA, CY);
    fireEvent.click(within(toolbar()).getByRole('button', { name: 'Fullscreen' }));
    expect(request).toHaveBeenCalledOnce();
    expect(request.mock.contexts[0]).toBe(layout());
    expect(isFullscreen()).toBe(true);
    // Only the stage is left, with its controls; the tiles are gone.
    expect(screen.queryByRole('list', { name: 'Shares' })).not.toBeInTheDocument();
    expect(layout()).toContainElement(toolbar());

    fireEvent.click(within(toolbar()).getByRole('button', { name: 'Exit fullscreen' }));
    expect(exit).toHaveBeenCalledOnce();
    expect(isFullscreen()).toBe(false);
    expect(screen.getByRole('list', { name: 'Shares' })).toBeInTheDocument();

    // Esc is the browser's: the layout follows.
    fireEvent.click(within(toolbar()).getByRole('button', { name: 'Fullscreen' }));
    act(() => {
      now.el = null;
      document.dispatchEvent(new Event('fullscreenchange'));
    });
    expect(isFullscreen()).toBe(false);
    expect(within(toolbar()).getByRole('button', { name: 'Fullscreen' })).toBeInTheDocument();
  });

  it('iPhone: opens the stage’s video in the native player (video-only)', async () => {
    viewer.registry.set('s_cy', 'video', new FakeMediaStreamTrack('video') as unknown as MediaStreamTrack);
    await renderLayout(platformWith({ fullscreen: 'video-only' }), BEA, CY);
    const video = stageVideo();
    const enter = vi.fn(() => {
      video.dispatchEvent(new Event('webkitbeginfullscreen'));
    });
    const exit = vi.fn(() => {
      video.dispatchEvent(new Event('webkitendfullscreen'));
    });
    define(video, 'webkitEnterFullscreen', { value: enter });
    define(video, 'webkitExitFullscreen', { value: exit });
    define(video, 'webkitSupportsFullscreen', { value: true });

    fireEvent.click(within(toolbar()).getByRole('button', { name: 'Fullscreen' }));
    expect(enter).toHaveBeenCalledOnce();
    expect(isFullscreen()).toBe(true);

    // The player's own "Done": back to the page, and the video, which iOS paused with it, plays again.
    video.pause();
    media.played.length = 0;
    act(() => {
      video.dispatchEvent(new Event('webkitendfullscreen'));
    });
    expect(isFullscreen()).toBe(false);
    expect(media.played).toContain(video);
  });

  it('nothing to ask the browser for: the CSS pseudo-fullscreen (none)', async () => {
    const request = vi.fn();
    define(HTMLElement.prototype, 'requestFullscreen', { value: request });
    await renderLayout(platformWith({ fullscreen: 'none' }), BEA, CY);
    const before = layout().className;

    fireEvent.click(within(toolbar()).getByRole('button', { name: 'Fullscreen' }));
    expect(request).not.toHaveBeenCalled();
    expect(isFullscreen()).toBe(true);
    expect(layout().className).not.toBe(before);
    expect(screen.queryByRole('list', { name: 'Shares' })).not.toBeInTheDocument();

    fireEvent.click(within(toolbar()).getByRole('button', { name: 'Exit fullscreen' }));
    expect(isFullscreen()).toBe(false);
    expect(layout().className).toBe(before);
  });

  it('toggles on a double click on the stage, and not on one of its controls', async () => {
    await renderLayout(platformWith({ fullscreen: 'none' }), BEA);
    const stage = screen.getByRole('region', { name: "Now watching: Bea's window" });
    fireEvent.dblClick(stage);
    expect(isFullscreen()).toBe(true);
    fireEvent.dblClick(within(toolbar()).getByRole('button', { name: 'Mute' }));
    expect(isFullscreen()).toBe(true);
    fireEvent.dblClick(stage);
    expect(isFullscreen()).toBe(false);
  });

  it('offers fullscreen for the own preview too, and works outside the app’s providers', () => {
    syncRoom(viewer, room(MINE), SELF);
    render(<ViewerLayout viewer={viewer} localPreviews={{ s_mine: preview() }} />);
    fireEvent.click(screen.getByRole('button', { name: /^Show your preview/ }));
    fireEvent.click(within(toolbar()).getByRole('button', { name: 'Fullscreen' }));
    expect(isFullscreen()).toBe(true);
  });

  it('ends when the stage becomes empty: no button would be left to leave it with', async () => {
    await renderLayout(platformWith({ fullscreen: 'none' }), BEA);
    fireEvent.click(within(toolbar()).getByRole('button', { name: 'Fullscreen' }));
    expect(isFullscreen()).toBe(true);
    act(() => {
      syncRoom(viewer, room(), SELF);
    });
    expect(isFullscreen()).toBe(false);
  });

  it('ends when the layout is unmounted (the user left the room page)', async () => {
    const { unmount } = await renderLayout(platformWith({ fullscreen: 'none' }), BEA);
    fireEvent.click(within(toolbar()).getByRole('button', { name: 'Fullscreen' }));
    unmount();
    expect(isFullscreen()).toBe(false);
  });
});

describe('picture-in-picture, only where the platform has it (05 §12.5, §12.8)', () => {
  /** A browser with the PiP calls: the element in the window, and what was asked. */
  function installPip() {
    const now: { el: Element | null } = { el: null };
    const request = vi.fn(function (this: Element) {
      now.el = this;
      this.dispatchEvent(new Event('enterpictureinpicture', { bubbles: true }));
      return Promise.resolve({});
    });
    const exit = vi.fn(() => {
      const el = now.el;
      now.el = null;
      el?.dispatchEvent(new Event('leavepictureinpicture', { bubbles: true }));
      return Promise.resolve();
    });
    define(document, 'pictureInPictureEnabled', { value: true });
    define(document, 'pictureInPictureElement', { get: () => now.el });
    define(document, 'exitPictureInPicture', { value: exit });
    define(HTMLVideoElement.prototype, 'requestPictureInPicture', { value: request });
    return { request, exit };
  }

  it('has a button that puts the stage’s video in the floating window', async () => {
    const pip = installPip();
    await renderLayout(platformWith({ pip: true }), BEA, CY);
    const video = stageVideo();
    fireEvent.click(within(toolbar()).getByRole('button', { name: 'Picture in picture' }));
    expect(pip.request.mock.contexts[0]).toBe(video);
    expect(viewer.store.getState().pipShareId).toBe('s_cy');

    fireEvent.click(within(toolbar()).getByRole('button', { name: 'Close picture in picture' }));
    expect(pip.exit).toHaveBeenCalledOnce();
    expect(viewer.store.getState().pipShareId).toBeNull();
  });

  it('closes the window when the stage moves to another share', async () => {
    const pip = installPip();
    await renderLayout(platformWith({ pip: true }), BEA, CY);
    fireEvent.click(within(toolbar()).getByRole('button', { name: 'Picture in picture' }));
    fireEvent.click(screen.getByRole('button', { name: /^Watch Bea's window/ }));
    expect(viewer.store.getState().pipShareId).toBeNull();
    expect(pip.exit).toHaveBeenCalledOnce();
    expect(within(toolbar()).getByRole('button', { name: 'Picture in picture' })).toBeInTheDocument();
  });

  it('has no button where the platform reports none, though the browser has the call', async () => {
    installPip();
    await renderLayout(platformWith({ pip: false }), BEA, CY);
    expect(within(toolbar()).getByRole('button', { name: 'Fullscreen' })).toBeInTheDocument();
    expect(screen.queryByRole('button', { name: /icture in picture/ })).not.toBeInTheDocument();
  });

  it('is hidden on iOS: the browser platform reports no PiP there, whatever the browser says', async () => {
    installPip();
    const iphone = new BrowserPlatform({
      env: { ua: UA_IPHONE, platform: 'iPhone', maxTouchPoints: 5, matches: () => false },
    });
    await expect(iphone.capabilities()).resolves.toMatchObject({ pip: false });
    await renderLayout(iphone, BEA, CY);
    expect(within(toolbar()).getByRole('button', { name: 'Fullscreen' })).toBeInTheDocument();
    expect(screen.queryByRole('button', { name: /icture in picture/ })).not.toBeInTheDocument();
  });

  it('is hidden on an iPad, which calls itself a Mac, and shown on a Mac', async () => {
    installPip();
    const ipad = new BrowserPlatform({
      env: { ua: UA_MAC, platform: 'MacIntel', maxTouchPoints: 5, matches: () => false },
    });
    const first = await renderLayout(ipad, BEA, CY);
    expect(screen.queryByRole('button', { name: /icture in picture/ })).not.toBeInTheDocument();
    first.unmount();

    const mac = new BrowserPlatform({
      env: { ua: UA_MAC, platform: 'MacIntel', maxTouchPoints: 0, matches: () => false },
    });
    await renderLayout(mac, BEA, CY);
    expect(within(toolbar()).getByRole('button', { name: 'Picture in picture' })).toBeInTheDocument();
  });

  it('has no button for the own preview, and none before the platform has answered', async () => {
    installPip();
    await renderLayout(platformWith({ pip: true }), BEA, MINE);
    expect(within(toolbar()).getByRole('button', { name: 'Picture in picture' })).toBeInTheDocument();
    fireEvent.click(screen.getByRole('button', { name: /^Show your preview/ }));
    expect(screen.queryByRole('button', { name: /icture in picture/ })).not.toBeInTheDocument();
  });

  it('offers nothing outside the app’s providers', () => {
    installPip();
    syncRoom(viewer, room(BEA), SELF);
    render(<ViewerLayout viewer={viewer} />);
    expect(screen.queryByRole('button', { name: /icture in picture/ })).not.toBeInTheDocument();
  });
});

describe('what plays, for the system’s media notification (05 §12.8)', () => {
  class FakeMetadata {
    readonly title: string;
    readonly artist: string;
    constructor(init: MediaMetadataInit = {}) {
      this.title = init.title ?? '';
      this.artist = init.artist ?? '';
    }
  }
  let session: { metadata: FakeMetadata | null };

  beforeEach(() => {
    session = { metadata: null };
    define(navigator, 'mediaSession', { value: session });
    vi.stubGlobal('MediaMetadata', FakeMetadata);
  });

  it('names the audible share, with the room as the artist', () => {
    syncRoom(viewer, room(BEA, CY), SELF);
    const { rerender, unmount } = render(<ViewerLayout viewer={viewer} roomName="Lounge" />);
    expect(session.metadata).toMatchObject({ title: "Cy's tab", artist: 'Lounge' });

    fireEvent.click(screen.getByRole('button', { name: 'Listen to Bea' }));
    expect(session.metadata).toMatchObject({ title: "Bea's window", artist: 'Lounge' });

    // The room was renamed.
    rerender(<ViewerLayout viewer={viewer} roomName="Movie room" />);
    expect(session.metadata).toMatchObject({ title: "Bea's window", artist: 'Movie room' });

    unmount();
    expect(session.metadata).toBeNull();
  });

  it('uses the app’s name while it knows no room name', () => {
    syncRoom(viewer, room(BEA), SELF);
    render(<ViewerLayout viewer={viewer} />);
    expect(session.metadata).toMatchObject({ title: "Bea's window", artist: 'isshoni' });
  });
});

describe('the screen wake lock (05 §12.5)', () => {
  /** A platform whose wake locks the test counts. */
  function lockingPlatform(wakeLock: boolean) {
    const releases: ReturnType<typeof vi.fn<() => Promise<void>>>[] = [];
    const requestWakeLock = vi.fn((): Promise<WakeLockHandle | null> => {
      const release = vi.fn(() => Promise.resolve());
      releases.push(release);
      return Promise.resolve({ release });
    });
    return { platform: platformWith({ wakeLock }, { requestWakeLock }), requestWakeLock, releases };
  }
  const settle = () =>
    act(async () => {
      await Promise.resolve();
      await Promise.resolve();
    });

  let hidden = false;
  const setHidden = (next: boolean): void => {
    act(() => {
      hidden = next;
      document.dispatchEvent(new Event('visibilitychange'));
    });
  };

  beforeEach(() => {
    hidden = false;
    define(document, 'visibilityState', { get: () => (hidden ? 'hidden' : 'visible') });
  });

  it('is held while a share is watched, and released when nothing is', async () => {
    const { platform, requestWakeLock, releases } = lockingPlatform(true);
    await renderLayout(platform);
    expect(requestWakeLock).not.toHaveBeenCalled(); // an empty room

    act(() => {
      syncRoom(viewer, room(BEA), SELF);
    });
    await settle();
    expect(requestWakeLock).toHaveBeenCalledTimes(1);
    expect(releases[0]).not.toHaveBeenCalled();

    act(() => {
      syncRoom(viewer, room(), SELF);
    });
    expect(releases[0]).toHaveBeenCalledOnce();
  });

  it('is released when the page is hidden and requested again when it is back', async () => {
    const { platform, requestWakeLock, releases } = lockingPlatform(true);
    await renderLayout(platform, BEA);
    await settle();
    expect(requestWakeLock).toHaveBeenCalledTimes(1);

    setHidden(true);
    expect(releases[0]).toHaveBeenCalledOnce();
    setHidden(false);
    await settle();
    expect(requestWakeLock).toHaveBeenCalledTimes(2);
    expect(releases[1]).not.toHaveBeenCalled();
  });

  it('is released when the layout is unmounted', async () => {
    const { platform, releases } = lockingPlatform(true);
    const { unmount } = await renderLayout(platform, BEA);
    await settle();
    unmount();
    expect(releases[0]).toHaveBeenCalledOnce();
  });

  it('is not held for the page’s own preview alone', async () => {
    const { platform, requestWakeLock } = lockingPlatform(true);
    await renderLayout(platform, MINE);
    await settle();
    expect(requestWakeLock).not.toHaveBeenCalled();
  });

  it('is never asked for where the platform has no wake lock', async () => {
    const { platform, requestWakeLock } = lockingPlatform(false);
    await renderLayout(platform, BEA);
    await settle();
    expect(requestWakeLock).not.toHaveBeenCalled();
  });

  it('follows what is in view: no lock for tiles that are all scrolled away', async () => {
    const io = installFakeIntersectionObserver();
    const { platform, requestWakeLock, releases } = lockingPlatform(true);
    await renderLayout(platform, BEA);
    await settle();
    expect(requestWakeLock).toHaveBeenCalledTimes(1);
    act(() => {
      for (const el of io.observed()) io.show(el, 0);
    });
    expect(releases[0]).toHaveBeenCalledOnce();
  });
});
