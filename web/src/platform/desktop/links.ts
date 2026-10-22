// The desktop SPA never navigates away (docs/m2/03 §5.5, layer 1 of 3; Go cancels foreign navigation as layer 2, and
// the /wails/* guard is layer 3). DesktopPlatform installs, at construction:
// - capture-phase click and auxclick (middle button) listeners on document: an <a> (or <area>) whose resolved URL is
//   not on the asset origin is cancelled and handed to openExternal; a modified click or middle click on an
//   asset-origin link, which would open a second window, is cancelled too (React Router leaves those to the browser);
// - window.open = (url) => { openExternal(url); return null; };
// - dragover and drop listeners that cancel the default, so a dropped file or link never navigates.

export interface LinkInterceptionOptions {
  /** Takes every URL that would leave the app; Go validates it (App.OpenExternal, docs/m2/03 §5.5). */
  openExternal: (url: string) => void;
  /** Default: globalThis.window. */
  win?: Window;
}

/** scheme://host[:port], compared as text: a custom scheme's URL.origin is "null" (wails://localhost on macOS). */
function originOf(u: URL): string {
  return `${u.protocol}//${u.host}`;
}

function linkOf(ev: Event): HTMLAnchorElement | HTMLAreaElement | null {
  for (const node of ev.composedPath()) {
    if (node instanceof HTMLAnchorElement || node instanceof HTMLAreaElement) return node;
  }
  return null;
}

/** Installs the listeners and the window.open override; returns the function that removes them (tests). */
export function installLinkInterception(opts: LinkInterceptionOptions): () => void {
  const win = opts.win ?? globalThis.window;
  const doc = win.document;
  const own = (): string => originOf(new URL(win.location.href));

  const onClick = (ev: MouseEvent): void => {
    // auxclick also fires for the right button, which opens a context menu, not the link.
    if (ev.type === 'auxclick' && ev.button !== 1) return;
    const a = linkOf(ev);
    if (a === null || a.href === '') return;
    let url: URL;
    try {
      url = new URL(a.href, win.location.href);
    } catch {
      ev.preventDefault();
      return;
    }
    if (originOf(url) !== own()) {
      ev.preventDefault();
      opts.openExternal(url.href);
      return;
    }
    // An asset-origin link: a plain left click is the router's; anything that opens a window is refused.
    const newWindow =
      ev.type === 'auxclick' || ev.ctrlKey || ev.metaKey || ev.shiftKey || a.getAttribute('target') === '_blank';
    if (newWindow) ev.preventDefault();
  };

  const onDragOver = (ev: DragEvent): void => {
    ev.preventDefault();
    if (ev.dataTransfer) ev.dataTransfer.dropEffect = 'none';
  };
  const onDrop = (ev: DragEvent): void => {
    ev.preventDefault();
  };

  // The descriptor, not the method: restored as it was (tests uninstall).
  const ownOpen = Object.getOwnPropertyDescriptor(win, 'open');
  win.open = (url?: string | URL): WindowProxy | null => {
    if (url === undefined || String(url) === '') return null;
    let target: URL;
    try {
      target = new URL(String(url), win.location.href);
    } catch {
      return null;
    }
    opts.openExternal(target.href);
    return null;
  };

  doc.addEventListener('click', onClick, true);
  doc.addEventListener('auxclick', onClick, true);
  doc.addEventListener('dragover', onDragOver);
  doc.addEventListener('drop', onDrop);
  return () => {
    if (ownOpen) Object.defineProperty(win, 'open', ownOpen);
    else Reflect.deleteProperty(win, 'open');
    doc.removeEventListener('click', onClick, true);
    doc.removeEventListener('auxclick', onClick, true);
    doc.removeEventListener('dragover', onDragOver);
    doc.removeEventListener('drop', onDrop);
  };
}
