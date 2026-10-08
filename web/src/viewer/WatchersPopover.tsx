// WatchersPopover (05 §12.6): the eye with the viewer count (share.watchers.length, 01 §8.5) is a button that opens
// the list of who is watching: "Watching: bo, cy, you". The plan's "no hidden viewers" applies to everyone, admins
// included, so the names are always one tap away.
//
// The panel is not ui/Popover: a tile and the stage clip what they contain, and that panel opens below its
// trigger, which sits at their bottom edge. This one is `position: fixed`, placed from the trigger's rectangle
// when it opens (above it when there is more room there, and always inside the viewport), so nothing clips it; it
// stays a descendant of the stage, so it shows in fullscreen too (05 §12.5). Like ui/Popover it takes focus when it
// opens and closes on Esc (focus back to the eye; a sheet around it stays open), on a press outside and when focus
// leaves it; and, being fixed, when the page scrolls or resizes under it.
import { Eye } from 'lucide-react';
import { useEffect, useId, useRef, useState, type CSSProperties } from 'react';
import { useTranslation } from 'react-i18next';

import { cx } from '../ui/cx';
import { watchingText } from './shareView';
import type { ViewerShare } from './viewerStore';
import styles from './WatchersPopover.module.css';

/** The space between the trigger and the panel, and between the panel and the viewport's edge (px). */
const GAP = 8;

/** The panel's smallest and largest width in rem (.panel in WatchersPopover.module.css). */
const MIN_WIDTH_REM = 10;
const MAX_WIDTH_REM = 20;

/**
 * Where the panel goes, inside the viewport: over or under the trigger, whichever has more room, with its end edge
 * at the trigger's. Where its smallest width doesn't fit between the viewport's start edge and there (the first
 * tile of a phone's strip), it starts at the viewport's start edge instead, and the stylesheet's max-width keeps
 * it inside.
 */
function placeAt(trigger: HTMLElement): CSSProperties {
  const r = trigger.getBoundingClientRect();
  const root = document.documentElement;
  const width = root.clientWidth;
  const height = root.clientHeight;
  const rem = Number.parseFloat(getComputedStyle(root).fontSize) || 16;
  // What an end-aligned panel may take: from the gap at the viewport's start edge to its own end edge.
  const room = Math.min(r.right, width - GAP) - GAP;
  const across: CSSProperties =
    room >= MIN_WIDTH_REM * rem
      ? { right: Math.max(GAP, width - r.right), maxWidth: Math.min(MAX_WIDTH_REM * rem, room) }
      : { left: GAP };
  const above = r.top;
  const below = height - r.bottom;
  return above > below
    ? { ...across, bottom: height - r.top + GAP, maxHeight: Math.max(0, above - 2 * GAP) }
    : { ...across, top: r.bottom + GAP, maxHeight: Math.max(0, below - 2 * GAP) };
}

export interface WatchersPopoverProps {
  share: ViewerShare;
  /** Show "3 watching" next to the eye (the stage) instead of the bare count (a tile). */
  withText?: boolean;
  /** The eye's tabindex: -1 in a tile that is not its list's tab stop (05 §12.7). */
  tabIndex?: number | undefined;
  className?: string | undefined;
}

export function WatchersPopover({ share, withText = false, tabIndex, className }: WatchersPopoverProps) {
  const { t } = useTranslation();
  const [open, setOpen] = useState(false);
  const [place, setPlace] = useState<CSSProperties>({});
  const id = useId();
  const rootRef = useRef<HTMLSpanElement>(null);
  const triggerRef = useRef<HTMLButtonElement>(null);
  const panelRef = useRef<HTMLDivElement>(null);

  const watching = watchingText(t, share);
  // The others as the server lists them, this user last: "bo, cy, you". A user is listed once (01 §4.1).
  const people = [
    ...share.watchers
      .filter((w) => !w.self)
      .map((w) => ({ userId: w.userId, name: w.name !== '' ? w.name : t('viewer.someone') })),
    ...share.watchers.filter((w) => w.self).map((w) => ({ userId: w.userId, name: t('viewer.watchers.you') })),
  ];

  useEffect(() => {
    const panel = panelRef.current;
    if (!open || !panel) return undefined;
    panel.focus();
    const close = (): void => {
      setOpen(false);
    };
    const onPointerDown = (e: PointerEvent): void => {
      if (e.target instanceof Node && !rootRef.current?.contains(e.target)) close();
    };
    const onKeyDown = (e: KeyboardEvent): void => {
      if (e.key !== 'Escape') return;
      // This Escape closes the panel only: unless the keydown is cancelled, the browser also closes a modal
      // <dialog> around it (the phone's "Shares" sheet), and the eye that takes the focus back with it.
      e.preventDefault();
      e.stopPropagation();
      close();
      triggerRef.current?.focus();
    };
    const onFocusOut = (e: FocusEvent): void => {
      const next = e.relatedTarget;
      // A click outside is handled by pointerdown; focus going nowhere (the window lost it) keeps the panel open.
      if (next instanceof Node && !rootRef.current?.contains(next)) close();
    };
    const onScroll = (e: Event): void => {
      // The list inside the panel may scroll; anything else moved the trigger away from the panel.
      if (!(e.target instanceof Node && panel.contains(e.target))) close();
    };
    document.addEventListener('pointerdown', onPointerDown);
    document.addEventListener('scroll', onScroll, true);
    window.addEventListener('resize', close);
    panel.addEventListener('keydown', onKeyDown);
    panel.addEventListener('focusout', onFocusOut);
    return () => {
      document.removeEventListener('pointerdown', onPointerDown);
      document.removeEventListener('scroll', onScroll, true);
      window.removeEventListener('resize', close);
      panel.removeEventListener('keydown', onKeyDown);
      panel.removeEventListener('focusout', onFocusOut);
    };
  }, [open]);

  return (
    <span className={cx(styles.root, className)} ref={rootRef}>
      <button
        type="button"
        ref={triggerRef}
        className={styles.trigger}
        aria-label={t('viewer.watchers.button', { watching })}
        aria-haspopup="dialog"
        aria-expanded={open}
        aria-controls={id}
        tabIndex={tabIndex}
        onClick={() => {
          if (!open && triggerRef.current) setPlace(placeAt(triggerRef.current));
          setOpen(!open);
        }}
      >
        {/* Over a tile's video the eye and the count sit on a dark chip; the stage's bar is dark already. */}
        <span className={cx(styles.chip, !withText && styles.onVideo)} aria-hidden="true">
          <Eye className={styles.eye} />
          {withText ? watching : share.watchers.length}
        </span>
      </button>
      <div
        ref={panelRef}
        id={id}
        role="dialog"
        aria-label={t('viewer.watchers.title')}
        tabIndex={-1}
        hidden={!open}
        className={styles.panel}
        style={place}
      >
        {open &&
          (people.length === 0 ? (
            <p>{t('viewer.watchers.none')}</p>
          ) : (
            <>
              <p className={styles.heading}>{t('viewer.watchers.heading')}</p>
              <ul className={styles.names}>
                {people.map((p) => (
                  <li key={p.userId}>{p.name}</li>
                ))}
              </ul>
            </>
          ))}
      </div>
    </span>
  );
}
