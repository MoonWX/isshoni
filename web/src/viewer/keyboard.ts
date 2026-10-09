// The viewer's keyboard (05 §12.7). The tile list is a roving-tabindex group, one tab stop that the arrow keys move,
// and these shortcuts are active while the focus isn't in a text field:
//
// | Key           | Action                                                     |
// |---------------|------------------------------------------------------------|
// | Arrow keys    | move between tiles (Home and End: the first and the last)  |
// | Enter / Space | focus the tile's share (the tile is a <button>)            |
// | 1–9           | focus the Nth share (newest first)                         |
// | F             | fullscreen on/off                                          |
// | M             | mute/unmute                                                |
// | L             | listen to the keyboard-focused tile (moves the audio)      |
// | Esc           | leave fullscreen, close dialogs (native <dialog>)          |
// | ?             | shortcuts dialog                                           |
// | Shift+D       | debug overlay                                              |
//
// This module is the map and its rules, without React and without the viewer: shortcutFor() names the action of a
// key press, shortcutAllowed() says whether it applies where the focus is, and attachShortcuts() listens on the
// document. ViewerLayout gives it the handlers. Enter and Space are not in the map: they activate the focused
// button, a tile's included, as in every browser.
//
// Rules that keep the shortcuts out of the way:
// - Never with Ctrl, Alt or Cmd held (Ctrl+F, Cmd+L and Alt+← are the browser's), and never during an IME
//   composition. A key that is held down acts once, except the arrow keys.
// - Never while the focus is in a text field, and never on a key press that something else handled already
//   (defaultPrevented): the watchers popover closes on its own Escape.
// - While a modal <dialog> is open, or the focus is inside a popover, only the arrow keys work, and only among the
//   tiles of a list the focus is in (the phone's "Shares" sheet): the dialog has the keyboard, and Esc is its own.
// - The arrow keys are left to a control that uses them itself (the volume slider, a select, a menu).
// - Letters are matched without Shift, so Caps Lock doesn't matter and Shift+F stays free. On a layout without
//   Latin letters (Cyrillic, Greek, …) the key's position counts instead (KeyboardEvent.code). "?" and the digits
//   are matched as characters, whatever keys the layout needs for them (AZERTY types digits with Shift).
// - Not every `keydown` is a key press: Chrome's autofill and password-manager extensions dispatch a plain Event of
//   that name on the field they fill. It has no `key`, and is no shortcut.

export type ShortcutAction =
  /** Arrow keys, Home, End: move the keyboard focus among the tiles. */
  | { readonly type: 'move'; readonly to: MoveTo }
  /** 1–9: focus the Nth share, newest first; `index` is N − 1. */
  | { readonly type: 'focusNth'; readonly index: number }
  | { readonly type: 'fullscreen' }
  | { readonly type: 'mute' }
  | { readonly type: 'listen' }
  | { readonly type: 'escape' }
  | { readonly type: 'help' }
  | { readonly type: 'debug' };

export type MoveTo = 'next' | 'previous' | 'first' | 'last';

/** The parts of a KeyboardEvent the map reads. */
export interface KeyLike {
  /** Absent on a `keydown` that is not a KeyboardEvent (see the rules above). */
  readonly key?: string;
  /** The physical key ("KeyF"): what a letter shortcut goes by when `key` is not a Latin letter. */
  readonly code?: string;
  readonly shiftKey?: boolean;
  readonly ctrlKey?: boolean;
  readonly metaKey?: boolean;
  readonly altKey?: boolean;
  readonly repeat?: boolean;
  readonly isComposing?: boolean;
}

const MOVES: Readonly<Record<string, MoveTo>> = {
  ArrowRight: 'next',
  ArrowDown: 'next',
  ArrowLeft: 'previous',
  ArrowUp: 'previous',
  Home: 'first',
  End: 'last',
};

/** The Latin letter of a key press, in lower case: the character it types, else the letter of its position. */
function letterOf(key: string, code: string | undefined): string | null {
  if (/^[a-z]$/i.test(key)) return key.toLowerCase();
  // Only for characters outside ASCII: "é" on the 2 key or ";" where QWERTY has no letter are not shortcuts.
  if (key.charCodeAt(0) < 128) return null;
  const position = /^Key([A-Z])$/.exec(code ?? '');
  return position?.[1]?.toLowerCase() ?? null;
}

/** The action of a key press, or null when the key is not a shortcut (05 §12.7). */
export function shortcutFor(e: KeyLike): ShortcutAction | null {
  const { key } = e;
  // Not a key press. This is the first check: attachShortcuts hears every keydown of the page, text fields included.
  if (typeof key !== 'string') return null;
  if (e.ctrlKey === true || e.metaKey === true || e.altKey === true || e.isComposing === true) return null;
  const shift = e.shiftKey === true;
  const to = MOVES[key];
  if (to !== undefined) return shift ? null : { type: 'move', to };
  // A held key repeats: one press is one toggle.
  if (e.repeat === true) return null;
  if (key === 'Escape') return { type: 'escape' };
  if (key === '?') return { type: 'help' };
  if (key.length !== 1) return null;
  if (key >= '1' && key <= '9') return { type: 'focusNth', index: Number(key) - 1 };
  switch (letterOf(key, e.code)) {
    case 'f':
      return shift ? null : { type: 'fullscreen' };
    case 'm':
      return shift ? null : { type: 'mute' };
    case 'l':
      return shift ? null : { type: 'listen' };
    case 'd':
      return shift ? { type: 'debug' } : null;
    default:
      return null;
  }
}

/** Input types that take no typed text: a shortcut's letter does nothing in them. */
const NON_TEXT_INPUTS = new Set(['button', 'checkbox', 'color', 'file', 'image', 'radio', 'range', 'reset', 'submit']);

/** Whether typing goes into this element: a text input, a textarea, a select, or editable content. */
export function isTextField(el: Element | null): boolean {
  if (el === null) return false;
  const tag = el.tagName.toLowerCase();
  if (tag === 'textarea' || tag === 'select') return true;
  if (tag === 'input') return !NON_TEXT_INPUTS.has((el.getAttribute('type') ?? 'text').toLowerCase());
  const editable = el.closest('[contenteditable]')?.getAttribute('contenteditable');
  return editable !== undefined && editable !== null && editable !== 'false';
}

/** Controls that use the arrow keys, Home and End themselves. */
const ARROW_USERS =
  'input, select, textarea, [role="slider"], [role="menu"], [role="menubar"], [role="listbox"], [role="radiogroup"], [role="tablist"], [role="tree"], [role="grid"], [role="spinbutton"]';

/** The viewer's tile list: the <ul> that ViewerLayout marks. The arrow keys move among its tiles. */
export const TILE_LIST = '[data-viewer-tiles]';
/** A tile's main button inside it: what the arrow keys focus. */
export const TILE_MAIN = '[data-tile-main]';

/**
 * Whether an action applies to a key press on `target` (the event's target; null for none) in `doc`. See the rules
 * at the top of this file.
 */
export function shortcutAllowed(action: ShortcutAction, target: Element | null, doc: Document): boolean {
  if (isTextField(target)) return false;
  const around = target?.closest('dialog, [role="dialog"]') ?? null;
  const inDialog = around !== null || doc.querySelector('dialog[open]') !== null;
  if (action.type !== 'move') return !inDialog;
  if (target?.closest(ARROW_USERS) != null) return false;
  // In a dialog only when it lists tiles: the phone's "Shares" sheet.
  return !inDialog || around?.querySelector(TILE_LIST) != null;
}

/**
 * Where a move goes in a list of `count` tiles, from the tile at `from` (−1: the focus is on none of them, and the
 * move enters the list at `entry`, its tab stop). It stops at both ends. −1 when there is no tile.
 */
export function moveIndex(from: number, count: number, to: MoveTo, entry = 0): number {
  if (count <= 0) return -1;
  if (to === 'first') return 0;
  if (to === 'last') return count - 1;
  if (from < 0 || from >= count) return Math.min(Math.max(entry, 0), count - 1);
  return Math.min(Math.max(from + (to === 'next' ? 1 : -1), 0), count - 1);
}

/**
 * Moves the keyboard focus among the tiles of `scope` (the viewer's layout, or the document): from the tile the
 * focus is in to its neighbour, or into the list at its tab stop when the focus is elsewhere. The list that holds
 * the focus wins over another one. Returns whether a tile was focused.
 */
export function moveTileFocus(scope: ParentNode, to: MoveTo, active: Element | null): boolean {
  const own = active?.closest(TILE_LIST);
  const list = own != null && scope.contains(own) ? own : scope.querySelector(TILE_LIST);
  if (list == null) return false;
  const mains = [...list.querySelectorAll<HTMLElement>(TILE_MAIN)];
  // The tile the focus is in, whichever of its controls has it (the eye and the speaker button count).
  const from = mains.findIndex((main) => active !== null && main.parentElement?.contains(active) === true);
  const entry = mains.findIndex((main) => main.tabIndex === 0);
  const next = mains[moveIndex(from, mains.length, to, entry)];
  if (next === undefined) return false;
  next.focus();
  return true;
}

export interface ShortcutsDeps {
  /** Where the key presses are heard; default the global document. */
  doc?: Document;
  /**
   * Runs an action that applies. Return false when nothing was done with it (no share to focus, not in
   * fullscreen): the key press then goes on to the browser as if it were no shortcut.
   */
  run: (action: ShortcutAction, e: KeyboardEvent) => boolean;
}

/** Listens for the shortcuts on the document. Returns the function that stops it. */
export function attachShortcuts(deps: ShortcutsDeps): () => void {
  const doc = deps.doc ?? document;
  const onKeyDown = (e: KeyboardEvent): void => {
    if (e.defaultPrevented) return;
    const action = shortcutFor(e);
    if (action === null) return;
    const target = e.target instanceof Element ? e.target : null;
    if (!shortcutAllowed(action, target, doc)) return;
    // Handled: no page scroll for an arrow key, no second meaning for a letter.
    if (deps.run(action, e)) e.preventDefault();
  };
  doc.addEventListener('keydown', onKeyDown);
  return () => {
    doc.removeEventListener('keydown', onKeyDown);
  };
}

/** One row of the shortcuts dialog: which keys, in the order of the table above. */
export type ShortcutRow = 'move' | 'pick' | 'nth' | 'fullscreen' | 'mute' | 'listen' | 'escape' | 'help' | 'debug';

export const SHORTCUT_ROWS: readonly ShortcutRow[] = [
  'move',
  'pick',
  'nth',
  'fullscreen',
  'mute',
  'listen',
  'escape',
  'help',
  'debug',
];
