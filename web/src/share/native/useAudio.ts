// React's side of 05's audio service through the bridge (docs/m2/03 §6.2, §6.3, §10.2, §10.3; 05 §14.4–§14.5):
// "What friends hear" (Audio.FriendsHear and the audio.report event), the level meters (Audio.WatchLevels and
// audio.levels at 10 Hz) and the watch of the pages that show the app list (Audio.Watch: the share sheet, the
// first-share sheet, the wizard's voice step, Settings → Audio). Each hook subscribes first and then pulls, so it
// misses nothing (03 §6.3), and ends its watch on unmount: the bridge counts the watchers.
import { useEffect, useState } from 'react';

import { useApp } from '../../app/context';
import type { FriendsHear, Levels, NativeBridge } from '../../platform/desktop/bridge';

/** The desktop app's bridge; null in the browser (or a page without the desktop features). */
export function useAudioBridge(): NativeBridge | null {
  const { platform } = useApp();
  return platform.desktop?.bridge ?? null;
}

/** Whether a FriendsHear payload has the fields this UI reads (a fake bridge may answer nothing). */
function isFriendsHear(v: unknown): v is FriendsHear {
  return typeof v === 'object' && v !== null && Array.isArray((v as FriendsHear).heard);
}

/**
 * The latest "What friends hear"; null until the first one arrived (and always in the browser). With `watch`, the
 * page counts as one that shows the app list (Audio.Watch(true) on mount, false on unmount): 05's watcher runs, so
 * the list is current before a share too (05 §14.2).
 */
export function useFriendsHear(bridge: NativeBridge | null, opts: { watch?: boolean } = {}): FriendsHear | null {
  const [fh, setFh] = useState<FriendsHear | null>(null);
  const watch = opts.watch === true;
  useEffect(() => {
    if (bridge === null) return undefined;
    let alive = true;
    let seq = -1;
    const take = (next: unknown): void => {
      // A pulled answer may come after a newer event: the higher seq wins (05 §14.5: seq goes up with every change).
      if (!alive || !isFriendsHear(next) || next.seq < seq) return;
      seq = next.seq;
      setFh(next);
    };
    const off = bridge.on('audio.report', take);
    if (watch) bridge.audio.watch(true);
    bridge.audio.friendsHear().then(take, () => undefined);
    return () => {
      alive = false;
      off();
      if (watch) bridge.audio.watch(false);
    };
  }, [bridge, watch]);
  return fh;
}

/** The latest level sample while `on` (Audio.WatchLevels: 10 Hz audio.levels); null while off or before the first. */
export function useLevels(bridge: NativeBridge | null, on: boolean): Levels | null {
  const [levels, setLevels] = useState<Levels | null>(null);
  useEffect(() => {
    if (bridge === null || !on) return undefined;
    const off = bridge.on('audio.levels', (l) => {
      setLevels(l);
    });
    bridge.audio.watchLevels(true);
    return () => {
      off();
      bridge.audio.watchLevels(false);
      setLevels(null);
    };
  }, [bridge, on]);
  return levels;
}
