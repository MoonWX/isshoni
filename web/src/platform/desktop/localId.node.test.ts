// @vitest-environment node
// The derived local id (docs/m2/03 §8.1) behind the storage prefix: the vectors that DesktopPlatform.test.ts uses are
// what servers.LocalID computes, so the SPA's prefix and Go's bridge.js id agree.
import { createHash } from 'node:crypto';

import { describe, expect, it } from 'vitest';

import { LocalIdVectors } from './testing/localIdVectors';

/** servers.LocalID: lowercase Crockford base32 of the first 5 bytes of SHA-256(origin), 8 characters. */
function localId(origin: string): string {
  const alphabet = '0123456789abcdefghjkmnpqrstvwxyz';
  const bytes = createHash('sha256').update(origin).digest().subarray(0, 5);
  let n = 0n;
  for (const b of bytes) n = (n << 8n) | BigInt(b);
  let id = '';
  for (let i = 7; i >= 0; i--) id += alphabet.charAt(Number((n >> BigInt(i * 5)) & 31n));
  return id;
}

describe('the derived local id', () => {
  it.each(LocalIdVectors)('%s → %s', (origin, id) => {
    expect(localId(origin)).toBe(id);
    expect(id).toMatch(/^[0-9a-hjkmnp-tv-z]{8}$/);
  });
});
