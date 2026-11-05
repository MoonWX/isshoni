// The state the Settings page and the update parts read from Go (docs/m2/03 §6.2, §6.3, §14.2, §16.2): each hook
// subscribes to its event first and then pulls, so it misses nothing (§6.3), and a pull that fails leaves what the
// event brings. Methods whose slice hasn't landed answer not_implemented (README §4): the page then shows what it
// can and says so where the user acts.
import type { TFunction } from 'i18next';
import { useCallback, useEffect, useState } from 'react';

import { i18n } from '../i18n';
import type {
  NativeBridge,
  SettingsPatch,
  SettingsView,
  ShareViewState,
  UpdateStatus,
} from '../platform/desktop/bridge';
import { failureOf } from './authText';

/** The prefix of the update codes (codes.gen.ts' UpdatePrefix), whose texts are desktop.update.error.<code>. */
const UPDATE_PREFIX = 'update.';

/**
 * The text of a failed bridge call on these pages: an update code's desktop.update.error.<code> (03 §6.4, README §3
 * item 42), else desktop.errors.<code>, else the generic one.
 */
export function bridgeText(t: TFunction, err: unknown): string {
  const { code } = failureOf(err);
  if (code.startsWith(UPDATE_PREFIX)) {
    const key = `desktop.update.error.${code.slice(UPDATE_PREFIX.length)}`;
    if (i18n.exists(key)) return t(key);
  }
  const key = `desktop.errors.${code}`;
  return i18n.exists(key) ? t(key) : t('desktop.errors.internal');
}

/** Whether a bridge answer is there: an object (a test's fake bridge answers undefined for what it wasn't given). */
export function isObject<T>(v: T | null | undefined): v is T {
  return typeof v === 'object' && v !== null;
}

export interface SettingsResult {
  /** null until Settings.Get() or settings.changed answered. */
  view: SettingsView | null;
  /** Settings.Get() failed (and no event came). */
  failed: boolean;
  /** Settings.Update(patch); resolves with the new view, rejects with the bridge error. */
  update: (patch: SettingsPatch) => Promise<SettingsView>;
}

/** settings.json's user-facing part (03 §14.2): settings.changed, then Settings.Get(). */
export function useSettings(bridge: NativeBridge): SettingsResult {
  const [view, setView] = useState<SettingsView | null>(null);
  const [failed, setFailed] = useState(false);
  useEffect(() => {
    let live = true;
    const off = bridge.on('settings.changed', (v) => {
      if (live && isObject(v)) setView(v);
    });
    bridge.settings.get().then(
      (v) => {
        if (live && isObject(v)) setView((prev) => prev ?? v);
      },
      () => {
        if (live) setFailed(true);
      },
    );
    return () => {
      live = false;
      off();
    };
  }, [bridge]);
  const update = useCallback(
    (patch: SettingsPatch) =>
      bridge.settings.update(patch).then((v) => {
        if (isObject(v)) setView(v);
        return v;
      }),
    [bridge],
  );
  return { view, failed, update };
}

/** The updater's status (03 §16.2): update.state, then Updates.Status(); null until one answered. */
export function useUpdateStatus(bridge: NativeBridge): [UpdateStatus | null, (s: UpdateStatus) => void] {
  const [status, setStatus] = useState<UpdateStatus | null>(null);
  useEffect(() => {
    let live = true;
    const off = bridge.on('update.state', (s) => {
      if (live && isObject(s)) setStatus(s);
    });
    bridge.updates.status().then(
      (s) => {
        if (live && isObject(s)) setStatus((prev) => prev ?? s);
      },
      () => {
        // not_implemented until the updater is wired (README WA84): no status, so no status line.
      },
    );
    return () => {
      live = false;
      off();
    };
  }, [bridge]);
  const set = useCallback((s: UpdateStatus) => {
    if (isObject(s)) setStatus(s);
  }, []);
  return [status, set];
}

/** The share states during which Updates.Install() answers busy and Quit asks first (03 §16.2, §11.6). */
const LIVE_STATES: ReadonlySet<ShareViewState> = new Set([
  'starting',
  'live',
  'paused',
  'reconnecting',
  'unreachable',
  'stopping',
]);

/** Whether the app shares right now: share.state, then Share.Current(). */
export function useSharing(bridge: NativeBridge): boolean {
  const [sharing, setSharing] = useState(false);
  useEffect(() => {
    let live = true;
    let evented = false;
    const off = bridge.on('share.state', (v) => {
      evented = true;
      if (live) setSharing(isObject(v) && LIVE_STATES.has(v.state));
    });
    bridge.share.current().then(
      (v) => {
        if (live && !evented) setSharing(isObject(v) && LIVE_STATES.has(v.state));
      },
      () => undefined,
    );
    return () => {
      live = false;
      off();
    };
  }, [bridge]);
  return sharing;
}

/** The zero time Go sends before the first check (06 §8.9's CheckedAt). */
function isZeroTime(iso: string): boolean {
  const ms = Date.parse(iso);
  return !Number.isFinite(ms) || ms <= 0;
}

/** checkedAt in the reader's language, or null before the first check. */
export function checkedAtText(iso: string, lng: string): string | null {
  if (iso === '' || isZeroTime(iso)) return null;
  return new Intl.DateTimeFormat(lng, { dateStyle: 'medium', timeStyle: 'short' }).format(new Date(iso));
}
