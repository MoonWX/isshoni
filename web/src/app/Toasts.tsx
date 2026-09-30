// Toasts (05 §6.1 uiStore): short messages at the bottom of the screen ("Sharing was stopped from another tab",
// "bo started sharing [Watch]"). Errors use role="alert", others role="status". A toast leaves after its duration;
// the timer pauses while the pointer is over it or focus is inside it (WCAG 2.2.1), and ✕ dismisses it at once.
// Under prefers-reduced-motion they appear without sliding (05 §16.6).
import { CircleAlert, CircleCheck, Info, X } from 'lucide-react';
import { useEffect, useRef, useState } from 'react';
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
    <li ref={item} className={cx(styles.toast, styles[toast.kind])} role={toast.kind === 'error' ? 'alert' : 'status'}>
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

export function Toasts() {
  const { t } = useTranslation();
  const { ui } = useApp();
  const toasts = useUi((s) => s.toasts);
  return (
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
}
