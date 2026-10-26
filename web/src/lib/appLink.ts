// The isshoni:// deep link (docs/m2/02-device-auth.md §12.1) as a web page builds it, for the "Open in the app"
// buttons of 03-desktop-app and 05 and the download page: appLinkFor(serverOrigin, roomId?) is the TypeScript copy
// of Go's deeplink.Build, and both test suites run the cases of internal/client/deeplink/testdata/links.json
// (appLink.node.test.ts).
//
//   isshoni://<host>[:<port>]/                → the server https://<host>[:<port>], its default room
//   isshoni://<host>[:<port>]/r/<roomId>      → that server, room <roomId>
//
// Only https servers have a link (every TLS mode ends with a publicly trusted certificate; dev servers on
// http://localhost are added through the app's own field, never through a link), so a page served over http gets "".
// The link never carries a query or a fragment: nothing but the server and the room travels to the app. The app
// reads it with deeplink.Parse, which is the authority; the host rules here only keep this side from building a link
// that Parse would refuse. The browser's URL parser does the IDNA mapping (UTS #46, as Go's idna.Lookup does), and
// this file adds what Go's canonicalHost has on top of it: the STD3 rules (letters, digits and "-" in each label,
// no "%" or "\"), the hyphen rules (also inside an xn-- label, which it decodes), labels of 1–63 bytes and names
// of at most 253, and netip's spelling of an IPv4-mapped IPv6 address. One rule is left to Parse: RFC 5893's bidi
// rule for a name with a right-to-left label (Arabic or Hebrew letters, Arabic-Indic digits), which needs Unicode's
// bidi classes; for such a name this side may build a link that the app then refuses as invalid.

/** The URL scheme the app registers (06-windows-release §2.3). */
export const APP_LINK_SCHEME = 'isshoni';

/** The room-id rule (01 §4.1): 1–64 of [A-Za-z0-9_-]; Go's deeplink.ValidRoomID. */
const ROOM_ID = /^[A-Za-z0-9_-]{1,64}$/;

/** A link and an address are at most this many bytes (deeplink.MaxBytes). */
const MAX_BYTES = 2048;

/** A space, a control character or a bidi control anywhere in the address (deeplink's checkChars). */
// eslint-disable-next-line no-control-regex -- refusing control characters is the point
const BAD_CHARS = /[\s\u0000-\u001f\u007f-\u009f؜‎‏‪-‮⁦-⁩]/u;

/** A port as written: 1–65535 without a leading zero. */
const PORT = /^[1-9]\d{0,4}$/;

/** A host whose last label is a number (decimal or 0x hex), which browsers read as an IPv4 address. */
const NUMERIC_LAST_LABEL = /(?:^|\.)(?:0x[0-9a-f]*|\d+)$/i;

/** A DNS name in ASCII is at most this many bytes (deeplink's maxHostBytes; no trailing dot). */
const MAX_HOST_BYTES = 253;

/**
 * A label of a host name in ASCII as Go's STD3 rules allow it: 1–63 letters, digits and "-" (deeplink's
 * maxLabelBytes). The URL parser has lower-cased it.
 */
const LDH_LABEL = /^[a-z0-9-]{1,63}$/;

/**
 * Characters UTS #46 lists as disallowed_STD3_valid: ≠, ≮ and ≯ (also as "=", "<" or ">" with U+0338, which
 * normalization composes). Go's STD3 rules refuse them; the URL parser, without those rules, keeps them.
 */
const STD3_DISALLOWED_VALID = /[\u2260\u226e\u226f]/u;

/** An IPv6 literal without its brackets, as NormalizeOrigin reads one: hex digits, dots and two or more ":". */
function bareIPv6(s: string): boolean {
  return /^[0-9a-f:.]+$/i.test(s) && s.indexOf(':') !== s.lastIndexOf(':');
}

export function validRoomId(id: string): boolean {
  return ROOM_ID.test(id);
}

/** An IPv4 address that is no server's: unspecified or multicast (deeplink's serverAddr). */
function badIPv4(a: number, b: number, c: number, d: number): boolean {
  return (a === 0 && b === 0 && c === 0 && d === 0) || (a >= 224 && a <= 239);
}

/** The dotted IPv4 address of an IPv4-mapped IPv6 one as the URL parser writes it (::ffff:<hex>:<hex>), or null. */
function mappedIPv4(v6: string): [number, number, number, number] | null {
  const mapped = /^::ffff:([0-9a-f]{1,4}):([0-9a-f]{1,4})$/.exec(v6);
  if (mapped === null) return null;
  const hi = parseInt(mapped[1] ?? '0', 16);
  const lo = parseInt(mapped[2] ?? '0', 16);
  return [hi >> 8, hi & 0xff, lo >> 8, lo & 0xff];
}

/**
 * Whether a host as the URL parser canonicalized it (lower case, IDN in its xn-- form, IPv4 dotted, IPv6 compressed
 * in brackets) is an address no server has: 0.0.0.0, ::, a multicast address, or an IPv4-mapped form of those.
 */
function notAServer(hostname: string): boolean {
  const v4 = /^(\d+)\.(\d+)\.(\d+)\.(\d+)$/.exec(hostname);
  if (v4 !== null) return badIPv4(Number(v4[1]), Number(v4[2]), Number(v4[3]), Number(v4[4]));
  if (!hostname.startsWith('[')) return false;
  const v6 = hostname.slice(1, -1);
  if (v6 === '::' || v6.startsWith('ff')) return true;
  const mapped = mappedIPv4(v6);
  return mapped !== null && badIPv4(...mapped);
}

/**
 * An IPv6 literal spelled as Go's netip writes it: the URL parser's form (RFC 5952: compressed, lower case), but an
 * IPv4-mapped address with its IPv4 part dotted ("[::ffff:192.0.2.10]", not "[::ffff:c000:20a]").
 */
function netipSpelling(hostname: string): string {
  const mapped = mappedIPv4(hostname.slice(1, -1));
  return mapped === null ? hostname : `[::ffff:${mapped.join('.')}]`;
}

// Punycode's parameters (RFC 3492 §5); damp (700), skew (38), the initial bias (72) and n (128) are written inline.
const PUNY_BASE = 36;
const PUNY_TMIN = 1;
const PUNY_TMAX = 26;
/** Larger than any value a 63-byte label needs; Go's decoder stops at int32 too. */
const PUNY_LIMIT = 0x7fffffff;

/** RFC 3492 §6.1's bias adaptation. */
function punyAdapt(delta: number, points: number, first: boolean): number {
  let d = first ? Math.floor(delta / 700) : Math.floor(delta / 2);
  d += Math.floor(d / points);
  let k = 0;
  while (d > ((PUNY_BASE - PUNY_TMIN) * PUNY_TMAX) / 2) {
    d = Math.floor(d / (PUNY_BASE - PUNY_TMIN));
    k += PUNY_BASE;
  }
  return k + Math.floor(((PUNY_BASE - PUNY_TMIN + 1) * d) / (d + 38));
}

/** The Unicode form of an xn-- label's Punycode part (RFC 3492 §6.2), or null for one that doesn't decode. */
function decodePunycode(input: string): string | null {
  const out: number[] = [];
  const delimiter = input.lastIndexOf('-');
  for (let j = 0; j < Math.max(0, delimiter); j++) out.push(input.charCodeAt(j));
  let n = 128;
  let bias = 72;
  let i = 0;
  for (let pos = delimiter > 0 ? delimiter + 1 : 0; pos < input.length;) {
    const before = i;
    for (let w = 1, k = PUNY_BASE; ; k += PUNY_BASE) {
      // Past the end, charCodeAt gives NaN: no digit, so a label cut short doesn't decode.
      const c = input.charCodeAt(pos++);
      // a–z are 0–25 (the URL parser has lower-cased the label), 0–9 are 26–35.
      const digit = c >= 0x61 && c <= 0x7a ? c - 0x61 : c >= 0x30 && c <= 0x39 ? c - 22 : -1;
      if (digit < 0 || digit * w > PUNY_LIMIT - i) return null;
      i += digit * w;
      const t = k <= bias ? PUNY_TMIN : k >= bias + PUNY_TMAX ? PUNY_TMAX : k - bias;
      if (digit < t) break;
      w *= PUNY_BASE - t;
      if (w > PUNY_LIMIT) return null;
    }
    bias = punyAdapt(i - before, out.length + 1, before === 0);
    n += Math.floor(i / (out.length + 1));
    i %= out.length + 1;
    if (n > 0x10ffff) return null;
    out.splice(i++, 0, n);
  }
  return String.fromCodePoint(...out);
}

/**
 * Go's CheckHyphens (UTS #46 V2 and V3) on a label in Unicode, by its UTF-8 bytes as x/net/idna reads them: no "-"
 * first or last, and not "-" in both the third and the fourth byte of a label longer than four.
 */
function hyphensAllowed(label: string): boolean {
  const b = new TextEncoder().encode(label);
  const hyphen = 0x2d;
  if (b.length === 0 || b[0] === hyphen || b[b.length - 1] === hyphen) return false;
  return !(b.length > 4 && b[2] === hyphen && b[3] === hyphen);
}

/**
 * Whether a host name in its ASCII form (from the URL parser) passes canonicalHost's checks on top of the IDNA
 * mapping: at most 253 bytes; each label 1–63 letters, digits and "-"; the hyphen rules on each label, an xn-- one
 * decoded (it must decode, to something that isn't plain ASCII). The bidi rule is left to Parse (see the top).
 */
function validHostName(hostname: string): boolean {
  if (hostname.length > MAX_HOST_BYTES) return false;
  return hostname.split('.').every((label) => {
    if (!LDH_LABEL.test(label)) return false;
    if (!label.startsWith('xn--')) return hyphensAllowed(label);
    const unicode = decodePunycode(label.slice(4));
    return unicode !== null && !/^\p{ASCII}*$/u.test(unicode) && hyphensAllowed(unicode);
  });
}

/**
 * The host[:port] of an https server origin as a link carries it (Go's NormalizeOrigin with https only), or null:
 * another scheme, a path other than "/", a query or a fragment (even an empty "?" or "#"), userinfo, a bad port, a
 * trailing dot, a numeric host the browser would rewrite ("127.1"), a host name Go's canonicalHost refuses (see the
 * top), a percent escape or a backslash in the host, or an address that is no server's. An origin without a scheme
 * is https, as in Go, and an IPv6 literal without one may also come without its brackets ("2001:db8::1"). The
 * default port 443 is dropped.
 */
function linkAuthority(origin: string): string | null {
  const s = origin.trim();
  if (s === '' || new TextEncoder().encode(s).length > MAX_BYTES || BAD_CHARS.test(s)) return null;
  const sep = s.indexOf('://');
  if (sep >= 0 && s.slice(0, sep).toLowerCase() !== 'https') return null;
  // Two or more ":" can't be host:port, so NormalizeOrigin brackets such a literal (and nothing else) itself.
  const rest = sep >= 0 ? s.slice(sep + 3) : bareIPv6(s) ? `[${s}]` : s;
  const end = rest.search(/[/?#]/);
  const authority = end < 0 ? rest : rest.slice(0, end);
  const path = end < 0 ? '' : rest.slice(end);
  if ((path !== '' && path !== '/') || authority === '' || authority.includes('@')) return null;
  // The URL parser would read a "\" as the path's start and decode a "%41" to "a": the friend would be shown one host
  // and the app would get another. Go decodes nothing and refuses both.
  if (authority.includes('\\') || authority.includes('%')) return null;

  // The host and the port as written.
  let rawHost = authority;
  let port = '';
  if (authority.startsWith('[')) {
    const close = authority.indexOf(']');
    if (close < 0) return null;
    rawHost = authority.slice(0, close + 1);
    const after = authority.slice(close + 1);
    if (after !== '') {
      if (!after.startsWith(':')) return null;
      port = after.slice(1);
      if (port === '') return null;
    }
  } else {
    const colon = authority.lastIndexOf(':');
    if (colon >= 0) {
      rawHost = authority.slice(0, colon);
      port = authority.slice(colon + 1);
      if (port === '') return null;
    }
  }
  if (port !== '' && (!PORT.test(port) || Number(port) > 65535)) return null;
  if (rawHost.endsWith('.') || rawHost.endsWith('.]')) return null;
  if (STD3_DISALLOWED_VALID.test(rawHost.normalize('NFKC'))) return null;

  let url: URL;
  try {
    url = new URL(`https://${authority}/`);
  } catch {
    return null;
  }
  // Nothing of the authority may have become a path, a query or a fragment.
  if (url.pathname !== '/' || url.search !== '' || url.hash !== '') return null;
  const hostname = url.hostname;
  if (hostname === '' || notAServer(hostname)) return null;
  // url.host is the hostname and, unless it is 443, the port.
  if (hostname.startsWith('[')) return netipSpelling(hostname) + url.host.slice(hostname.length);
  // The parser reads "127.1", "0x7f.1" or a fullwidth "１２７.０.０.１" as 127.0.0.1; Go refuses them, since the
  // friend would be shown one host and the app would connect to another.
  if (NUMERIC_LAST_LABEL.test(rawHost.normalize('NFKC')) && hostname !== rawHost) return null;
  // A host name, or an IPv4 address written as the parser writes it (whose labels pass the same checks).
  if (!validHostName(hostname)) return null;
  return url.host;
}

/**
 * The deep link of a server and, optionally, a room (02 §12.1): "isshoni://watch.example.com/r/lounge". The server's
 * link without a room for a room id that breaks the room-id rule, and "" for an origin that isn't https or has a
 * path, a query, a fragment or userinfo (Go's deeplink.Build).
 */
export function appLinkFor(serverOrigin: string, roomId?: string): string {
  const authority = linkAuthority(serverOrigin);
  if (authority === null) return '';
  const room = roomId !== undefined && validRoomId(roomId) ? `r/${roomId}` : '';
  return `${APP_LINK_SCHEME}://${authority}/${room}`;
}
