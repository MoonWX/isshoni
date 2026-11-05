// The global hotkeys as the share UI names them (docs/m2/03 §10.2, §10.3, §11.2; README WA80): the Pause and Stop
// combinations with their state, for the panel's tooltips ("Pause (Ctrl+Alt+Shift+F9)"), its "Pause hotkey
// unavailable: another app uses …" line and the first-share sheet's hotkey line.
//
// Settings.Hotkeys() is the source (Go registers the hotkeys while a share is live and reports `taken` for a
// combination another app has). Until a build binds it (it answers not_implemented, README WA84) the combinations of
// Settings.Get() stand in, as `idle`: what Go registers at live unless another app has it. The hook asks again when
// `refresh` changes (the share's state: the registration happens at live) and on settings.changed.
import { useEffect, useState } from 'react';

import type { HotkeyStatus, NativeBridge } from '../../platform/desktop/bridge';

export interface ShareHotkeys {
  pause: HotkeyStatus | null;
  stop: HotkeyStatus | null;
}

const NONE: ShareHotkeys = { pause: null, stop: null };

function isStatusList(v: unknown): v is HotkeyStatus[] {
  return Array.isArray(v) && v.every((h) => typeof h === 'object' && h !== null && 'id' in h && 'combo' in h);
}

function fromList(list: readonly HotkeyStatus[]): ShareHotkeys {
  return {
    pause: list.find((h) => h.id === 'pause') ?? null,
    stop: list.find((h) => h.id === 'stop') ?? null,
  };
}

/** Settings.Hotkeys(), or Settings.Get()'s combinations as idle; nothing when neither answers. */
export async function loadHotkeys(bridge: NativeBridge): Promise<ShareHotkeys> {
  try {
    const list: unknown = await bridge.settings.hotkeys();
    if (isStatusList(list)) return fromList(list);
  } catch {
    // not_implemented until the bound method exists: the settings' combinations below.
  }
  try {
    const s = await bridge.settings.get();
    const combos = (s as { hotkeys?: { pause?: unknown; stop?: unknown } } | undefined)?.hotkeys;
    const idle = (id: string, combo: unknown): HotkeyStatus | null =>
      typeof combo === 'string' && combo !== '' ? { id, combo, state: 'idle' } : null;
    return { pause: idle('pause', combos?.pause), stop: idle('stop', combos?.stop) };
  } catch {
    return NONE;
  }
}

/** Whether a hotkey works when the share is live: registered, or valid and not registered yet. */
export function usable(h: HotkeyStatus | null): h is HotkeyStatus {
  return h !== null && (h.state === 'registered' || h.state === 'idle');
}

/** The hotkeys; both null in the browser and until the first answer. */
export function useHotkeys(bridge: NativeBridge | null, refresh: unknown = null): ShareHotkeys {
  const [hotkeys, setHotkeys] = useState<ShareHotkeys>(NONE);
  useEffect(() => {
    if (bridge === null) return undefined;
    let alive = true;
    const load = (): void => {
      void loadHotkeys(bridge).then((h) => {
        if (alive) setHotkeys(h);
      });
    };
    const off = bridge.on('settings.changed', load);
    load();
    return () => {
      alive = false;
      off();
    };
  }, [bridge, refresh]);
  return hotkeys;
}
