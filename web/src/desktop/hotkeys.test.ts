// The hotkey field's key reading (docs/m2/03 §11.2): Go's text form, the field's rules, and the keys it passes on.
import { describe, expect, it } from 'vitest';

import { keyName, readKey, type KeyPress } from './hotkeys';

const press = (code: string, mods: Partial<KeyPress> = {}, key = ''): KeyPress => ({
  code,
  key,
  ctrlKey: false,
  altKey: false,
  shiftKey: false,
  metaKey: false,
  ...mods,
});

describe('keyName', () => {
  it.each([
    ['KeyA', 'A'],
    ['KeyZ', 'Z'],
    ['Digit0', '0'],
    ['Digit9', '9'],
    ['F1', 'F1'],
    ['F24', 'F24'],
    ['Numpad5', 'Num5'],
    ['Pause', 'Pause'],
    ['ScrollLock', 'ScrollLock'],
    ['Insert', 'Insert'],
    ['Home', 'Home'],
    ['End', 'End'],
    ['PageUp', 'PageUp'],
    ['PageDown', 'PageDown'],
  ])('%s is %s', (code, name) => {
    expect(keyName(code)).toBe(name);
  });

  it.each(['F25', 'F0', 'Space', 'Enter', 'Backquote', 'NumpadAdd', 'ArrowUp', 'Escape'])(
    '%s is no hotkey key',
    (c) => {
      expect(keyName(c)).toBeNull();
    },
  );
});

describe('readKey', () => {
  it('writes the modifiers as Ctrl, Alt, Shift, then the key', () => {
    expect(readKey(press('F9', { ctrlKey: true, altKey: true, shiftKey: true }))).toEqual({
      kind: 'combo',
      combo: 'Ctrl+Alt+Shift+F9',
    });
    expect(readKey(press('KeyP', { shiftKey: true, ctrlKey: true }, 'P'))).toEqual({
      kind: 'combo',
      combo: 'Ctrl+Shift+P',
    });
    expect(readKey(press('Numpad1', { altKey: true, shiftKey: true }))).toEqual({
      kind: 'combo',
      combo: 'Alt+Shift+Num1',
    });
  });

  it('takes an F-key with one modifier, anything else needs two', () => {
    expect(readKey(press('F10', { ctrlKey: true }))).toEqual({ kind: 'combo', combo: 'Ctrl+F10' });
    expect(readKey(press('KeyA', { ctrlKey: true }, 'a'))).toEqual({ kind: 'problem', problem: 'modifiers' });
    expect(readKey(press('F9'))).toEqual({ kind: 'problem', problem: 'modifiers' });
    expect(readKey(press('KeyA', {}, 'a'))).toEqual({ kind: 'problem', problem: 'modifiers' });
  });

  it('never takes the Windows key', () => {
    expect(readKey(press('F9', { ctrlKey: true, metaKey: true }))).toEqual({ kind: 'problem', problem: 'win' });
    expect(readKey(press('MetaLeft', { metaKey: true }))).toEqual({ kind: 'problem', problem: 'win' });
  });

  it('waits on modifiers alone, refuses keys without a name', () => {
    expect(readKey(press('ControlLeft', { ctrlKey: true }))).toEqual({ kind: 'pending' });
    expect(readKey(press('ShiftRight', { ctrlKey: true, shiftKey: true }))).toEqual({ kind: 'pending' });
    expect(readKey(press('Space', { ctrlKey: true, altKey: true }, ' '))).toEqual({ kind: 'problem', problem: 'key' });
  });

  it('Escape cancels and Tab passes, only without modifiers', () => {
    expect(readKey(press('Escape', {}, 'Escape'))).toEqual({ kind: 'cancel' });
    expect(readKey(press('Tab', {}, 'Tab'))).toEqual({ kind: 'pass' });
    expect(readKey(press('Escape', { ctrlKey: true, altKey: true }, 'Escape'))).toEqual({
      kind: 'problem',
      problem: 'key',
    });
  });
});
