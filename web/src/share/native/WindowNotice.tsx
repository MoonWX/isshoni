// The window notice (docs/m2/03 §11.6; README WA80): the first time the window is closed to the tray while a share
// is live, Go shows the toast "isshoni is still sharing. Stop it from the tray icon (^)." and, because a game's
// Do-not-disturb may swallow that toast, the same line as a dismissible notice at the top of the window the next
// time it opens. Go sets tips.trayNoticeShown only once this notice was shown: the page reports it (App.
// WindowNoticeShown) once it is on screen in a visible page, never while the window is hidden.
//
// Go's side: App.WindowNotice() (the pull, null while the window is hidden or nothing is due), the window.notice
// event when App.ShowWindow opens the window with a notice due, and App.WindowNoticeShown(code). The page pulls
// again each time it becomes visible, so a window shown another way (a deep link, a second launch) still shows a
// notice that is due. The bound methods are the bridge's
// (internal/desktop/bridge); a bridge without them shows nothing, and the toast stays the only notice.
import { useEffect, useState } from 'react';
import { useTranslation } from 'react-i18next';

import type { BridgeEventName, NativeBridge } from '../../platform/desktop/bridge';
import { Button } from '../../ui/Button';
import styles from './native.module.css';
import { useAudioBridge } from './useAudio';

/** A notice for the top of the window: its code and its text (from Go's catalog). */
export interface WindowNoticeView {
  code: string;
  text: string;
}

/** What the notice needs of the bridge's App service. */
export interface WindowNoticeApp {
  windowNotice(): Promise<WindowNoticeView | null>;
  windowNoticeShown(code: string): Promise<void>;
}

/** The window.notice event (docs/m2/03 §6.3's names, without the isshoni: prefix). */
export const WINDOW_NOTICE_EVENT = 'window.notice';

function isNotice(v: unknown): v is WindowNoticeView {
  return (
    typeof v === 'object' &&
    v !== null &&
    typeof (v as WindowNoticeView).code === 'string' &&
    (v as WindowNoticeView).code !== ''
  );
}

/** The bridge's App methods of the notice, or null for a bridge that has none. */
export function windowNoticeApp(bridge: NativeBridge | null): WindowNoticeApp | null {
  const app = bridge?.app as Partial<WindowNoticeApp> | undefined;
  if (typeof app?.windowNotice !== 'function' || typeof app.windowNoticeShown !== 'function') return null;
  return app as WindowNoticeApp;
}

export interface WindowNoticeProps {
  /** Default: the desktop app's bridge (null in the browser). */
  bridge?: NativeBridge | null;
}

/** The notice due at the top of the window; nothing when none is. */
export function WindowNotice({ bridge: given }: WindowNoticeProps) {
  const { t } = useTranslation();
  const own = useAudioBridge();
  const bridge = given === undefined ? own : given;
  const [notice, setNotice] = useState<WindowNoticeView | null>(null);
  const [dismissed, setDismissed] = useState<string | null>(null);

  // Subscribe first, then pull (03 §6.3), so a notice due right now is never missed; pull again whenever the page
  // becomes visible (Go answers null while the window is hidden, and not every way of showing it emits the event).
  useEffect(() => {
    const app = windowNoticeApp(bridge);
    if (bridge === null || app === null) return undefined;
    let alive = true;
    const take = (v: unknown): void => {
      if (alive && isNotice(v)) setNotice(v);
    };
    const pull = (): void => {
      app.windowNotice().then(take, () => undefined);
    };
    const onVisible = (): void => {
      if (document.visibilityState === 'visible') pull();
    };
    const off = bridge.on(WINDOW_NOTICE_EVENT as BridgeEventName, take);
    pull();
    document.addEventListener('visibilitychange', onVisible);
    return () => {
      alive = false;
      off();
      document.removeEventListener('visibilitychange', onVisible);
    };
  }, [bridge]);

  // Report it shown once it is on screen in a visible page (a hidden window's page keeps running).
  const code = notice?.code ?? null;
  useEffect(() => {
    const app = windowNoticeApp(bridge);
    if (code === null || app === null) return undefined;
    let reported = false;
    const report = (): void => {
      if (reported || document.visibilityState !== 'visible') return;
      reported = true;
      app.windowNoticeShown(code).catch(() => undefined);
    };
    report();
    document.addEventListener('visibilitychange', report);
    return () => {
      document.removeEventListener('visibilitychange', report);
    };
  }, [bridge, code]);

  if (notice === null || dismissed === notice.code) return null;
  return (
    <div className={styles.windowNotice} role="status" data-window-notice={notice.code}>
      <p>{notice.text !== '' ? notice.text : t('desktop.native.toast.stillSharing')}</p>
      <Button
        size="sm"
        variant="ghost"
        onClick={() => {
          setDismissed(notice.code);
        }}
      >
        {t('desktop.share.notice.dismiss')}
      </Button>
    </div>
  );
}
