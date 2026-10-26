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
// that Parse would refuse.

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

export function validRoomId(id: string): boolean {
  return ROOM_ID.test(id);
}

/** An IPv4 address that is no server's: unspecified or multicast (deeplink's serverAddr). */
function badIPv4(a: number, b: number, c: number, d: number): boolean {
  return (a === 0 && b === 0 && c === 0 && d === 0) || (a >= 224 && a <= 239);
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
  // The URL parser writes an IPv4-mapped address as ::ffff:<hex>:<hex>.
  const mapped = /^::ffff:([0-9a-f]{1,4}):([0-9a-f]{1,4})$/.exec(v6);
  if (mapped === null) return false;
  const hi = parseInt(mapped[1] ?? '0', 16);
  const lo = parseInt(mapped[2] ?? '0', 16);
  return badIPv4(hi >> 8, hi & 0xff, lo >> 8, lo & 0xff);
}

/**
 * The host[:port] of an https server origin as a link carries it (Go's NormalizeOrigin with https only), or null:
 * another scheme, a path other than "/", a query or a fragment (even an empty "?" or "#"), userinfo, a bad port, a
 * trailing dot, a numeric host the browser would rewrite ("127.1"), or an address that is no server's. An origin
 * without a scheme is https, as in Go. The default port 443 is dropped.
 */
function linkAuthority(origin: string): string | null {
  const s = origin.trim();
  if (s === '' || new TextEncoder().encode(s).length > MAX_BYTES || BAD_CHARS.test(s)) return null;
  const sep = s.indexOf('://');
  if (sep >= 0 && s.slice(0, sep).toLowerCase() !== 'https') return null;
  const rest = sep >= 0 ? s.slice(sep + 3) : s;
  const end = rest.search(/[/?#]/);
  const authority = end < 0 ? rest : rest.slice(0, end);
  const path = end < 0 ? '' : rest.slice(end);
  if ((path !== '' && path !== '/') || authority === '' || authority.includes('@')) return null;

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

  let url: URL;
  try {
    url = new URL(`https://${authority}/`);
  } catch {
    return null;
  }
  const hostname = url.hostname;
  // The parser reads "127.1", "0x7f.1" or a fullwidth "１２７.０.０.１" as 127.0.0.1; Go refuses them, since the
  // friend would be shown one host and the app would connect to another.
  if (!rawHost.startsWith('[') && NUMERIC_LAST_LABEL.test(rawHost.normalize('NFKC')) && hostname !== rawHost) {
    return null;
  }
  if (hostname === '' || notAServer(hostname)) return null;
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
