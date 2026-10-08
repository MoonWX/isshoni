// React's side of the stage's controls (05 §12.5, §12.8): ViewerLayout makes the fullscreen and PiP controllers
// for its own elements, holds the wake lock while a share is watched, and names what plays for the system. Each
// hook follows what the platform says it can do (PlatformCapabilities, 05 §8); the controllers themselves don't
// import React (05 §3).
//
// The controllers find the stage through the layout's root element: the stage is the element marked
// `data-viewer-stage`, and its <video> changes with the focused share, so it is looked up when it is needed.
import { useEffect, useMemo, useState } from 'react';
import { useTranslation } from 'react-i18next';
import { useStore } from 'zustand';

import type { Platform, PlatformCapabilities } from '../platform/types';
import { createFullscreen, type FullscreenController, type FullscreenMode } from './fullscreen';
import { attachMediaSession } from './mediaSession';
import { createPip, type PipController } from './pip';
import type { ViewerServices } from './services';
import { createWakeLock, isWatching } from './wakeLock';

/** The stage inside the viewer's layout (Stage.tsx sets the attribute). */
export const STAGE = '[data-viewer-stage]';

function stageVideo(root: HTMLElement | null): HTMLVideoElement | null {
  return root?.querySelector<HTMLVideoElement>(`${STAGE} video`) ?? null;
}

function stageShareId(root: HTMLElement | null): string | null {
  return root?.querySelector(STAGE)?.getAttribute('data-share-id') ?? null;
}

/**
 * What this device can do (platform.capabilities(), 05 §8): null until the platform has answered, and without a
 * platform. Until then nothing that needs a capability is offered.
 */
export function useCapabilities(platform: Platform | null): PlatformCapabilities | null {
  const [known, setKnown] = useState<{ platform: Platform; caps: PlatformCapabilities } | null>(null);
  useEffect(() => {
    if (platform === null) return undefined;
    let current = true;
    platform.capabilities().then(
      (caps) => {
        if (current) setKnown({ platform, caps });
      },
      () => undefined, // no answer: nothing is offered
    );
    return () => {
      current = false;
    };
  }, [platform]);
  return known !== null && known.platform === platform ? known.caps : null;
}

/**
 * The fullscreen controller for a layout (fullscreen.ts): `root` is the layout's element, which goes fullscreen
 * with the stage on it. It lives while the layout does, and leaves fullscreen when the layout goes.
 */
export function useFullscreen(
  viewer: ViewerServices,
  root: HTMLElement | null,
  mode: FullscreenMode,
): FullscreenController {
  const fullscreen = useMemo(
    () =>
      createFullscreen({
        store: viewer.store,
        mode,
        target: () => ({ container: root, video: stageVideo(root) }),
        onNativeExit: () => {
          viewer.videos.resume();
        },
      }),
    [viewer, root, mode],
  );
  useEffect(() => fullscreen.attach(), [fullscreen]);

  // Each share has its own <video> on the stage: a native player that showed the one of before is gone.
  const focused = useStore(viewer.store, (s) => s.focusedShareId);
  useEffect(() => {
    fullscreen.refresh();
  }, [fullscreen, focused]);

  // Nothing on the stage: nothing to fill the screen with, and no button on it to leave with (a phone has no Esc).
  const on = useStore(viewer.store, (s) => s.fullscreen);
  useEffect(() => {
    if (on && focused === null) fullscreen.exit();
  }, [fullscreen, on, focused]);

  return fullscreen;
}

/**
 * The PiP controller for a layout (pip.ts); `supported` is PlatformCapabilities.pip. The window closes when the
 * stage moves to another share and when the layout goes.
 */
export function usePip(viewer: ViewerServices, root: HTMLElement | null, supported: boolean): PipController {
  const pip = useMemo(
    () =>
      createPip({
        store: viewer.store,
        supported,
        target: () => ({ video: stageVideo(root), shareId: stageShareId(root) }),
      }),
    [viewer, root, supported],
  );
  useEffect(() => pip.attach(), [pip]);

  const focused = useStore(viewer.store, (s) => s.focusedShareId);
  useEffect(() => {
    pip.refresh();
  }, [pip, focused]);

  return pip;
}

/**
 * Keeps the screen on while the layout shows someone's share and the page is visible (wakeLock.ts). `supported`
 * is PlatformCapabilities.wakeLock; without a platform nothing is asked for.
 */
export function useWakeLock(viewer: ViewerServices, platform: Platform | null, supported: boolean): void {
  const watching = useStore(viewer.store, isWatching);
  const lock = useMemo(
    () => (platform === null ? null : createWakeLock({ request: () => platform.requestWakeLock() })),
    [platform],
  );
  useEffect(() => {
    lock?.set(supported && watching);
  }, [lock, supported, watching]);
  useEffect(
    () => () => {
      lock?.dispose();
    },
    [lock],
  );
}

/**
 * Tells the system what is heard (mediaSession.ts): the audible share's title, and the room's name as the artist.
 * Without a room name the app's own name stands in.
 */
export function useMediaSession(viewer: ViewerServices, roomName: string | undefined): void {
  const { t } = useTranslation();
  useEffect(
    () =>
      attachMediaSession(viewer, {
        t,
        artist: () => (roomName !== undefined && roomName !== '' ? roomName : t('common.appName')),
      }),
    [viewer, t, roomName],
  );
}
