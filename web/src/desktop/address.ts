// What the server step reads of a typed address before it asks Go (docs/m2/03 §8.1): an invite or setup link gets
// the "open it in your browser first" hint instead of a probe, since the fragment that carries its token must stay
// in the browser. Everything else goes to Auth.Probe as typed: Go normalizes it (servers.Normalize) and drops the
// path, query and fragment.

/** The address as an absolute URL: https:// is added to one without a scheme. null when it can't be one. */
function asURL(raw: string): URL | null {
  const s = raw.trim();
  if (s === '') return null;
  try {
    return new URL(/^[a-z][a-z\d+.-]*:\/\//i.test(s) ? s : `https://${s}`);
  } catch {
    return null;
  }
}

/**
 * The URL to open in the browser when the address is an invite or setup link of a server (its path is /invite or
 * /setup, with anything after it), else null. The link is opened as typed, fragment and all.
 */
export function inviteLink(raw: string): string | null {
  const u = asURL(raw);
  if (u === null || (u.protocol !== 'https:' && u.protocol !== 'http:')) return null;
  return /^\/(?:invite|setup)(?:\/|$)/.test(u.pathname) ? u.href : null;
}

/** The host[:port] of an origin ("https://watch.example.com" → "watch.example.com"); the origin itself otherwise. */
export function hostOf(origin: string): string {
  try {
    return new URL(origin).host;
  } catch {
    return origin;
  }
}
