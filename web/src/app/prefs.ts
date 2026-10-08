// prefsStore (05 §6.1): per-device preferences, persisted through platform.storage.local (a try/catch wrapper, so
// the page works when storage is blocked). lastRoomId has its own key, isshoni.lastRoomId, because 01 §10.5 reads
// it for rejoining; the rest is one JSON value under isshoni.prefs. Values that fail validation fall back to the
// defaults one by one.
//
// Other tabs share the storage, and each tab's store holds what it loaded. So a change writes only what changed:
// lastRoomId only when this tab joins another room, and the JSON value read back, with just the changed fields
// replaced (dismissals are merged), so a volume change in one tab doesn't undo a room or a dismissal from another.
import { createStore, type StoreApi } from 'zustand/vanilla';

import { PresetAuto, PresetGame, PresetMovie, PresetText, type Preset } from '../protocol/types.gen';
import type { KeyValueStore } from '../platform/types';

/** 01 §10.5 reads this key. */
export const LAST_ROOM_KEY = 'isshoni.lastRoomId';
export const PREFS_KEY = 'isshoni.prefs';

const PRESETS: readonly Preset[] = [PresetAuto, PresetGame, PresetMovie, PresetText];

export interface Prefs {
  /** Stage volume, 0–1 (05 §10.3). iOS ignores it. */
  readonly volume: number;
  /** The last room joined; the root redirect goes there if it still exists (05 §5). */
  readonly lastRoomId: string | null;
  /** The sharer's last preset (05 §13.5). */
  readonly preset: Preset;
  /** One-time cards and hints: id → when it was dismissed (Date.now()). */
  readonly dismissed: Readonly<Record<string, number>>;
  /** The debug overlay is open (05 §10.7). */
  readonly debug: boolean;
}

export interface PrefsActions {
  setVolume(volume: number): void;
  setLastRoomId(roomId: string | null): void;
  setPreset(preset: Preset): void;
  dismiss(id: string, at?: number): void;
  /** Whether id was dismissed less than maxAgeMs ago (every dismissal counts when maxAgeMs is omitted). */
  isDismissed(id: string, maxAgeMs?: number, now?: number): boolean;
  setDebug(debug: boolean): void;
}

export type PrefsState = Prefs & PrefsActions;
export type PrefsStore = StoreApi<PrefsState>;

export const DEFAULT_PREFS: Prefs = Object.freeze({
  volume: 1,
  lastRoomId: null,
  preset: PresetAuto,
  dismissed: Object.freeze({}),
  debug: false,
});

function clampVolume(v: number): number {
  return Math.min(1, Math.max(0, v));
}

function isRecord(v: unknown): v is Record<string, unknown> {
  return typeof v === 'object' && v !== null && !Array.isArray(v);
}

/** Reads stored prefs, keeping every valid field and defaulting the rest. */
export function loadPrefs(storage: KeyValueStore): Prefs {
  let raw: unknown;
  try {
    raw = JSON.parse(storage.get(PREFS_KEY) ?? 'null');
  } catch {
    raw = null;
  }
  const r = isRecord(raw) ? raw : {};
  const dismissed: Record<string, number> = {};
  if (isRecord(r['dismissed'])) {
    for (const [k, v] of Object.entries(r['dismissed'])) {
      if (typeof v === 'number' && Number.isFinite(v)) dismissed[k] = v;
    }
  }
  const lastRoomId = storage.get(LAST_ROOM_KEY);
  return {
    volume:
      typeof r['volume'] === 'number' && Number.isFinite(r['volume']) ? clampVolume(r['volume']) : DEFAULT_PREFS.volume,
    lastRoomId: lastRoomId !== null && lastRoomId !== '' ? lastRoomId : null,
    preset: PRESETS.find((p) => p === r['preset']) ?? DEFAULT_PREFS.preset,
    dismissed,
    debug: r['debug'] === true,
  };
}

type JsonPrefs = Omit<Prefs, 'lastRoomId'>;
const JSON_FIELDS = ['volume', 'preset', 'dismissed', 'debug'] as const satisfies readonly (keyof JsonPrefs)[];

/** Both tabs' dismissals; an id dismissed in both keeps the later time. */
function mergeDismissed(
  stored: Readonly<Record<string, number>>,
  mine: Readonly<Record<string, number>>,
): Record<string, number> {
  const out: Record<string, number> = { ...stored };
  for (const [id, at] of Object.entries(mine)) out[id] = Math.max(out[id] ?? at, at);
  return out;
}

/** Writes the fields that changed from prev to next, on top of what storage holds now. */
function saveChanges(storage: KeyValueStore, next: Prefs, prev: Prefs): void {
  if (next.lastRoomId !== prev.lastRoomId) {
    if (next.lastRoomId === null) storage.remove(LAST_ROOM_KEY);
    else storage.set(LAST_ROOM_KEY, next.lastRoomId);
  }
  const changed = JSON_FIELDS.filter((k) => next[k] !== prev[k]);
  if (changed.length === 0) return;
  const { volume, preset, dismissed, debug } = loadPrefs(storage);
  const merged: { -readonly [K in keyof JsonPrefs]: JsonPrefs[K] } = { volume, preset, dismissed, debug };
  for (const k of changed) {
    if (k === 'volume') merged.volume = next.volume;
    else if (k === 'preset') merged.preset = next.preset;
    else if (k === 'dismissed') merged.dismissed = mergeDismissed(dismissed, next.dismissed);
    else merged.debug = next.debug;
  }
  storage.set(PREFS_KEY, JSON.stringify(merged));
}

export function createPrefsStore(storage: KeyValueStore): PrefsStore {
  const store = createStore<PrefsState>()((set, get) => ({
    ...loadPrefs(storage),
    setVolume(volume) {
      set({ volume: clampVolume(volume) });
    },
    setLastRoomId(lastRoomId) {
      set({ lastRoomId });
    },
    setPreset(preset) {
      set({ preset });
    },
    dismiss(id, at = Date.now()) {
      set((s) => ({ dismissed: { ...s.dismissed, [id]: at } }));
    },
    isDismissed(id, maxAgeMs, now = Date.now()) {
      const at = get().dismissed[id];
      if (at === undefined) return false;
      return maxAgeMs === undefined || now - at < maxAgeMs;
    },
    setDebug(debug) {
      set({ debug });
    },
  }));
  store.subscribe((next, prev) => {
    saveChanges(storage, next, prev);
  });
  return store;
}
