import { X } from 'lucide-react';
import { useEffect, useId, useRef, type ReactNode } from 'react';
import { useTranslation } from 'react-i18next';

import { cx } from './cx';
import styles from './Dialog.module.css';

export interface DialogProps {
  /** Controlled: the dialog shows while open is true. */
  open: boolean;
  /** Esc, the close button, a backdrop click (when dismissible) or the form's method="dialog" submit. */
  onClose: () => void;
  /** The heading (translated); it names the dialog. */
  title: ReactNode;
  children: ReactNode;
  /** Buttons, right-aligned (stacked on phones). */
  footer?: ReactNode;
  /** Whether Esc, the ✕ button and a backdrop click close it. Default true; false for a choice that must be made. */
  dismissible?: boolean;
  /** Width: sm 24 rem (confirmations), md 32 rem (forms), lg 44 rem. */
  size?: 'sm' | 'md' | 'lg';
  /** For Sheet: extra class on the <dialog>. */
  className?: string;
}

/**
 * A modal dialog on the native <dialog> with showModal() (05 §16.6): focus trap, Esc and the top layer come from
 * the browser. Where showModal is missing (old browsers, jsdom) it falls back to the open attribute.
 */
export function Dialog({
  open,
  onClose,
  title,
  children,
  footer,
  dismissible = true,
  size = 'md',
  className,
}: DialogProps) {
  const { t } = useTranslation();
  const ref = useRef<HTMLDialogElement>(null);
  const titleId = useId();
  const onCloseRef = useRef(onClose);
  useEffect(() => {
    onCloseRef.current = onClose;
  });

  useEffect(() => {
    const el = ref.current;
    if (!el) return;
    if (open && !el.open) {
      if (typeof el.showModal === 'function') el.showModal();
      else el.setAttribute('open', '');
    } else if (!open && el.open) {
      if (typeof el.close === 'function') el.close();
      else el.removeAttribute('open');
    }
  }, [open]);

  // A click on the <dialog> element itself (not its content) is a click on the backdrop. A native listener: the
  // keyboard equivalent is Esc (onCancel), so this is a pointer-only convenience.
  useEffect(() => {
    const el = ref.current;
    if (!el || !dismissible) return undefined;
    const onClick = (e: MouseEvent): void => {
      if (e.target === el) onCloseRef.current();
    };
    el.addEventListener('click', onClick);
    return () => {
      el.removeEventListener('click', onClick);
    };
  }, [dismissible]);

  // Leave the top layer when unmounted while open (navigation away).
  useEffect(() => {
    const el = ref.current;
    return () => {
      if (el?.open && typeof el.close === 'function') el.close();
    };
  }, []);

  return (
    <dialog
      ref={ref}
      className={cx(styles.dialog, styles[size], className)}
      aria-labelledby={titleId}
      onCancel={(e) => {
        // Esc: the browser would close the <dialog> itself; keep it controlled.
        e.preventDefault();
        if (dismissible) onCloseRef.current();
      }}
    >
      <div className={styles.frame}>
        <header className={styles.header}>
          <h2 id={titleId} className={styles.title}>
            {title}
          </h2>
          {dismissible && (
            <button
              type="button"
              className={styles.close}
              aria-label={t('common.close')}
              onClick={() => {
                onCloseRef.current();
              }}
            >
              <X aria-hidden="true" />
            </button>
          )}
        </header>
        <div className={styles.body}>{children}</div>
        {footer !== undefined && <footer className={styles.footer}>{footer}</footer>}
      </div>
    </dialog>
  );
}
