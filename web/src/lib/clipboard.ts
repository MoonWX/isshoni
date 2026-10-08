// Clipboard writes (05 §16.3, §20): only ever from a click handler. When the async Clipboard API is missing or
// refuses (no permission, an in-app browser, an insecure context), the caller shows the text in a selected read-only
// field instead, so there's no execCommand fallback here.

/** Copies text; true on success, false when the browser refused or can't. Never throws. */
export async function copyText(text: string): Promise<boolean> {
  const clipboard = globalThis.navigator.clipboard as Clipboard | undefined;
  if (typeof clipboard?.writeText !== 'function') return false;
  try {
    await clipboard.writeText(text);
    return true;
  } catch {
    return false;
  }
}
