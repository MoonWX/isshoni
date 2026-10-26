// The user code of the device flow (docs/m2/02-device-auth.md §3, RFC 8628 §6.1): 8 letters of a 20-consonant
// alphabet, shown as WDJB-MJHT. normalizeUserCode is the TypeScript copy of Go's api.NormalizeUserCode; both test
// suites run §3's table (userCode.test.ts, internal/protocol/api's device_test.go).
import { UserCodeAlphabet, UserCodeLength } from '../protocol/api.gen';

/** The letters on each side of the "-". */
const HALF = UserCodeLength / 2;

/** An ASCII lower-case letter as upper case; anything else as it is. */
function upper(ch: string): string {
  return ch >= 'a' && ch <= 'z' ? ch.toUpperCase() : ch;
}

/**
 * §3's rule: drop ASCII spaces, tabs and "-", map ASCII a–z to upper case; the result must be exactly 8 letters of
 * the alphabet. The normalized code ("WDJBMJHT"), or null when the input isn't a user code (a vowel, a digit, 7 or 9
 * letters, an en dash, full-width letters, "").
 */
export function normalizeUserCode(s: string): string | null {
  let out = '';
  for (const ch of s) {
    if (ch === ' ' || ch === '\t' || ch === '-') continue;
    const c = upper(ch);
    // One UTF-16 unit: includes() would also find a two-letter string.
    if (out.length === UserCodeLength || c.length !== 1 || !UserCodeAlphabet.includes(c)) return null;
    out += c;
  }
  return out.length === UserCodeLength ? out : null;
}

/**
 * A normalized code as it is shown, with a "-" after the fourth letter: "WDJBMJHT" → "WDJB-MJHT". Any other input,
 * including a code that isn't normalized yet, is returned unchanged (Go's api.FormatUserCode).
 */
export function formatUserCode(normalized: string): string {
  if (normalizeUserCode(normalized) !== normalized) return normalized;
  return `${normalized.slice(0, HALF)}-${normalized.slice(HALF)}`;
}

/**
 * What a screen reader reads for a code: letter by letter, a pause between the halves (02 §10.1: "W D J B, M J H T").
 * Takes the code normalized or formatted.
 */
export function spokenUserCode(code: string): string {
  const letters = code.replaceAll('-', '');
  return `${Array.from(letters.slice(0, HALF)).join(' ')}, ${Array.from(letters.slice(HALF)).join(' ')}`;
}

/**
 * The code field as the friend types or pastes into it (02 §10.1): ASCII letters only, upper case, at most 8, with
 * the "-" inserted after the fourth. Everything else is dropped (spaces, dashes, the "-" itself as typed, digits),
 * so a pasted "wdjb mjht" reads WDJB-MJHT. Letters outside the alphabet (vowels, Y) stay visible, and Continue stays
 * off until normalizeUserCode accepts the value.
 */
export function formatCodeInput(raw: string): string {
  let letters = '';
  for (const ch of raw) {
    const c = upper(ch);
    if (c >= 'A' && c <= 'Z') letters += c;
    if (letters.length === UserCodeLength) break;
  }
  return letters.length > HALF ? `${letters.slice(0, HALF)}-${letters.slice(HALF)}` : letters;
}
