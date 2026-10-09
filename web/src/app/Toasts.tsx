// Toasts (05 §6.1 uiStore): short messages at the bottom of the screen ("Sharing was stopped from another tab",
// "bo started sharing [Watch]"), a plain list in a labelled region. Screen readers hear them through the Announcer's
// persistent live regions (uiStore.toast() announces the message: errors assertively, others politely), because a
// live region inserted together with its text is announced unreliably, and a role on the <li> would break the list.
// A toast leaves after its duration; the timer pauses while the pointer is over it or focus is inside it
// (WCAG 2.2.1), and ✕ dismisses it at once. Under prefers-reduced-motion they appear without sliding (05 §16.6).
//
// While an element of the page is fullscreen (the viewer's layout, 05 §12.5), the browser draws nothing outside
// it, so the region moves into that element for as long as it is: "bo started sharing [Watch]" is for the very
// person who watches fullscreen. A toast that is on screen at that moment starts its time again.
import { CircleAlert, CircleCheck, Info, X } from 'lucide-react';
import { useEffect, useRef, useState, useSyncExternalStore } from 'react';
import { createPortal } from 'react-dom';
import { useTranslation } from 'react-i18next';

import { cx } from '../ui/cx';
import { useApp, useUi } from './context';
import styles from './Toasts.module.css';
import type { Toast } from './uiStore';

const ICONS = { info: Info, success: CircleCheck, error: CircleAlert } as const;

function ToastItem({ toast, onDismiss }: { toast: Toast; onDismiss: () => void }) {
  const { t } = useTranslation();
  const [paused, setPaused] = useState(false);
  const remaining = useRef(toast.durationMs);
  const item = useRef<HTMLLIElement>(null);
  const onDismissRef = useRef(onDismiss);
  useEffect(() => {
    onDismissRef.current = onDismiss;
  });

  // Pause while hovered or focused. Native listeners: pausing a timer isn't an interaction with the item itself.
  useEffect(() => {
    const el = item.current;
    if (!el) return undefined;
    const pause = (): void => {
      setPaused(true);
    };
    const resumeOnLeave = (): void => {
      if (!el.contains(document.activeElement)) setPaused(false);
    };
    const resumeOnBlur = (e: FocusEvent): void => {
      if (!(e.relatedTarget instanceof Node && el.contains(e.relatedTarget)) && !el.matches(':hover')) {
        setPaused(false);
      }
    };
    el.addEventListener('pointerenter', pause);
    el.addEventListener('pointerleave', resumeOnLeave);
    el.addEventListener('focusin', pause);
    el.addEventListener('focusout', resumeOnBlur);
    return () => {
      el.removeEventListener('pointerenter', pause);
      el.removeEventListener('pointerleave', resumeOnLeave);
      el.removeEventListener('focusin', pause);
      el.removeEventListener('focusout', resumeOnBlur);
    };
  }, []);

  useEffect(() => {
    if (paused || remaining.current === null) return undefined;
    const started = Date.now();
    const id = setTimeout(() => {
      onDismissRef.current();
    }, remaining.current);
    return () => {
      clearTimeout(id);
      if (remaining.current !== null) remaining.current = Math.max(0, remaining.current - (Date.now() - started));
    };
  }, [paused]);

  const Icon = ICONS[toast.kind];
  return (
    <li ref={item} className={cx(styles.toast, styles[toast.kind])} data-kind={toast.kind}>
      <Icon className={styles.icon} aria-hidden="true" />
      <p className={styles.message}>{toast.message}</p>
      {toast.action && (
        <button
          type="button"
          className={styles.action}
          onClick={() => {
            toast.action?.run();
            onDismiss();
          }}
        >
          {toast.action.label}
        </button>
      )}
      <button type="button" className={styles.dismiss} aria-label={t('a11y.dismiss')} onClick={onDismiss}>
        <X aria-hidden="true" />
      </button>
    </li>
  );
}

/**
 * The fullscreen element that can hold the toasts, or null: no element is fullscreen, or it is one that shows no
 * children of its own (a <video> in the native player, an <iframe>).
 */
function fullscreenHost(): Element | null {
  const doc = document as Document & { webkitFullscreenElement?: Element | null };
  // Before fullscreenElement was unprefixed, Safari had only the prefixed one.
  const el = (doc.fullscreenElement as Element | null | undefined) ?? doc.webkitFullscreenElement ?? null;
  return el === null || el instanceof HTMLMediaElement || el instanceof HTMLIFrameElement ? null : el;
}

function onFullscreenChange(changed: () => void): () => void {
  document.addEventListener('fullscreenchange', changed);
  document.addEventListener('webkitfullscreenchange', changed);
  return () => {
    document.removeEventListener('fullscreenchange', changed);
    document.removeEventListener('webkitfullscreenchange', changed);
  };
}

export function Toasts() {
  const { t } = useTranslation();
  const { ui } = useApp();
  const toasts = useUi((s) => s.toasts);
  const host = useSyncExternalStore(onFullscreenChange, fullscreenHost, () => null);
  const region = (
    <section className={styles.region} aria-label={t('a11y.notifications')}>
      <ol className={styles.list}>
        {toasts.map((toast) => (
          <ToastItem
            key={toast.id}
            toast={toast}
            onDismiss={() => {
              ui.getState().dismissToast(toast.id);
            }}
          />
        ))}
      </ol>
    </section>
  );
  return host === null ? region : createPortal(region, host);
}
