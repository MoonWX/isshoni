// @vitest-environment node
// The bridge error codes (docs/m2/03 §6.4, generated from internal/desktop/bridge/codes into codes.gen.ts) each have
// their text: desktop.errors.<code> in the lazy desktop namespace (03 §18.1), the engine codes as
// desktop.errors.engine.<media code>, plus §10.5's reason texts. The update.* codes are the update screens'
// (desktop.update.error.<code>, README WA82), and wire codes passed through keep M1's errors.<code>.
import { describe, expect, it } from 'vitest';

import catalog from '../../i18n/lazy/desktop.en.json';
import * as codes from './codes.gen';
import { BridgeCodeInternal, BridgeCodeNotImplemented } from './errors';

/** The message at a dotted path, or undefined. */
function message(path: string): unknown {
  let node: unknown = catalog;
  for (const part of path.split('.')) {
    if (typeof node !== 'object' || node === null) return undefined;
    node = (node as Record<string, unknown>)[part];
  }
  return node;
}

/** Every Code constant of codes.gen.ts. */
const all = Object.entries(codes)
  .filter(([name, v]) => name.startsWith('Code') && typeof v === 'string')
  .map(([, v]) => v as string);

describe('the bridge error codes', () => {
  it('are generated', () => {
    expect(all.length).toBeGreaterThan(40);
    expect(all).toContain('invalid_argument');
    expect(all).toContain('engine.internal');
    expect(all).not.toContain('engine.cancelled');
    // errors.ts' transport-local codes are codes of the Go side too.
    expect(all).toContain(BridgeCodeInternal);
    expect(all).toContain(BridgeCodeNotImplemented);
  });

  it.each(all.filter((c) => !c.startsWith(codes.UpdatePrefix)))('%s has a text', (code) => {
    const text = message(`desktop.errors.${code}`);
    expect(typeof text).toBe('string');
    expect(text).not.toBe('');
  });

  it('engine.invalid and engine.busy show engine.internal’s text', () => {
    expect(message('desktop.errors.engine.invalid')).toBe(message('desktop.errors.engine.internal'));
    expect(message('desktop.errors.engine.busy')).toBe(message('desktop.errors.engine.internal'));
  });

  it.each(['selfWindow', 'driverTooOld', 'noMediaFoundation', 'blockedByPolicy', 'reinstall', 'deviceRemoved'])(
    'the reason %s has a text (03 §10.5)',
    (reason) => {
      expect(typeof message(`desktop.errors.engine.reason.${reason}`)).toBe('string');
    },
  );
});
