import { useCallback, useEffect, useId, useRef, useState, type ReactNode, type RefObject } from 'react';

import { cx } from './cx';
import styles from './Popover.module.css';

/** Spread onto the trigger <button>. */
export interface PopoverTriggerProps {
  ref: RefObject<HTMLButtonElement | null>;
  'aria-expanded': boolean;
  'aria-controls': string;
  'aria-haspopup': 'dialog';
  onClick: () => void;
}

export interface PopoverProps {
  /** Renders the trigger button with the given props. */
  trigger: (props: PopoverTriggerProps) => ReactNode;
  /** The panel's accessible name (translated), e.g. "Watching now". */
  label: string;
  /** The content, or a function that gets close() for actions inside. */
  children: ReactNode | ((close: () => void) => ReactNode);
  /** Which trigger edge the panel lines up with. */
  align?: 'start' | 'end';
  /** Controlled open state (optional). */
  open?: boolean;
  onOpenChange?: (open: boolean) => void;
}

/**
 * A small non-modal panel anchored under its trigger (the watchers list, the account menu). It opens on the
 * trigger's click, takes focus, and closes on Esc (focus back to the trigger), on a pointer press outside, and when
 * focus leaves it.
 */
export function Popover({ trigger, label, children, align = 'start', open, onOpenChange }: PopoverProps) {
  const [ownOpen, setOwnOpen] = useState(false);
  const isOpen = open ?? ownOpen;
  const id = useId();
  const triggerRef = useRef<HTMLButtonElement>(null);
  const panelRef = useRef<HTMLDivElement>(null);
  const rootRef = useRef<HTMLSpanElement>(null);

  const setOpen = useCallback(
    (next: boolean) => {
      if (open === undefined) setOwnOpen(next);
      onOpenChange?.(next);
    },
    [open, onOpenChange],
  );
  const close = useCallback(() => {
    setOpen(false);
  }, [setOpen]);

  useEffect(() => {
    const panel = panelRef.current;
    if (!isOpen || !panel) return undefined;
    panel.focus();
    const onPointerDown = (e: PointerEvent): void => {
      if (e.target instanceof Node && !rootRef.current?.contains(e.target)) setOpen(false);
    };
    const onKeyDown = (e: KeyboardEvent): void => {
      if (e.key !== 'Escape') return;
      e.stopPropagation();
      setOpen(false);
      triggerRef.current?.focus();
    };
    const onFocusOut = (e: FocusEvent): void => {
      const next = e.relatedTarget;
      // Focus went elsewhere on the page (Tab past the end). A click outside is handled by pointerdown; focus
      // going nowhere (null: the window lost focus) keeps it open.
      if (next instanceof Node && !rootRef.current?.contains(next)) setOpen(false);
    };
    document.addEventListener('pointerdown', onPointerDown);
    panel.addEventListener('keydown', onKeyDown);
    panel.addEventListener('focusout', onFocusOut);
    return () => {
      document.removeEventListener('pointerdown', onPointerDown);
      panel.removeEventListener('keydown', onKeyDown);
      panel.removeEventListener('focusout', onFocusOut);
    };
  }, [isOpen, setOpen]);

  return (
    <span className={styles.root} ref={rootRef}>
      {trigger({
        ref: triggerRef,
        'aria-expanded': isOpen,
        'aria-controls': id,
        'aria-haspopup': 'dialog',
        onClick: () => {
          setOpen(!isOpen);
        },
      })}
      <div
        ref={panelRef}
        id={id}
        role="dialog"
        aria-label={label}
        tabIndex={-1}
        hidden={!isOpen}
        className={cx(styles.panel, styles[align])}
      >
        {isOpen && (typeof children === 'function' ? children(close) : children)}
      </div>
    </span>
  );
}
