// The hotkey field's key reading (docs/m2/03 §11.2): a key press becomes the combination's text in the form of Go's
// internal/desktop/hotkey (Ctrl, Alt, Shift in that order, then the key, joined by "+"), and the field's rules are
// checked here first, so a combination that Go would refuse never leaves the page: it needs two modifiers, or an
// F-key with at least one, and the Windows key is never part of one. Go checks again (Settings.TestHotkey answers
// `invalid` for anything it can't parse), and only Go knows whether the combination is taken or types a character.
//
// The key comes from KeyboardEvent.code, the physical key, so the text doesn't depend on the keyboard layout (AltGr
// would turn KeyboardEvent.key into a character).

/** The defaults of 03 D9 (Go's hotkey.DefaultPause and DefaultStop). */
export const DEFAULT_HOTKEYS = { pause: 'Ctrl+Alt+Shift+F9', stop: 'Ctrl+Alt+Shift+F10' } as const;

export type HotkeyId = keyof typeof DEFAULT_HOTKEYS;

/** Why a key press isn't a combination the field takes. */
export type HotkeyProblem = 'win' | 'key' | 'modifiers';

export type KeyReading =
  /** Only modifiers so far: keep listening. */
  | { kind: 'pending' }
  /** Escape without modifiers: leave the field unchanged. */
  | { kind: 'cancel' }
  /** Tab without modifiers: let the focus move on. */
  | { kind: 'pass' }
  | { kind: 'combo'; combo: string }
  | { kind: 'problem'; problem: HotkeyProblem };

/** The parts of a KeyboardEvent that are read. */
export interface KeyPress {
  code: string;
  key: string;
  ctrlKey: boolean;
  altKey: boolean;
  shiftKey: boolean;
  metaKey: boolean;
}

const MODIFIER_CODES = new Set([
  'ControlLeft',
  'ControlRight',
  'AltLeft',
  'AltRight',
  'ShiftLeft',
  'ShiftRight',
  'MetaLeft',
  'MetaRight',
  'OSLeft',
  'OSRight',
]);

const NAMED_CODES: Readonly<Record<string, string>> = {
  Pause: 'Pause',
  ScrollLock: 'ScrollLock',
  Insert: 'Insert',
  Home: 'Home',
  End: 'End',
  PageUp: 'PageUp',
  PageDown: 'PageDown',
};

/** The key's name in Go's form, or null for a key a hotkey can't use. */
export function keyName(code: string): string | null {
  let m = /^Key([A-Z])$/.exec(code);
  if (m) return m[1] ?? null;
  m = /^Digit([0-9])$/.exec(code);
  if (m) return m[1] ?? null;
  m = /^Numpad([0-9])$/.exec(code);
  if (m) return `Num${m[1] ?? ''}`;
  m = /^F([0-9]{1,2})$/.exec(code);
  if (m) {
    const n = Number(m[1]);
    return n >= 1 && n <= 24 ? `F${String(n)}` : null;
  }
  return NAMED_CODES[code] ?? null;
}

/** Whether a key name is an F-key (F1–F24), which needs only one modifier. */
function isFKey(name: string): boolean {
  return /^F[0-9]{1,2}$/.test(name);
}

/** Reads one keydown of the field. */
export function readKey(ev: KeyPress): KeyReading {
  const mods = (ev.ctrlKey ? 1 : 0) + (ev.altKey ? 1 : 0) + (ev.shiftKey ? 1 : 0);
  if (
    ev.metaKey ||
    ev.code === 'MetaLeft' ||
    ev.code === 'MetaRight' ||
    ev.code === 'OSLeft' ||
    ev.code === 'OSRight'
  ) {
    return { kind: 'problem', problem: 'win' };
  }
  if (MODIFIER_CODES.has(ev.code)) return { kind: 'pending' };
  if (mods === 0 && ev.key === 'Escape') return { kind: 'cancel' };
  if (mods === 0 && ev.key === 'Tab') return { kind: 'pass' };
  const name = keyName(ev.code);
  if (name === null) return { kind: 'problem', problem: 'key' };
  if (mods < 2 && !(isFKey(name) && mods >= 1)) return { kind: 'problem', problem: 'modifiers' };
  const parts = [ev.ctrlKey && 'Ctrl', ev.altKey && 'Alt', ev.shiftKey && 'Shift', name].filter(
    (p): p is string => typeof p === 'string',
  );
  return { kind: 'combo', combo: parts.join('+') };
}
