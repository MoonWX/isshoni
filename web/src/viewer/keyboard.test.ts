// The viewer's keyboard map (05 §12.7): which key is which action, where a shortcut applies, and how the arrow
// keys move among the tiles.
import { afterEach, describe, expect, it, vi } from 'vitest';

import {
  attachShortcuts,
  isTextField,
  moveIndex,
  moveTileFocus,
  SHORTCUT_ROWS,
  shortcutAllowed,
  shortcutFor,
  type KeyLike,
  type ShortcutAction,
} from './keyboard';

const key = (k: string, over: Partial<KeyLike> = {}): KeyLike => ({ key: k, ...over });

afterEach(() => {
  document.body.replaceChildren();
});

/** Parses markup into the page and returns the element with the given id. */
function page(html: string): (id: string) => HTMLElement {
  document.body.innerHTML = html;
  return (id) => {
    const el = document.getElementById(id);
    if (!el) throw new Error(`no #${id}`);
    return el;
  };
}

describe('shortcutFor: the table of 05 §12.7', () => {
  it.each<[string, KeyLike, ShortcutAction]>([
    ['→', key('ArrowRight'), { type: 'move', to: 'next' }],
    ['↓', key('ArrowDown'), { type: 'move', to: 'next' }],
    ['←', key('ArrowLeft'), { type: 'move', to: 'previous' }],
    ['↑', key('ArrowUp'), { type: 'move', to: 'previous' }],
    ['Home', key('Home'), { type: 'move', to: 'first' }],
    ['End', key('End'), { type: 'move', to: 'last' }],
    ['1', key('1'), { type: 'focusNth', index: 0 }],
    ['9', key('9'), { type: 'focusNth', index: 8 }],
    ['F', key('f'), { type: 'fullscreen' }],
    ['M', key('m'), { type: 'mute' }],
    ['L', key('l'), { type: 'listen' }],
    ['Esc', key('Escape'), { type: 'escape' }],
    ['?', key('?', { shiftKey: true }), { type: 'help' }],
    ['Shift+D', key('D', { shiftKey: true }), { type: 'debug' }],
  ])('%s', (_name, e, action) => {
    expect(shortcutFor(e)).toEqual(action);
  });

  it('has a row in the shortcuts dialog for every line of the table', () => {
    expect(SHORTCUT_ROWS).toEqual(['move', 'pick', 'nth', 'fullscreen', 'mute', 'listen', 'escape', 'help', 'debug']);
  });

  it('leaves Enter and Space to the focused button, and every other key alone', () => {
    for (const k of ['Enter', ' ', 'Tab', '0', 'a', 'd', 'x', '/', 'F5', 'Shift', 'Dead', 'PageDown']) {
      expect(shortcutFor(key(k)), k).toBeNull();
    }
  });

  it('never takes a key that is pressed with Ctrl, Alt or Cmd, or during an IME composition', () => {
    for (const k of ['f', 'm', 'l', '1', 'ArrowLeft', 'Escape', '?']) {
      expect(shortcutFor(key(k, { ctrlKey: true })), `Ctrl+${k}`).toBeNull();
      expect(shortcutFor(key(k, { metaKey: true })), `Cmd+${k}`).toBeNull();
      expect(shortcutFor(key(k, { altKey: true })), `Alt+${k}`).toBeNull();
      expect(shortcutFor(key(k, { isComposing: true })), `composing ${k}`).toBeNull();
    }
  });

  it('matches letters without Shift (Caps Lock is fine), and D only with it', () => {
    expect(shortcutFor(key('F'))).toEqual({ type: 'fullscreen' }); // Caps Lock
    expect(shortcutFor(key('F', { shiftKey: true }))).toBeNull();
    expect(shortcutFor(key('M', { shiftKey: true }))).toBeNull();
    expect(shortcutFor(key('L', { shiftKey: true }))).toBeNull();
    expect(shortcutFor(key('d'))).toBeNull();
    expect(shortcutFor(key('d', { shiftKey: true }))).toEqual({ type: 'debug' }); // Caps Lock and Shift
    expect(shortcutFor(key('ArrowRight', { shiftKey: true }))).toBeNull(); // extending a selection
  });

  it('takes digits and "?" as characters, whatever keys the layout needs for them', () => {
    // AZERTY types its digits with Shift.
    expect(shortcutFor(key('3', { shiftKey: true }))).toEqual({ type: 'focusNth', index: 2 });
    expect(shortcutFor(key('?'))).toEqual({ type: 'help' });
    // … and has "é" on the 2 key without it: not a shortcut.
    expect(shortcutFor(key('é', { code: 'Digit2' }))).toBeNull();
  });

  it('goes by the key’s position on a layout without Latin letters', () => {
    expect(shortcutFor(key('а', { code: 'KeyF' }))).toEqual({ type: 'fullscreen' }); // Cyrillic
    expect(shortcutFor(key('ь', { code: 'KeyM' }))).toEqual({ type: 'mute' });
    expect(shortcutFor(key('В', { code: 'KeyD', shiftKey: true }))).toEqual({ type: 'debug' });
    expect(shortcutFor(key('λ', { code: 'KeyL' }))).toEqual({ type: 'listen' }); // Greek
    // A Latin layout that moved its letters keeps what the keys type: Dvorak's "u" is on QWERTY's F key.
    expect(shortcutFor(key('u', { code: 'KeyF' }))).toBeNull();
    expect(shortcutFor(key(';', { code: 'KeyZ' }))).toBeNull();
  });

  it('acts once for a key that is held down, except the arrow keys', () => {
    for (const k of ['f', 'm', 'l', '1', '?', 'Escape']) {
      expect(shortcutFor(key(k, { repeat: true })), k).toBeNull();
    }
    expect(shortcutFor(key('D', { shiftKey: true, repeat: true }))).toBeNull();
    expect(shortcutFor(key('ArrowDown', { repeat: true }))).toEqual({ type: 'move', to: 'next' });
  });

  it('is no shortcut without a key: a keydown that is not a KeyboardEvent has none', () => {
    expect(shortcutFor({})).toBeNull();
    expect(shortcutFor({ code: 'KeyF' })).toBeNull();
    expect(shortcutFor(new Event('keydown') as KeyboardEvent)).toBeNull();
    expect(shortcutFor(key(''))).toBeNull();
  });
});

describe('isTextField', () => {
  it('is true where typing goes into the element', () => {
    const el = page(`
      <input id="plain"><input id="text" type="text"><input id="search" type="search"><input id="pw" type="password">
      <input id="num" type="number"><textarea id="area"></textarea><select id="select"></select>
      <div contenteditable="true"><span id="rich">x</span></div><div id="bare" contenteditable></div>
    `);
    for (const id of ['plain', 'text', 'search', 'pw', 'num', 'area', 'select', 'rich', 'bare']) {
      expect(isTextField(el(id)), id).toBe(true);
    }
  });

  it('is false for buttons, sliders, checkboxes and plain content', () => {
    const el = page(`
      <button id="button"></button><input id="range" type="range"><input id="check" type="checkbox">
      <input id="radio" type="radio"><input id="submit" type="submit"><a id="link" href="#x">x</a>
      <div contenteditable="false"><span id="fixed">x</span></div><p id="text">x</p>
    `);
    for (const id of ['button', 'range', 'check', 'radio', 'submit', 'link', 'fixed', 'text']) {
      expect(isTextField(el(id)), id).toBe(false);
    }
    expect(isTextField(null)).toBe(false);
  });
});

describe('shortcutAllowed', () => {
  const FULLSCREEN: ShortcutAction = { type: 'fullscreen' };
  const MOVE: ShortcutAction = { type: 'move', to: 'next' };
  const ALL: ShortcutAction[] = [
    FULLSCREEN,
    MOVE,
    { type: 'mute' },
    { type: 'listen' },
    { type: 'focusNth', index: 0 },
    { type: 'escape' },
    { type: 'help' },
    { type: 'debug' },
  ];

  it('allows every shortcut on the page itself and on its buttons', () => {
    const el = page('<button id="b"></button>');
    for (const action of ALL) {
      expect(shortcutAllowed(action, null, document), action.type).toBe(true);
      expect(shortcutAllowed(action, document.body, document), action.type).toBe(true);
      expect(shortcutAllowed(action, el('b'), document), action.type).toBe(true);
    }
  });

  it('allows none while the focus is in a text field', () => {
    const el = page('<input id="name"><textarea id="area"></textarea>');
    for (const action of ALL) {
      expect(shortcutAllowed(action, el('name'), document), action.type).toBe(false);
      expect(shortcutAllowed(action, el('area'), document), action.type).toBe(false);
    }
  });

  it('leaves the arrow keys to a control that uses them, and takes letters there', () => {
    const el = page(`
      <input id="volume" type="range"><input id="radio" type="radio">
      <div role="menu"><button id="item" role="menuitem"></button></div>
      <div role="slider" id="slider" tabindex="0"></div>
    `);
    for (const id of ['volume', 'radio', 'item', 'slider']) {
      expect(shortcutAllowed(MOVE, el(id), document), id).toBe(false);
      expect(shortcutAllowed(FULLSCREEN, el(id), document), id).toBe(true);
    }
  });

  it('allows none while a modal dialog is open, wherever the focus is', () => {
    const el = page('<button id="behind"></button><dialog open><button id="inside"></button></dialog>');
    for (const action of ALL) {
      expect(shortcutAllowed(action, el('inside'), document), action.type).toBe(false);
      expect(shortcutAllowed(action, el('behind'), document), action.type).toBe(false);
      expect(shortcutAllowed(action, null, document), action.type).toBe(false);
    }
  });

  it('allows none inside a popover: its Esc is its own', () => {
    const el = page('<div role="dialog"><button id="inside"></button></div><button id="outside"></button>');
    for (const action of ALL) expect(shortcutAllowed(action, el('inside'), document), action.type).toBe(false);
    expect(shortcutAllowed(FULLSCREEN, el('outside'), document)).toBe(true);
  });

  it('keeps the arrow keys among the tiles of a dialog that lists them (the "Shares" sheet), and nothing else', () => {
    const el = page(`
      <dialog open>
        <button id="close"></button>
        <ul data-viewer-tiles>
          <li data-share-id="s_a"><button id="tile" data-tile-main></button>
            <span><button id="eye"></button><div role="dialog"><p id="names" tabindex="-1">x</p></div></span></li>
        </ul>
      </dialog>
    `);
    expect(shortcutAllowed(MOVE, el('tile'), document)).toBe(true);
    expect(shortcutAllowed(MOVE, el('eye'), document)).toBe(true);
    expect(shortcutAllowed(MOVE, el('close'), document)).toBe(true);
    // The watchers popover of a tile is a dialog of its own, without tiles.
    expect(shortcutAllowed(MOVE, el('names'), document)).toBe(false);
    for (const action of ALL.filter((a) => a.type !== 'move')) {
      expect(shortcutAllowed(action, el('tile'), document), action.type).toBe(false);
    }
  });
});

describe('moveIndex', () => {
  it('steps to the neighbour and stops at both ends', () => {
    expect(moveIndex(0, 3, 'next')).toBe(1);
    expect(moveIndex(1, 3, 'next')).toBe(2);
    expect(moveIndex(2, 3, 'next')).toBe(2);
    expect(moveIndex(2, 3, 'previous')).toBe(1);
    expect(moveIndex(0, 3, 'previous')).toBe(0);
  });

  it('jumps to the first and the last', () => {
    expect(moveIndex(1, 3, 'first')).toBe(0);
    expect(moveIndex(1, 3, 'last')).toBe(2);
    expect(moveIndex(-1, 3, 'last')).toBe(2);
  });

  it('enters the list at its tab stop when the focus is on no tile', () => {
    expect(moveIndex(-1, 3, 'next')).toBe(0);
    expect(moveIndex(-1, 3, 'previous')).toBe(0);
    expect(moveIndex(-1, 3, 'next', 2)).toBe(2);
    expect(moveIndex(-1, 3, 'previous', 1)).toBe(1);
    expect(moveIndex(-1, 3, 'next', 7)).toBe(2);
    expect(moveIndex(-1, 3, 'next', -1)).toBe(0);
  });

  it('has nowhere to go without tiles', () => {
    expect(moveIndex(0, 0, 'next')).toBe(-1);
    expect(moveIndex(-1, 0, 'first')).toBe(-1);
  });
});

describe('moveTileFocus', () => {
  const TILES = `
    <button id="outside"></button>
    <ul data-viewer-tiles>
      <li data-share-id="s_a"><button id="a" data-tile-main tabindex="-1"></button><button id="a-eye" tabindex="-1"></button></li>
      <li data-share-id="s_b"><button id="b" data-tile-main tabindex="0"></button><button id="b-eye"></button></li>
      <li data-share-id="s_c"><button id="c" data-tile-main tabindex="-1"></button></li>
    </ul>
  `;
  const focused = (): string => document.activeElement?.id ?? '';

  it('moves from the tile the focus is in to its neighbours', () => {
    const el = page(TILES);
    el('a').focus();
    expect(moveTileFocus(document, 'next', document.activeElement)).toBe(true);
    expect(focused()).toBe('b');
    moveTileFocus(document, 'next', document.activeElement);
    expect(focused()).toBe('c');
    moveTileFocus(document, 'next', document.activeElement);
    expect(focused()).toBe('c');
    moveTileFocus(document, 'first', document.activeElement);
    expect(focused()).toBe('a');
    moveTileFocus(document, 'last', document.activeElement);
    expect(focused()).toBe('c');
    moveTileFocus(document, 'previous', document.activeElement);
    expect(focused()).toBe('b');
  });

  it('counts a tile’s other controls as the tile: from its eye to the next tile’s main button', () => {
    const el = page(TILES);
    el('a-eye').focus();
    moveTileFocus(document, 'next', document.activeElement);
    expect(focused()).toBe('b');
  });

  it('enters the list at its tab stop when the focus is elsewhere', () => {
    const el = page(TILES);
    el('outside').focus();
    expect(moveTileFocus(document, 'next', document.activeElement)).toBe(true);
    expect(focused()).toBe('b');
    el('outside').focus();
    moveTileFocus(document, 'previous', null);
    expect(focused()).toBe('b');
  });

  it('does nothing without a tile list', () => {
    const el = page('<button id="outside"></button><ul data-viewer-tiles></ul>');
    el('outside').focus();
    expect(moveTileFocus(document, 'next', document.activeElement)).toBe(false);
    expect(focused()).toBe('outside');
    document.body.replaceChildren();
    expect(moveTileFocus(document, 'next', null)).toBe(false);
  });

  it('stays in the list that has the focus when the scope has two', () => {
    const el = page(`
      <ul data-viewer-tiles><li><button id="x" data-tile-main></button></li></ul>
      <ul data-viewer-tiles><li><button id="y1" data-tile-main></button></li><li><button id="y2" data-tile-main></button></li></ul>
    `);
    el('y1').focus();
    moveTileFocus(document, 'next', document.activeElement);
    expect(focused()).toBe('y2');
  });
});

describe('attachShortcuts', () => {
  const press = (target: EventTarget, init: KeyboardEventInit): KeyboardEvent => {
    const e = new KeyboardEvent('keydown', { bubbles: true, cancelable: true, ...init });
    target.dispatchEvent(e);
    return e;
  };

  it('runs the action of a key press and marks it handled', () => {
    const run = vi.fn<(action: ShortcutAction) => boolean>(() => true);
    const off = attachShortcuts({ run });
    const e = press(document.body, { key: 'f' });
    expect(run).toHaveBeenCalledWith({ type: 'fullscreen' }, e);
    expect(e.defaultPrevented).toBe(true);

    off();
    press(document.body, { key: 'f' });
    expect(run).toHaveBeenCalledTimes(1);
  });

  it('lets the browser have a key press that nothing was done with', () => {
    const off = attachShortcuts({ run: () => false });
    expect(press(document.body, { key: 'ArrowDown' }).defaultPrevented).toBe(false);
    off();
  });

  it('ignores keys that are no shortcut, key presses in a text field, and ones that were handled already', () => {
    const run = vi.fn(() => true);
    const off = attachShortcuts({ run });
    const el = page('<input id="name"><div id="popover"></div>');
    expect(press(document.body, { key: 'x' }).defaultPrevented).toBe(false);
    expect(press(el('name'), { key: 'f' }).defaultPrevented).toBe(false);
    // The watchers popover closes on its own Escape and stops there.
    el('popover').addEventListener('keydown', (e) => {
      e.preventDefault();
    });
    press(el('popover'), { key: 'Escape' });
    expect(run).not.toHaveBeenCalled();
    off();
  });

  it('ignores a keydown that is no key press: the plain Event that autofill and password managers dispatch', () => {
    const run = vi.fn(() => true);
    // A listener that throws doesn't stop dispatchEvent: the page gets an `error` event instead.
    const errors = vi.fn<(e: ErrorEvent) => void>((e) => {
      e.preventDefault();
    });
    window.addEventListener('error', errors);
    const off = attachShortcuts({ run });
    try {
      const el = page('<input id="name"><button id="b"></button>');
      for (const target of [el('name'), el('b'), document]) {
        const e = new Event('keydown', { bubbles: true, cancelable: true });
        target.dispatchEvent(e);
        expect(e.defaultPrevented).toBe(false);
      }
      expect(errors).not.toHaveBeenCalled();
      expect(run).not.toHaveBeenCalled();
    } finally {
      off();
      window.removeEventListener('error', errors);
    }
  });
});
